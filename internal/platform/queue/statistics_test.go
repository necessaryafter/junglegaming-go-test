package queue

import (
	"math"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

func TestLatestAgeRequiresRecentValidSample(t *testing.T) {
	now := time.Now().UTC()
	start := now.Add(-5 * time.Minute)
	older := now.Add(-2 * time.Minute)
	newer := now.Add(-time.Minute)
	stale := now.Add(-10 * time.Minute)
	future := now.Add(time.Minute)
	tests := []struct {
		name      string
		points    []types.Datapoint
		want      float64
		timestamp time.Time
	}{
		{name: "missing data"},
		{name: "out of order samples", points: []types.Datapoint{
			{Timestamp: &newer, Maximum: aws.Float64(42)},
			{Timestamp: &older, Maximum: aws.Float64(70)},
		}, want: 42, timestamp: newer},
		{name: "real zero", points: []types.Datapoint{{Timestamp: &newer, Maximum: aws.Float64(0)}}, timestamp: newer},
		{name: "stale data", points: []types.Datapoint{{Timestamp: &stale, Maximum: aws.Float64(42)}}},
		{name: "future data", points: []types.Datapoint{{Timestamp: &future, Maximum: aws.Float64(42)}}},
		{name: "negative age", points: []types.Datapoint{{Timestamp: &newer, Maximum: aws.Float64(-1)}}},
		{name: "NaN", points: []types.Datapoint{{Timestamp: &newer, Maximum: aws.Float64(math.NaN())}}},
		{name: "infinity", points: []types.Datapoint{{Timestamp: &newer, Maximum: aws.Float64(math.Inf(1))}}},
		{name: "no timestamp", points: []types.Datapoint{{Maximum: aws.Float64(42)}}},
		{name: "no maximum", points: []types.Datapoint{{Timestamp: &newer}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			age, timestamp := latestAge(tt.points, start, now)

			if age != tt.want {
				t.Fatalf("latest age = %g, want %g", age, tt.want)
			}

			if !timestamp.Equal(tt.timestamp) {
				t.Fatalf("sample timestamp = %v, want %v", timestamp, tt.timestamp)
			}
		})
	}
}

func TestQueueCountsRejectUnknownValues(t *testing.T) {
	tests := []struct {
		name  string
		value string
		valid bool
	}{
		{name: "zero", value: "0", valid: true},
		{name: "positive", value: "12", valid: true},
		{name: "missing"},
		{name: "negative", value: "-1"},
		{name: "malformed", value: "unknown"},
		{name: "overflow", value: "999999999999999999999"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := queueCounts(map[string]string{
				"ApproximateNumberOfMessages":           tt.value,
				"ApproximateNumberOfMessagesNotVisible": "0",
				"ApproximateNumberOfMessagesDelayed":    "0",
			})

			if (err == nil) != tt.valid {
				t.Fatalf("queueCounts() = %v, want valid %t", err, tt.valid)
			}
		})
	}
}
