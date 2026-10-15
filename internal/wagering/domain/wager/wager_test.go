package wager_test

import (
	"errors"
	"testing"
	"time"

	"jungle/internal/wagering/domain/money"
	"jungle/internal/wagering/domain/wager"
)

func input(t *testing.T, kind wager.Kind, amount string) wager.Input {
	t.Helper()

	value, err := money.New(amount, money.BRL)
	if err != nil {
		t.Fatal(err)
	}

	return wager.Input{ProviderID: "provider", ExternalTransactionID: "external", WalletID: "wallet",
		PlayerID: "player", RoundID: "round", GameID: "game", Kind: kind, Money: value}
}

func TestExternalKinds(t *testing.T) {
	tests := []struct {
		name   string
		kind   wager.Kind
		amount string
		valid  bool
	}{
		{name: "zero bet", kind: wager.Bet, amount: "0.00"},
		{name: "positive bet", kind: wager.Bet, amount: "1.00", valid: true},
		{name: "zero win", kind: wager.Win, amount: "0.00"},
		{name: "positive win", kind: wager.Win, amount: "1.00", valid: true},
		{name: "zero loss", kind: wager.Loss, amount: "0.00", valid: true},
		{name: "positive loss", kind: wager.Loss, amount: "1.00"},
		{name: "zero refund", kind: wager.Refund, amount: "0.00"},
		{name: "positive refund", kind: wager.Refund, amount: "1.00", valid: true},
		{name: "zero rollback", kind: wager.Rollback, amount: "0.00"},
		{name: "positive rollback", kind: wager.Rollback, amount: "1.00", valid: true},
		{name: "zero external opening", kind: wager.Opening, amount: "0.00"},
		{name: "positive external opening", kind: wager.Opening, amount: "1.00"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := input(t, tt.kind, tt.amount)

			if tt.kind == wager.Refund || tt.kind == wager.Rollback {
				request.ReferenceExternalTransactionID = "original"
			}

			err := request.Validate()
			if (err == nil) != tt.valid {
				t.Fatalf("Validate() = %v, want valid=%v", err, tt.valid)
			}
		})
	}
}

func TestCanonicalHash(t *testing.T) {
	first := input(t, wager.Bet, "00025.00")
	second := input(t, wager.Bet, "25.00")

	firstHash, err := first.Hash()
	if err != nil {
		t.Fatal(err)
	}

	secondHash, err := second.Hash()
	if err != nil {
		t.Fatal(err)
	}

	if firstHash != secondHash {
		t.Fatal("equivalent money must have the same business hash")
	}

	second.RoundID = "another"
	changed, err := second.Hash()
	if err != nil {
		t.Fatal(err)
	}

	if changed == firstHash {
		t.Fatal("different business payload must change hash")
	}
}

func TestTransitions(t *testing.T) {
	now := time.Now().UTC()
	request := input(t, wager.Bet, "1.00")

	for _, terminal := range []wager.Status{wager.Processed, wager.Rejected, wager.Failed} {
		t.Run(string(terminal), func(t *testing.T) {
			transaction, err := wager.New("id", "key", request, request.Money, now)
			if err != nil {
				t.Fatal(err)
			}

			code := ""

			if terminal != wager.Processed {
				code = "FAILURE"
			}

			if terminal == wager.Failed {
				code = wager.InfrastructureDataLoss
			}

			finished, err := transaction.Transition(terminal, request.Money, code, "", now)
			if err != nil {
				t.Fatal(err)
			}

			restored, err := wager.Restore(finished.Snapshot())
			if err != nil || restored.Snapshot() != finished.Snapshot() {
				t.Fatalf("Restore() = %v, %v", restored, err)
			}

			if _, err := finished.Transition(wager.Processed, request.Money, "", "", now); !errors.Is(err, wager.ErrTerminal) {
				t.Fatalf("terminal transition error = %v", err)
			}
		})
	}
}
