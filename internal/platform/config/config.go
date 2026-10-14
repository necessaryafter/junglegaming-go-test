// Package config reads and validates startup settings in one place.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"time"
)

type Config struct {
	HTTPAddress         string
	DatabaseURL         string
	OIDCIssuer          string
	OIDCAudience        string
	OIDCJWKSURL         string
	SQSRegion           string
	SQSEndpoint         string
	RequestsQueue       string
	EventsQueue         string
	OperationTimeout    time.Duration
	OutboxLeaseDuration time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		HTTPAddress: value("HTTP_ADDRESS", ":8081"), DatabaseURL: os.Getenv("DATABASE_URL"),
		OIDCIssuer: os.Getenv("OIDC_ISSUER"), OIDCAudience: value("OIDC_AUDIENCE", "jungle-api"),
		OIDCJWKSURL: os.Getenv("OIDC_JWKS_URL"), SQSRegion: value("AWS_REGION", "us-east-1"),
		SQSEndpoint: os.Getenv("SQS_ENDPOINT"), RequestsQueue: value("SQS_REQUESTS_QUEUE", "wager-transactions.fifo"),
		EventsQueue: value("SQS_EVENTS_QUEUE", "wager-events.fifo"), OperationTimeout: 10 * time.Second,
	}

	leaseDuration, err := time.ParseDuration(value("OUTBOX_LEASE_DURATION", "60s"))
	if err != nil {
		return Config{}, fmt.Errorf("config: OUTBOX_LEASE_DURATION: %w", err)
	}

	if leaseDuration < 2*cfg.OperationTimeout || leaseDuration%time.Millisecond != 0 {
		return Config{}, errors.New("config: OUTBOX_LEASE_DURATION must be at least 20s in whole milliseconds")
	}

	cfg.OutboxLeaseDuration = leaseDuration

	if _, _, err := net.SplitHostPort(cfg.HTTPAddress); err != nil {
		return Config{}, fmt.Errorf("config: HTTP_ADDRESS: %w", err)
	}

	if cfg.DatabaseURL == "" || cfg.OIDCIssuer == "" {
		return Config{}, errors.New("config: DATABASE_URL and OIDC_ISSUER are required")
	}

	for name, address := range map[string]string{"DATABASE_URL": cfg.DatabaseURL, "OIDC_ISSUER": cfg.OIDCIssuer, "OIDC_JWKS_URL": cfg.OIDCJWKSURL, "SQS_ENDPOINT": cfg.SQSEndpoint} {
		if address == "" {
			continue
		}

		parsed, err := url.Parse(address)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid %s", name)
		}

		if parsed.Host == "" || parsed.Scheme == "" {
			return Config{}, fmt.Errorf("config: invalid %s", name)
		}
	}

	if cfg.OIDCJWKSURL == "" {
		cfg.OIDCJWKSURL = cfg.OIDCIssuer + "/protocol/openid-connect/certs"
	}

	return cfg, nil
}

func value(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}

	return fallback
}
