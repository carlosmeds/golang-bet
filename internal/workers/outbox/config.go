package outbox

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Backoff is the exponential delay between publish attempts of one event:
// BaseDelay, 2*BaseDelay, ... capped at MaxDelay. Events are never abandoned;
// an unpublished event keeps being retried at the MaxDelay rate.
type Backoff struct {
	BaseDelay time.Duration
	MaxDelay  time.Duration
}

// Delay returns the wait after the given number of failed attempts (>= 1).
func (b Backoff) Delay(attempt int) time.Duration {
	d := b.BaseDelay
	for i := 1; i < attempt; i++ {
		if d >= b.MaxDelay/2 {
			return b.MaxDelay
		}
		d *= 2
	}
	return min(d, b.MaxDelay)
}

// Config tunes one publisher instance.
type Config struct {
	Backoff Backoff
	// Owner identifies this instance in lease_owner. Empty means generated.
	Owner string
	// BatchSize is the maximum number of events claimed per pass.
	BatchSize int
	// Concurrency is the number of aggregates published in parallel. Events of
	// one aggregate in a batch are sent sequentially in outbox order.
	Concurrency int
	// Lease is how long a claim blocks other instances. A crashed publisher's
	// events become claimable again after this time.
	Lease time.Duration
	// SendTimeout bounds one Sender call; AckTimeout bounds the SQL update that
	// records its outcome. Together they must fit inside Lease, so a claim
	// normally cannot expire while an event is being published.
	SendTimeout time.Duration
	AckTimeout  time.Duration
	// PollInterval is the idle wait when a pass finds no more due work. It is
	// also the pause after a claim error.
	PollInterval time.Duration
}

// DefaultConfig returns production defaults.
func DefaultConfig() Config {
	return Config{
		Backoff:      Backoff{BaseDelay: time.Second, MaxDelay: 5 * time.Minute},
		BatchSize:    20,
		Concurrency:  4,
		Lease:        30 * time.Second,
		SendTimeout:  10 * time.Second,
		AckTimeout:   5 * time.Second,
		PollInterval: 500 * time.Millisecond,
	}
}

// Validate rejects settings that would break lease safety or spin.
func (c Config) Validate() error {
	switch {
	case c.Backoff.BaseDelay <= 0 || c.Backoff.MaxDelay < c.Backoff.BaseDelay:
		return errors.New("outbox publisher: backoff requires 0 < base delay <= max delay")
	case c.BatchSize < 1:
		return errors.New("outbox publisher: batch size must be positive")
	case c.Concurrency < 1:
		return errors.New("outbox publisher: concurrency must be positive")
	case c.Lease <= 0 || c.SendTimeout <= 0 || c.AckTimeout <= 0 || c.PollInterval <= 0:
		return errors.New("outbox publisher: lease, send timeout, ack timeout and poll interval must be positive")
	case c.SendTimeout+c.AckTimeout >= c.Lease:
		return errors.New("outbox publisher: send timeout plus ack timeout must be shorter than the lease")
	}
	return nil
}

// LoadConfig reads optional OUTBOX_* environment variables over the defaults
// and validates the result:
//
//	OUTBOX_BACKOFF_BASE_DELAY, OUTBOX_BACKOFF_MAX_DELAY, OUTBOX_BATCH_SIZE,
//	OUTBOX_CONCURRENCY, OUTBOX_LEASE, OUTBOX_SEND_TIMEOUT, OUTBOX_ACK_TIMEOUT,
//	OUTBOX_POLL_INTERVAL
func LoadConfig() (Config, error) {
	c := DefaultConfig()
	var errs []error
	dur := func(name string, dst *time.Duration) {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				return
			}
			*dst = d
		}
	}
	num := func(name string, dst *int) {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				return
			}
			*dst = n
		}
	}
	dur("OUTBOX_BACKOFF_BASE_DELAY", &c.Backoff.BaseDelay)
	dur("OUTBOX_BACKOFF_MAX_DELAY", &c.Backoff.MaxDelay)
	num("OUTBOX_BATCH_SIZE", &c.BatchSize)
	num("OUTBOX_CONCURRENCY", &c.Concurrency)
	dur("OUTBOX_LEASE", &c.Lease)
	dur("OUTBOX_SEND_TIMEOUT", &c.SendTimeout)
	dur("OUTBOX_ACK_TIMEOUT", &c.AckTimeout)
	dur("OUTBOX_POLL_INTERVAL", &c.PollInterval)
	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}
