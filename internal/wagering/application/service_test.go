package application_test

import (
	"errors"
	"testing"

	"jungle/internal/wagering/application"
	"jungle/internal/wagering/domain/money"
	"jungle/internal/wagering/domain/wager"
)

func TestUnauthorizedRequestsNeverReachPersistence(t *testing.T) {
	// No repository is wired: every forbidden request must return before I/O.
	service := application.New(nil)
	principal := application.Principal{ProviderID: "provider-a"}

	_, err := service.Open(t.Context(), principal, "player", money.Money{})
	if !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("Open() error = %v", err)
	}

	_, err = service.Wallet(t.Context(), principal, "wallet")
	if !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("Wallet() error = %v", err)
	}

	_, err = service.Ledger(t.Context(), principal, "wallet", "", 50)
	if !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("Ledger() error = %v", err)
	}

	_, err = service.Reconcile(t.Context(), principal, "wallet")
	if !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("Reconcile() error = %v", err)
	}

	_, err = service.Submit(t.Context(), principal, application.Command{Input: wager.Input{ProviderID: "provider-b"}})
	if !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("Submit() error = %v", err)
	}

	_, err = service.Transaction(t.Context(), principal, "", "provider-b", "transaction")
	if !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("Transaction() error = %v", err)
	}
}
