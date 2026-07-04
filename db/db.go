// Package db owns database initialization: it opens the SQLite file, applies
// the schema (idempotent auto-migration) and seeds demo data into an empty DB.
package db

import (
	"database/sql"
	"embed"
	"log"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql seed.sql
var files embed.FS

// Open opens (creating parent dirs as needed), applies the schema, and seeds if empty.
func Open(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		os.MkdirAll(dir, 0o755)
	}
	conn, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	if err := exec(conn, "schema.sql"); err != nil {
		return nil, err
	}
	if err := migrate(conn); err != nil {
		return nil, err
	}
	var n int
	if err := conn.QueryRow("SELECT COUNT(*) FROM invoices").Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		if err := exec(conn, "seed.sql"); err != nil {
			return nil, err
		}
		log.Print("seeded empty database with demo data")
	}
	return conn, nil
}

// migrate applies additive column changes to databases created before a column
// existed (CREATE IF NOT EXISTS in schema.sql never adds columns to existing tables).
// Each statement is idempotent: a "duplicate column name" error means it already ran.
func migrate(conn *sql.DB) error {
	stmts := []string{
		`ALTER TABLE company ADD COLUMN place TEXT NOT NULL DEFAULT ''`,
		// backfill place for the seeded demo company only (matched by its OIB), so
		// pre-existing dev databases show a place without clobbering a real company.
		`UPDATE company SET place = 'Zagreb' WHERE oib = '11111111111' AND place = ''`,
		`ALTER TABLE invoices ADD COLUMN deleted_at TEXT`,
		`ALTER TABLE company ADD COLUMN bank TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE company ADD COLUMN owner_address TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE invoice_items ADD COLUMN unit TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE invoice_items ADD COLUMN discount_pct REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE company ADD COLUMN vat_exempt INTEGER NOT NULL DEFAULT 1`,
	}
	for _, s := range stmts {
		if _, err := conn.Exec(s); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	return nil
}

func exec(conn *sql.DB, name string) error {
	b, err := files.ReadFile(name)
	if err != nil {
		return err
	}
	_, err = conn.Exec(string(b))
	return err
}
