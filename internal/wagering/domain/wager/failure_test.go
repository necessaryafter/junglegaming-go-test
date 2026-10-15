package wager_test

import (
	"errors"
	"testing"
	"time"

	"jungle/internal/wagering/domain/wager"
)

func TestFailurePolicy(t *testing.T) {
	tests := []struct {
		name   string
		status wager.Status
		code   string
		valid  bool
	}{
		{name: "confirmed irrecoverable data loss", status: wager.Failed, code: wager.InfrastructureDataLoss, valid: true},
		{name: "missing cause", status: wager.Failed},
		{name: "unknown cause", status: wager.Failed, code: "FAILURE"},
		{name: "exhausted retries", status: wager.Failed, code: "RETRIES_EXHAUSTED"},
		{name: "dead letter delivery", status: wager.Failed, code: "DLQ"},
		{name: "timeout", status: wager.Failed, code: "TIMEOUT"},
		{name: "database unavailable", status: wager.Failed, code: "DATABASE_UNAVAILABLE"},
		{name: "constraint violation", status: wager.Failed, code: "CONSTRAINT_VIOLATION"},
		{name: "unauthorized", status: wager.Failed, code: "UNAUTHORIZED"},
		{name: "business rejection is not infrastructure failure", status: wager.Failed, code: "INSUFFICIENT_BALANCE"},
		{name: "reference expiration is not infrastructure failure", status: wager.Failed, code: "REFERENCE_NOT_FOUND"},
		{name: "business rejection", status: wager.Rejected, code: "INSUFFICIENT_BALANCE", valid: true},
		{name: "data loss is not business rejection", status: wager.Rejected, code: wager.InfrastructureDataLoss},
	}

	for _, origin := range []wager.Status{wager.Pending, wager.PendingReference} {
		t.Run(string(origin), func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					now := time.Now().UTC()
					request := input(t, wager.Refund, "1.00")

					request.ReferenceExternalTransactionID = "original"

					transaction, err := wager.New("id", "key", request, request.Money, now)
					if err != nil {
						t.Fatal(err)
					}

					state := transaction.Snapshot()
					state.Status = origin
					transaction, err = wager.Restore(state)
					if err != nil {
						t.Fatal(err)
					}

					_, transitionErr := transaction.Transition(tt.status, request.Money, tt.code, "", now)
					state.Status = tt.status
					state.FailureCode = tt.code
					_, restoreErr := wager.Restore(state)

					for _, err := range []error{transitionErr, restoreErr} {
						if tt.valid {
							if err != nil {
								t.Fatalf("valid failure outcome rejected: %v", err)
							}

							continue
						}

						if !errors.Is(err, wager.ErrInvalid) {
							t.Fatalf("unsupported failure outcome error = %v, want ErrInvalid", err)
						}
					}
				})
			}
		})
	}
}
