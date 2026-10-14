package wager_test

import (
	"testing"

	"jungle/internal/wagering/domain/money"
	"jungle/internal/wagering/domain/wager"
	"jungle/internal/wagering/domain/wallet"
)

func TestReferenceKinds(t *testing.T) {
	tests := []struct {
		name      string
		kind      wager.Kind
		original  wager.Kind
		direction wallet.Direction
		code      string
	}{
		{"refund bet", wager.Refund, wager.Bet, wallet.Credit, ""},
		{"refund win", wager.Refund, wager.Win, "", "REFERENCE_KIND_INVALID"},
		{"refund refund", wager.Refund, wager.Refund, "", "REFERENCE_KIND_INVALID"},
		{"refund rollback", wager.Refund, wager.Rollback, "", "REFERENCE_KIND_INVALID"},
		{"refund loss", wager.Refund, wager.Loss, "", "REFERENCE_KIND_INVALID"},
		{"refund opening", wager.Refund, wager.Opening, "", "REFERENCE_KIND_INVALID"},
		{"rollback bet", wager.Rollback, wager.Bet, wallet.Credit, ""},
		{"rollback win", wager.Rollback, wager.Win, wallet.Debit, ""},
		{"rollback refund", wager.Rollback, wager.Refund, wallet.Debit, ""},
		{"rollback rollback", wager.Rollback, wager.Rollback, "", "REFERENCE_KIND_INVALID"},
		{"rollback loss", wager.Rollback, wager.Loss, "", "REFERENCE_KIND_INVALID"},
		{"rollback opening", wager.Rollback, wager.Opening, "", "REFERENCE_KIND_INVALID"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := input(t, tt.kind, "1.00")
			request.ReferenceExternalTransactionID = "original"
			original := wager.Snapshot{ID: "original", Status: wager.Processed, Input: input(t, tt.original, "1.00")}
			original.Input.ExternalTransactionID = "original"

			direction, code := request.ReferenceDirection(&original)
			if direction != tt.direction || code != tt.code {
				t.Fatalf("ReferenceDirection() = %q, %q; want %q, %q", direction, code, tt.direction, tt.code)
			}
		})
	}
}

func TestReferenceValidation(t *testing.T) {
	tests := []struct {
		name   string
		change func(*wager.Snapshot)
		code   string
	}{
		{"missing", nil, "REFERENCE_PENDING"},
		{"pending", func(s *wager.Snapshot) { s.Status = wager.Pending }, "REFERENCE_PENDING"},
		{"pending reference", func(s *wager.Snapshot) { s.Status = wager.PendingReference }, "REFERENCE_PENDING"},
		{"rejected", func(s *wager.Snapshot) { s.Status = wager.Rejected }, "REFERENCE_UNSUCCESSFUL"},
		{"failed", func(s *wager.Snapshot) { s.Status = wager.Failed }, "REFERENCE_UNSUCCESSFUL"},
		{"external identity", func(s *wager.Snapshot) { s.Input.ExternalTransactionID = "other" }, "REFERENCE_MISMATCH"},
		{"provider", func(s *wager.Snapshot) { s.Input.ProviderID = "other" }, "REFERENCE_MISMATCH"},
		{"player", func(s *wager.Snapshot) { s.Input.PlayerID = "other" }, "REFERENCE_MISMATCH"},
		{"wallet", func(s *wager.Snapshot) { s.Input.WalletID = "other" }, "REFERENCE_MISMATCH"},
		{"round", func(s *wager.Snapshot) { s.Input.RoundID = "other" }, "REFERENCE_MISMATCH"},
		{"currency", func(s *wager.Snapshot) {
			value, err := money.New("1.00", money.USD)
			if err != nil {
				t.Fatal(err)
			}

			s.Input.Money = value
		}, "REFERENCE_MISMATCH"},
		{"partial amount", func(s *wager.Snapshot) { s.Input.Money = input(t, wager.Bet, "2.00").Money }, "REFERENCE_AMOUNT_MISMATCH"},
		{"excess amount", func(s *wager.Snapshot) { s.Input.Money = input(t, wager.Bet, "0.50").Money }, "REFERENCE_AMOUNT_MISMATCH"},
	}

	for _, kind := range []wager.Kind{wager.Refund, wager.Rollback} {
		t.Run(string(kind), func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					request := input(t, kind, "1.00")
					request.ReferenceExternalTransactionID = "original"
					var original *wager.Snapshot

					if tt.change != nil {
						original = &wager.Snapshot{ID: "original", Status: wager.Processed, Input: input(t, wager.Bet, "1.00")}
						original.Input.ExternalTransactionID = "original"
						tt.change(original)
					}

					direction, code := request.ReferenceDirection(original)
					if direction != "" || code != tt.code {
						t.Fatalf("ReferenceDirection() = %q, %q; want no movement, %q", direction, code, tt.code)
					}
				})
			}
		})
	}
}
