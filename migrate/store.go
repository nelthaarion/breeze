package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"time"
)

// appliedRecord tracks a migration that has been applied.
type appliedRecord struct {
	Version   int
	Name      string
	Checksum  string
	AppliedAt time.Time
}

// lockVersion is the version number of the sentinel row that serves as the
// migration lock. It shares the ledger table with real migrations because the
// primary key is what makes the lock mutually exclusive, and it is negative so
// it can never collide with a discovered migration — those are parsed from
// filenames as unsigned digits.
//
// Every read of the ledger has to exclude it. See appliedVersions.
const lockVersion = -1

// ensureVersionTable creates the breeze_migrations table if it does not exist.
// Uses ANSI SQL that works on both Postgres and SQLite.
func ensureVersionTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS breeze_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			checksum TEXT NOT NULL,
			applied_at TIMESTAMP NOT NULL
		)
	`)
	return err
}

// appliedVersions reads all applied migrations from the database and returns
// them as a map keyed by version.
//
// The lock sentinel is excluded. It lives in the same table, and Down acquires
// the lock before reading the ledger — so without this filter Down saw a
// migration numbered -1, found no file for it, and failed with "applied
// migration -1 not found in migration files" every single time it ran.
func appliedVersions(ctx context.Context, db *sql.DB) (map[int]appliedRecord, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT version, name, checksum, applied_at FROM breeze_migrations
		WHERE version >= 0 ORDER BY version
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[int]appliedRecord)
	for rows.Next() {
		var rec appliedRecord
		if err := rows.Scan(&rec.Version, &rec.Name, &rec.Checksum, &rec.AppliedAt); err != nil {
			return nil, fmt.Errorf("failed to scan migration record: %w", err)
		}
		result[rec.Version] = rec
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating migration records: %w", err)
	}
	return result, nil
}

// descendingByVersion flattens the applied-migrations map into newest-first
// order, which is the order Down must roll back in: `down 1` undoes the most
// recent migration.
//
// A named function rather than a loop inside Down because the ordering is the
// entire correctness claim, and it is worth being able to assert directly. The
// version this replaced was a hand-rolled bubble sort whose comparison was
// inverted, so it sorted *ascending* under a comment saying descending — making
// `down 1` roll back the oldest migration in the project. Nothing failed: rolling
// back 0001 commits just as quietly as rolling back 0003.
func descendingByVersion(applied map[int]appliedRecord) []appliedRecord {
	out := make([]appliedRecord, 0, len(applied))
	for _, rec := range applied {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	return out
}

// recordApplied inserts a migration record into the database within the given transaction.
func recordApplied(ctx context.Context, tx *sql.Tx, version int, name, sqlContent string) error {
	checksum := computeChecksum(sqlContent)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO breeze_migrations (version, name, checksum, applied_at)
		VALUES (?, ?, ?, ?)
	`, version, name, checksum, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("failed to record migration %d: %w", version, err)
	}
	return nil
}

// removeApplied deletes a migration record from the database within the given transaction.
func removeApplied(ctx context.Context, tx *sql.Tx, version int) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM breeze_migrations WHERE version = ?`, version)
	if err != nil {
		return fmt.Errorf("failed to remove migration %d: %w", version, err)
	}
	return nil
}

// computeChecksum returns the SHA-256 hex digest of the given SQL content.
func computeChecksum(sqlContent string) string {
	h := sha256.Sum256([]byte(sqlContent))
	return hex.EncodeToString(h[:])
}
