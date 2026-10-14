// Package telemetry keeps bounded process-local counters for Prometheus scraping.
package telemetry

import "sync/atomic"

type Metrics struct {
	Requests                  atomic.Uint64
	Processed                 atomic.Uint64
	Rejected                  atomic.Uint64
	Pending                   atomic.Uint64
	Duplicates                atomic.Uint64
	Retries                   atomic.Uint64
	Published                 atomic.Uint64
	ReconciliationDivergences atomic.Uint64
	ProcessingNanoseconds     atomic.Uint64
}
