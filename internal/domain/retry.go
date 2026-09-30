package domain

import "time"

// RetryPolicy bounds the wait for a missing or pending reference (REQ-042,
// REQ-043). Backoff is exponential from BaseDelay, capped at MaxDelay. The wait
// ends when TTL has elapsed since the transaction was created or, when
// MaxAttempts is positive, when that many reference lookups have failed.
type RetryPolicy struct {
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	TTL         time.Duration
	MaxAttempts int // 0 = bounded by TTL only
}

// DefaultRetryPolicy mirrors the documented defaults: 1s base, 5 minute
// ceiling, 24 hour TTL, no attempt cap. Deployments override it by config.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{BaseDelay: time.Second, MaxDelay: 5 * time.Minute, TTL: 24 * time.Hour}
}

// Validate checks the policy values.
func (p RetryPolicy) Validate() error {
	if p.BaseDelay <= 0 || p.MaxDelay < p.BaseDelay || p.TTL <= 0 || p.MaxAttempts < 0 {
		return newInvalid(CodeInvalidOperation, "retry policy requires 0 < base <= max delay, positive ttl and non-negative max attempts")
	}
	return nil
}

// Backoff returns the delay before the next lookup after the given number of
// failed attempts (attempt >= 1): base, 2*base, 4*base, ... up to MaxDelay.
func (p RetryPolicy) Backoff(attempt int) time.Duration {
	d := p.BaseDelay
	for i := 1; i < attempt; i++ {
		if d >= p.MaxDelay/2 {
			return p.MaxDelay
		}
		d *= 2
	}
	if d > p.MaxDelay {
		return p.MaxDelay
	}
	return d
}

// Exhausted reports whether the wait is over once `attempts` lookups have
// failed, evaluated at now for a transaction created at createdAt.
func (p RetryPolicy) Exhausted(attempts int, createdAt, now time.Time) bool {
	if p.MaxAttempts > 0 && attempts >= p.MaxAttempts {
		return true
	}
	return !now.Before(createdAt.Add(p.TTL))
}
