package pg

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"wagering/internal/domain"
	"wagering/migrations"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	admin := os.Getenv("WAGERING_TEST_ADMIN_URL")
	if admin == "" {
		t.Skip("set WAGERING_TEST_ADMIN_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}

	db := "wagering_pg_" + strings.ReplaceAll(domain.NewUUID().String(), "-", "")
	if _, err = conn.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, e := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
		if e != nil {
			t.Errorf("drop test DB: %v", e)
		}
		conn.Close(ctx)
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + db
	s, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	names, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var ups []string
	for _, n := range names {
		if strings.HasSuffix(n.Name(), ".up.sql") {
			ups = append(ups, n.Name())
		}
	}
	sort.Strings(ups)
	for _, name := range ups {
		sql, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("migration %s: %v", name, err)
		}
	}
	return s
}
func TestAtomicOpeningAndConflict(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	money, _ := domain.NewMoney(1500, "BRL")
	open, err := domain.OpenWallet(domain.OpenWalletInput{PlayerID: "player-1", InitialBalance: money, CorrelationID: "open-1", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	save := func(tx *Tx) error {
		if err := tx.InsertWallet(ctx, open.Wallet); err != nil {
			return err
		}
		if err := tx.InsertTransaction(ctx, open.Opening, time.Time{}, nil); err != nil {
			return err
		}
		if err := tx.InsertLedger(ctx, open.Ledger, 1); err != nil {
			return err
		}
		for _, event := range open.Events {
			if err := tx.InsertOutbox(ctx, event, now); err != nil {
				return err
			}
		}
		return nil
	}
	fail := errors.New("abort")
	if err := s.WithTx(ctx, func(tx *Tx) error {
		if err := save(tx); err != nil {
			return err
		}
		return fail
	}); !errors.Is(err, fail) {
		t.Fatalf("rollback error: %v", err)
	}
	if err := s.WithTx(ctx, func(tx *Tx) error {
		_, err := tx.GetWallet(ctx, open.Wallet.ID())
		if !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("wallet after rollback: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WithTx(ctx, save); err != nil {
		t.Fatal(err)
	}
	if err := s.WithTx(ctx, func(tx *Tx) error {
		w, err := tx.LockWallet(ctx, open.Wallet.ID())
		if err != nil {
			return err
		}
		if w.Balance().Minor() != 1500 || w.Version() != 1 {
			return fmt.Errorf("wallet mismatch: %+v", w.State())
		}
		stored, err := tx.GetTransaction(ctx, open.Opening.ID())
		if err != nil {
			return err
		}
		if stored.Transaction.ResultBalance().Minor() != 1500 {
			return fmt.Errorf("result mismatch")
		}
		entries, err := tx.ListLedger(ctx, w.ID())
		if err != nil {
			return err
		}
		if len(entries) != 1 {
			return fmt.Errorf("ledger count %d", len(entries))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	err = s.WithTx(ctx, save)
	var conflict *Conflict
	if !errors.As(err, &conflict) || conflict.Constraint != "wallets_pkey" {
		t.Fatalf("duplicate conflict: %v", err)
	}
}
