package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"jungle/internal/wagering/postgres"
)

func logRecords(t *testing.T, data []byte) []map[string]any {
	t.Helper()

	decoder := json.NewDecoder(bytes.NewReader(data))
	var records []map[string]any

	for {
		var record map[string]any
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			return records
		}

		if err != nil {
			t.Fatal(err)
		}

		records = append(records, record)
	}
}

func TestEventLogsSelectOnlyDiagnosticMetadata(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    map[string]any
		count   int
	}{
		{
			name: "external event",
			payload: `{"correlationId":"correlation","causationId":"message","AccessToken":"credential-secret",
				"data":{"transactionId":"transaction","providerId":"provider","money":{"amount":"25.00"},"playerId":"private-player"}}`,
			want: map[string]any{
				"correlationId": "correlation", "causationId": "message",
				"transactionId": "transaction", "providerId": "provider",
			},
			count: 1,
		},
		{
			name: "internal event without provider",
			payload: `{"correlationId":"opening","data":{"transactionId":"transaction",
				"balanceAfter":{"amount":"25.00"}}}`,
			want:  map[string]any{"correlationId": "opening", "transactionId": "transaction"},
			count: 1,
		},
		{name: "malformed metadata", payload: `{"data":"credential-secret`, count: 2},
		{
			name: "oversized identifier omitted",
			payload: `{"correlationId":"` + strings.Repeat("x", 201) + `",
				"data":{"transactionId":"transaction"}}`,
			want:  map[string]any{"transactionId": "transaction"},
			count: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			publisher := &Publisher{logger: slog.New(slog.NewJSONHandler(&output, nil))}
			logger := publisher.eventLogger(postgres.Delivery{
				ID: "event", AggregateID: "wallet", Payload: []byte(tt.payload), ProcessingAttempts: 2,
			})
			logger.Info("outbox publication accepted")
			records := logRecords(t, output.Bytes())

			if len(records) != tt.count {
				t.Fatalf("log records = %d, want %d", len(records), tt.count)
			}

			for _, record := range records {
				for key, want := range map[string]any{
					"operation": "outbox_publish", "eventId": "event", "walletId": "wallet", "processingAttempt": float64(2),
				} {
					if record[key] != want {
						t.Errorf("%s = %v, want %v", key, record[key], want)
					}
				}
			}

			last := records[len(records)-1]

			for _, key := range []string{"correlationId", "causationId", "transactionId", "providerId"} {
				if last[key] != tt.want[key] {
					t.Errorf("%s = %v, want %v", key, last[key], tt.want[key])
				}
			}

			for _, forbidden := range []string{"credential-secret", `"25.00"`, "private-player", "money", "balanceAfter", "AccessToken", "payload"} {
				if strings.Contains(output.String(), forbidden) {
					t.Errorf("log exposed %s", forbidden)
				}
			}
		})
	}
}

func TestMessageLogsOmitAbsentAndOversizedIdentifiers(t *testing.T) {
	tests := []struct {
		name      string
		messageID string
		want      string
	}{
		{name: "available identity", messageID: "message", want: "message"},
		{name: "identifier length boundary", messageID: strings.Repeat("x", 200), want: strings.Repeat("x", 200)},
		{name: "decoding failed"},
		{name: "oversized identity", messageID: strings.Repeat("x", 201)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			consumer := &Consumer{logger: slog.New(slog.NewJSONHandler(&output, nil))}
			consumer.messageLogger("broker-id", requested{MessageID: tt.messageID}).Warn("message retained")
			records := logRecords(t, output.Bytes())

			if len(records) != 1 {
				t.Fatalf("log records = %d, want one", len(records))
			}

			record := records[0]
			if record["brokerMessageId"] != "broker-id" {
				t.Fatal("broker identity was lost")
			}

			for _, key := range []string{"messageId", "correlationId"} {
				if tt.want == "" {
					if _, exists := record[key]; exists {
						t.Errorf("unavailable %s was logged", key)
					}
				} else if record[key] != tt.want {
					t.Errorf("%s = %v, want %s", key, record[key], tt.want)
				}
			}
		})
	}
}
