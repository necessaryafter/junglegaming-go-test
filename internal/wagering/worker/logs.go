package worker

import (
	"encoding/json"
	"log/slog"

	"jungle/internal/wagering/postgres"
)

// withIdentifier omits absent or oversized transport values. It never formats
// payloads, credentials or arbitrary errors into a diagnostic field.
func withIdentifier(logger *slog.Logger, key string, value string) *slog.Logger {
	if value == "" {
		return logger
	}

	if len(value) > 200 {
		return logger
	}

	return logger.With(key, value)
}

func (w *Consumer) messageLogger(brokerID string, envelope requested) *slog.Logger {
	logger := w.logger.With("operation", "sqs_consume")
	logger = withIdentifier(logger, "brokerMessageId", brokerID)
	logger = withIdentifier(logger, "messageId", envelope.MessageID)
	logger = withIdentifier(logger, "correlationId", envelope.MessageID)

	return withIdentifier(logger, "walletId", envelope.Data.WalletID)
}

func (w *Publisher) eventLogger(delivery postgres.Delivery) *slog.Logger {
	logger := w.logger.With(
		"operation", "outbox_publish",
		"eventId", delivery.ID,
		"walletId", delivery.AggregateID,
		"processingAttempt", delivery.ProcessingAttempts,
	)

	var metadata struct {
		CorrelationID string `json:"correlationId"`
		CausationID   string `json:"causationId"`
		Data          struct {
			TransactionID string `json:"transactionId"`
			ProviderID    string `json:"providerId"`
		} `json:"data"`
	}

	if err := json.Unmarshal(delivery.Payload, &metadata); err != nil {
		logger.Warn("outbox log metadata unavailable", "reason", "invalid_event_metadata")

		return logger
	}

	logger = withIdentifier(logger, "correlationId", metadata.CorrelationID)
	logger = withIdentifier(logger, "causationId", metadata.CausationID)
	logger = withIdentifier(logger, "transactionId", metadata.Data.TransactionID)

	return withIdentifier(logger, "providerId", metadata.Data.ProviderID)
}
