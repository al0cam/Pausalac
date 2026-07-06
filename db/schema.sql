-- Schema runs on every boot; CREATE IF NOT EXISTS makes it an idempotent auto-migration.
-- Money is stored in integer cents (EUR) to avoid float rounding. ponytail: no VAT columns
-- yet, most paušalisti are not in the PDV sustav; add a vat_rate column when one is.

CREATE TABLE IF NOT EXISTS company (
  id            INTEGER PRIMARY KEY,
  name          TEXT NOT NULL,
  owner         TEXT NOT NULL,
  oib           TEXT NOT NULL,
  address       TEXT NOT NULL,                 -- adresa obavljanja djelatnosti (business)
  place         TEXT NOT NULL DEFAULT '',      -- mjesto izdavanja (business seat city)
  iban          TEXT NOT NULL DEFAULT '',
  bank          TEXT NOT NULL DEFAULT '',      -- naziv banke
  swift         TEXT NOT NULL DEFAULT '',      -- SWIFT/BIC banke
  owner_address TEXT NOT NULL DEFAULT '',      -- adresa vlasnika (may differ from business)
  vat_exempt    INTEGER NOT NULL DEFAULT 1     -- 1 = not in PDV sustav: PDV 0 + exemption note
);

CREATE TABLE IF NOT EXISTS customers (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  name    TEXT NOT NULL,
  oib     TEXT NOT NULL DEFAULT '',
  address TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS invoices (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  number         TEXT NOT NULL UNIQUE,
  issue_date     TEXT NOT NULL,            -- ISO YYYY-MM-DD
  customer_id    INTEGER NOT NULL REFERENCES customers(id),
  note           TEXT NOT NULL DEFAULT '',
  issue_time     TEXT NOT NULL DEFAULT '', -- vrijeme izrade (HH:MM)
  delivery_date  TEXT NOT NULL DEFAULT '', -- datum isporuke (ISO)
  due_date       TEXT NOT NULL DEFAULT '', -- dospijeće plaćanja (ISO)
  payment_method TEXT NOT NULL DEFAULT '', -- način plaćanja
  poziv          TEXT NOT NULL DEFAULT '', -- poziv na broj (payment reference)
  paid_date       TEXT NOT NULL DEFAULT '',    -- nadnevak naplate (ISO); '' = unpaid
  paid_cash_cents INTEGER NOT NULL DEFAULT 0,  -- naplaćeno gotovinom
  paid_bank_cents INTEGER NOT NULL DEFAULT 0,  -- naplaćeno virmanski (preko računa)
  payment_ref     TEXT NOT NULL DEFAULT '',    -- broj izvoda/uplatnice
  created_at     TEXT NOT NULL DEFAULT (datetime('now')),
  deleted_at     TEXT                      -- soft delete: non-NULL means deleted
);

-- Append-only audit log: one row per create/edit/delete, with a self-contained
-- JSON snapshot of the invoice state at that moment (customer name, not id).
CREATE TABLE IF NOT EXISTS invoice_revisions (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  invoice_id INTEGER NOT NULL REFERENCES invoices(id),
  action     TEXT NOT NULL,             -- created | edited | deleted
  changed_at TEXT NOT NULL DEFAULT (datetime('now')),
  snapshot   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS invoice_items (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  invoice_id       INTEGER NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
  description      TEXT NOT NULL,
  unit             TEXT NOT NULL DEFAULT '',   -- jedinica mjere (JM): kg, kom, l, sat
  quantity         REAL NOT NULL DEFAULT 1,
  unit_price_cents INTEGER NOT NULL,
  discount_pct     REAL NOT NULL DEFAULT 0,    -- rabat as a percentage of the line
  line_total_cents INTEGER NOT NULL            -- already net of discount_pct
);

-- Šifrarnik: reusable picklist values shown as suggestions on the invoice form.
-- kind = unit (JM) | note (napomena preset). Products/services are the articles table.
CREATE TABLE IF NOT EXISTS catalog (
  id    INTEGER PRIMARY KEY AUTOINCREMENT,
  kind  TEXT NOT NULL,
  value TEXT NOT NULL,
  UNIQUE(kind, value)
);

-- Articles (artikli): managed products/services with a default unit and price,
-- offered as suggestions that autofill an invoice line.
CREATE TABLE IF NOT EXISTS articles (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  name             TEXT NOT NULL UNIQUE,
  unit             TEXT NOT NULL DEFAULT '',
  unit_price_cents INTEGER NOT NULL DEFAULT 0
);
