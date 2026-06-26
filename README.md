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
- [ ] PDV (VAT) support for traders in the system
- [ ] Fiskalizacija 2.0 e-invoice (UBL/CII XML) issuance & receipt
- [ ] Authentication / multi-tenant (one SQLite file per customer for hosted)

## TODO

- [ ] **Customer loading via QR** — scan a QR code to fill in customer details for faster invoice creation
- [ ] **Excel import** — import values from existing Excel files previously used for invoice creation
- [ ] **Knjiga prometa (KPR)** — the mandatory turnover ledger, aggregated from invoices

## Disclaimer

Calculations are informational. Verify against the
[Porezna uprava](https://porezna-uprava.gov.hr/) rules; this software carries no
liability for errors.
