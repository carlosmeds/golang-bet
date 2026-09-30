package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"wagering/internal/domain"
)

// Operation is the business payload shared by HTTP and SQS. The transport
// idempotency key is deliberately separate and absent from its canonical hash.
type Operation struct {
	ProviderID                     string       `json:"providerId"`
	ExternalTransactionID          string       `json:"externalTransactionId"`
	PlayerID                       string       `json:"playerId"`
	WalletID                       domain.UUID  `json:"walletId"`
	RoundID                        string       `json:"roundId"`
	GameID                         string       `json:"gameId"`
	Kind                           domain.Kind  `json:"kind"`
	Money                          domain.Money `json:"money"`
	ReferenceExternalTransactionID string       `json:"referenceExternalTransactionId,omitempty"`
}

// ParseHTTP accepts exactly one JSON object and rejects unknown fields.
// Idempotency-Key is a required HTTP header and is validated separately.
func ParseHTTP(body []byte) (Operation, error) {
	var op Operation
	if err := decodeOne(body, &op); err != nil {
		return Operation{}, err
	}
	if err := op.Validate(); err != nil {
		return Operation{}, err
	}
	return op, nil
}

func (o Operation) Validate() error {
	for name, value := range map[string]string{
		"providerId": o.ProviderID, "externalTransactionId": o.ExternalTransactionID,
		"playerId": o.PlayerID, "roundId": o.RoundID, "gameId": o.GameID,
	} {
		if err := validateIdentifier(name, value); err != nil {
			return err
		}
	}
	if o.WalletID.IsNil() {
		return errors.New("walletId is required")
	}
	kind, err := domain.ParseExternalKind(string(o.Kind))
	if err != nil {
		return err
	}
	if err := domain.ValidateAmount(kind, o.Money); err != nil {
		return err
	}
	rules, _ := domain.RulesFor(kind)
	switch rules.Reference {
	case domain.ReferenceForbidden:
		if o.ReferenceExternalTransactionID != "" {
			return fmt.Errorf("%s does not accept referenceExternalTransactionId", kind)
		}
	case domain.ReferenceRequired:
		if o.ReferenceExternalTransactionID == "" {
			return fmt.Errorf("%s requires referenceExternalTransactionId", kind)
		}
	}
	if o.ReferenceExternalTransactionID != "" {
		if err := validateIdentifier("referenceExternalTransactionId", o.ReferenceExternalTransactionID); err != nil {
			return err
		}
	}
	return nil
}

// ValidateIdempotencyKey never substitutes a calculated key for the caller's
// supplied key. It permits the same opaque identifier syntax as the domain.
func ValidateIdempotencyKey(key string) error {
	return validateIdentifier("idempotency key", key)
}

func validateIdentifier(name, s string) error {
	if s == "" || len(s) > domain.MaxIdentifierLength || strings.TrimSpace(s) != s || !utf8.ValidString(s) {
		return fmt.Errorf("%s must be a nonblank identifier of at most %d bytes without surrounding whitespace", name, domain.MaxIdentifierLength)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains a control character", name)
		}
	}
	return nil
}

func decodeOne(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("invalid JSON: multiple values")
		}
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// SQSRequest is the inbound FIFO message contract. MessageID is the durable
// inbox identity and Data.IdempotencyKey is the financial idempotency identity.
type SQSRequest struct {
	MessageID  string    `json:"messageId"`
	Type       string    `json:"type"`
	OccurredAt time.Time `json:"occurredAt"`
	Data       struct {
		Operation
		IdempotencyKey string `json:"idempotencyKey"`
	} `json:"data"`
}

func ParseSQS(body []byte) (SQSRequest, error) {
	var request SQSRequest
	if err := decodeOne(body, &request); err != nil {
		return SQSRequest{}, err
	}
	if err := validateIdentifier("messageId", request.MessageID); err != nil {
		return SQSRequest{}, err
	}
	if request.Type != "WagerTransactionRequested" {
		return SQSRequest{}, fmt.Errorf("unsupported message type %q", request.Type)
	}
	if request.OccurredAt.IsZero() {
		return SQSRequest{}, errors.New("occurredAt is required")
	}
	if err := ValidateIdempotencyKey(request.Data.IdempotencyKey); err != nil {
		return SQSRequest{}, err
	}
	if err := request.Data.Operation.Validate(); err != nil {
		return SQSRequest{}, err
	}
	return request, nil
}
