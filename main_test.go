package main

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		"issue_date":  {"2025-02-01"}, // number is auto-generated, not sent
		"customer_id": {"1"},
		"description": {"Usluga A", "Usluga B", ""}, // blank row must be skipped
		"quantity":    {"2", "1", "1"},
		"unit_price":  {"10,00", "5,50", ""}, // comma decimals must parse
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
	// view must show the issuer company and place/date of issue
	for _, want := range []string{"Sitna riba", "Mjesto i datum izdavanja", "Zagreb, 2025-02-01"} {
		if !strings.Contains(b, want) {
			t.Fatalf("invoice view missing issuer info %q in:\n%s", want, b)
		}
	}
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
