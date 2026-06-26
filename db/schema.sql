-- Schema runs on every boot; CREATE IF NOT EXISTS makes it an idempotent auto-migration.
-- Money is stored in integer cents (EUR) to avoid float rounding. ponytail: no VAT columns
-- yet, most paušalisti are not in the PDV sustav; add a vat_rate column when one is.

CREATE TABLE IF NOT EXISTS company (
  id         INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  owner      TEXT NOT NULL,
  oib        TEXT NOT NULL,
  address    TEXT NOT NULL,
  place      TEXT NOT NULL DEFAULT '',   -- mjesto izdavanja (business seat city)
  iban       TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS customers (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  name    TEXT NOT NULL,
  oib     TEXT NOT NULL DEFAULT '',
  address TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS invoices (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  number      TEXT NOT NULL UNIQUE,
  issue_date  TEXT NOT NULL,            -- ISO YYYY-MM-DD
  customer_id INTEGER NOT NULL REFERENCES customers(id),
  note        TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS invoice_items (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  invoice_id       INTEGER NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
  description      TEXT NOT NULL,
  quantity         REAL NOT NULL DEFAULT 1,
  unit_price_cents INTEGER NOT NULL,
  line_total_cents INTEGER NOT NULL
);
