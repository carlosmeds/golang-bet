package idempotency

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"wagering/internal/contract"
)

// CanonicalJSON encodes precisely the business fields of an operation. Go's
// encoding/json sorts map keys, including nested money keys, lexicographically.
// UUIDs and Money are already normalized by their value types. Opaque external
// identifiers retain their exact validated spelling. The idempotency key,
// message envelope, timestamps and HTTP headers are intentionally excluded.
func CanonicalJSON(op contract.Operation) ([]byte, error) {
	if err := op.Validate(); err != nil {
		return nil, err
	}
	fields := map[string]any{
		"providerId":            op.ProviderID,
		"externalTransactionId": op.ExternalTransactionID,
		"playerId":              op.PlayerID,
		"walletId":              op.WalletID.String(),
		"roundId":               op.RoundID,
		"gameId":                op.GameID,
		"kind":                  op.Kind,
		"money": map[string]string{
			"amount":   op.Money.Amount(),
			"currency": op.Money.Currency().String(),
		},
	}
	if op.ReferenceExternalTransactionID != "" {
		fields["referenceExternalTransactionId"] = op.ReferenceExternalTransactionID
	}
	return json.Marshal(fields)
}

func Hash(op contract.Operation) (string, error) {
	canonical, err := CanonicalJSON(op)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
