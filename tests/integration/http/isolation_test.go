package httpauth_test

import (
	"fmt"
	"net/http"
	"testing"

	"wagering/internal/domain"
	"wagering/tests/integration/auth/idptest"
)

func get(w *world, token, path string) idptest.Response {
	return w.Do(idptest.Request{Method: http.MethodGet, Path: path, Token: token})
}

// sameRefusal asserts two 404s are indistinguishable, so a foreign record is
// not revealed by any difference from a record that does not exist.
func sameRefusal(t *testing.T, what string, got, nonexistent idptest.Response) {
	t.Helper()
	if got.Status != http.StatusNotFound {
		t.Errorf("%s: status %d, want 404; body %s", what, got.Status, got.Raw)
		return
	}
	a, b := got.JSON(t), nonexistent.JSON(t)
	delete(a, "correlationId")
	delete(b, "correlationId")
	if a["code"] != b["code"] || a["message"] != b["message"] || len(a) != len(b) {
		t.Errorf("%s differs from a nonexistent record: %v vs %v", what, a, b)
	}
}

// TestProviderCannotSeeAnotherProvidersTransactions covers both lookups: by
// internal transaction ID and by (provider, external transaction ID).
func TestProviderCannotSeeAnotherProvidersTransactions(t *testing.T) {
	w := newWorld(t)
	wallet := w.wallet(t, "100.00")
	player := w.player(t, wallet)
	ext := w.next("a-ext")
	a := w.bet(t, w.provA, "provider-a", wallet, player, ext, w.next("a-key"), "25.00")
	aID := a.JSON(t)["transactionId"].(string)

	missingID := domain.NewUUID().String()
	missingByID := get(w, w.provB, "/wagering/transactions/"+missingID)
	missingExt := get(w, w.provB, "/providers/provider-b/wagering/transactions/"+w.next("nope"))
	missingForeignExt := get(w, w.provB, "/providers/provider-a/wagering/transactions/"+w.next("nope"))

	t.Run("owner reads both ways", func(t *testing.T) {
		for _, path := range []string{"/wagering/transactions/" + aID, "/providers/provider-a/wagering/transactions/" + ext} {
			r := get(w, w.provA, path)
			if r.Status != http.StatusOK {
				t.Fatalf("%s: %d %s", path, r.Status, r.Raw)
			}
			body := r.JSON(t)
			if body["transactionId"] != aID || body["providerId"] != "provider-a" || body["externalTransactionId"] != ext || body["status"] != "PROCESSED" {
				t.Errorf("%s: %v", path, body)
			}
		}
	})

	t.Run("other provider by internal id", func(t *testing.T) {
		r := get(w, w.provB, "/wagering/transactions/"+aID)
		sameRefusal(t, "provider-b by id", r, missingByID)
		mustNotContain(t, "refusal", r.Raw, aID, ext, wallet, player, "25.00", "provider-a")
	})

	t.Run("other provider by external id in own namespace", func(t *testing.T) {
		r := get(w, w.provB, "/providers/provider-b/wagering/transactions/"+ext)
		sameRefusal(t, "provider-b own namespace", r, missingExt)
		mustNotContain(t, "refusal", r.Raw, aID, ext, wallet, player, "25.00")
	})

	t.Run("other provider by external id naming the owner's namespace", func(t *testing.T) {
		r := get(w, w.provB, "/providers/provider-a/wagering/transactions/"+ext)
		sameRefusal(t, "provider-b foreign namespace", r, missingForeignExt)
		mustNotContain(t, "refusal", r.Raw, aID, ext, wallet, player, "25.00")
	})

	t.Run("internal service is not a provider", func(t *testing.T) {
		for _, path := range []string{"/wagering/transactions/" + aID, "/providers/provider-a/wagering/transactions/" + ext} {
			if r := get(w, w.internal, path); r.Status != http.StatusForbidden {
				t.Errorf("%s: %d, want 403", path, r.Status)
			}
		}
	})
}

// TestSameExternalIDUnderAnotherProviderIsIndependent: identifiers are
// namespaced by provider, so provider-b reusing provider-a's external ID
// creates its own record and never touches provider-a's.
func TestSameExternalIDUnderAnotherProviderIsIndependent(t *testing.T) {
	w := newWorld(t)
	wallet := w.wallet(t, "100.00")
	player := w.player(t, wallet)
	ext := w.next("shared-ext")
	a := w.bet(t, w.provA, "provider-a", wallet, player, ext, w.next("a-key"), "10.00").JSON(t)
	b := w.bet(t, w.provB, "provider-b", wallet, player, ext, w.next("b-key"), "20.00").JSON(t)
	if a["transactionId"] == b["transactionId"] {
		t.Fatal("providers share a transaction")
	}
	ra := get(w, w.provA, "/providers/provider-a/wagering/transactions/"+ext).JSON(t)
	rb := get(w, w.provB, "/providers/provider-b/wagering/transactions/"+ext).JSON(t)
	if ra["transactionId"] != a["transactionId"] || amountOf(t, ra, "amount") != "10.00" {
		t.Errorf("provider-a record changed: %v", ra)
	}
	if rb["transactionId"] != b["transactionId"] || amountOf(t, rb, "amount") != "20.00" {
		t.Errorf("provider-b record wrong: %v", rb)
	}
}

// TestProviderCannotActForAnotherProvider: the providerId in a request is
// authorized against the token (REQ-060); a mismatch has no effect.
func TestProviderCannotActForAnotherProvider(t *testing.T) {
	w := newWorld(t)
	wallet := w.wallet(t, "100.00")
	player := w.player(t, wallet)
	minor, version := w.Balance(wallet)
	before := w.Counts()

	for _, kind := range []string{"BET", "WIN", "LOSS"} {
		amount := "5.00"
		if kind == "LOSS" {
			amount = "0.00"
		}
		ext := w.next("spoof")
		key := w.next("spoof-key")
		r := w.Submit(w.provB, key, idptest.Operation("provider-a", ext, wallet, player, kind, amount))
		if r.Status != http.StatusForbidden || r.Code() != "FORBIDDEN" {
			t.Errorf("%s as provider-a with provider-b token: %d %s", kind, r.Status, r.Raw)
		}
		assertUnchanged(t, w, "spoofed "+kind, before, wallet, minor, version)
		// The refusal must not have consumed the identifiers: the real owner can still use them.
		ok := w.Submit(w.provA, key, idptest.Operation("provider-a", ext, wallet, player, kind, amount))
		if ok.Status != http.StatusCreated || ok.JSON(t)["idempotentReplay"] != false {
			t.Errorf("owner after spoof attempt: %d %s", ok.Status, ok.Raw)
		}
		before = w.Counts()
		minor, version = w.Balance(wallet)
	}
}

// TestProviderCannotReverseAnotherProvidersTransaction: references resolve
// inside the caller's own provider namespace only, so provider-b can neither
// refund nor roll back provider-a's bet.
func TestProviderCannotReverseAnotherProvidersTransaction(t *testing.T) {
	w := newWorld(t)
	wallet := w.wallet(t, "100.00")
	player := w.player(t, wallet)
	betExt := w.next("a-bet")
	w.bet(t, w.provA, "provider-a", wallet, player, betExt, w.next("a-key"), "40.00")
	minor, version := w.Balance(wallet)

	for _, kind := range []string{"REFUND", "ROLLBACK"} {
		op := idptest.Operation("provider-b", w.next("b-"+kind), wallet, player, kind, "40.00")
		op["referenceExternalTransactionId"] = betExt
		r := w.Submit(w.provB, w.next("b-key"), op)
		if r.Status != http.StatusAccepted || r.JSON(t)["status"] != "PENDING_REFERENCE" {
			t.Errorf("provider-b %s of provider-a's bet: %d %s; want pending, unresolved reference", kind, r.Status, r.Raw)
		}
		if m, v := w.Balance(wallet); m != minor || v != version {
			t.Fatalf("provider-b %s moved provider-a's funds: balance %d->%d", kind, minor, m)
		}
	}
	// The owner can still reverse it exactly once.
	op := idptest.Operation("provider-a", w.next("a-refund"), wallet, player, "REFUND", "40.00")
	op["referenceExternalTransactionId"] = betExt
	if r := w.Submit(w.provA, w.next("a-key"), op); r.Status != http.StatusCreated {
		t.Errorf("owner refund: %d %s", r.Status, r.Raw)
	}
}

// TestReplaySecurity: idempotent replays return only the caller's own result
// (REQ-061) and failed authentication never consumes or reveals a key.
func TestReplaySecurity(t *testing.T) {
	w := newWorld(t)
	wallet := w.wallet(t, "100.00")
	player := w.player(t, wallet)
	ext, key := w.next("replay-ext"), w.next("replay-key")
	op := idptest.Operation("provider-a", ext, wallet, player, "BET", "30.00")

	t.Run("unauthenticated attempts with a fresh key leave it unused", func(t *testing.T) {
		fresh := w.next("unused-key")
		freshOp := idptest.Operation("provider-a", w.next("unused-ext"), wallet, player, "BET", "1.00")
		minor, version := w.Balance(wallet)
		before := w.Counts()
		for _, tok := range []string{"", "garbage", w.internal} {
			if r := w.Submit(tok, fresh, freshOp); r.Status != http.StatusUnauthorized && r.Status != http.StatusForbidden {
				t.Fatalf("token %.8q: %d", tok, r.Status)
			}
		}
		assertUnchanged(t, w, "unauthorized submit", before, wallet, minor, version)
		r := w.Submit(w.provA, fresh, freshOp)
		if r.Status != http.StatusCreated || r.JSON(t)["idempotentReplay"] != false {
			t.Errorf("first authorized use of the key: %d %s", r.Status, r.Raw)
		}
	})

	first := w.Submit(w.provA, key, op)
	if first.Status != http.StatusCreated {
		t.Fatalf("first: %d %s", first.Status, first.Raw)
	}
	orig := first.JSON(t)
	// Move the wallet so a replay that recomputed would show a different balance.
	w.bet(t, w.provA, "provider-a", wallet, player, w.next("later"), w.next("later-key"), "5.00")
	minor, version := w.Balance(wallet)
	before := w.Counts()
	origBalance := amountOf(t, orig, "balance")
	if current := fmt.Sprintf("%d.%02d", minor/100, minor%100); current == origBalance {
		t.Fatalf("wallet did not move after the original request (%s)", current)
	}

	t.Run("owner replay returns the original result", func(t *testing.T) {
		r := w.Submit(w.provA, key, op)
		if r.Status != http.StatusOK {
			t.Fatalf("replay: %d %s", r.Status, r.Raw)
		}
		got := r.JSON(t)
		if got["idempotentReplay"] != true || got["transactionId"] != orig["transactionId"] ||
			amountOf(t, got, "balance") != amountOf(t, orig, "balance") || got["walletVersion"] != orig["walletVersion"] {
			t.Errorf("replay %v differs from original %v", got, orig)
		}
		assertUnchanged(t, w, "owner replay", before, wallet, minor, version)
	})

	t.Run("other provider cannot replay the owner's request", func(t *testing.T) {
		r := w.Submit(w.provB, key, op) // same key, same body, other token
		if r.Status != http.StatusForbidden {
			t.Fatalf("status %d, want 403; body %s", r.Status, r.Raw)
		}
		mustNotContain(t, "foreign replay", r.Raw, orig["transactionId"].(string), ext, origBalance, "30.00", wallet)
		assertUnchanged(t, w, "foreign replay", before, wallet, minor, version)
	})

	t.Run("reusing the owner's key for its own request yields its own transaction", func(t *testing.T) {
		bOp := idptest.Operation("provider-b", w.next("b-ext"), wallet, player, "BET", "1.00")
		r := w.Submit(w.provB, key, bOp)
		if r.Status != http.StatusCreated {
			t.Fatalf("status %d body %s", r.Status, r.Raw)
		}
		got := r.JSON(t)
		if got["transactionId"] == orig["transactionId"] || got["providerId"] != "provider-b" || got["idempotentReplay"] != false {
			t.Errorf("provider-b key reuse crossed providers: %v", got)
		}
		again := get(w, w.provA, "/wagering/transactions/"+orig["transactionId"].(string)).JSON(t)
		if again["providerId"] != "provider-a" || amountOf(t, again, "amount") != "30.00" {
			t.Errorf("provider-a record altered: %v", again)
		}
	})

	t.Run("credentials are required to replay", func(t *testing.T) {
		before := w.Counts()
		minor, version := w.Balance(wallet)
		for name, tok := range map[string]string{"none": "", "garbage": "garbage", "internal": w.internal} {
			r := w.Submit(tok, key, op)
			if r.Status != http.StatusUnauthorized && r.Status != http.StatusForbidden {
				t.Errorf("%s: replay served with status %d", name, r.Status)
			}
			mustNotContain(t, name+" replay", r.Raw, orig["transactionId"].(string), origBalance)
		}
		assertUnchanged(t, w, "unauthenticated replay", before, wallet, minor, version)
	})

	t.Run("changed payload under the same key conflicts without effect", func(t *testing.T) {
		before := w.Counts()
		minor, version := w.Balance(wallet)
		changed := idptest.Operation("provider-a", ext, wallet, player, "BET", "31.00")
		if r := w.Submit(w.provA, key, changed); r.Status != http.StatusConflict {
			t.Errorf("status %d, want 409; body %s", r.Status, r.Raw)
		}
		assertUnchanged(t, w, "conflicting replay", before, wallet, minor, version)
	})

	t.Run("a second key cannot reapply the same external transaction", func(t *testing.T) {
		before := w.Counts()
		minor, version := w.Balance(wallet)
		if r := w.Submit(w.provA, w.next("second-key"), op); r.Status != http.StatusConflict {
			t.Errorf("status %d, want 409; body %s", r.Status, r.Raw)
		}
		assertUnchanged(t, w, "second key", before, wallet, minor, version)
	})
}

// TestIdempotencyKeyIsRequired: the key is never calculated on the caller's
// behalf (REQ-047).
func TestIdempotencyKeyIsRequired(t *testing.T) {
	w := newWorld(t)
	wallet := w.wallet(t, "100.00")
	player := w.player(t, wallet)
	minor, version := w.Balance(wallet)
	before := w.Counts()
	ext := w.next("nokey")
	op := idptest.Operation("provider-a", ext, wallet, player, "BET", "5.00")
	for name, key := range map[string]string{"absent": "", "blank": " "} {
		headers := map[string]string{}
		if name != "absent" {
			headers["Idempotency-Key"] = key
		}
		r := w.Do(idptest.Request{Method: http.MethodPost, Path: "/wagering/transactions", Token: w.provA, Headers: headers, Body: op})
		if r.Status != http.StatusBadRequest {
			t.Errorf("%s key: status %d, want 400; body %s", name, r.Status, r.Raw)
		}
	}
	assertUnchanged(t, w, "keyless submit", before, wallet, minor, version)
	if r := get(w, w.provA, "/providers/provider-a/wagering/transactions/"+ext); r.Status != http.StatusNotFound {
		t.Errorf("a keyless request left a record behind: %d %s", r.Status, r.Raw)
	}
}
