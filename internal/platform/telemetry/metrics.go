// Package telemetry exposes bounded process-local metrics for Prometheus.
package telemetry

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics supports concurrent recording and scraping. Its zero value is ready
// for use; every instance owns a registry and never registers global collectors.
type Metrics struct {
	Requests                  atomic.Uint64
	Processed                 atomic.Uint64
	Rejected                  atomic.Uint64
	Pending                   atomic.Uint64
	Failed                    atomic.Uint64
	Duplicates                atomic.Uint64
	Retries                   atomic.Uint64
	Published                 atomic.Uint64
	ReconciliationDivergences atomic.Uint64
	ProcessingNanoseconds     atomic.Uint64

	once                sync.Once
	registry            *prometheus.Registry
	httpDuration        *prometheus.HistogramVec
	processingDuration  *prometheus.HistogramVec
	results             *prometheus.CounterVec
	sqsDuration         *prometheus.HistogramVec
	sqsOperations       *prometheus.CounterVec
	sqsMessages         *prometheus.CounterVec
	sqsRetries          *prometheus.CounterVec
	conflicts           *prometheus.CounterVec
	databaseConflicts   *prometheus.CounterVec
	queueDepth          *prometheus.GaugeVec
	queueCollection     *prometheus.GaugeVec
	queueLastSuccess    *prometheus.GaugeVec
	dlqConfigured       *prometheus.GaugeVec
	queueAge            *prometheus.GaugeVec
	queueAgeAvailable   *prometheus.GaugeVec
	queueAgeCollection  *prometheus.GaugeVec
	queueAgeTimestamp   *prometheus.GaugeVec
	databaseCollection  prometheus.Gauge
	databaseLastSuccess prometheus.Gauge
	outboxPending       prometheus.Gauge
	outboxOldest        prometheus.Gauge
	pendingReferences   prometheus.Gauge
}

func (m *Metrics) initialize() {
	m.once.Do(func() {
		m.registry = prometheus.NewRegistry()
		buckets := []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
		m.httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "jungle_http_request_duration_seconds",
			Help:    "Business HTTP request latency, including authentication and response writing.",
			Buckets: buckets,
		}, []string{"route", "method", "status"})
		m.processingDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "jungle_processing_duration_seconds",
			Help:    "Financial processing latency, including persistent replays, reference retries and errors.",
			Buckets: buckets,
		}, []string{"transport", "status"})
		m.results = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "jungle_transaction_results_total",
			Help: "Financial outcomes per delivery or reference attempt, including persistent replays.",
		}, []string{"transport", "status"})
		m.sqsDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "jungle_sqs_operation_duration_seconds",
			Help:    "SQS SDK call latency, including SDK retries and receive long polling.",
			Buckets: buckets,
		}, []string{"operation", "result"})
		m.sqsOperations = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "jungle_sqs_operations_total", Help: "Completed SQS SDK calls by operation and result.",
		}, []string{"operation", "result"})
		m.sqsMessages = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "jungle_sqs_messages_total", Help: "Messages received, redelivered, deleted or published by this process.",
		}, []string{"action"})
		m.sqsRetries = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "jungle_sqs_retries_total", Help: "Consumer deliveries retained for retry or broker redrive, by reason.",
		}, []string{"reason"})
		m.conflicts = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "jungle_conflicts_total", Help: "Application conflicts returned to HTTP or SQS callers.",
		}, []string{"transport"})
		m.databaseConflicts = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "jungle_database_conflicts_total", Help: "Database write conflicts, including races resolved by replay.",
		}, []string{"kind"})
		m.queueDepth = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "jungle_sqs_queue_messages", Help: "Approximate queue depth; NaN when collection is unavailable.",
		}, []string{"queue", "state"})
		m.queueCollection = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "jungle_sqs_collection_success", Help: "Whether the latest queue attribute collection succeeded.",
		}, []string{"queue"})
		m.queueLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "jungle_sqs_last_success_timestamp_seconds", Help: "Timestamp of the last successful queue attribute collection.",
		}, []string{"queue"})
		m.dlqConfigured = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "jungle_sqs_dlq_configured", Help: "Whether the source queue has a redrive policy; NaN when unknown.",
		}, []string{"queue"})
		m.queueAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "jungle_sqs_oldest_message_age_seconds", Help: "Latest CloudWatch oldest-message age; NaN without a recent sample.",
		}, []string{"queue"})
		m.queueAgeAvailable = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "jungle_sqs_oldest_message_age_available", Help: "Whether a recent oldest-message age sample is available.",
		}, []string{"queue"})
		m.queueAgeCollection = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "jungle_sqs_age_collection_success", Help: "Whether the latest CloudWatch age request succeeded.",
		}, []string{"queue"})
		m.queueAgeTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "jungle_sqs_oldest_message_age_timestamp_seconds", Help: "CloudWatch sample timestamp; zero when unavailable.",
		}, []string{"queue"})
		m.databaseCollection = prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "jungle_database_collection_success", Help: "Whether the latest financial backlog collection succeeded.",
		})
		m.databaseLastSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "jungle_database_last_success_timestamp_seconds", Help: "Timestamp of the last successful financial backlog collection.",
		})
		m.outboxPending = prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "jungle_outbox_pending", Help: "Unpublished outbox events; NaN when collection is unavailable.",
		})
		m.outboxOldest = prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "jungle_outbox_oldest_seconds", Help: "Age of the oldest unpublished event; NaN when collection is unavailable.",
		})
		m.pendingReferences = prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "jungle_pending_references", Help: "Transactions waiting for a reference; NaN when collection is unavailable.",
		})
		m.registry.MustRegister(
			m.httpDuration, m.processingDuration, m.results, m.sqsDuration, m.sqsOperations,
			m.sqsMessages, m.sqsRetries, m.conflicts, m.databaseConflicts, m.queueDepth,
			m.queueCollection, m.queueLastSuccess, m.dlqConfigured, m.queueAge,
			m.queueAgeAvailable, m.queueAgeCollection, m.queueAgeTimestamp,
			m.databaseCollection, m.databaseLastSuccess, m.outboxPending,
			m.outboxOldest, m.pendingReferences,
		)

		counters := []struct {
			name  string
			help  string
			value *atomic.Uint64
		}{
			{"jungle_requests_total", "Financial processing attempts over HTTP, SQS and the reference worker.", &m.Requests},
			{"jungle_processed_total", "Processed submission results, including replays.", &m.Processed},
			{"jungle_rejected_total", "Rejected submission results, including replays.", &m.Rejected},
			{"jungle_pending_total", "Pending submission results, including replays.", &m.Pending},
			{"jungle_failed_total", "Permanent failure results, including replays.", &m.Failed},
			{"jungle_duplicates_total", "Persistent submission replays recognized by this process.", &m.Duplicates},
			{"jungle_retries_total", "Consumer retry deliveries and failed outbox publications.", &m.Retries},
			{"jungle_events_published_total", "Outbox sends accepted by SQS, before lease completion.", &m.Published},
			{"jungle_reconciliation_divergences_total", "Reconciliation divergences observed by this process.", &m.ReconciliationDivergences},
			{"jungle_processing_nanoseconds_total", "Cumulative shared submission latency in nanoseconds.", &m.ProcessingNanoseconds},
		}

		for _, counter := range counters {
			m.registry.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
				Name: counter.name, Help: counter.help,
			}, func() float64 { return float64(counter.value.Load()) }))
		}

		m.outboxPending.Set(math.NaN())
		m.outboxOldest.Set(math.NaN())
		m.pendingReferences.Set(math.NaN())

		for _, role := range []string{"requests", "events"} {
			m.dlqConfigured.WithLabelValues(role).Set(math.NaN())
		}

		for _, role := range []string{"requests", "events", "requests_dlq", "events_dlq"} {
			m.queueCollection.WithLabelValues(role).Set(0)
			m.queueLastSuccess.WithLabelValues(role).Set(0)
			m.queueAge.WithLabelValues(role).Set(math.NaN())
			m.queueAgeAvailable.WithLabelValues(role).Set(0)
			m.queueAgeCollection.WithLabelValues(role).Set(0)
			m.queueAgeTimestamp.WithLabelValues(role).Set(0)

			for _, state := range []string{"visible", "inflight", "delayed"} {
				m.queueDepth.WithLabelValues(role, state).Set(math.NaN())
			}
		}
	})
}

// Handler serves an in-memory snapshot without database or broker calls.
func (m *Metrics) Handler() http.Handler {
	m.initialize()

	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *Metrics) ObserveHTTP(
	route string,
	method string,
	status int,
	elapsed time.Duration,
) {
	m.initialize()
	m.httpDuration.WithLabelValues(route, method, strconv.Itoa(status)).Observe(elapsed.Seconds())
}

func (m *Metrics) ObserveProcessing(
	transport string,
	status string,
	replay bool,
	elapsed time.Duration,
) {
	m.initialize()
	m.Requests.Add(1)
	m.ProcessingNanoseconds.Add(uint64(elapsed))

	if replay {
		m.Duplicates.Add(1)
	}

	switch status {
	case "PROCESSED":
		m.Processed.Add(1)
	case "REJECTED":
		m.Rejected.Add(1)
	case "PENDING", "PENDING_REFERENCE":
		m.Pending.Add(1)
	case "FAILED":
		m.Failed.Add(1)
	default:
		status = "ERROR"
	}

	m.results.WithLabelValues(transport, status).Inc()
	m.processingDuration.WithLabelValues(transport, status).Observe(elapsed.Seconds())
}

func (m *Metrics) ObserveSQS(operation string, result string, elapsed time.Duration) {
	m.initialize()
	m.sqsOperations.WithLabelValues(operation, result).Inc()
	m.sqsDuration.WithLabelValues(operation, result).Observe(elapsed.Seconds())
}

func (m *Metrics) AddSQSMessages(action string, count int) {
	m.initialize()
	m.sqsMessages.WithLabelValues(action).Add(float64(count))
}

func (m *Metrics) RecordSQSRetry(reason string) {
	m.initialize()
	m.sqsRetries.WithLabelValues(reason).Inc()
}

func (m *Metrics) RecordConflict(transport string) {
	m.initialize()
	m.conflicts.WithLabelValues(transport).Inc()
}

func (m *Metrics) RecordDatabaseConflict(kind string) {
	m.initialize()
	m.databaseConflicts.WithLabelValues(kind).Inc()
}

func (m *Metrics) SetDatabase(pending int64, oldest int64, references int64, err error) {
	m.initialize()

	if err != nil {
		m.databaseCollection.Set(0)
		m.outboxPending.Set(math.NaN())
		m.outboxOldest.Set(math.NaN())
		m.pendingReferences.Set(math.NaN())

		return
	}

	m.outboxPending.Set(float64(pending))
	m.outboxOldest.Set(float64(oldest))
	m.pendingReferences.Set(float64(references))
	m.databaseLastSuccess.SetToCurrentTime()
	m.databaseCollection.Set(1)
}

func (m *Metrics) SetQueue(
	role string,
	visible int64,
	inflight int64,
	delayed int64,
	err error,
) {
	m.initialize()

	if err != nil {
		m.queueCollection.WithLabelValues(role).Set(0)

		for _, state := range []string{"visible", "inflight", "delayed"} {
			m.queueDepth.WithLabelValues(role, state).Set(math.NaN())
		}

		return
	}

	m.queueDepth.WithLabelValues(role, "visible").Set(float64(visible))
	m.queueDepth.WithLabelValues(role, "inflight").Set(float64(inflight))
	m.queueDepth.WithLabelValues(role, "delayed").Set(float64(delayed))
	m.queueLastSuccess.WithLabelValues(role).SetToCurrentTime()
	m.queueCollection.WithLabelValues(role).Set(1)
}

func (m *Metrics) SetDLQ(role string, configured float64) {
	m.initialize()
	m.dlqConfigured.WithLabelValues(role).Set(configured)
}

// SetQueueAge distinguishes a successful API call without samples from a valid
// zero-age sample. Timestamp is the provider's sample time, not scrape time.
func (m *Metrics) SetQueueAge(
	role string,
	seconds float64,
	timestamp time.Time,
	err error,
) {
	m.initialize()

	if err != nil {
		timestamp = time.Time{}
		m.queueAgeCollection.WithLabelValues(role).Set(0)
	} else {
		m.queueAgeCollection.WithLabelValues(role).Set(1)
	}

	if timestamp.IsZero() {
		m.queueAge.WithLabelValues(role).Set(math.NaN())
		m.queueAgeTimestamp.WithLabelValues(role).Set(0)
		m.queueAgeAvailable.WithLabelValues(role).Set(0)

		return
	}

	m.queueAge.WithLabelValues(role).Set(seconds)
	m.queueAgeTimestamp.WithLabelValues(role).Set(float64(timestamp.Unix()))
	m.queueAgeAvailable.WithLabelValues(role).Set(1)
}
