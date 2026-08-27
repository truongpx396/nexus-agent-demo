package store

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// bootstrapMigrationsTableSQL creates the tracking table if it does not
// already exist. It is the one migration not itself tracked in the table.
const bootstrapMigrationsTableSQL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
	version     text PRIMARY KEY,
	applied_at  timestamptz NOT NULL DEFAULT now()
);`

// Applied is one migration file that Migrate ran during this call.
type Applied struct {
	Version string // the filename, e.g. "0002_sessions.sql"
}

// Migrate applies every *.sql file in dir not yet recorded in
// schema_migrations, in filename order, each inside its own transaction.
// Migrations are additive-only (expand/contract, docs/constitution.md) —
// this runner has no concept of "down" on purpose: a rollback is a new,
// later-numbered migration, never a mutation of one already applied.
func Migrate(ctx context.Context, pool *pgxpool.Pool, dir fs.FS) ([]Applied, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, bootstrapMigrationsTableSQL); err != nil {
		return nil, fmt.Errorf("bootstrap schema_migrations: %w", err)
	}

	applied := map[string]bool{}
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("list applied migrations: %w", err)
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list applied migrations: %w", err)
	}

	entries, err := fs.ReadDir(dir, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		versions = append(versions, e.Name())
	}
	sort.Strings(versions)

	var result []Applied
	for _, version := range versions {
		if applied[version] {
			continue
		}
		sqlBytes, err := fs.ReadFile(dir, version)
		if err != nil {
			return result, fmt.Errorf("read migration %s: %w", version, err)
		}
		if err := applyOne(ctx, pool, version, string(sqlBytes)); err != nil {
			return result, fmt.Errorf("apply migration %s: %w", version, err)
		}
		result = append(result, Applied{Version: version})
	}
	return result, nil
}

func applyOne(ctx context.Context, pool *pgxpool.Pool, version, sqlText string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a documented no-op

	if _, err := tx.Exec(ctx, sqlText); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
		return fmt.Errorf("record migration: %w", err)
	}
	return tx.Commit(ctx)
}

// RLSTableCount reports how many tables in the current search_path have row
// level security enabled AND forced — the two settings together are what
// make tenant isolation hold even for the role that owns the tables
// (README.md §5: "RLS enabled on N/N tenant tables").
func RLSTableCount(ctx context.Context, pool *pgxpool.Pool) (enabled, total int, err error) {
	const q = `
		SELECT count(*) FILTER (WHERE relrowsecurity AND relforcerowsecurity),
		       count(*)
		FROM pg_class
		WHERE relkind = 'r'
		  AND relnamespace = 'public'::regnamespace
		  AND relname NOT IN ('schema_migrations')`
	row := pool.QueryRow(ctx, q)
	if err := row.Scan(&enabled, &total); err != nil {
		return 0, 0, fmt.Errorf("count RLS tables: %w", err)
	}
	return enabled, total, nil
}
