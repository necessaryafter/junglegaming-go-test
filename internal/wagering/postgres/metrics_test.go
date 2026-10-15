package postgres

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"jungle/internal/platform/telemetry"
	"jungle/internal/wagering/application"
)

func TestClassifyConflictPreservesDatabaseCause(t *testing.T) {
	cause := &pgconn.PgError{Code: "23505", ConstraintName: "unique_external_transaction"}
	err := classify(cause)

	if !errors.Is(err, application.ErrConflict) {
		t.Fatalf("classification = %v, want application conflict", err)
	}

	var restored *pgconn.PgError

	if !errors.As(err, &restored) {
		t.Fatal("classification lost the database cause")
	}

	if restored != cause {
		t.Fatal("classification replaced the database cause")
	}
}

func TestDatabaseConflictCategories(t *testing.T) {
	metrics := &telemetry.Metrics{}
	store := New(nil, nil, metrics)

	for _, code := range []string{"23505", "40001", "40P01", "55P03", "23514"} {
		store.observeConflict(fmt.Errorf("wrapped SQL failure: %w", &pgconn.PgError{Code: code}))
	}

	store.observeConflict(application.ErrConflict)
	store.observeConflict(nil)
	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))

	for _, kind := range []string{"uniqueness", "serialization", "deadlock", "lock_unavailable"} {
		want := fmt.Sprintf("jungle_database_conflicts_total{kind=%q} 1", kind)
		if !strings.Contains(recorder.Body.String(), want) {
			t.Fatalf("missing database conflict metric %s", want)
		}
	}

	if strings.Contains(recorder.Body.String(), "23514") {
		t.Fatal("integrity failures were misclassified as retryable database conflicts")
	}
}
