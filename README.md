# Pausalac

Self-hostable invoicing for Croatian **paušalni obrt** (flat-rate sole traders).
Issue invoices and (planned) keep the mandatory **Knjiga prometa (KPR)** and
**Obrazac PO-SD** — without a spreadsheet.

Single Go binary, embedded SQLite, no external services. Runs on a tiny VPS for a
few euros, or self-host it yourself.

## Status

Early. Working today: invoice creation, listing, and viewing. See [Roadmap](#roadmap).

## Requirements

- Go 1.26+ (to build/run from source), **or**
- Docker (to run the container — nothing else needed)

## Run

### Local (source)

```sh
go run .
```

Open http://localhost:8080. The database is created and seeded with demo data on
first boot at `./data/pausalac.db`.

### Docker

```sh
docker compose up --build
```

Same app on http://localhost:8080. The SQLite file lives in the `pausalac-data`
volume and persists across restarts.

## Configuration

| Env var   | Default              | Description                |
|-----------|----------------------|----------------------------|
| `PORT`    | `8080`               | HTTP port                  |
| `DB_PATH` | `data/pausalac.db`   | SQLite file path           |

## Test

```sh
go test ./...
```

Tests run against a throwaway SQLite file per test, exercising the full
auto-migrate + seed path. No fixtures or external setup.

## Format templates

HTML templates are formatted with [djlint](https://djlint.com) (config in
`.djlintrc`, `golang` profile so it understands `{{ }}`):

```sh
pip install djlint
djlint templates/ --reformat   # check only: djlint templates/ --check
```

## How it works

- **`db.Open(path)`** is the single database entry point. On start it connects,
  applies `db/schema.sql` (idempotent `CREATE IF NOT EXISTS` — acts as
  auto-migration), and if the DB is empty runs `db/seed.sql` to insert demo data.
  So a fresh `docker run` yields an immediately usable app.
- Money is stored as **integer cents (EUR)** to avoid float rounding.
- The frontend is server-rendered `html/template`, embedded in the binary — no
  build step, no JS toolchain.

## Project layout

```
db/
  db.go         # db.Open(): connect + auto-migrate + seed-if-empty
  schema.sql    # tables (money in integer cents)
  seed.sql      # demo obrt + customer + sample invoice (guarded inserts)
main.go         # HTTP handlers: list / new / create / view
templates/      # html/template pages (embedded)
main_test.go    # end-to-end handler tests
Dockerfile      # CGO_ENABLED=0 static build -> scratch image
docker-compose.yml
```

## Backup

The entire app state is one SQLite file. Back up = copy `DB_PATH` (or the
`pausalac-data` Docker volume).

## Roadmap

- [x] Invoice create / list / view
- [ ] Knjiga prometa (KPR) view, aggregated from invoices
- [ ] Obrazac PO-SD
- [ ] PDF / print output
- [x] PDV (VAT) support for traders in the system
- [ ] Fiskalizacija 2.0 e-invoice (UBL/CII XML) issuance & receipt
- [ ] Authentication / multi-tenant (one SQLite file per customer for hosted)

## TODO

### Convenience

- [x] **Articles (artikli)** — dedicated tab to manage products/services (name, unit, price); offered as invoice-line suggestions that autofill unit + price
- [ ] **Customer loading via QR** — scan a QR code to fill in customer details for faster invoice creation
- [x] **Excel import** — import from an existing invoice or Plavi ured workbook (`.xlsx`, `.xlsm`, `.ods`, `.pdf`): company data (PODACI sheet / invoice header) and, from Plavi ured workbooks, invoices + customers (BAZA sheet). Idempotent — re-importing skips invoices whose number already exists.
- [x] **Customer management** — add/edit/delete customers in-app, plus an on-the-spot modal on the invoice form
- [x] **Company settings page** — edit obrt master data in-app (`/settings`)
- [x] **Catalogs (šifranici)** — datalist suggestions for products/services, units (kg, kom, l, sat), and note presets on the invoice form, plus an on-the-spot modal to add new values

### Gaps vs. the Plavi ured example spreadsheet

Build order is dictated by data dependencies: **payment tracking → KPR → PO-SD**. The
ledger and annual report are computed from *collected payments* (naplaćeno), which the
app does not model yet. Line-item and header fields below are independent and cheaper.

Core (legally required, payment-based):

- [x] **Payment / collection tracking** — per invoice: collected amount, date paid, and method (cash `gotovina` vs bank transfer `virmanski`, stored as separate cash/bank cents for KPR). Recorded from the invoice view; imported from the BAZA sheet; drives a paid/unpaid (nenaplaćeni računi) list filter. Prerequisite for KPR and PO-SD.
- [x] **Knjiga prometa (KPR)** — turnover ledger at `/kpr`, per calendar year, built from collected payments: redni broj, nadnevak, broj temeljnice, broj računa, iznos u gotovini, iznos virmanski, ukupno naplaćeno, with cash/bank/total sums. Print-friendly.
- [ ] **Obrazac PO-SD** — annual flat-tax report: yearly receipts (cash + non-cash), tax base, paušalni porez, prirez, months of operation. Needs tax-bracket tables (`razine`) + months-of-operation logic.

Invoice line items:

- [x] **Unit of measure (JM)** per line — kg, kom, l, sat (with catalog suggestions)
- [x] **Discount (Rabat)** per line — percentage per line, feeds the totals
- [x] **PDV (VAT) breakdown** — IZNOS / RABAT / OSNOVICA / PDV (25%) / UKUPNI IZNOS; VAT charged only when the obrt is in the PDV sustav (Settings toggle), otherwise PDV 0 + exemption note
- [x] **PDV-exemption note presets** — čl. 90 st. 2 / čl. 17 st. 1 seeded as note-field suggestions (extendable via the catalog modal)

Invoice header / footer fields:

- [x] **Payment due date** (Dospijeće plaćanja)
- [x] **Delivery date** (Datum isporuke)
- [x] **Time of issue** (Vrijeme izrade) — alongside the date
- [x] **Payment method** (Način plaćanja) — with bank/cash suggestions
- [x] **Poziv na broj** (payment reference, typically the invoice number)
- [ ] **Operator mark** (Oznaka operatera)
- [ ] **Customer city** (Mjesto) as a separate field from address

Company master data:

- [x] **Bank name** (Banka)
- [x] **Owner address** (Adresa vlasnika, separate from business address)
- [ ] **Activity code + name** (NKD, e.g. "70.22 Poslovno savjetovanje")
- [ ] **Operator mark** (Oznaka operatera)

Document types:

- [ ] **Predujam (advance) + Storno** — advance-payment invoices and credit/cancellation invoices that reference a predujam

## Disclaimer

Calculations are informational. Verify against the
[Porezna uprava](https://porezna-uprava.gov.hr/) rules; this software carries no
liability for errors.
