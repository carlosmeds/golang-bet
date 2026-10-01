//go:build systemfault

package outbox

import (
	"context"
	"os"
)

// This hook is compiled only into test binaries made with -tags=systemfault.
// It deliberately holds real PostgreSQL lease claims until the test kills the
// process, making claim recovery deterministic.
func afterOutboxClaim(ctx context.Context) error {
	marker := os.Getenv("WAGERING_FAULT_AFTER_OUTBOX_CLAIM_MARKER")
	if marker == "" {
		return nil
	}
	if err := os.WriteFile(marker, []byte("claimed"), 0600); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}
