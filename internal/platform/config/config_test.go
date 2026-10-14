package config_test

import (
	"testing"
	"time"

	"jungle/internal/platform/config"
)

func TestOutboxLeaseDuration(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/jungle")
	t.Setenv("OIDC_ISSUER", "http://identity.test/realms/jungle")
	t.Setenv("HTTP_ADDRESS", ":8081")
	t.Setenv("SQS_ENDPOINT", "")
	t.Setenv("OIDC_JWKS_URL", "")

	tests := []struct {
		name    string
		input   string
		want    time.Duration
		invalid bool
	}{
		{name: "default", want: time.Minute},
		{name: "custom", input: "45s", want: 45 * time.Second},
		{name: "minimum", input: "20s", want: 20 * time.Second},
		{name: "millisecond precision", input: "20.5s", want: 20500 * time.Millisecond},
		{name: "invalid syntax", input: "forever", invalid: true},
		{name: "zero", input: "0s", invalid: true},
		{name: "negative", input: "-1m", invalid: true},
		{name: "shorter than processing deadlines", input: "19s", invalid: true},
		{name: "submillisecond precision", input: "20.0000001s", invalid: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OUTBOX_LEASE_DURATION", tt.input)

			cfg, err := config.Load()
			if tt.invalid {
				if err == nil {
					t.Fatal("Load accepted an invalid lease duration")
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if cfg.OutboxLeaseDuration != tt.want {
				t.Fatalf("lease = %v, want %v", cfg.OutboxLeaseDuration, tt.want)
			}
		})
	}
}
