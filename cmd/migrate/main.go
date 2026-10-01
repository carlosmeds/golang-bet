package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"wagering/internal/storage/pg"
)

func main() {
	direction := flag.String("direction", "up", "migration direction: up or down")
	count := flag.Int("count", 0, "number of down migrations; 0 reverts all")
	flag.Parse()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fatal(err)
	}
	defer pool.Close()
	switch *direction {
	case "up":
		err = pg.ApplyMigrations(ctx, pool)
	case "down":
		err = pg.RevertMigrations(ctx, pool, *count)
	default:
		fatal(fmt.Errorf("invalid direction %q", *direction))
	}
	if err != nil {
		fatal(err)
	}
}
func fatal(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
