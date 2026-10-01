//go:build !systemfault

package outbox

import "context"

func afterOutboxClaim(context.Context) error { return nil }
