package pg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"wagering/migrations"
)

// ApplyMigrations serializes all service instances with a PostgreSQL advisory
// lock. Each version and its record commit together; an existing version with
// changed contents is refused rather than silently reinterpreted.
func ApplyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	const lockID int64 = 0x7761676572696e67 // "wagering"
	if _, err = conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockID); err != nil {
		return err
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, lockID)
	}()
	if _, err = conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(name text PRIMARY KEY, sha256 text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	files, err := migrations.FS.ReadDir(".")
	if err != nil {
		return err
	}
	var names []string
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".up.sql") {
			names = append(names, f.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		sql, err := migrations.FS.ReadFile(name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(sql)
		hash := hex.EncodeToString(sum[:])
		var prior string
		err = conn.QueryRow(ctx, `SELECT sha256 FROM schema_migrations WHERE name=$1`, name).Scan(&prior)
		if err == nil {
			if prior != hash {
				return fmt.Errorf("migration %s content differs from applied version", name)
			}
			continue
		}
		if classify(err) != ErrNotFound {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(name,sha256) VALUES($1,$2)`, name, hash); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}
