package telemetry

import (
	"errors"
	"math"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
)

func metric(
	t *testing.T,
	metrics *Metrics,
	name string,
	labels map[string]string,
) *dto.Metric {
	t.Helper()
	metrics.initialize()

	families, err := metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}

	for _, family := range families {
		if family.GetName() != name {
			continue
		}

		for _, sample := range family.Metric {
			matches := len(sample.Label) == len(labels)

			for _, label := range sample.Label {
				if labels[label.GetName()] != label.GetValue() {
					matches = false
				}
			}

			if matches {
				return sample
			}
		}
	}

	t.Fatalf("metric %s with labels %v was not exported", name, labels)

	return nil
}

func TestConcurrentProcessingAndScraping(t *testing.T) {
	metrics := &Metrics{}
	var group sync.WaitGroup

	for worker := range 8 {
		group.Go(func() {
			transport := "http"
			if worker%2 == 0 {
				transport = "sqs"
			}

			for i := range 25 {
				metrics.ObserveProcessing(transport, "PROCESSED", i == 0, 10*time.Millisecond)
				recorder := httptest.NewRecorder()
				metrics.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))

				if recorder.Code != 200 {
					t.Errorf("concurrent scrape status = %d", recorder.Code)
				}
			}
		})
	}

	group.Wait()

	if got := metrics.Requests.Load(); got != 200 {
		t.Fatalf("requests = %d, want 200", got)
	}

	if got := metrics.Duplicates.Load(); got != 8 {
		t.Fatalf("duplicates = %d, want 8", got)
	}

	for _, transport := range []string{"http", "sqs"} {
		histogram := metric(t, metrics, "jungle_processing_duration_seconds", map[string]string{
			"transport": transport, "status": "PROCESSED",
		}).GetHistogram()

		if histogram.GetSampleCount() != 100 {
			t.Fatalf("%s processing observations = %d, want 100", transport, histogram.GetSampleCount())
		}

		if math.Abs(histogram.GetSampleSum()-1) > 1e-9 {
			t.Fatalf("%s latency sum = %g, want 1 second", transport, histogram.GetSampleSum())
		}
	}
}

func TestHistogramBoundariesAndLegacyCounters(t *testing.T) {
	metrics := &Metrics{}

	for _, duration := range []time.Duration{0, 5 * time.Millisecond, 100 * time.Millisecond} {
		metrics.ObserveProcessing("http", "unexpected", false, duration)
	}

	histogram := metric(t, metrics, "jungle_processing_duration_seconds", map[string]string{
		"transport": "http", "status": "ERROR",
	}).GetHistogram()

	if histogram.GetSampleCount() != 3 {
		t.Fatalf("observations = %d, want 3 including zero latency", histogram.GetSampleCount())
	}

	for _, bucket := range histogram.Bucket {
		if bucket.GetUpperBound() == 0.005 {
			if bucket.GetCumulativeCount() != 2 {
				t.Fatalf("5ms cumulative count = %d, want 2", bucket.GetCumulativeCount())
			}
		}
	}

	metrics.Duplicates.Add(7)
	legacy := metric(t, metrics, "jungle_duplicates_total", nil).GetCounter().GetValue()
	if legacy != 7 {
		t.Fatalf("legacy duplicate counter = %g, want 7", legacy)
	}
}

func TestCollectionFailureDoesNotLookLikeAnEmptyQueue(t *testing.T) {
	metrics := &Metrics{}
	metrics.SetQueue("requests_dlq", 2, 1, 0, nil)
	metrics.SetDatabase(3, 10, 1, nil)
	failure := errors.New("dependency unavailable")
	metrics.SetQueue("requests_dlq", 0, 0, 0, failure)
	metrics.SetDatabase(0, 0, 0, failure)

	depth := metric(t, metrics, "jungle_sqs_queue_messages", map[string]string{
		"queue": "requests_dlq", "state": "visible",
	}).GetGauge().GetValue()

	if !math.IsNaN(depth) {
		t.Fatalf("failed queue collection reported %g instead of unknown", depth)
	}

	if got := metric(t, metrics, "jungle_sqs_collection_success", map[string]string{
		"queue": "requests_dlq",
	}).GetGauge().GetValue(); got != 0 {
		t.Fatalf("failed queue collection status = %g", got)
	}

	if got := metric(t, metrics, "jungle_outbox_pending", nil).GetGauge().GetValue(); !math.IsNaN(got) {
		t.Fatalf("failed database collection reported %g instead of unknown", got)
	}

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))

	if recorder.Code != 200 {
		t.Fatalf("dependency failure prevented scraping process metrics: %d", recorder.Code)
	}

	if !strings.Contains(recorder.Body.String(), "jungle_requests_total 0") {
		t.Fatal("dependency failure hid process counters")
	}
}

func TestAgeAvailability(t *testing.T) {
	tests := []struct {
		name      string
		timestamp time.Time
		err       error
		available float64
		success   float64
	}{
		{name: "missing sample", success: 1},
		{name: "valid zero age", timestamp: time.Now(), available: 1, success: 1},
		{name: "failed provider", err: errors.New("unavailable")},
		{name: "error invalidates sample", timestamp: time.Now(), err: errors.New("unavailable")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metrics := &Metrics{}
			metrics.SetQueueAge("requests_dlq", 0, tt.timestamp, tt.err)
			labels := map[string]string{"queue": "requests_dlq"}
			available := metric(t, metrics, "jungle_sqs_oldest_message_age_available", labels).GetGauge().GetValue()

			if available != tt.available {
				t.Fatalf("age availability = %g, want %g", available, tt.available)
			}

			success := metric(t, metrics, "jungle_sqs_age_collection_success", labels).GetGauge().GetValue()
			if success != tt.success {
				t.Fatalf("provider collection status = %g, want %g", success, tt.success)
			}

			age := metric(t, metrics, "jungle_sqs_oldest_message_age_seconds", labels).GetGauge().GetValue()
			if tt.available == 0 {
				if !math.IsNaN(age) {
					t.Fatalf("missing sample reported age %g", age)
				}
			} else if age != 0 {
				t.Fatalf("valid zero-age sample = %g", age)
			}
		})
	}
}

func TestProcessingOutcomesIncludeReplaysAndErrors(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		counter    string
		wantStatus string
	}{
		{name: "processed replay", status: "PROCESSED", counter: "jungle_processed_total", wantStatus: "PROCESSED"},
		{name: "business rejection", status: "REJECTED", counter: "jungle_rejected_total", wantStatus: "REJECTED"},
		{name: "pending", status: "PENDING", counter: "jungle_pending_total", wantStatus: "PENDING"},
		{name: "reference retry", status: "PENDING_REFERENCE", counter: "jungle_pending_total", wantStatus: "PENDING_REFERENCE"},
		{name: "permanent failure replay", status: "FAILED", counter: "jungle_failed_total", wantStatus: "FAILED"},
		{name: "infrastructure error", status: "ERROR", wantStatus: "ERROR"},
		{name: "unknown outcome", status: "unrecognized", wantStatus: "ERROR"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metrics := &Metrics{}
			metrics.ObserveProcessing("reference", tt.status, true, time.Millisecond)
			labels := map[string]string{"transport": "reference", "status": tt.wantStatus}
			result := metric(t, metrics, "jungle_transaction_results_total", labels).GetCounter().GetValue()

			if result != 1 {
				t.Fatalf("result counter = %g, want one attempt", result)
			}

			if metrics.Requests.Load() != 1 {
				t.Fatal("attempt was not counted")
			}

			if metrics.Duplicates.Load() != 1 {
				t.Fatal("replay was not counted")
			}

			if tt.counter == "" {
				return
			}

			if got := metric(t, metrics, tt.counter, nil).GetCounter().GetValue(); got != 1 {
				t.Fatalf("legacy result counter = %g, want one replay result", got)
			}
		})
	}
}
