package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func evCtx() EventContext {
	return EventContext{EventID: id(9), CorrelationID: "corr-1", CausationID: "cause-1", OccurredAt: time.Date(2026, 3, 4, 5, 6, 7, 123000000, time.FixedZone("x", -3*3600))}
}

func TestEventEnvelopeSerialization(t *testing.T) {
	w := newWallet(t, "100.00")
	mv, _ := w.Debit(brl(t, "30.00"), t0)
	ev, err := NewWalletBalanceChanged(evCtx(), w.ID(), id(200), mv)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"eventId", "eventType", "aggregateId", "correlationId", "causationId", "occurredAt", "version", "data"} {
		if _, ok := generic[k]; !ok {
			t.Errorf("missing %s in %s", k, raw)
		}
	}
	if generic["eventType"] != "WalletBalanceChanged" || generic["version"].(float64) != 1 ||
		generic["occurredAt"] != "2026-03-04T08:06:07.123Z" || generic["aggregateId"] != w.ID().String() {
		t.Errorf("header %s", raw)
	}
	data := generic["data"].(map[string]any)
	for _, k := range []string{"walletId", "transactionId", "direction", "money", "balanceBefore", "balanceAfter", "walletVersion"} {
		if _, ok := data[k]; !ok {
			t.Errorf("missing data.%s in %s", k, raw)
		}
	}
	if m := data["money"].(map[string]any); m["amount"] != "30.00" || m["currency"] != "BRL" {
		t.Errorf("money %v", m)
	}
	if strings.Contains(string(raw), "e+") || strings.Contains(string(raw), `"amount":3`) {
		t.Errorf("money must be a decimal string: %s", raw)
	}
	var back Envelope[WalletBalanceChangedData]
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Data != ev.Data || back.EventID != ev.EventID || !back.OccurredAt.Equal(ev.OccurredAt) || back.OccurredAt.Location() != time.UTC {
		t.Errorf("round trip %+v vs %+v", back, ev)
	}
}

func TestEventConstructorsSetTypeVersionAndUTC(t *testing.T) {
	proc := newTx(t, KindBet, "1.00", "")
	_ = proc.MarkProcessed(brl(t, "9.00"), t0)
	rej := newTx(t, KindBet, "1.00", "")
	_ = rej.MarkRejected(FailureInsufficientFundsBet, Money{}, t0)
	pend := newTx(t, KindRefund, "1.00", "b")
	_ = pend.MarkPendingReference(t0, DefaultRetryPolicy())

	a, err := NewWagerTransactionProcessed(evCtx(), proc)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewWagerTransactionRejected(evCtx(), rej)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewWagerTransactionPendingReference(evCtx(), pend)
	if err != nil {
		t.Fatal(err)
	}
	want := map[EventType]EventMeta{
		EventWagerTransactionProcessed: a.Meta(), EventWagerTransactionRejected: b.Meta(), EventWagerTransactionPendingReference: c.Meta(),
	}
	for typ, m := range want {
		if m.EventType != typ || m.Version != EventVersion || m.OccurredAt.Location() != time.UTC || m.AggregateID != id(100) {
			t.Errorf("%s meta %+v", typ, m)
		}
	}
	if b.Data.ObservedBalance != nil {
		t.Error("unknown observed balance must be omitted")
	}
	raw, _ := json.Marshal(b)
	if strings.Contains(string(raw), "observedBalance") {
		t.Errorf("observedBalance not omitted: %s", raw)
	}
	if b.Data.FailureCode != FailureInsufficientFundsBet || c.Data.AttemptCount != 1 || c.Data.ReferenceExternalTransactionID != "b" {
		t.Errorf("payloads %+v %+v", b.Data, c.Data)
	}
}

func TestEventConstructorsRefuseWrongStateAndBadContext(t *testing.T) {
	pending := newTx(t, KindBet, "1.00", "")
	if _, err := NewWagerTransactionProcessed(evCtx(), pending); err == nil {
		t.Error("processed event for PENDING")
	}
	if _, err := NewWagerTransactionRejected(evCtx(), pending); err == nil {
		t.Error("rejected event for PENDING")
	}
	if _, err := NewWagerTransactionPendingReference(evCtx(), pending); err == nil {
		t.Error("pending event for PENDING")
	}
	proc := newTx(t, KindBet, "1.00", "")
	_ = proc.MarkProcessed(brl(t, "1"), t0)
	for name, mut := range map[string]func(*EventContext){
		"no event id":    func(c *EventContext) { c.EventID = UUID{} },
		"no correlation": func(c *EventContext) { c.CorrelationID = "" },
		"no time":        func(c *EventContext) { c.OccurredAt = time.Time{} },
		"bad causation":  func(c *EventContext) { c.CausationID = " x" },
	} {
		c := evCtx()
		mut(&c)
		if _, err := NewWagerTransactionProcessed(c, proc); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	w := newWallet(t, "1.00")
	if _, err := NewWalletBalanceChanged(evCtx(), w.ID(), id(1), Movement{}); err == nil {
		t.Error("empty movement accepted")
	}
	c := evCtx()
	c.CausationID = ""
	ev, err := NewWagerTransactionProcessed(c, proc)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(ev); strings.Contains(string(raw), "causationId") {
		t.Errorf("empty causationId must be omitted: %s", raw)
	}
}

func TestOutboxSnapshotIsImmutableAndRehydrates(t *testing.T) {
	proc := newTx(t, KindBet, "1.00", "")
	_ = proc.MarkProcessed(brl(t, "1"), t0)
	ev, _ := NewWagerTransactionProcessed(evCtx(), proc)
	o, err := NewOutboxEvent(ev, t0)
	if err != nil {
		t.Fatal(err)
	}
	if o.EventID() != ev.EventID || o.AggregateID() != ev.AggregateID || o.EventType() != EventWagerTransactionProcessed || o.IsPublished() || !o.IsDue(t0) || o.IsDue(t0.Add(-time.Second)) {
		t.Fatalf("%+v", o.State())
	}
	p := o.Payload()
	p[0] = 'X'
	if o.Payload()[0] != '{' {
		t.Error("payload is not defensively copied")
	}
	st := o.State()
	st.Payload[0] = 'X'
	if o.Payload()[0] != '{' {
		t.Error("state payload aliases internal bytes")
	}
	back, err := RehydrateOutboxEvent(o.State())
	if err != nil || string(back.Payload()) != string(o.Payload()) {
		t.Fatalf("rehydrate: %v", err)
	}

	bad := map[string]func(*OutboxEventState){
		"garbage payload":    func(s *OutboxEventState) { s.Payload = []byte("nope") },
		"empty payload":      func(s *OutboxEventState) { s.Payload = nil },
		"id mismatch":        func(s *OutboxEventState) { s.EventID = id(77) },
		"aggregate mismatch": func(s *OutboxEventState) { s.AggregateID = id(77) },
		"type mismatch":      func(s *OutboxEventState) { s.EventType = EventWalletBalanceChanged },
		"no next attempt":    func(s *OutboxEventState) { s.NextAttemptAt = time.Time{} },
		"published+next":     func(s *OutboxEventState) { s.PublishedAt = t0 },
		"negative attempts":  func(s *OutboxEventState) { s.Attempts = -1 },
	}
	for name, mut := range bad {
		s := o.State()
		mut(&s)
		if _, err := RehydrateOutboxEvent(s); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestOutboxPublishLifecycle(t *testing.T) {
	policy := RetryPolicy{BaseDelay: time.Second, MaxDelay: time.Minute, TTL: time.Hour}
	proc := newTx(t, KindBet, "1.00", "")
	_ = proc.MarkProcessed(brl(t, "1"), t0)
	ev, _ := NewWagerTransactionProcessed(evCtx(), proc)
	o, _ := NewOutboxEvent(ev, t0)

	if err := o.RecordPublishFailure(t0, policy); err != nil {
		t.Fatal(err)
	}
	if o.Attempts() != 1 || !o.NextAttemptAt().Equal(t0.Add(time.Second)) || o.IsDue(t0) || !o.IsDue(t0.Add(time.Second)) {
		t.Fatalf("%+v", o.State())
	}
	_ = o.RecordPublishFailure(t0, policy)
	if !o.NextAttemptAt().Equal(t0.Add(2 * time.Second)) {
		t.Errorf("backoff %v", o.NextAttemptAt())
	}
	id0 := o.EventID()
	if err := o.MarkPublished(t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !o.IsPublished() || !o.NextAttemptAt().IsZero() || o.IsDue(t0.Add(time.Hour)) || o.EventID() != id0 {
		t.Fatalf("%+v", o.State())
	}
	if o.MarkPublished(t0) == nil || o.RecordPublishFailure(t0, policy) == nil {
		t.Error("published event accepted further transitions")
	}
	if _, err := RehydrateOutboxEvent(o.State()); err != nil {
		t.Errorf("published rehydrate: %v", err)
	}
}

func TestInboxMessage(t *testing.T) {
	m, err := NewInboxMessage("wagering-consumer", "msg-1", hash64, t0)
	if err != nil {
		t.Fatal(err)
	}
	if m.IsCompleted() || m.CheckReplay(hash64) != nil {
		t.Fatal("fresh message")
	}
	err = m.CheckReplay(strings.Repeat("cd", 32))
	if !IsConflict(err) || !errorsIs(err, ErrInboxHashConflict) {
		t.Errorf("hash conflict: %v", err)
	}
	if m.Complete(t0.Add(-time.Second)) == nil {
		t.Error("completion before receipt accepted")
	}
	if err := m.Complete(t0.Add(time.Second)); err != nil || !m.IsCompleted() {
		t.Fatal(err)
	}
	if m.Complete(t0.Add(2*time.Second)) == nil {
		t.Error("double completion accepted")
	}
	if back, err := RehydrateInboxMessage(m.State()); err != nil || back.State() != m.State() {
		t.Errorf("rehydrate: %v", err)
	}
	for name, s := range map[string]InboxMessageState{
		"no consumer":               {MessageID: "m", RequestHash: hash64, ReceivedAt: t0},
		"no message":                {ConsumerName: "c", RequestHash: hash64, ReceivedAt: t0},
		"bad hash":                  {ConsumerName: "c", MessageID: "m", RequestHash: "x", ReceivedAt: t0},
		"no time":                   {ConsumerName: "c", MessageID: "m", RequestHash: hash64},
		"completed before received": {ConsumerName: "c", MessageID: "m", RequestHash: hash64, ReceivedAt: t0, CompletedAt: t0.Add(-time.Second)},
	} {
		if _, err := RehydrateInboxMessage(s); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
