// Package brokertest gives the real-broker integration tests the Compose
// broker's identities (infra/localstack/init-sqs.sh). The broker is reached
// through the authenticating gateway; every identity is a provisioned IAM user.
//
// Environment:
//
//	WAGERING_TEST_SQS_ENDPOINT                 e.g. http://localhost:4566 (tests skip when unset)
//	WAGERING_TEST_BROKER_CREDENTIALS_FILE      AWS shared-credentials file written by
//	                                           init-sqs.sh; copy it out with
//	                                           infra/localstack/export-broker-credentials.sh
package brokertest

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// Provisioned identities.
const (
	Producer = "wagering-producer"        // SendMessage to the inbound queue only
	App      = "wagering-app"             // consumes inbound, publishes events
	Operator = "wagering-broker-operator" // manages queues; denied inbound SendMessage
	Provider = "wagering-provider-probe"  // a provider-side identity with no queue access
	Region   = "us-east-1"
	account  = "000000000000"
	InboundQ = "wager-transactions.fifo"
	DLQ      = "wager-transactions-dlq.fifo"
	EventsQ  = "wager-events.fifo"
)

// Endpoint returns the broker endpoint (the gateway) or skips the test.
func Endpoint(t testing.TB) string {
	t.Helper()
	v := strings.TrimRight(strings.TrimSpace(os.Getenv("WAGERING_TEST_SQS_ENDPOINT")), "/")
	if v == "" {
		t.Skip("WAGERING_TEST_SQS_ENDPOINT is not set")
	}
	return v
}

// QueueURL is the provisioned queue's URL behind endpoint.
func QueueURL(endpoint, name string) string { return endpoint + "/" + account + "/" + name }

// Credentials returns the access key and secret of a provisioned identity.
func Credentials(t testing.TB, identity string) (string, string) {
	t.Helper()
	path := os.Getenv("WAGERING_TEST_BROKER_CREDENTIALS_FILE")
	if path == "" {
		t.Fatal("WAGERING_TEST_BROKER_CREDENTIALS_FILE is not set (run infra/localstack/export-broker-credentials.sh)")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("broker credentials: %v", err)
	}
	defer f.Close()
	var profile, id, secret string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			profile = line[1 : len(line)-1]
		case profile == identity:
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			switch strings.TrimSpace(k) {
			case "aws_access_key_id":
				id = strings.TrimSpace(v)
			case "aws_secret_access_key":
				secret = strings.TrimSpace(v)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("broker credentials: %v", err)
	}
	if id == "" || secret == "" {
		t.Fatalf("identity %q not found in %s", identity, path)
	}
	return id, secret
}

// ClientWith builds an SQS client for the endpoint that signs with the given static key.
func ClientWith(t testing.TB, endpoint, keyID, secret string) *sqs.Client {
	t.Helper()
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(keyID, secret, "")))
	if err != nil {
		t.Fatal(err)
	}
	return sqs.NewFromConfig(cfg, func(o *sqs.Options) { o.BaseEndpoint = aws.String(endpoint) })
}

// Client builds an SQS client that signs as a provisioned identity.
func Client(t testing.TB, endpoint, identity string) *sqs.Client {
	t.Helper()
	id, secret := Credentials(t, identity)
	return ClientWith(t, endpoint, id, secret)
}

// SetProcessEnv exports an identity's credentials for code that resolves them
// from the AWS default chain (the service under test).
func SetProcessEnv(t *testing.T, identity string) {
	t.Helper()
	id, secret := Credentials(t, identity)
	t.Setenv("AWS_ACCESS_KEY_ID", id)
	t.Setenv("AWS_SECRET_ACCESS_KEY", secret)
}

// Env returns "AWS_ACCESS_KEY_ID=..." and "AWS_SECRET_ACCESS_KEY=..." for a child process.
func Env(t testing.TB, identity string) []string {
	t.Helper()
	id, secret := Credentials(t, identity)
	return []string{fmt.Sprintf("AWS_ACCESS_KEY_ID=%s", id), fmt.Sprintf("AWS_SECRET_ACCESS_KEY=%s", secret)}
}
