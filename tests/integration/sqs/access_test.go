package sqs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/smithy-go"
	"wagering/tests/integration/brokertest"
)

// REQ-062 / D12: only the internal producer may publish to the inbound queue.
// These tests run against the live Compose broker through its gateway.

// denied reports whether the broker refused the call for lack of authorization
// (IAM/queue-policy denial or gateway rejection) rather than failing otherwise.
func denied(err error) bool {
	if err == nil {
		return false
	}
	var re *awshttp.ResponseError
	if errors.As(err, &re) && (re.HTTPStatusCode() == http.StatusForbidden || re.HTTPStatusCode() == http.StatusUnauthorized) {
		return true
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "AccessDenied", "AccessDeniedException", "MissingAuthenticationToken", "UnrecognizedClientException", "InvalidClientTokenId":
			return true
		}
	}
	return false
}

var dedupSeq atomic.Int64

func sendTo(c *sqs.Client, queueURL, body string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := c.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(queueURL), MessageBody: aws.String(body),
		MessageGroupId: aws.String("access-test"), MessageDeduplicationId: aws.String(fmt.Sprintf("access-%d-%d", time.Now().UnixNano(), dedupSeq.Add(1)))})
	return err
}

func TestOnlyTheInternalProducerCanPublishToInbound(t *testing.T) {
	endpoint := brokertest.Endpoint(t)
	inbound := brokertest.QueueURL(endpoint, brokertest.InboundQ)
	const body = `{"messageId":"r19f1-unauthorized-publish"`

	rejected := map[string]*sqs.Client{
		"provider identity (no queue permissions)":    brokertest.Client(t, endpoint, brokertest.Provider),
		"service identity (consumes, never produces)": brokertest.Client(t, endpoint, brokertest.App),
		"operator identity (blocked by queue policy)": brokertest.Client(t, endpoint, brokertest.Operator),
		"emulator default root key test/test":         brokertest.ClientWith(t, endpoint, "test", "test"),
		"account-id root key":                         brokertest.ClientWith(t, endpoint, "000000000000", "x"),
		"unknown access key":                          brokertest.ClientWith(t, endpoint, "AKIA0000000000000000", "x"),
		"arbitrary key (attacker)":                    brokertest.ClientWith(t, endpoint, "attacker", "attacker"),
	}
	for name, c := range rejected {
		t.Run(name, func(t *testing.T) {
			if err := sendTo(c, inbound, body); !denied(err) {
				t.Fatalf("SendMessage to the inbound queue was not denied: %v", err)
			}
		})
	}
}

func TestProducerAndServiceIdentitiesAreLeastPrivilege(t *testing.T) {
	endpoint := brokertest.Endpoint(t)
	inbound := brokertest.QueueURL(endpoint, brokertest.InboundQ)
	events := brokertest.QueueURL(endpoint, brokertest.EventsQ)
	ctx := context.Background()
	producer := brokertest.Client(t, endpoint, brokertest.Producer)
	app := brokertest.Client(t, endpoint, brokertest.App)

	attrs := func(c *sqs.Client, queue string) error {
		_, err := c.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(queue), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameAll}})
		return err
	}
	receive := func(c *sqs.Client, queue string) error {
		_, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(queue), VisibilityTimeout: 0})
		return err
	}
	// What the service needs works (the live app relies on the same grants).
	if err := attrs(app, inbound); err != nil {
		t.Errorf("service cannot read inbound attributes: %v", err)
	}
	if err := attrs(app, events); err != nil {
		t.Errorf("service cannot read events attributes: %v", err)
	}
	if err := attrs(producer, inbound); err != nil {
		t.Errorf("producer cannot read inbound attributes: %v", err)
	}
	// The metrics poller follows the inbound redrive policy and reads the
	// configured DLQ's visible-message count using the app identity.
	dlqResult, err := app.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(brokertest.DLQ)})
	if err != nil {
		t.Fatalf("service cannot resolve the configured DLQ for metrics: %v", err)
	}
	if _, err := app.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: dlqResult.QueueUrl, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages}}); err != nil {
		t.Fatalf("service cannot read DLQ metrics: %v", err)
	}
	// What it does not need is refused.
	for name, err := range map[string]error{
		"producer receives inbound":       receive(producer, inbound),
		"producer publishes events":       sendTo(producer, events, "x"),
		"producer creates queues":         createQueueErr(producer),
		"service receives its own events": receive(app, events),
		"service receives from the DLQ":   receive(app, aws.ToString(dlqResult.QueueUrl)),
		"service publishes to the DLQ":    sendTo(app, aws.ToString(dlqResult.QueueUrl), "x"),
		"service creates queues":          createQueueErr(app),
	} {
		if !denied(err) {
			t.Errorf("%s was not denied: %v", name, err)
		}
	}
}

func createQueueErr(c *sqs.Client) error {
	_, err := c.CreateQueue(context.Background(), &sqs.CreateQueueInput{QueueName: aws.String("r19f1-denied.fifo"), Attributes: map[string]string{"FifoQueue": "true"}})
	return err
}

// The broker has unauthenticated or unsafe surfaces (unsigned requests, the
// emulator's admin endpoints, credentials in the query string); the gateway
// must not forward them.
func TestGatewayRejectsUnsignedAndAdminRequests(t *testing.T) {
	endpoint := brokertest.Endpoint(t)
	inbound := brokertest.QueueURL(endpoint, brokertest.InboundQ)
	cases := map[string]struct {
		path    string
		query   string
		headers map[string]string
		body    string
	}{
		"unsigned query-protocol send":               {path: "/" + "000000000000/" + brokertest.InboundQ, body: "Action=SendMessage&MessageBody=x&MessageGroupId=g&QueueUrl=" + url.QueryEscape(inbound)},
		"unsigned JSON-protocol send":                {path: "/", headers: map[string]string{"X-Amz-Target": "AmazonSQS.SendMessage", "Content-Type": "application/x-amz-json-1.0"}, body: `{"QueueUrl":"` + inbound + `","MessageBody":"x","MessageGroupId":"g"}`},
		"unsigned admin reset":                       {path: "/_ministack/reset"},
		"unsigned admin message dump":                {path: "/_ministack/sqs/messages"},
		"root key in the query string":               {path: "/", query: "X-Amz-Credential=test%2F20260101%2Fus-east-1%2Fsqs%2Faws4_request", body: "Action=ListQueues"},
		"escaped admin path with an IAM-looking key": {path: "/%5Fministack/reset", headers: map[string]string{"Authorization": "AWS4-HMAC-SHA256 Credential=AKIA0000000000000000/20260101/us-east-1/sqs/aws4_request, SignedHeaders=host, Signature=00"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			u := endpoint + tc.path
			if tc.query != "" {
				u += "?" + tc.query
			}
			req, err := http.NewRequest(http.MethodPost, u, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
	// The emulator's state survived the attempted reset.
	op := brokertest.Client(t, endpoint, brokertest.Operator)
	if _, err := op.GetQueueUrl(context.Background(), &sqs.GetQueueUrlInput{QueueName: aws.String(brokertest.InboundQ)}); err != nil {
		t.Fatalf("provisioned inbound queue vanished: %v", err)
	}
}

// The authorized producer publishes to the provisioned inbound queue. With
// WAGERING_TEST_COMPOSE_APP_URL set, the message is a poison one that the live
// Compose service (signing as wagering-app) must consume until the broker's
// redrive moves it to the DLQ; that exercises the service identity for real.
func TestAuthorizedProducerPublishesAndServiceConsumes(t *testing.T) {
	endpoint := brokertest.Endpoint(t)
	inbound := brokertest.QueueURL(endpoint, brokertest.InboundQ)
	dlq := brokertest.QueueURL(endpoint, brokertest.DLQ)
	producer := brokertest.Client(t, endpoint, brokertest.Producer)
	operator := brokertest.Client(t, endpoint, brokertest.Operator)
	marker := randomName(t, "r19f1-authorized-")
	body := `{"messageId":"` + marker + `","type":"WagerTransactionRequested"`

	if err := sendTo(producer, inbound, body); err != nil {
		t.Fatalf("authorized producer could not publish: %v", err)
	}

	// Find the message (in the DLQ once the live service has exhausted it, or
	// still on the inbound queue when no service runs), and remove it either way.
	live := os.Getenv("WAGERING_TEST_COMPOSE_APP_URL") != ""
	deadline := time.Now().Add(45 * time.Second)
	found := ""
	for time.Now().Before(deadline) && found == "" {
		for _, q := range []string{dlq, inbound} {
			if q == inbound && live {
				continue // the service owns the inbound receipts; wait for redrive
			}
			out, err := operator.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{QueueUrl: aws.String(q), MaxNumberOfMessages: 10, WaitTimeSeconds: 1, VisibilityTimeout: 5})
			if err != nil {
				t.Fatalf("operator receive from %s: %v", q, err)
			}
			for _, m := range out.Messages {
				if strings.Contains(aws.ToString(m.Body), marker) {
					found = q
				}
				if strings.Contains(aws.ToString(m.Body), "r19f1-") {
					_, _ = operator.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{QueueUrl: aws.String(q), ReceiptHandle: m.ReceiptHandle})
				}
			}
		}
	}
	if found == "" {
		t.Fatalf("published message %s was not found on the inbound queue or the DLQ", marker)
	}
	if live && found != dlq {
		t.Fatalf("message found on %s, want the DLQ after the live service consumed it", found)
	}
}
