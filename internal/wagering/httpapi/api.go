// Package httpapi maps HTTP contracts to the shared application service.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"jungle/internal/platform/auth"
	"jungle/internal/platform/config"
	"jungle/internal/platform/queue"
	"jungle/internal/platform/telemetry"
	"jungle/internal/wagering/application"
	"jungle/internal/wagering/domain/money"
	"jungle/internal/wagering/domain/wager"
	"jungle/internal/wagering/postgres"
)

type API struct {
	service *application.Service
	auth    *auth.Verifier
	store   *postgres.Store
	queue   *queue.Client
	cfg     config.Config
	metrics *telemetry.Metrics
	logger  *slog.Logger
}

func New(
	service *application.Service,
	verifier *auth.Verifier,
	store *postgres.Store,
	queue *queue.Client,
	cfg config.Config,
	metrics *telemetry.Metrics,
	logger *slog.Logger,
) *API {
	return &API{service: service, auth: verifier, store: store, queue: queue, cfg: cfg, metrics: metrics, logger: logger}
}

type endpoint func(http.ResponseWriter, *http.Request, application.Principal)

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	a.Register(mux)

	return mux
}

// Register contributes the module's routes to the process HTTP server.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		a.write(w, http.StatusOK, map[string]string{"status": "live"})
	})

	mux.HandleFunc("GET /health/ready", a.ready)
	mux.HandleFunc("GET /metrics", a.scrape)
	mux.HandleFunc("POST /wallets", a.instrument("POST /wallets", a.authorize(a.open)))
	mux.HandleFunc("GET /wallets/{walletID}", a.instrument("GET /wallets/{walletID}", a.authorize(a.wallet)))
	mux.HandleFunc(
		"GET /wallets/{walletID}/ledger",
		a.instrument("GET /wallets/{walletID}/ledger", a.authorize(a.ledger)),
	)
	mux.HandleFunc(
		"POST /wallets/{walletID}/reconciliation",
		a.instrument("POST /wallets/{walletID}/reconciliation", a.authorize(a.reconcile)),
	)
	mux.HandleFunc(
		"POST /wagering/transactions",
		a.instrument("POST /wagering/transactions", a.authorize(a.submit)),
	)
	mux.HandleFunc(
		"GET /wagering/transactions/{transactionID}",
		a.instrument("GET /wagering/transactions/{transactionID}", a.authorize(a.transaction)),
	)
	mux.HandleFunc(
		"GET /providers/{providerID}/wagering/transactions/{externalID}",
		a.instrument("GET /providers/{providerID}/wagering/transactions/{externalID}", a.authorize(a.transaction)),
	)
}

func (a *API) authorize(next endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), a.cfg.OperationTimeout)
		defer cancel()

		parts := strings.Fields(r.Header.Get("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			w.Header().Set("WWW-Authenticate", "Bearer")
			a.write(w, http.StatusUnauthorized, map[string]string{"code": "UNAUTHORIZED"})

			return
		}

		principal, err := a.auth.Verify(ctx, parts[1])
		if err != nil {
			w.Header().Set("WWW-Authenticate", "Bearer")
			a.write(w, http.StatusUnauthorized, map[string]string{"code": "UNAUTHORIZED"})

			return
		}

		next(w, r.WithContext(ctx), principal)
	}
}

func decode(w http.ResponseWriter, r *http.Request, value any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 262144)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(value); err != nil {
		return errors.Join(application.ErrInvalid, err)
	}

	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return application.ErrInvalid
	}

	return nil
}

func (a *API) open(
	w http.ResponseWriter,
	r *http.Request,
	principal application.Principal,
) {
	var input struct {
		PlayerID       string      `json:"playerId"`
		InitialBalance money.Money `json:"initialBalance"`
	}

	if err := decode(w, r, &input); err != nil {
		a.fail(w, err)

		return
	}

	state, err := a.service.Open(r.Context(), principal, input.PlayerID, input.InitialBalance)
	if err != nil {
		a.fail(w, err)

		return
	}

	a.write(w, http.StatusCreated, state)
}

func (a *API) wallet(
	w http.ResponseWriter,
	r *http.Request,
	principal application.Principal,
) {
	state, err := a.service.Wallet(r.Context(), principal, r.PathValue("walletID"))
	if err != nil {
		a.fail(w, err)

		return
	}

	a.write(w, http.StatusOK, state)
}

func (a *API) submit(
	w http.ResponseWriter,
	r *http.Request,
	principal application.Principal,
) {
	var input wager.Input

	if err := decode(w, r, &input); err != nil {
		a.fail(w, err)

		return
	}

	start := time.Now()
	result, err := a.service.Submit(r.Context(), principal, application.Command{
		Input:         input,
		Key:           r.Header.Get("Idempotency-Key"),
		CorrelationID: r.Header.Get("X-Correlation-ID"),
	})
	if err != nil {
		a.metrics.ObserveProcessing("http", "ERROR", false, time.Since(start))
		a.fail(w, err)

		return
	}

	a.metrics.ObserveProcessing("http", string(result.Status), result.IdempotentReplay, time.Since(start))

	status := http.StatusOK

	switch result.Status {
	case wager.Rejected:
		status = http.StatusUnprocessableEntity
	case wager.Pending, wager.PendingReference:
		status = http.StatusAccepted
	}

	a.logger.Info("transaction handled", "transactionId", result.TransactionID, "walletId", input.WalletID,
		"providerId", principal.ProviderID, "correlationId", r.Header.Get("X-Correlation-ID"), "status", result.Status)
	a.write(w, status, result)
}

func (a *API) transaction(
	w http.ResponseWriter,
	r *http.Request,
	principal application.Principal,
) {
	state, err := a.service.Transaction(
		r.Context(),
		principal,
		r.PathValue("transactionID"),
		r.PathValue("providerID"),
		r.PathValue("externalID"),
	)
	if err != nil {
		a.fail(w, err)

		return
	}

	a.write(w, http.StatusOK, state)
}

func (a *API) ledger(
	w http.ResponseWriter,
	r *http.Request,
	principal application.Principal,
) {
	limit := 50

	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			a.fail(w, application.ErrInvalid)

			return
		}

		limit = parsed
	}

	page, err := a.service.Ledger(
		r.Context(),
		principal,
		r.PathValue("walletID"),
		r.URL.Query().Get("cursor"),
		limit,
	)
	if err != nil {
		a.fail(w, err)

		return
	}

	a.write(w, http.StatusOK, page)
}

func (a *API) reconcile(
	w http.ResponseWriter,
	r *http.Request,
	principal application.Principal,
) {
	result, err := a.service.Reconcile(r.Context(), principal, r.PathValue("walletID"))
	if err != nil {
		a.fail(w, err)

		return
	}

	if !result.Consistent {
		a.metrics.ReconciliationDivergences.Add(1)
		a.logger.Error("wallet reconciliation diverged", "walletId", result.WalletID)
	}

	a.write(w, http.StatusOK, result)
}

func (a *API) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	if err := a.store.Ready(ctx); err != nil {
		a.fail(w, err)

		return
	}

	if err := a.queue.Ready(ctx); err != nil {
		a.fail(w, err)

		return
	}

	a.write(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (a *API) scrape(w http.ResponseWriter, r *http.Request) {
	a.metrics.Handler().ServeHTTP(w, r)
}

func (a *API) fail(w http.ResponseWriter, err error) {
	status, code := http.StatusServiceUnavailable, "UNAVAILABLE"

	switch {
	case errors.Is(err, application.ErrInvalid):
		status, code = http.StatusBadRequest, "INVALID_REQUEST"
	case errors.Is(err, application.ErrForbidden):
		status, code = http.StatusForbidden, "FORBIDDEN"
	case errors.Is(err, application.ErrConflict):
		a.metrics.RecordConflict("http")
		status, code = http.StatusConflict, "CONFLICT"
	case errors.Is(err, application.ErrNotFound):
		status, code = http.StatusNotFound, "NOT_FOUND"
	default:
		a.logger.Error("request dependency failed")
	}

	a.write(w, status, map[string]string{"code": code})
}

func (a *API) write(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		a.logger.Error("response encoding failed")
		http.Error(w, `{"code":"INTERNAL_ERROR"}`, http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if _, err := w.Write(data); err != nil {
		a.logger.Warn("response write failed")
	}
}
