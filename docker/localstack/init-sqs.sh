#!/usr/bin/env bash
set -euo pipefail

# Explicit deduplication IDs let tests exercise application-level idempotency.
create_fifo() {
  awslocal sqs create-queue --queue-name "$1" \
    --attributes '{"FifoQueue":"true","ContentBasedDeduplication":"false","VisibilityTimeout":"60","MessageRetentionPeriod":"1209600"}' \
    --query QueueUrl --output text
}

configure_redrive() {
  local source_url="$1" dlq_url="$2" dlq_arn
  dlq_arn=$(awslocal sqs get-queue-attributes --queue-url "$dlq_url" \
    --attribute-names QueueArn --query Attributes.QueueArn --output text)
  awslocal sqs set-queue-attributes --queue-url "$source_url" \
    --attributes "$(python3 -c 'import json,sys; print(json.dumps({"RedrivePolicy":json.dumps({"deadLetterTargetArn":sys.argv[1],"maxReceiveCount":"5"})}))' "$dlq_arn")"
}

requests_dlq=$(create_fifo wager-transactions-dlq.fifo)
requests=$(create_fifo wager-transactions.fifo)
configure_redrive "$requests" "$requests_dlq"

events_dlq=$(create_fifo wager-events-dlq.fifo)
events=$(create_fifo wager-events.fifo)
configure_redrive "$events" "$events_dlq"

touch /tmp/jungle-sqs-ready
