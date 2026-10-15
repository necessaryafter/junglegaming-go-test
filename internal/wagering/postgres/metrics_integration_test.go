//go:build integration

package postgres_test

import (
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"jungle/internal/platform/telemetry"
	"jungle/internal/wagering/application"
	"jungle/internal/wagering/postgres"
)

func TestRealWalletConflictMetrics(t *testing.T) {
	fixture := setup(t)
	metrics := &telemetry.Metrics{}
	store := postgres.New(fixture.pool, slog.Default(), metrics)
	playerID := id(t)
	initial := amount(t, "100.00")

	if _, err := store.Open(t.Context(), playerID, initial); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Open(t.Context(), playerID, initial); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("duplicate wallet = %v, want conflict", err)
	}

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(recorder.Body.String(), `jungle_database_conflicts_total{kind="uniqueness"} 1`) {
		t.Fatal("real uniqueness violation was not counted")
	}

	state, err := store.Statistics(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if state.OutboxPending != 2 {
		t.Fatalf("outbox pending = %d, want two events from one committed opening", state.OutboxPending)
	}

	if state.PendingReferences != 0 {
		t.Fatalf("pending references = %d, want zero", state.PendingReferences)
	}
}
