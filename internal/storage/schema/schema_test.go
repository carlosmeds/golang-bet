package schema_test

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"

	"wagering/internal/storage/schema"
	"wagering/migrations"
)

var fileName = regexp.MustCompile(`^(\d{6})_([a-z0-9_]+)\.(up|down)\.sql$`)

// Every migration has a reversal, numbering is gap-free, and files stay portable
// across migration runners (no psql meta-commands, no non-transactional DDL).
func TestMigrationFilesArePairedAndPortable(t *testing.T) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	ups := map[string]string{}
	downs := map[string]string{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		m := fileName.FindStringSubmatch(e.Name())
		if m == nil {
			t.Fatalf("unexpected file name %q", e.Name())
		}
		body, err := fs.ReadFile(migrations.FS, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		if strings.TrimSpace(text) == "" {
			t.Errorf("%s is empty", e.Name())
		}
		if regexp.MustCompile(`(?m)^\s*\\`).MatchString(text) {
			t.Errorf("%s contains a psql meta-command", e.Name())
		}
		if regexp.MustCompile(`(?i)CONCURRENTLY|\bBEGIN;|\bCOMMIT;`).MatchString(text) {
			t.Errorf("%s contains transaction control or CONCURRENTLY", e.Name())
		}
		key := m[1] + "_" + m[2]
		if m[3] == "up" {
			ups[key] = e.Name()
		} else {
			downs[key] = e.Name()
		}
	}
	if len(ups) == 0 {
		t.Fatal("no migrations found")
	}
	var keys []string
	for k := range ups {
		keys = append(keys, k)
		if _, ok := downs[k]; !ok {
			t.Errorf("%s has no down migration", ups[k])
		}
	}
	for k := range downs {
		if _, ok := ups[k]; !ok {
			t.Errorf("%s has no up migration", downs[k])
		}
	}
	sort.Strings(keys)
	for i, k := range keys {
		want := regexp.MustCompile(`^\d{6}`).FindString(k)
		if got := fmt.Sprintf("%06d", i+1); got != want {
			t.Errorf("migration %s: expected version %s (versions must be consecutive from 000001)", k, got)
		}
	}
}

// The exported names must exist in the SQL so they cannot drift from it.
func TestExportedNamesExistInMigrations(t *testing.T) {
	var all strings.Builder
	entries, _ := fs.ReadDir(migrations.FS, ".")
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			b, _ := fs.ReadFile(migrations.FS, e.Name())
			all.Write(b)
		}
	}
	sql := all.String()
	names := []string{
		schema.TableWallets, schema.TableWagerTransactions, schema.TableLedgerEntries,
		schema.TableInboxMessages, schema.TableOutboxEvents,
		schema.UniqueWalletPlayerCurrency, schema.UniqueTransactionIdempotencyKey,
		schema.UniqueTransactionExternalID, schema.UniqueTransactionOpening,
		schema.UniqueTransactionInternalKey, schema.UniqueProcessedReversal,
		schema.UniqueLedgerWalletTransaction, schema.UniqueLedgerWalletVersion,
		schema.UniqueInboxMessage, schema.UniqueOutboxEvent,
		schema.CheckWalletBalanceNonNegative, schema.CheckWalletVersionStep,
		schema.CheckWalletLedgerMatch, schema.CheckLedgerEquation, schema.CheckLedgerChain,
		schema.CheckLedgerTransactionMatch, schema.CheckLedgerWalletMatch,
		schema.CheckTransactionLedgerMatch, schema.CheckTransactionReferenceMatch,
		schema.RestrictLedgerAppendOnly, schema.RestrictTransactionTerminal,
	}
	for _, n := range names {
		if !strings.Contains(sql, n) {
			t.Errorf("%q is not defined by any migration", n)
		}
	}
}

// Runs the PostgreSQL behaviour suite (migration reversal, constraints, triggers,
// concurrency) when WAGERING_TEST_ADMIN_URL points at a server; needs psql.
func TestPostgresSchemaSuite(t *testing.T) {
	url := os.Getenv("WAGERING_TEST_ADMIN_URL")
	if url == "" {
		t.Skip("WAGERING_TEST_ADMIN_URL not set")
	}
	if _, err := exec.LookPath("psql"); err != nil {
		t.Skip("psql not installed")
	}
	cmd := exec.Command("./run_tests.sh")
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("schema suite failed: %v", err)
	}
}
