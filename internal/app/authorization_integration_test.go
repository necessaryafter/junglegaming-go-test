//go:build integration

package app_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"jungle/internal/wagering/application"
	"jungle/internal/wagering/domain/wager"
	"jungle/internal/wagering/postgres"
)

func assertTransactionQueryIsolation(
	t *testing.T,
	client *http.Client,
	address string,
	issuer string,
	transactionID string,
) {
	t.Helper()

	owner := token(t, issuer, "provider-a")
	other := token(t, issuer, "provider-b")
	routes := []struct {
		name       string
		path       string
		deniedCode string
		denied     int
	}{
		{
			name:       "transaction ID",
			path:       "/wagering/transactions/" + transactionID,
			deniedCode: "NOT_FOUND",
			denied:     http.StatusNotFound,
		},
		{
			name:       "provider and external ID",
			path:       "/providers/provider-a/wagering/transactions/bet",
			deniedCode: "FORBIDDEN",
			denied:     http.StatusForbidden,
		},
	}

	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			status, body := queryTransaction(t, client, address+route.path, owner)
			if status != http.StatusOK {
				t.Fatalf("owner query = %d: %s", status, body)
			}

			var snapshot wager.Snapshot
			if err := json.Unmarshal(body, &snapshot); err != nil {
				t.Fatal(err)
			}

			if snapshot.ID != transactionID {
				t.Fatalf("transaction ID = %q; want %q", snapshot.ID, transactionID)
			}

			if snapshot.Input.ProviderID != "provider-a" {
				t.Fatalf("transaction provider = %q; want provider-a", snapshot.Input.ProviderID)
			}

			status, body = queryTransaction(t, client, address+route.path, other)
			if status != route.denied {
				t.Fatalf("other provider query = %d: %s; want %d", status, body, route.denied)
			}

			assertAuthorizationError(t, body, route.deniedCode)
		})
	}
}

func assertExpiredKeycloakCredential(
	t *testing.T,
	client *http.Client,
	address string,
	issuer string,
	operation string,
	store *postgres.Store,
	walletID string,
	transactionID string,
) {
	t.Helper()

	credential := token(t, issuer, "provider-a-expiring")
	parts := strings.Split(credential, ".")
	if len(parts) != 3 {
		t.Fatal("IdP did not issue a compact JWT")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}

	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}

	// Read the signed token's deadline without changing its claims or the verifier's clock.
	expiresAt := time.Unix(claims.ExpiresAt, 0)
	remaining := time.Until(expiresAt)
	if remaining <= 0 {
		t.Fatal("IdP issued an already expired token")
	}

	if remaining > 30*time.Second {
		t.Fatal("expiring test client must issue tokens lasting at most 30 seconds; reimport the test realm")
	}

	paths := []string{
		"/wagering/transactions/" + transactionID,
		"/providers/provider-a/wagering/transactions/bet",
	}

	for _, path := range paths {
		status, body := queryTransaction(t, client, address+path, credential)
		if status != http.StatusOK {
			t.Fatalf("credential before expiry = %d: %s", status, body)
		}
	}

	before, err := store.Wallet(t.Context(), walletID)
	if err != nil {
		t.Fatal(err)
	}

	timer := time.NewTimer(time.Until(expiresAt.Add(time.Second)))
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal("canceled waiting for real token expiration")
	}

	for _, path := range paths {
		status, body := queryTransaction(t, client, address+path, credential)
		if status != http.StatusUnauthorized {
			t.Fatalf("credential after expiry = %d: %s; want 401", status, body)
		}

		assertAuthorizationError(t, body, "UNAUTHORIZED")
	}

	expiredOperation := strings.Replace(operation, `"externalTransactionId":"bet"`, `"externalTransactionId":"expired-bet"`, 1)
	status, body := request(t, client, address+"/wagering/transactions", credential, "expired-key", expiredOperation)
	if status != http.StatusUnauthorized {
		t.Fatalf("expired submission = %d: %s; want 401", status, body)
	}

	assertAuthorizationError(t, body, "UNAUTHORIZED")

	after, err := store.Wallet(t.Context(), walletID)
	if err != nil {
		t.Fatal(err)
	}

	if after != before {
		t.Fatal("expired credential changed the wallet")
	}

	_, err = store.Transaction(t.Context(), "", "provider-a", "expired-bet")
	if !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("expired submission persisted a transaction: %v", err)
	}
}

func queryTransaction(
	t *testing.T,
	client *http.Client,
	address string,
	credential string,
) (int, []byte) {
	t.Helper()

	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, address, nil)
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("Authorization", "Bearer "+credential)
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close transaction query response: %v", err)
		}
	}()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	return response.StatusCode, body
}

func assertAuthorizationError(t *testing.T, body []byte, code string) {
	t.Helper()

	var response map[string]json.RawMessage
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}

	if len(response) != 1 {
		t.Fatalf("denied request exposed fields beyond the error code: %s", body)
	}

	var actual string
	if err := json.Unmarshal(response["code"], &actual); err != nil {
		t.Fatal(err)
	}

	if actual != code {
		t.Fatalf("error code = %q; want %q", actual, code)
	}
}
