package postgres_test

import (
	"errors"
	"testing"
	"time"

	"jungle/internal/wagering/postgres"
)

func TestClaimEventRejectsInvalidLeaseBeforeIO(t *testing.T) {
	store := postgres.New(nil, nil, nil)

	for _, duration := range []time.Duration{0, -time.Second, time.Nanosecond, 1500 * time.Microsecond} {
		t.Run(duration.String(), func(t *testing.T) {
			_, err := store.ClaimEvent(t.Context(), duration)
			if !errors.Is(err, postgres.ErrInvalidLease) {
				t.Fatalf("ClaimEvent() error = %v, want ErrInvalidLease", err)
			}
		})
	}
}
