package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"jungle/internal/platform/config"
	"jungle/internal/platform/telemetry"
	"jungle/internal/wagering/application"
)

func TestMetricsRemainAvailableWithoutDependencies(t *testing.T) {
	metrics := &telemetry.Metrics{}
	api := New(
		nil,
		nil,
		nil,
		nil,
		config.Config{OperationTimeout: time.Second},
		metrics,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	handler := api.Handler()

	for _, path := range []string{"/wallets/private-wallet-one", "/wallets/private-wallet-two"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest("GET", path, nil))

		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("missing credential response = %d, want 401", recorder.Code)
		}
	}

	api.fail(httptest.NewRecorder(), application.ErrConflict)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("scrape with unavailable database and queue = %d", recorder.Code)
	}

	body := recorder.Body.String()
	if strings.Contains(body, "private-wallet") {
		t.Fatal("wallet identifiers leaked into metric labels")
	}

	if !strings.Contains(
		body,
		`jungle_http_request_duration_seconds_count{method="GET",route="/wallets/{walletID}",status="401"} 2`,
	) {
		t.Fatal("authentication failures were not observed with a bounded route label")
	}

	if !strings.Contains(body, `jungle_conflicts_total{transport="http"} 1`) {
		t.Fatal("HTTP conflict was not counted")
	}
}

func TestResponseStatusPreservesFirstHeader(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := &responseStatus{ResponseWriter: recorder}
	writer.WriteHeader(http.StatusCreated)
	writer.WriteHeader(http.StatusBadGateway)

	if writer.status != http.StatusCreated {
		t.Fatalf("observed response status = %d, want 201", writer.status)
	}

	if recorder.Code != http.StatusCreated {
		t.Fatalf("response wrapper changed the wire status: %d", recorder.Code)
	}
}
