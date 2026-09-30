#!/bin/bash
set -x

awslocal sqs create-queue \
  --queue-name wager-transactions-dlq.fifo \
  --attributes FifoQueue=true,ContentBasedDeduplication=true

awslocal sqs create-queue \
  --queue-name wager-transactions.fifo \
  --attributes FifoQueue=true,ContentBasedDeduplication=true,RedrivePolicy="\"{\\\"deadLetterTargetArn\\\":\\\"arn:aws:sqs:us-east-1:000000000000:wager-transactions-dlq.fifo\\\",\\\"maxReceiveCount\\\":\\\"3\\\"}\""

awslocal sqs create-queue \
  --queue-name wager-events.fifo \
  --attributes FifoQueue=true,ContentBasedDeduplication=true

POLICY=$(cat << 'POLICY_EOF'
{
  "Version": "2012-10-17",
  "Id": "WagerQueuePolicy",
  "Statement": [
    {
      "Sid": "RestrictToAuthorizedUsers",
      "Effect": "Allow",
      "Principal": {
        "AWS": "arn:aws:iam::000000000000:root"
      },
      "Action": [
        "sqs:SendMessage",
        "sqs:ReceiveMessage",
        "sqs:DeleteMessage",
        "sqs:GetQueueAttributes"
      ],
      "Resource": "arn:aws:sqs:us-east-1:000000000000:wager-transactions.fifo"
    }
  ]
}
POLICY_EOF
)

# Convert policy to JSON string
POLICY_JSON=$(echo "$POLICY" | jq -c .)

awslocal sqs set-queue-attributes \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --attributes Policy="$POLICY_JSON"

awslocal sqs set-queue-attributes \
  --queue-url http://localhost:4566/000000000000/wager-events.fifo \
  --attributes Policy="$POLICY_JSON"

echo "SQS Queues and Policies created successfully."
