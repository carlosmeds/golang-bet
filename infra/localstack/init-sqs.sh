#!/bin/bash
# Provisions the SQS queues and the broker identities (REQ-062, REQ-063, D12).
#
# Fail closed: any error aborts the script before the readiness marker is
# written, and the Compose healthcheck requires that marker, so the application
# never starts against a broker with missing queues, identities or policies.
#
# Identities (IAM users with generated access keys, written as AWS shared
# credentials profiles to $CREDENTIALS_DIR/credentials):
#   wagering-producer        internal producer: SendMessage to the inbound queue only
#   wagering-app             the service: consume inbound, publish events
#   wagering-broker-operator tests/operations: manage queues; cannot publish inbound
#   wagering-provider-probe  stands in for a provider identity: no queue access
set -euo pipefail

REGION=us-east-1
ACCOUNT=000000000000
CREDENTIALS_DIR=${CREDENTIALS_DIR:-/broker-credentials}
READY_MARKER=${READY_MARKER:-/run/provision/ready}
ENDPOINT=http://localhost:4566

INBOUND=wager-transactions.fifo
DLQ=wager-transactions-dlq.fifo
EVENTS=wager-events.fifo
arn() { echo "arn:aws:sqs:${REGION}:${ACCOUNT}:$1"; }
user_arn() { echo "arn:aws:iam::${ACCOUNT}:user/$1"; }

rm -f "$READY_MARKER"
workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

awslocal sqs create-queue \
  --queue-name "$DLQ" \
  --attributes FifoQueue=true,ContentBasedDeduplication=true

# maxReceiveCount is mirrored by tests/integration/sqs (provisionedMaxReceiveCount).
awslocal sqs create-queue \
  --queue-name "$INBOUND" \
  --attributes FifoQueue=true,ContentBasedDeduplication=true,RedrivePolicy="\"{\\\"deadLetterTargetArn\\\":\\\"$(arn "$DLQ")\\\",\\\"maxReceiveCount\\\":\\\"3\\\"}\""

awslocal sqs create-queue \
  --queue-name "$EVENTS" \
  --attributes FifoQueue=true,ContentBasedDeduplication=true

# Identity policies (least privilege).
cat > "$workdir/producer.json" <<EOF
{"Version":"2012-10-17","Statement":[
  {"Sid":"PublishInbound","Effect":"Allow",
   "Action":["sqs:SendMessage","sqs:GetQueueUrl","sqs:GetQueueAttributes"],
   "Resource":"$(arn "$INBOUND")"}]}
EOF
cat > "$workdir/app.json" <<EOF
{"Version":"2012-10-17","Statement":[
  {"Sid":"ConsumeInbound","Effect":"Allow",
   "Action":["sqs:ReceiveMessage","sqs:DeleteMessage","sqs:ChangeMessageVisibility","sqs:GetQueueUrl","sqs:GetQueueAttributes"],
   "Resource":"$(arn "$INBOUND")"},
  {"Sid":"PublishEvents","Effect":"Allow",
   "Action":["sqs:SendMessage","sqs:GetQueueUrl","sqs:GetQueueAttributes"],
   "Resource":"$(arn "$EVENTS")"}]}
EOF
cat > "$workdir/operator.json" <<EOF
{"Version":"2012-10-17","Statement":[
  {"Sid":"ManageQueues","Effect":"Allow","Action":"sqs:*","Resource":"arn:aws:sqs:${REGION}:${ACCOUNT}:*"}]}
EOF

# Queue (resource) policy on the inbound queue: an explicit Deny wins over any
# identity policy, so only the internal producer can publish even if another
# identity is ever granted sqs:SendMessage.
cat > "$workdir/inbound-policy.json" <<EOF
{"Version":"2012-10-17","Id":"WagerInboundPolicy","Statement":[
  {"Sid":"OnlyInternalProducerPublishes","Effect":"Deny","Principal":"*",
   "Action":"sqs:SendMessage","Resource":"$(arn "$INBOUND")",
   "Condition":{"ArnNotEquals":{"aws:PrincipalArn":"$(user_arn wagering-producer)"}}}]}
EOF
python3 -c 'import json,sys; print(json.dumps({"Policy": sys.stdin.read()}))' \
  < "$workdir/inbound-policy.json" > "$workdir/inbound-attributes.json"
awslocal sqs set-queue-attributes \
  --queue-url "$ENDPOINT/$ACCOUNT/$INBOUND" \
  --attributes "file://$workdir/inbound-attributes.json"

mkdir -p "$CREDENTIALS_DIR"
credentials_tmp="$workdir/credentials"
: > "$credentials_tmp"

# create-access-key returns the secret only once, so id and secret are read from one call.
create_identity_profile() { # user-name policy-file|""
  awslocal iam create-user --user-name "$1" > /dev/null
  if [ -n "$2" ]; then
    awslocal iam put-user-policy --user-name "$1" --policy-name "$1" \
      --policy-document "file://$2"
  fi
  local out
  out=$(awslocal iam create-access-key --user-name "$1" --query '[AccessKey.AccessKeyId,AccessKey.SecretAccessKey]' --output text)
  [ -n "$out" ] || { echo "no access key for $1" >&2; exit 1; }
  printf '[%s]\naws_access_key_id = %s\naws_secret_access_key = %s\n\n' \
    "$1" "$(echo "$out" | cut -f1)" "$(echo "$out" | cut -f2)" >> "$credentials_tmp"
}

create_identity_profile wagering-producer "$workdir/producer.json"
create_identity_profile wagering-app "$workdir/app.json"
create_identity_profile wagering-broker-operator "$workdir/operator.json"
create_identity_profile wagering-provider-probe ""

# Verify what was applied before declaring the broker ready.
applied=$(awslocal sqs get-queue-attributes --queue-url "$ENDPOINT/$ACCOUNT/$INBOUND" \
  --attribute-names Policy --query Attributes.Policy --output text)
case "$applied" in
  *OnlyInternalProducerPublishes*) ;;
  *) echo "inbound queue policy was not applied: '$applied'" >&2; exit 1 ;;
esac
for user in wagering-producer wagering-app wagering-broker-operator; do
  [ "$(awslocal iam list-user-policies --user-name "$user" --query 'length(PolicyNames)' --output text)" = 1 ] \
    || { echo "identity policy missing for $user" >&2; exit 1; }
done

cp "$credentials_tmp" "$CREDENTIALS_DIR/credentials"
chmod 0644 "$CREDENTIALS_DIR/credentials"
mkdir -p "$(dirname "$READY_MARKER")"
touch "$READY_MARKER"
echo "SQS queues, broker identities and policies provisioned successfully."
