package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// An older database created before the `place` column existed must gain it on Open,
// and the seeded demo company must be backfilled — not crash with "no such column".
func TestMigrateAddsPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// company table WITHOUT the place column, plus one invoice so seed is skipped.
	_, err = old.Exec(`
		CREATE TABLE company (id INTEGER PRIMARY KEY, name TEXT, owner TEXT, oib TEXT, address TEXT, iban TEXT DEFAULT '');
		INSERT INTO company VALUES (1, 'Sitna riba', 'Jura', '11111111111', 'adr', 'iban');
		CREATE TABLE invoices (id INTEGER PRIMARY KEY, number TEXT, issue_date TEXT, customer_id INTEGER, note TEXT, created_at TEXT);
		INSERT INTO invoices VALUES (1, '1/1/2025', '2025-01-01', 1, '', '');`)
	if err != nil {
		t.Fatal(err)
	}
	old.Close()

	conn, err := Open(path)
	if err != nil {
		t.Fatalf("Open should migrate the old DB, got: %v", err)
	}
	defer conn.Close()

	var place string
	if err := conn.QueryRow(`SELECT place FROM company WHERE id = 1`).Scan(&place); err != nil {
		t.Fatalf("place column missing after migrate: %v", err)
	}
	if place != "Zagreb" {
		t.Fatalf("demo company place = %q, want backfilled \"Zagreb\"", place)
	}
}
