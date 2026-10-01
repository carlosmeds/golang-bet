package sqs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"wagering/internal/contract"
	wager "wagering/internal/usecase/wagering"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// F-2: a PostgreSQL outage must not spend the inbound queue's redrive budget.
// outageLength exceeds the 7 s in which the old provisioning (maxReceiveCount=3,
// 1 s/2 s backoff, receives never paused) dead-lettered a valid wager.
const outageLength = 15 * time.Second

// database stands in for PostgreSQL: Ready is the readiness ping and Execute
// the durable commit. Both fail while it is down.
type database struct {
	mu          sync.Mutex
	up          bool
	failNextTxn bool // the next Execute fails and takes the database down
	attempts    int  // Execute calls while the database was down
	committed   map[string]int
}

func newDatabase(up bool) *database { return &database{up: up, committed: map[string]int{}} }

func (d *database) setUp(up bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.up = up
}

func (d *database) Ready(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.up {
		return errors.New("connection refused")
	}
	return nil
}

func (d *database) Execute(_ context.Context, _ contract.Operation, _ string, messageID string, _ *wager.Inbox) (wager.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failNextTxn {
		d.failNextTxn, d.up = false, false
	}
	if !d.up {
		d.attempts++
		return wager.Result{}, errors.New("connection refused")
	}
	d.committed[messageID]++
	return wager.Result{}, nil
}

func (d *database) snapshot() (attempts int, committed map[string]int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]int{}
	for k, v := range d.committed {
		out[k] = v
	}
	return d.attempts, out
}

func startWorker(t *testing.T, c *sqs.Client, queue string, db *database) {
	t.Helper()
	w := worker(t, c, queue, db)
	w.Ready = db.Ready
	w.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = w.Stop(ctx)
	})
}

func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func bodyWithID(id string) string {
	return strings.Replace(validBody, `"messageId":"t19-msg-1"`, `"messageId":"`+id+`"`, 1)
}

// The database is down when the wager arrives and stays down for longer than
// the old redrive window: nothing is received meanwhile, and after recovery
// the wager is committed exactly once and never reaches the DLQ.
func TestValidWagerSurvivesDatabaseOutageBeforeDelivery(t *testing.T) {
	t.Parallel()
	c, _ := client(t)
	queue, dlq := pair(t, c)
	db := newDatabase(false)
	startWorker(t, c, queue, db)

	send(t, c, queue, bodyWithID("f2-before"))
	time.Sleep(outageLength)

	if attempts, committed := db.snapshot(); attempts != 0 || len(committed) != 0 {
		t.Fatalf("during the outage: %d processing attempts, committed %v; receives must be paused", attempts, committed)
	}
	if n := count(t, c, queue); n != 1 {
		t.Fatalf("inbound queue holds %d messages during the outage, want 1", n)
	}
	if n := count(t, c, dlq); n != 0 {
		t.Fatalf("DLQ holds %d messages during the outage", n)
	}

	db.setUp(true)
	waitUntil(t, 30*time.Second, "commit after recovery", func() bool { _, c := db.snapshot(); return c["f2-before"] == 1 })
	waitUntil(t, 30*time.Second, "inbound queue to drain", func() bool { return count(t, c, queue) == 0 })
	if _, committed := db.snapshot(); committed["f2-before"] != 1 {
		t.Fatalf("committed %d times, want exactly once", committed["f2-before"])
	}
	if n := count(t, c, dlq); n != 0 {
		t.Fatalf("valid wager reached the DLQ (%d)", n)
	}
}

// The database fails while the wager is being processed (one attempt is
// spent), then stays down for longer than the old window.
func TestValidWagerSurvivesDatabaseOutageDuringProcessing(t *testing.T) {
	t.Parallel()
	c, _ := client(t)
	queue, dlq := pair(t, c)
	db := newDatabase(true)
	db.failNextTxn = true
	startWorker(t, c, queue, db)

	send(t, c, queue, bodyWithID("f2-during"))
	waitUntil(t, 10*time.Second, "the failed attempt", func() bool { a, _ := db.snapshot(); return a == 1 })
	time.Sleep(outageLength)

	if attempts, _ := db.snapshot(); attempts != 1 {
		t.Fatalf("%d processing attempts during the outage, want 1 (receives must pause after the first failure)", attempts)
	}
	if n := count(t, c, dlq); n != 0 {
		t.Fatalf("DLQ holds %d messages during the outage", n)
	}

	db.setUp(true)
	waitUntil(t, 30*time.Second, "commit after recovery", func() bool { _, c := db.snapshot(); return c["f2-during"] == 1 })
	waitUntil(t, 30*time.Second, "inbound queue to drain", func() bool { return count(t, c, queue) == 0 })
	if _, committed := db.snapshot(); committed["f2-during"] != 1 {
		t.Fatalf("committed %d times, want exactly once", committed["f2-during"])
	}
	if n := count(t, c, dlq); n != 0 {
		t.Fatalf("valid wager reached the DLQ (%d)", n)
	}
}

// A poison message that arrives with a valid one during an outage is still
// dead-lettered once receiving resumes; the valid one is committed once.
func TestPoisonMessageStillReachesDLQAfterOutage(t *testing.T) {
	t.Parallel()
	c, _ := client(t)
	queue, dlq := pair(t, c)
	db := newDatabase(false)
	startWorker(t, c, queue, db)

	send(t, c, queue, `{"messageId":"f2-poison","type":"WagerTransactionRequested"`)
	send(t, c, queue, bodyWithID("f2-valid"))
	time.Sleep(outageLength)
	if n := count(t, c, dlq); n != 0 {
		t.Fatalf("DLQ holds %d messages during the outage", n)
	}

	db.setUp(true)
	waitUntil(t, 90*time.Second, "poison in the DLQ and valid wager committed", func() bool {
		_, committed := db.snapshot()
		return count(t, c, dlq) == 1 && committed["f2-valid"] == 1
	})
	waitUntil(t, 30*time.Second, "inbound queue to drain", func() bool { return count(t, c, queue) == 0 })
	if _, committed := db.snapshot(); committed["f2-valid"] != 1 || len(committed) != 1 {
		t.Fatalf("committed %v, want only f2-valid once", committed)
	}
}
