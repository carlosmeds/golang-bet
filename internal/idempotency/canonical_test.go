package idempotency

import (
	"strings"
	"testing"

	"wagering/internal/contract"
)

const walletID = "0192f291-27dd-7d3f-8071-5f8685deef37"

func TestHTTPAndSQSShareCanonicalHash(t *testing.T) {
	httpBody := `{"providerId":"provider-a","externalTransactionId":"transaction-123","playerId":"player-1","walletId":"0192F291-27DD-7D3F-8071-5F8685DEEF37","roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25","currency":"brl"}}`
	sqsBody := `{"messageId":"msg-123","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00Z","data":{"gameId":"fortune-chimp","roundId":"round-987","walletId":"` + walletID + `","playerId":"player-1","providerId":"provider-a","externalTransactionId":"transaction-123","kind":"BET","money":{"currency":"BRL","amount":"25.00"},"idempotencyKey":"provider-a:transaction-123"}}`
	httpOp, err := contract.ParseHTTP([]byte(httpBody))
	if err != nil {
		t.Fatal(err)
	}
	sqs, err := contract.ParseSQS([]byte(sqsBody))
	if err != nil {
		t.Fatal(err)
	}
	h1, err := Hash(httpOp)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := Hash(sqs.Data.Operation)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("hash differs across transports: %s != %s", h1, h2)
	}
	canonical, err := CanonicalJSON(httpOp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canonical), "idempotencyKey") || strings.Contains(string(canonical), "messageId") {
		t.Fatalf("transport identity leaked into canonical JSON: %s", canonical)
	}
	if !strings.Contains(string(canonical), `"amount":"25.00"`) || !strings.Contains(string(canonical), `"currency":"BRL"`) {
		t.Fatalf("money was not normalized: %s", canonical)
	}
}

func TestBusinessFieldChangesHash(t *testing.T) {
	a := `{"providerId":"p","externalTransactionId":"x","playerId":"a","walletId":"` + walletID + `","roundId":"r","gameId":"g","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}`
	b := strings.Replace(a, `"amount":"25.00"`, `"amount":"26.00"`, 1)
	opA, err := contract.ParseHTTP([]byte(a))
	if err != nil {
		t.Fatal(err)
	}
	opB, err := contract.ParseHTTP([]byte(b))
	if err != nil {
		t.Fatal(err)
	}
	ha, _ := Hash(opA)
	hb, _ := Hash(opB)
	if ha == hb {
		t.Fatal("different amount produced same hash")
	}
}

func TestTransportRejectsOpeningAndMissingSQSKey(t *testing.T) {
	opening := `{"providerId":"p","externalTransactionId":"x","playerId":"a","walletId":"` + walletID + `","roundId":"r","gameId":"g","kind":"OPENING","money":{"amount":"25.00","currency":"BRL"}}`
	if _, err := contract.ParseHTTP([]byte(opening)); err == nil {
		t.Fatal("external OPENING accepted")
	}
	sqs := `{"messageId":"msg","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00Z","data":{"providerId":"p","externalTransactionId":"x","playerId":"a","walletId":"` + walletID + `","roundId":"r","gameId":"g","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}}`
	if _, err := contract.ParseSQS([]byte(sqs)); err == nil {
		t.Fatal("missing SQS financial idempotency key accepted")
	}
}
