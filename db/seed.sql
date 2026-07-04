-- Seeds default/sample data on an empty DB so `docker run` yields a usable app.
-- Guarded per-row with WHERE NOT EXISTS so re-running is harmless.

INSERT INTO company (id, name, owner, oib, address, place, iban, bank, owner_address, vat_exempt)
SELECT 1, 'Sitna riba, Obrt za savjetovanje', 'Jura Plavić', '11111111111',
       'Hitri korak 28, 10000 Zagreb', 'Zagreb', 'HR1210010051863000160',
       'VB Vaša banka d.d.', 'Hitri korak 28, 10000 Zagreb', 1
WHERE NOT EXISTS (SELECT 1 FROM company WHERE id = 1);

INSERT INTO customers (id, name, oib, address)
SELECT 1, 'Demo Klijent d.o.o.', '22222222222', 'Ilica 1, 10000 Zagreb'
WHERE NOT EXISTS (SELECT 1 FROM customers WHERE id = 1);

INSERT INTO invoices (id, number, issue_date, customer_id, note)
SELECT 1, '1/1/2025', '2025-01-12', 1, 'Primjer računa'
WHERE NOT EXISTS (SELECT 1 FROM invoices WHERE id = 1);

INSERT INTO invoice_items (invoice_id, description, unit, quantity, unit_price_cents, line_total_cents)
SELECT 1, 'Poslovno savjetovanje', 'sat', 1, 5000, 5000
WHERE NOT EXISTS (SELECT 1 FROM invoice_items WHERE invoice_id = 1);

-- Šifrarnik defaults (units from the JM list, note presets from the PODACI sheet).
INSERT OR IGNORE INTO catalog (kind, value) VALUES
  ('unit', 'kom'), ('unit', 'sat'), ('unit', 'kg'), ('unit', 'l'),
  ('note', 'Oslobođeno PDV-a temeljem članka 90. st. 2 Zakona o PDV-u'),
  ('note', 'Oslobođeno PDV-a temeljem članka 17. st. 1 Zakona o PDV-u - reverse charge');

-- Demo articles (products/services with default unit + price).
INSERT OR IGNORE INTO articles (name, unit, unit_price_cents) VALUES
  ('Poslovno savjetovanje', 'sat', 5000);
