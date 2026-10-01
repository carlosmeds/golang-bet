package httpauth_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"wagering/tests/integration/auth/idptest"
)

type page struct {
	ids  []string
	next string
}

func ledgerPage(t *testing.T, w *world, wallet, cursor string, limit int) (idptest.Response, page) {
	t.Helper()
	path := fmt.Sprintf("/wallets/%s/ledger?limit=%d", wallet, limit)
	if cursor != "" {
		path += "&cursor=" + cursor
	}
	r := get(w, w.internal, path)
	if r.Status != http.StatusOK {
		return r, page{}
	}
	body := r.JSON(t)
	var p page
	for _, e := range body["entries"].([]any) {
		p.ids = append(p.ids, e.(map[string]any)["id"].(string))
	}
	if n, ok := body["nextCursor"].(string); ok {
		p.next = n
	}
	return r, p
}

func walk(t *testing.T, w *world, wallet string, limit int) []string {
	t.Helper()
	var all []string
	cursor := ""
	for i := 0; i < 50; i++ {
		r, p := ledgerPage(t, w, wallet, cursor, limit)
		if r.Status != http.StatusOK {
			t.Fatalf("ledger page: %d %s", r.Status, r.Raw)
		}
		all = append(all, p.ids...)
		if p.next == "" {
			return all
		}
		cursor = p.next
	}
	t.Fatal("pagination did not terminate")
	return nil
}

// TestLedgerPaginationIsOpaqueAndStable (REQ-053).
func TestLedgerPaginationIsOpaqueAndStable(t *testing.T) {
	w := newWorld(t)
	wallet := w.wallet(t, "100.00") // OPENING entry + 5 bets = 6 entries
	player := w.player(t, wallet)
	for i := 0; i < 5; i++ {
		w.bet(t, w.provA, "provider-a", wallet, player, w.next("page-ext"), w.next("page-key"), "1.00")
	}
	full := walk(t, w, wallet, 200)
	if len(full) != 6 {
		t.Fatalf("ledger has %d entries, want 6", len(full))
	}

	t.Run("pages concatenate to the unpaged order without gaps or repeats", func(t *testing.T) {
		for _, limit := range []int{1, 2, 4} {
			if got := walk(t, w, wallet, limit); strings.Join(got, ",") != strings.Join(full, ",") {
				t.Errorf("limit %d: %v\nwant %v", limit, got, full)
			}
		}
	})

	t.Run("cursor is opaque text, not an offset or identifier", func(t *testing.T) {
		_, p := ledgerPage(t, w, wallet, "", 2)
		if p.next == "" {
			t.Fatal("no cursor on a partial page")
		}
		for _, bad := range []string{wallet, "2", "1"} {
			if p.next == bad || strings.Contains(p.next, wallet) {
				t.Errorf("cursor %q exposes %q", p.next, bad)
			}
		}
	})

	t.Run("order stays stable while new entries arrive", func(t *testing.T) {
		_, first := ledgerPage(t, w, wallet, "", 3)
		w.bet(t, w.provA, "provider-a", wallet, player, w.next("late-ext"), w.next("late-key"), "1.00")
		_, second := ledgerPage(t, w, wallet, first.next, 3)
		_, third := ledgerPage(t, w, wallet, second.next, 3)
		got := append(append(append([]string{}, first.ids...), second.ids...), third.ids...)
		if len(got) != 7 || strings.Join(got[:6], ",") != strings.Join(full, ",") {
			t.Errorf("walk during writes = %v, want the original 6 then the new one", got)
		}
	})

	t.Run("bad cursors and limits are rejected", func(t *testing.T) {
		other := w.wallet(t, "10.00")
		_, mine := ledgerPage(t, w, wallet, "", 1)
		for name, path := range map[string]string{
			"garbage cursor":           "/wallets/" + wallet + "/ledger?cursor=!!!",
			"cursor of another wallet": "/wallets/" + other + "/ledger?cursor=" + mine.next,
			"zero limit":               "/wallets/" + wallet + "/ledger?limit=0",
			"huge limit":               "/wallets/" + wallet + "/ledger?limit=100000",
			"non numeric limit":        "/wallets/" + wallet + "/ledger?limit=x",
		} {
			if r := get(w, w.internal, path); r.Status != http.StatusBadRequest {
				t.Errorf("%s: %d %s", name, r.Status, r.Raw)
			}
		}
	})

	t.Run("ledger entries describe each movement", func(t *testing.T) {
		r := get(w, w.internal, "/wallets/"+wallet+"/ledger?limit=200")
		entries := r.JSON(t)["entries"].([]any)
		prev := ""
		for i, e := range entries {
			m := e.(map[string]any)
			if i == 0 && (m["direction"] != "CREDIT" || amountOf(t, m, "balanceBefore") != "0.00") {
				t.Errorf("first entry is not the opening credit: %v", m)
			}
			if i > 0 && amountOf(t, m, "balanceBefore") != prev {
				t.Errorf("entry %d balanceBefore %s does not continue %s", i, amountOf(t, m, "balanceBefore"), prev)
			}
			prev = amountOf(t, m, "balanceAfter")
		}
	})
}

// TestTransactionReadsExposeStateAndFailureCodes (REQ-054).
func TestTransactionReadsExposeStateAndFailureCodes(t *testing.T) {
	w := newWorld(t)
	wallet := w.wallet(t, "10.00")
	player := w.player(t, wallet)

	readBoth := func(id, ext string) []map[string]any {
		var out []map[string]any
		for _, path := range []string{"/wagering/transactions/" + id, "/providers/provider-a/wagering/transactions/" + ext} {
			r := get(w, w.provA, path)
			if r.Status != http.StatusOK {
				t.Fatalf("%s: %d %s", path, r.Status, r.Raw)
			}
			out = append(out, r.JSON(t))
		}
		return out
	}

	t.Run("pending reference", func(t *testing.T) {
		ext := w.next("pending")
		op := idptest.Operation("provider-a", ext, wallet, player, "REFUND", "1.00")
		op["referenceExternalTransactionId"] = w.next("not-yet-there")
		r := w.Submit(w.provA, w.next("pending-key"), op)
		if r.Status != http.StatusAccepted || r.JSON(t)["status"] != "PENDING_REFERENCE" {
			t.Fatalf("submit: %d %s", r.Status, r.Raw)
		}
		for _, body := range readBoth(r.JSON(t)["transactionId"].(string), ext) {
			if body["status"] != "PENDING_REFERENCE" || body["referenceExternalTransactionId"] != op["referenceExternalTransactionId"] {
				t.Errorf("pending read = %v", body)
			}
			if _, done := body["completedAt"]; done {
				t.Errorf("pending transaction claims completion: %v", body)
			}
		}
	})

	t.Run("business rejection exposes its failure code", func(t *testing.T) {
		ext := w.next("broke")
		r := w.Submit(w.provA, w.next("broke-key"), idptest.Operation("provider-a", ext, wallet, player, "BET", "999.00"))
		if r.Status != http.StatusUnprocessableEntity {
			t.Fatalf("submit: %d %s", r.Status, r.Raw)
		}
		for _, body := range readBoth(r.JSON(t)["transactionId"].(string), ext) {
			if body["status"] != "REJECTED" || body["failureCode"] != "INSUFFICIENT_FUNDS_BET" {
				t.Errorf("rejected read = %v", body)
			}
		}
		if m, v := w.Balance(wallet); m != 1000 || v != 1 {
			t.Errorf("rejection moved the wallet: %d v%d", m, v)
		}
	})

	t.Run("processed", func(t *testing.T) {
		ext := w.next("ok")
		r := w.bet(t, w.provA, "provider-a", wallet, player, ext, w.next("ok-key"), "2.00")
		for _, body := range readBoth(r.JSON(t)["transactionId"].(string), ext) {
			if body["status"] != "PROCESSED" || amountOf(t, body, "balance") != "8.00" || body["walletVersion"] == nil {
				t.Errorf("processed read = %v", body)
			}
			if _, bad := body["failureCode"]; bad {
				t.Errorf("processed transaction has a failure code: %v", body)
			}
		}
	})

	t.Run("malformed identifiers", func(t *testing.T) {
		if r := get(w, w.provA, "/wagering/transactions/not-a-uuid"); r.Status != http.StatusBadRequest {
			t.Errorf("bad id: %d %s", r.Status, r.Raw)
		}
	})
}

// TestWalletEndpointsAndReconciliation covers the internal-only routes of
// REQ-052 with an authorized caller.
func TestWalletEndpointsAndReconciliation(t *testing.T) {
	w := newWorld(t)
	wallet := w.wallet(t, "50.00")
	player := w.player(t, wallet)
	w.bet(t, w.provA, "provider-a", wallet, player, w.next("rec-ext"), w.next("rec-key"), "20.00")

	got := get(w, w.internal, "/wallets/"+wallet).JSON(t)
	if amountOf(t, got, "balance") != "30.00" || got["version"] != float64(2) || got["playerId"] != player {
		t.Errorf("wallet = %v", got)
	}
	minor, version := w.Balance(wallet)
	before := w.Counts()
	r := w.Do(idptest.Request{Method: http.MethodPost, Path: "/wallets/" + wallet + "/reconciliation", Token: w.internal})
	if r.Status != http.StatusOK {
		t.Fatalf("reconcile: %d %s", r.Status, r.Raw)
	}
	rec := r.JSON(t)
	if rec["consistent"] != true || amountOf(t, rec, "storedBalance") != "30.00" || amountOf(t, rec, "calculatedBalance") != "30.00" ||
		amountOf(t, rec, "difference") != "0.00" || rec["checkedEntries"] != float64(2) {
		t.Errorf("reconciliation = %v", rec)
	}
	assertUnchanged(t, w, "reconciliation", before, wallet, minor, version)

	if r := get(w, w.internal, "/wallets/"+w.next("x")); r.Status != http.StatusBadRequest {
		t.Errorf("malformed wallet id: %d", r.Status)
	}
	dup := w.Do(idptest.Request{Method: http.MethodPost, Path: "/wallets", Token: w.internal,
		Body: map[string]any{"playerId": player, "initialBalance": map[string]string{"amount": "1.00", "currency": "BRL"}}})
	if dup.Status != http.StatusConflict {
		t.Errorf("reopening the same player/currency: %d %s", dup.Status, dup.Raw)
	}
}
