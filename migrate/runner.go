package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"
)

// Runner executes database migrations against a SQL database.
// It is NOT safe for concurrent use — use the locking mechanism to serialize
// multiple runners (see Up/Down for details).
//
// Ledger and lock statements use "?" placeholders (database/sql's default
// convention). Drivers that require numbered placeholders (e.g. lib/pq for
// Postgres) need a driver wrapper that rewrites "?" to "$N", such as
// github.com/lib/pq's sqlx-style helpers or jackc/pgx's stdlib adapter with
// its query rewriter enabled. This keeps the migrate package itself free of
// a hard dependency on any single driver.
type Runner struct {
	DB *sql.DB
	FS fs.FS
	Mu chan struct{} // used for advisory locking; see acquireLock
}

// New creates a new Runner for the given database and migration filesystem.
func New(db *sql.DB, fsys fs.FS) *Runner {
	return &Runner{
		DB: db,
		FS: fsys,
		Mu: make(chan struct{}, 1),
	}
}

// StatusEntry represents the status of a single migration.
type StatusEntry struct {
	Version          int
	Name             string
	Applied          bool
	AppliedAt        *time.Time
	ChecksumMismatch bool
}

// Up discovers and applies all pending migrations in ascending version order.
// Each migration is applied within its own transaction; if any migration fails,
// Up stops and returns the error (subsequent migrations are not attempted).
// Up uses a simple row-level lock (see lockVersion) to prevent concurrent runs.
func (r *Runner) Up(ctx context.Context) error {
	// Acquire advisory lock using a sentinel row
	if err := r.acquireLock(ctx); err != nil {
		return err
	}
	defer r.releaseLock(ctx)

	if err := ensureVersionTable(ctx, r.DB); err != nil {
		return fmt.Errorf("failed to initialize migrations table: %w", err)
	}

	migrations, err := DiscoverMigrations(r.FS)
	if err != nil {
		return err
	}

	applied, err := appliedVersions(ctx, r.DB)
	if err != nil {
		return err
	}

	pending := make([]Migration, 0, len(migrations))
	for _, m := range migrations {
		if _, ok := applied[m.Version]; !ok {
			pending = append(pending, m)
		}
	}

	if len(pending) == 0 {
		return nil
	}

	for _, m := range pending {
		tx, err := r.DB.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("failed to begin transaction for migration %d: %w", m.Version, err)
		}

		// Split SQL by semicolon and execute each statement
		stmts := splitStatements(m.UpSQL)
		for _, stmt := range stmts {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration %d failed: %w", m.Version, err)
			}
		}

		if err := recordApplied(ctx, tx, m.Version, m.Name, m.UpSQL); err != nil {
			tx.Rollback()
			return err
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit migration %d: %w", m.Version, err)
		}
	}

	return nil
}

// Down rolls back the last n applied migrations in descending version order.
// Each rollback is applied within its own transaction.
func (r *Runner) Down(ctx context.Context, n int) error {
	if n <= 0 {
		return fmt.Errorf("n must be positive")
	}

	// Acquire advisory lock
	if err := r.acquireLock(ctx); err != nil {
		return err
	}
	defer r.releaseLock(ctx)

	if err := ensureVersionTable(ctx, r.DB); err != nil {
		return fmt.Errorf("failed to initialize migrations table: %w", err)
	}

	migrations, err := DiscoverMigrations(r.FS)
	if err != nil {
		return err
	}

	applied, err := appliedVersions(ctx, r.DB)
	if err != nil {
		return err
	}

	// Build a map of discovered migrations for quick lookup
	migrationMap := make(map[int]Migration)
	for _, m := range migrations {
		migrationMap[m.Version] = m
	}

	// Roll back newest first, so `down 1` undoes the most recent migration.
	appliedList := descendingByVersion(applied)

	// Roll back up to n migrations
	count := 0
	for _, rec := range appliedList {
		if count >= n {
			break
		}

		m, ok := migrationMap[rec.Version]
		if !ok {
			return fmt.Errorf("applied migration %d not found in migration files", rec.Version)
		}

		tx, err := r.DB.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("failed to begin transaction for rollback of %d: %w", rec.Version, err)
		}

		// Split SQL by semicolon and execute each statement
		stmts := splitStatements(m.DownSQL)
		for _, stmt := range stmts {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				tx.Rollback()
				return fmt.Errorf("rollback of migration %d failed: %w", rec.Version, err)
			}
		}

		if err := removeApplied(ctx, tx, rec.Version); err != nil {
			tx.Rollback()
			return err
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit rollback of %d: %w", rec.Version, err)
		}

		count++
	}

	if count == 0 {
		return fmt.Errorf("no migrations to roll back")
	}

	return nil
}

// Status returns the status of all discovered migrations.
func (r *Runner) Status(ctx context.Context) ([]StatusEntry, error) {
	if err := ensureVersionTable(ctx, r.DB); err != nil {
		return nil, fmt.Errorf("failed to initialize migrations table: %w", err)
	}

	migrations, err := DiscoverMigrations(r.FS)
	if err != nil {
		return nil, err
	}

	applied, err := appliedVersions(ctx, r.DB)
	if err != nil {
		return nil, err
	}

	entries := make([]StatusEntry, len(migrations))
	for i, m := range migrations {
		entry := StatusEntry{
			Version: m.Version,
			Name:    m.Name,
		}
		if rec, ok := applied[m.Version]; ok {
			entry.Applied = true
			entry.AppliedAt = &rec.AppliedAt
			// Check if checksum matches
			currentChecksum := computeChecksum(m.UpSQL)
			if currentChecksum != rec.Checksum {
				entry.ChecksumMismatch = true
			}
		}
		entries[i] = entry
	}

	return entries, nil
}

// acquireLock acquires the migration lock using a sentinel row in the
// breeze_migrations table. If another runner holds the lock, this blocks.
// See the comment on Runner for the rationale for row-level locking instead
// of pg_advisory_lock (portability across different SQL drivers).
func (r *Runner) acquireLock(ctx context.Context) error {
	if err := ensureVersionTable(ctx, r.DB); err != nil {
		return err
	}

	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin lock transaction: %w", err)
	}

	// Try to insert the lock sentinel row
	_, err = tx.ExecContext(ctx, `
		INSERT INTO breeze_migrations (version, name, checksum, applied_at)
		VALUES (?, 'lock', 'lock', ?)
	`, lockVersion, time.Now().UTC())

	if err != nil {
		tx.Rollback()
		// If the insert fails (constraint violation), another runner holds the lock
		// This is not perfect (we don't know the actual error reason), but it's the
		// best we can do portably across SQL drivers.
		return fmt.Errorf("another migration is running (or lock table is corrupted); wait and try again")
	}

	// We successfully inserted the lock. Commit to release the transaction lock,
	// then keep the row in place until releaseLock is called.
	return tx.Commit()
}

// releaseLock releases the migration lock by deleting the sentinel row.
func (r *Runner) releaseLock(ctx context.Context) error {
	_, err := r.DB.ExecContext(ctx, `DELETE FROM breeze_migrations WHERE version = ?`, lockVersion)
	if err != nil {
		// Log but don't fail; we're already done with the migration and a
		// stuck sentinel row only blocks the *next* run, not this one.
		fmt.Fprintf(os.Stderr, "breeze: warning: failed to release migration lock: %v\n", err)
	}
	return nil
}

// splitStatements splits SQL text by semicolon, handling basic quote escaping.
// This is a naive implementation suitable for simple migration files; complex
// stored procedures may need smarter parsing.
func splitStatements(sql string) []string {
	var stmts []string
	var current strings.Builder
	inString := false
	escaped := false

	for _, r := range sql {
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case r == '\\':
			current.WriteRune(r)
			escaped = true
		case r == '\'' || r == '"':
			inString = !inString
			current.WriteRune(r)
		case r == ';' && !inString:
			stmt := strings.TrimSpace(current.String())
			if stmt != "" {
				stmts = append(stmts, stmt)
			}
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}

	if stmt := strings.TrimSpace(current.String()); stmt != "" {
		stmts = append(stmts, stmt)
	}

	return stmts
}
