// Package testdb gives tests a real, freshly migrated Postgres database.
// It never mocks: locking and transactions can only be tested for real.
package testdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Tests use their own database (money_test), never the dev one, because
// every test truncates all tables.
const defaultURL = "postgres://money:money@localhost:5432/money_test"

// Setup creates the test database if needed, rebuilds the schema from
// migrations/ (so tests always run against the current SQL, unlike the
// docker init scripts which only run once), and returns a connection pool.
// Call it once, from TestMain.
func Setup(ctx context.Context) (*pgxpool.Pool, error) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = defaultURL
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse TEST_DATABASE_URL: %w", err)
	}
	// migrate() drops the whole schema. Refuse to run against anything that
	// isn't obviously a test database, so a mistyped URL can't wipe dev data.
	if !strings.HasSuffix(cfg.ConnConfig.Database, "_test") {
		return nil, fmt.Errorf("refusing to use database %q: test database names must end in _test", cfg.ConnConfig.Database)
	}
	// The default pool size is max(4, number of CPUs). If a concurrency test
	// starts 20 goroutines but only 4 can get a connection, the rest queue in
	// Go instead of competing inside Postgres, and the test proves much less
	// than it appears to. Give every goroutine its own connection.
	cfg.MaxConns = 50
	if err := createDatabase(ctx, cfg.ConnConfig); err != nil {
		return nil, err
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// Reset empties every table and restarts ID sequences. Call it at the start
// of each test so no test depends on another's leftovers.
func Reset(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`TRUNCATE ledger_entries, transfers, accounts RESTART IDENTITY`)
	if err != nil {
		t.Fatalf("reset test db: %v", err)
	}
}

func createDatabase(ctx context.Context, target *pgx.ConnConfig) error {
	// CREATE DATABASE has to run while connected to some other database.
	admin := target.Copy()
	admin.Database = "postgres"
	conn, err := pgx.ConnectConfig(ctx, admin)
	if err != nil {
		return fmt.Errorf("connect to postgres (is docker compose up?): %w", err)
	}
	defer conn.Close(ctx)

	var exists bool
	err = conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, target.Database,
	).Scan(&exists)
	if err != nil || exists {
		return err
	}
	_, err = conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{target.Database}.Sanitize())
	return err
}

func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		return fmt.Errorf("reset schema: %w", err)
	}

	// Locate migrations/ relative to this source file, so it works no matter
	// which package's tests are running.
	_, thisFile, _, _ := runtime.Caller(0)
	files, err := filepath.Glob(filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations", "*.sql"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no migrations found")
	}
	sort.Strings(files)
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("apply %s: %w", filepath.Base(f), err)
		}
	}
	return nil
}
