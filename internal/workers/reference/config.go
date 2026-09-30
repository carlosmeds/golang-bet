package reference

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"wagering/internal/domain"
)

// Config tunes the worker and the retry policy it shares with the HTTP/SQS
// entry path. Both must use the same Retry value: the entry path schedules the
// first attempt and the worker schedules every later one and decides expiry.
type Config struct {
	// Retry bounds the wait for a reference: exponential backoff from
	// BaseDelay to MaxDelay, ending at TTL or MaxAttempts (0 = TTL only).
	Retry domain.RetryPolicy
	// Owner identifies this instance in lease_owner. Empty means generated.
	Owner string
	// BatchSize is the maximum number of rows claimed per pass.
	BatchSize int
	// Concurrency is the number of rows processed in parallel within a pass.
	Concurrency int
	// Lease is how long a claim blocks other instances. A crashed worker's
	// rows become claimable again after this time.
	Lease time.Duration
	// ItemTimeout bounds one RetryPending call. It must be shorter than Lease
	// so a claim normally cannot expire while its row is being processed.
	ItemTimeout time.Duration
	// PollInterval is the idle wait when a pass finds no more due work. It is
	// also the pause after a claim error.
	PollInterval time.Duration
}

// DefaultConfig returns production defaults (D08: 1s base, 5m cap, 24h TTL).
func DefaultConfig() Config {
	return Config{
		Retry:        domain.DefaultRetryPolicy(),
		BatchSize:    20,
		Concurrency:  4,
		Lease:        time.Minute,
		ItemTimeout:  20 * time.Second,
		PollInterval: time.Second,
	}
}

// Validate rejects settings that would break lease safety or spin.
func (c Config) Validate() error {
	if err := c.Retry.Validate(); err != nil {
		return err
	}
	switch {
	case c.BatchSize < 1:
		return errors.New("reference worker: batch size must be positive")
	case c.Concurrency < 1:
		return errors.New("reference worker: concurrency must be positive")
	case c.Lease <= 0 || c.ItemTimeout <= 0 || c.PollInterval <= 0:
		return errors.New("reference worker: lease, item timeout and poll interval must be positive")
	case c.ItemTimeout >= c.Lease:
		return errors.New("reference worker: item timeout must be shorter than the lease")
	}
	return nil
}

// LoadConfig reads optional REFERENCE_* environment variables over the
// defaults and validates the result:
//
//	REFERENCE_RETRY_BASE_DELAY, REFERENCE_RETRY_MAX_DELAY, REFERENCE_RETRY_TTL
//	REFERENCE_RETRY_MAX_ATTEMPTS, REFERENCE_WORKER_BATCH_SIZE,
//	REFERENCE_WORKER_CONCURRENCY, REFERENCE_WORKER_LEASE,
//	REFERENCE_WORKER_ITEM_TIMEOUT, REFERENCE_WORKER_POLL_INTERVAL
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
	dur("REFERENCE_RETRY_BASE_DELAY", &c.Retry.BaseDelay)
	dur("REFERENCE_RETRY_MAX_DELAY", &c.Retry.MaxDelay)
	dur("REFERENCE_RETRY_TTL", &c.Retry.TTL)
	num("REFERENCE_RETRY_MAX_ATTEMPTS", &c.Retry.MaxAttempts)
	num("REFERENCE_WORKER_BATCH_SIZE", &c.BatchSize)
	num("REFERENCE_WORKER_CONCURRENCY", &c.Concurrency)
	dur("REFERENCE_WORKER_LEASE", &c.Lease)
	dur("REFERENCE_WORKER_ITEM_TIMEOUT", &c.ItemTimeout)
	dur("REFERENCE_WORKER_POLL_INTERVAL", &c.PollInterval)
	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}
