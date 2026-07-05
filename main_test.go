package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pausalac/db"
)

// fresh DB in a temp dir: exercises auto-migrate + seed in one shot.
func freshDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func body(t *testing.T, r *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSeedAndList(t *testing.T) {
	db := freshDB(t)
	srv := httptest.NewServer(listInvoices(db))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	b := body(t, resp)
	// seed inserts invoice "1/1/2025" for "Demo Klijent d.o.o." with one 50.00 EUR item
	for _, want := range []string{"1/1/2025", "Demo Klijent", "50.00"} {
		if !strings.Contains(b, want) {
			t.Fatalf("seed data missing %q in:\n%s", want, b)
		}
	}
}

func TestCreateInvoice(t *testing.T) {
	db := freshDB(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /invoices", createInvoice(db))
	mux.HandleFunc("GET /invoices/{id}", viewInvoice(db))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	form := url.Values{
		"issue_date":     {"2025-02-01"}, // number is auto-generated, not sent
		"customer_id":    {"1"},
		"delivery_date":  {"2025-02-03"},
		"due_date":       {"2025-02-15"},
		"payment_method": {"Transakcijski račun"},
		"poziv":          {"2-2-2025"},
		"description":    {"Usluga A", "Usluga B", ""}, // blank row must be skipped
		"quantity":       {"2", "1", "1"},
		"unit_price":     {"10,00", "5,50", ""}, // comma decimals must parse
	}
	resp, err := http.PostForm(srv.URL+"/invoices", form)
	if err != nil {
		t.Fatal(err)
	}
	b := body(t, resp)
	// 2*10.00 + 1*5.50 = 25.50
	if !strings.Contains(b, "25.50") {
		t.Fatalf("expected total 25.50, got:\n%s", b)
	}
	// seed already has 1 invoice in 2025, this is issued 2025-02 -> "2/2/2025"
	if !strings.Contains(b, "2/2/2025") {
		t.Fatalf("expected generated number 2/2/2025, got:\n%s", b)
	}
	// header/footer fields round-trip and render in Croatian date format
	for _, want := range []string{"03.02.2025.", "15.02.2025.", "Transakcijski račun", "2-2-2025"} {
		if !strings.Contains(b, want) {
			t.Fatalf("invoice view missing header field %q in:\n%s", want, b)
		}
	}
	// view must show the issuer company and place/date of issue
	for _, want := range []string{"Sitna riba", "Mjesto i datum izdavanja", "Zagreb, 01.02.2025."} {
		if !strings.Contains(b, want) {
			t.Fatalf("invoice view missing issuer info %q in:\n%s", want, b)
		}
	}
}

// Duplicating an invoice prefills the new-invoice form with the source's customer,
// items, and note, but with today's date and no number (a fresh one on save).
func TestDuplicateInvoice(t *testing.T) {
	db := freshDB(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /invoices/{id}/duplicate", duplicateInvoice(db))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// seed invoice id 1: customer "Demo Klijent", item "Poslovno savjetovanje" @ 50.00
	resp, err := http.Get(srv.URL + "/invoices/1/duplicate")
	if err != nil {
		t.Fatal(err)
	}
	b := body(t, resp)
	for _, want := range []string{
		"Poslovno savjetovanje", // item description carried over
		"50.00",                 // unit price formatted from cents
		`value="1" selected`,    // seeded customer preselected
	} {
		if !strings.Contains(b, want) {
			t.Fatalf("duplicate form missing %q in:\n%s", want, b)
		}
	}
	// the source number must NOT be carried into the new form
	if strings.Contains(b, "1/1/2025") {
		t.Fatalf("duplicate form should not contain the source number, got:\n%s", b)
	}
}

// Editing updates the invoice in place and records a revision; soft-deleting hides
// it from the list but keeps it viewable, and the history shows every change.
func TestEditDeleteHistory(t *testing.T) {
	db := freshDB(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", listInvoices(db))
	mux.HandleFunc("POST /invoices", createInvoice(db))
	mux.HandleFunc("GET /invoices/{id}", viewInvoice(db))
	mux.HandleFunc("POST /invoices/{id}", updateInvoice(db))
	mux.HandleFunc("POST /invoices/{id}/delete", deleteInvoice(db))
	mux.HandleFunc("GET /invoices/{id}/history", invoiceHistory(db))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// don't follow the 303 redirects, so we can read Location and drive each step
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	// create -> records "created" with "Usluga A"
	resp, err := client.PostForm(srv.URL+"/invoices", url.Values{
		"issue_date":  {"2025-02-01"},
		"customer_id": {"1"},
		"description": {"Usluga A"},
		"quantity":    {"1"},
		"unit_price":  {"10,00"},
	})
	if err != nil {
		t.Fatal(err)
	}
	loc := resp.Header.Get("Location") // /invoices/2 (seed holds id 1)
	if loc == "" {
		t.Fatalf("create did not redirect, status %d", resp.StatusCode)
	}

	// edit -> records "edited" with the new description
	if _, err := client.PostForm(srv.URL+loc, url.Values{
		"issue_date":  {"2025-02-01"},
		"customer_id": {"1"},
		"description": {"Promijenjena stavka"},
		"quantity":    {"1"},
		"unit_price":  {"7,00"},
	}); err != nil {
		t.Fatal(err)
	}

	// history before delete: both the original and edited state, with action labels
	hb := body(t, mustGet(t, client, srv.URL+loc+"/history"))
	for _, want := range []string{"Stvoreno", "Promijenjeno", "Usluga A", "Promijenjena stavka"} {
		if !strings.Contains(hb, want) {
			t.Fatalf("history missing %q in:\n%s", want, hb)
		}
	}

	// soft delete
	if _, err := client.PostForm(srv.URL+loc+"/delete", nil); err != nil {
		t.Fatal(err)
	}

	// list must no longer show this invoice's number
	lb := body(t, mustGet(t, client, srv.URL+"/"))
	if strings.Contains(lb, "2/2/2025") {
		t.Fatalf("deleted invoice still listed:\n%s", lb)
	}

	// view still works and is marked deleted
	vb := body(t, mustGet(t, client, srv.URL+loc))
	if !strings.Contains(vb, "Ovaj račun je obrisan.") {
		t.Fatalf("deleted invoice view missing deleted notice:\n%s", vb)
	}

	// history now includes the deletion event
	hb = body(t, mustGet(t, client, srv.URL+loc+"/history"))
	if !strings.Contains(hb, "Obrisano") {
		t.Fatalf("history missing delete event in:\n%s", hb)
	}
}

// The settings page shows current company data, saves edits, and rejects a
// missing required field with a 422 + inline error.
func TestCompanySettings(t *testing.T) {
	db := freshDB(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /settings", settingsPage(db))
	mux.HandleFunc("POST /settings", saveSettings(db))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	// GET prefills from the seeded company
	if b := body(t, mustGet(t, client, srv.URL+"/settings")); !strings.Contains(b, "Sitna riba") {
		t.Fatalf("settings form missing seeded company:\n%s", b)
	}

	// valid save -> 303 redirect to ?saved=1, and the new value persists
	resp, err := client.PostForm(srv.URL+"/settings", url.Values{
		"name": {"Nova Firma d.o.o."}, "owner": {"Ana Anić"}, "oib": {"99999999999"},
		"bank": {"Nova banka"}, "iban": {"HR00"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("valid save status = %d, want 303", resp.StatusCode)
	}
	if b := body(t, mustGet(t, client, srv.URL+"/settings?saved=1")); !strings.Contains(b, "Nova Firma d.o.o.") || !strings.Contains(b, "Nova banka") {
		t.Fatalf("saved company data not persisted:\n%s", b)
	}

	// missing required name -> 422 with inline error, no crash
	resp, err = client.PostForm(srv.URL+"/settings", url.Values{
		"name": {""}, "owner": {"Ana"}, "oib": {"123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("missing name status = %d, want 422", resp.StatusCode)
	}
	if b := body(t, resp); !strings.Contains(b, "Obavezno polje.") {
		t.Fatalf("missing required-field error:\n%s", b)
	}
}

// The catalog seeds units/products into the form datalists, a new value added via
// /catalog shows up on the next form load, and an item's unit round-trips to view.
func TestCatalogAndUnit(t *testing.T) {
	db := freshDB(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /invoices/new", newInvoice(db))
	mux.HandleFunc("POST /catalog", addCatalog(db))
	mux.HandleFunc("POST /invoices", createInvoice(db))
	mux.HandleFunc("GET /invoices/{id}", viewInvoice(db))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// seeded datalist values present on the form
	b := body(t, mustGet(t, http.DefaultClient, srv.URL+"/invoices/new"))
	for _, want := range []string{`id="list-unit"`, `value="sat"`, "Poslovno savjetovanje"} {
		if !strings.Contains(b, want) {
			t.Fatalf("new form missing seeded catalog %q", want)
		}
	}

	// add a unit on the spot, then it must appear on the next form load
	if _, err := http.PostForm(srv.URL+"/catalog", url.Values{"kind": {"unit"}, "value": {"paket"}}); err != nil {
		t.Fatal(err)
	}
	b = body(t, mustGet(t, http.DefaultClient, srv.URL+"/invoices/new"))
	if !strings.Contains(b, `value="paket"`) {
		t.Fatalf("catalog-added unit not in datalist:\n%s", b)
	}

	// a submitted unit must persist and render on the invoice
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm(srv.URL+"/invoices", url.Values{
		"issue_date": {"2025-03-01"}, "customer_id": {"1"},
		"description": {"Usluga"}, "unit": {"sat"}, "quantity": {"2"}, "unit_price": {"10,00"},
	})
	if err != nil {
		t.Fatal(err)
	}
	vb := body(t, mustGet(t, http.DefaultClient, srv.URL+resp.Header.Get("Location")))
	if !strings.Contains(vb, ">sat<") {
		t.Fatalf("item unit not shown on invoice view:\n%s", vb)
	}
}

// Articles can be created and then appear both in the management list and as an
// autofill-enabled suggestion (data-price) on the invoice form.
func TestArticles(t *testing.T) {
	db := freshDB(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /articles", listArticles(db))
	mux.HandleFunc("POST /articles", createArticle(db))
	mux.HandleFunc("GET /invoices/new", newInvoice(db))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm(srv.URL+"/articles", url.Values{
		"name": {"Web razvoj"}, "unit": {"sat"}, "price": {"45,00"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create article status = %d, want 303", resp.StatusCode)
	}

	// shows in the management list with its formatted price
	if b := body(t, mustGet(t, http.DefaultClient, srv.URL+"/articles")); !strings.Contains(b, "Web razvoj") || !strings.Contains(b, "45.00") {
		t.Fatalf("article not listed with price:\n%s", b)
	}

	// appears on the invoice form as an autofill option (name + data-price)
	b := body(t, mustGet(t, http.DefaultClient, srv.URL+"/invoices/new"))
	if !strings.Contains(b, `value="Web razvoj"`) || !strings.Contains(b, `data-price="45.00"`) {
		t.Fatalf("article not offered as invoice suggestion:\n%s", b)
	}
}

func TestCustomers(t *testing.T) {
	db := freshDB(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /customers", listCustomers(db))
	mux.HandleFunc("POST /customers", createCustomer(db))
	mux.HandleFunc("POST /customers/quick", quickCustomer(db))
	mux.HandleFunc("POST /customers/{id}/delete", deleteCustomer(db))
	mux.HandleFunc("GET /invoices/new", newInvoice(db))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// tab: create a customer, it shows in the list and on the invoice form
	resp, err := client.PostForm(srv.URL+"/customers", url.Values{
		"name": {" Acme d.o.o."}, "oib": {"12345678901"}, "address": {"Ilica 1, Zagreb"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create customer status = %d, want 303", resp.StatusCode)
	}
	if b := body(t, mustGet(t, http.DefaultClient, srv.URL+"/customers")); !strings.Contains(b, "Acme d.o.o.") || !strings.Contains(b, "Ilica 1, Zagreb") {
		t.Fatalf("customer not listed:\n%s", b)
	}
	if b := body(t, mustGet(t, http.DefaultClient, srv.URL+"/invoices/new")); !strings.Contains(b, "Acme d.o.o.") {
		t.Fatalf("customer not offered on invoice form:\n%s", b)
	}

	// modal: quick-add returns the new row as JSON so the page can select it
	resp, err = client.PostForm(srv.URL+"/customers/quick", url.Values{"name": {"Beta obrt"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("quick customer status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ID == 0 || got.Name != "Beta obrt" {
		t.Fatalf("quick customer json = %+v", got)
	}

	// the seed's demo customer is referenced by the seed invoice: delete must be refused
	resp, err = client.PostForm(srv.URL+"/customers/1/delete", nil)
	if err != nil {
		t.Fatal(err)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "err=inuse") {
		t.Fatalf("referenced customer delete redirected to %q, want err=inuse", loc)
	}
}

func TestHrDate(t *testing.T) {
	if got := hrDate("2026-07-05"); got != "05.07.2026." {
		t.Errorf("hrDate = %q, want 05.07.2026.", got)
	}
	if got := hrDateTime("2026-07-05 14:30:00"); got != "05.07.2026. 14:30" {
		t.Errorf("hrDateTime = %q, want 05.07.2026. 14:30", got)
	}
	// unparseable input is passed through untouched
	if got := hrDate("n/a"); got != "n/a" {
		t.Errorf("hrDate passthrough = %q", got)
	}
	// isoUTC marks the instant as UTC so the browser can localise it
	if got := isoUTC("2026-07-05 14:30:00"); got != "2026-07-05T14:30:00Z" {
		t.Errorf("isoUTC = %q, want 2026-07-05T14:30:00Z", got)
	}
}

// computeTotals is the money path: discount reduces the base, VAT applies only
// when not exempt, and it's charged on the whole base.
func TestComputeTotals(t *testing.T) {
	// 2 x 10.00, no discount, exempt -> everything = 20.00, no VAT
	items := []Item{{Quantity: 2, UnitPriceCents: 1000, LineTotalCents: 2000}}
	got := computeTotals(items, true)
	if got.GrossCents != 2000 || got.DiscountCents != 0 || got.BaseCents != 2000 || got.VatCents != 0 || got.TotalCents != 2000 {
		t.Fatalf("exempt no-discount: %+v", got)
	}

	// 1 x 10.00 with 10% rabat -> line 9.00, discount 1.00
	items = []Item{{Quantity: 1, UnitPriceCents: 1000, DiscountPct: 10, LineTotalCents: 900}}
	got = computeTotals(items, true)
	if got.GrossCents != 1000 || got.DiscountCents != 100 || got.BaseCents != 900 || got.TotalCents != 900 {
		t.Fatalf("exempt with discount: %+v", got)
	}

	// same line, VAT-registered -> PDV 25% of 900 = 225, total 1125
	got = computeTotals(items, false)
	if got.VatCents != 225 || got.TotalCents != 1125 {
		t.Fatalf("vat-registered: %+v", got)
	}
}

func mustGet(t *testing.T, c *http.Client, url string) *http.Response {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// Missing fields must redisplay the form with inline errors (422), not an error page,
// and must preserve what the user already typed.
func TestCreateInvoiceValidationRedisplay(t *testing.T) {
	db := freshDB(t)
	srv := httptest.NewServer(createInvoice(db))
	defer srv.Close()

	form := url.Values{
		"issue_date":  {""}, // missing
		"customer_id": {""}, // missing
		"description": {"Tipkana stavka", "", ""},
		"quantity":    {"3", "1", "1"},
		"unit_price":  {"9,99", "", ""},
	}
	resp, err := http.PostForm(srv.URL+"/invoices", form)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	b := body(t, resp)
	for _, want := range []string{
		"Datum izdavanja je obavezan.", // issue_date error shown inline
		"Odaberite kupca.",             // customer_id error shown inline
		"Tipkana stavka",               // typed value preserved
		"9,99",                         // typed price preserved
	} {
		if !strings.Contains(b, want) {
			t.Fatalf("redisplayed form missing %q in:\n%s", want, b)
		}
	}
}

// TestImportFormats runs local sample invoices through the extractor. The
// pdf/xlsm/ods samples are the same invoice in three formats, so every format
// must extract identical fields (this catches format-specific parsing bugs
// without hardcoding anyone's personal data). Skips if the untracked, private
// invoices/ folder is absent — the samples contain real details and are never
// committed.
func TestImportFormats(t *testing.T) {
	if _, err := os.Stat("invoices"); err != nil {
		t.Skip("no sample invoices/ folder")
	}
	samples := []string{
		"invoices/1-06-26.pdf",
		"invoices/Tablica za izradu računa ENG.xlsm",
		"invoices/Tablica za izradu računa.ods",
	}
	var first map[string]string
	for _, f := range samples {
		lines, err := readDocLines(f, mustRead(t, f))
		if err != nil {
			t.Fatalf("%s: readDocLines: %v", f, err)
		}
		c := extractCompany(lines)
		for _, k := range []string{"name", "owner", "iban", "bank", "swift"} {
			if c[k] == "" {
				t.Errorf("%s: %s not extracted", f, k)
			}
		}
		if first == nil {
			first = c
			continue
		}
		// Same invoice, different format: fields must agree.
		for _, k := range []string{"name", "owner", "iban", "bank", "swift"} {
			if c[k] != first[k] {
				t.Errorf("%s: %s = %q, differs from pdf %q", f, k, c[k], first[k])
			}
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
