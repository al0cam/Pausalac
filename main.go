package main

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"embed"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ledongthuc/pdf"
	"github.com/xuri/excelize/v2"

	"pausalac/db"
)

//go:embed templates/*.html
var assets embed.FS

// One parsed template set per language; the "T" func is bound to that language
// at parse time, so templates just call {{T "key"}} with no per-request cost.
var tmpls = parseTemplates()

func parseTemplates() map[string]*template.Template {
	eur := func(cents int64) string { return strconv.FormatFloat(float64(cents)/100, 'f', 2, 64) }
	// dict builds a map from key/value pairs so a template can pass a custom
	// context to a shared partial, e.g. {{template "items-table" (dict "Items" .Items "Total" .T)}}.
	dict := func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	m := map[string]*template.Template{}
	for lang := range messages {
		l := lang
		m[lang] = template.Must(template.New("").Funcs(template.FuncMap{
			"eur":      eur,
			"dict":     dict,
			"date":     hrDate,
			"datetime": hrDateTime,
			"isoutc":   isoUTC,
			"T":        func(key string) string { return tr(l, key) },
		}).ParseFS(assets, "templates/*.html"))
	}
	return m
}

// langOf resolves the request language from the "lang" cookie, defaulting to hr.
func langOf(r *http.Request) string {
	if c, err := r.Cookie("lang"); err == nil && messages[c.Value] != nil {
		return c.Value
	}
	return defaultLang
}

// withLang lets ?lang=xx set the language: it stores the choice in a cookie and
// redirects to the clean URL so the param doesn't linger.
func withLang(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if l := r.URL.Query().Get("lang"); messages[l] != nil {
			http.SetCookie(w, &http.Cookie{Name: "lang", Value: l, Path: "/", MaxAge: 31536000})
			http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type Invoice struct {
	ID              int64
	Number          string
	IssueDate       string
	IssueTime       string
	DeliveryDate    string
	DueDate         string
	PaymentMethod   string
	Poziv           string
	Customer        string
	CustomerAddress string
	CustomerOIB     string
	Note            string
	TotalCents      int64
	Deleted         bool
	PaidDate        string
	PaidCashCents   int64
	PaidBankCents   int64
	PaymentRef      string
}

// Paid reports whether a collection has been recorded (payment date set).
func (v Invoice) Paid() bool { return v.PaidDate != "" }

// PaidCents is the total collected (cash + bank).
func (v Invoice) PaidCents() int64 { return v.PaidCashCents + v.PaidBankCents }

// PaidMethodKey is the i18n key describing how the invoice was collected.
func (v Invoice) PaidMethodKey() string {
	switch {
	case v.PaidCashCents > 0 && v.PaidBankCents > 0:
		return "method_both"
	case v.PaidBankCents > 0:
		return "method_bank"
	case v.PaidCashCents > 0:
		return "method_cash"
	default:
		return ""
	}
}

type Item struct {
	Description    string  `json:"description"`
	Unit           string  `json:"unit"`
	Quantity       float64 `json:"quantity"`
	UnitPriceCents int64   `json:"unit_price_cents"`
	DiscountPct    float64 `json:"discount_pct"`
	LineTotalCents int64   `json:"line_total_cents"` // already net of the discount
}

// Totals is the RAČUN totals block: gross, discount, base, VAT, grand total.
type Totals struct {
	GrossCents    int64 `json:"gross_cents"`    // IZNOS (before discount)
	DiscountCents int64 `json:"discount_cents"` // RABAT
	BaseCents     int64 `json:"base_cents"`     // OSNOVICA
	VatCents      int64 `json:"vat_cents"`      // PDV
	TotalCents    int64 `json:"total_cents"`    // UKUPNI IZNOS
}

// vatRate is the single Croatian standard rate; PDV only applies when the obrt is
// in the PDV sustav (company.VatExempt == false).
const vatRate = 0.25

// computeTotals derives the totals block from line items. VAT is charged on the
// whole base (not per line), matching the RAČUN layout.
func computeTotals(items []Item, vatExempt bool) Totals {
	var t Totals
	for _, it := range items {
		t.GrossCents += int64(math.Round(it.Quantity * float64(it.UnitPriceCents)))
		t.BaseCents += it.LineTotalCents
	}
	t.DiscountCents = t.GrossCents - t.BaseCents
	if !vatExempt {
		t.VatCents = int64(math.Round(float64(t.BaseCents) * vatRate))
	}
	t.TotalCents = t.BaseCents + t.VatCents
	return t
}

// catalogs holds the šifrarnik suggestion lists shown on the invoice form.
type catalogs struct {
	Units []string
	Notes []string
}

// Article is a managed product/service with a default unit and price.
type Article struct {
	ID         int64
	Name       string
	Unit       string
	PriceCents int64
}

type Customer struct {
	ID      int64
	Name    string
	OIB     string
	Address string
}

type Company struct {
	Name         string
	Owner        string
	OIB          string
	Address      string
	Place        string
	IBAN         string
	Bank         string
	Swift        string
	OwnerAddress string
	VatExempt    bool
}

func main() {
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "data/pausalac.db"
	}
	conn, err := db.Open(dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", listInvoices(conn))
	mux.HandleFunc("GET /invoices/new", newInvoice(conn))
	mux.HandleFunc("POST /invoices", createInvoice(conn))
	mux.HandleFunc("GET /invoices/{id}", viewInvoice(conn))
	mux.HandleFunc("GET /invoices/{id}/duplicate", duplicateInvoice(conn))
	mux.HandleFunc("GET /invoices/{id}/edit", editInvoice(conn))
	mux.HandleFunc("POST /invoices/{id}", updateInvoice(conn))
	mux.HandleFunc("POST /invoices/{id}/delete", deleteInvoice(conn))
	mux.HandleFunc("POST /invoices/{id}/payment", recordPayment(conn))
	mux.HandleFunc("POST /invoices/{id}/payment/clear", clearPayment(conn))
	mux.HandleFunc("GET /invoices/{id}/history", invoiceHistory(conn))
	mux.HandleFunc("GET /kpr", kprPage(conn))
	mux.HandleFunc("GET /import", importPage(conn))
	mux.HandleFunc("POST /import", runImport(conn))
	mux.HandleFunc("GET /settings", settingsPage(conn))
	mux.HandleFunc("POST /settings", saveSettings(conn))
	mux.HandleFunc("POST /catalog", addCatalog(conn))
	mux.HandleFunc("GET /articles", listArticles(conn))
	mux.HandleFunc("GET /articles/new", newArticle(conn))
	mux.HandleFunc("POST /articles", createArticle(conn))
	mux.HandleFunc("GET /articles/{id}/edit", editArticle(conn))
	mux.HandleFunc("POST /articles/{id}", updateArticle(conn))
	mux.HandleFunc("POST /articles/{id}/delete", deleteArticle(conn))
	mux.HandleFunc("GET /customers", listCustomers(conn))
	mux.HandleFunc("GET /customers/new", newCustomer(conn))
	mux.HandleFunc("POST /customers", createCustomer(conn))
	mux.HandleFunc("POST /customers/quick", quickCustomer(conn))
	mux.HandleFunc("GET /customers/{id}/edit", editCustomer(conn))
	mux.HandleFunc("POST /customers/{id}", updateCustomer(conn))
	mux.HandleFunc("POST /customers/{id}/delete", deleteCustomer(conn))

	addr := ":" + cmp(os.Getenv("PORT"), "8080")
	log.Printf("pausalac listening on %s (db=%s)", addr, dbPath)
	log.Fatal(http.ListenAndServe(addr, withLang(mux)))
}

func cmp(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func listInvoices(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// ?filter=unpaid shows only nenaplaćeni računi (no payment recorded yet).
		filter := r.URL.Query().Get("filter")
		where := "i.deleted_at IS NULL"
		if filter == "unpaid" {
			where += " AND i.paid_date = ''"
		}
		rows, err := db.Query(`
			SELECT i.id, i.number, i.issue_date, c.name, i.note, i.paid_date,
			       i.paid_cash_cents, i.paid_bank_cents,
			       COALESCE(SUM(it.line_total_cents), 0)
			FROM invoices i
			JOIN customers c ON c.id = i.customer_id
			LEFT JOIN invoice_items it ON it.invoice_id = i.id
			WHERE ` + where + `
			GROUP BY i.id
			ORDER BY i.issue_date DESC, i.id DESC`)
		if err != nil {
			httpErr(w, err)
			return
		}
		defer rows.Close()
		// the listed amount is the grand total; add PDV when the obrt is VAT-registered
		var vatExempt int
		db.QueryRow(`SELECT vat_exempt FROM company WHERE id = 1`).Scan(&vatExempt)
		var invs []Invoice
		for rows.Next() {
			var v Invoice
			if err := rows.Scan(&v.ID, &v.Number, &v.IssueDate, &v.Customer, &v.Note,
				&v.PaidDate, &v.PaidCashCents, &v.PaidBankCents, &v.TotalCents); err != nil {
				httpErr(w, err)
				return
			}
			if vatExempt == 0 {
				v.TotalCents += int64(math.Round(float64(v.TotalCents) * vatRate))
			}
			invs = append(invs, v)
		}
		render(w, r, "list.html", map[string]any{"Invoices": invs, "Filter": filter})
	}
}

// itemInput holds an item row exactly as submitted, so a failed form can be
// redisplayed with the user's values intact.
type itemInput struct{ Description, Unit, Quantity, UnitPrice, Discount string }

// invoiceForm is the data the new/edit invoice template renders: submitted values,
// the customer list, and per-field validation errors (empty on a fresh form).
// ID is 0 for a new invoice and the invoice id when editing (the form then POSTs
// to /invoices/{id} and shows the immutable Number).
type invoiceForm struct {
	ID            int64
	Number        string
	IssueDate     string
	IssueTime     string
	DeliveryDate  string
	DueDate       string
	PaymentMethod string
	Poziv         string
	CustomerID    string
	Note          string
	Items         []itemInput
	Errors        map[string]string
	Customers     []Customer
	Catalog       catalogs
	Articles      []Article
}

// renderNew loads customers, ensures at least one item row, and renders the form.
func renderNew(w http.ResponseWriter, r *http.Request, db *sql.DB, f invoiceForm) {
	custs, err := allCustomers(db)
	if err != nil {
		httpErr(w, err)
		return
	}
	f.Customers = custs
	cat, err := loadCatalogs(db)
	if err != nil {
		httpErr(w, err)
		return
	}
	f.Catalog = cat
	arts, err := loadArticles(db)
	if err != nil {
		httpErr(w, err)
		return
	}
	f.Articles = arts
	if len(f.Items) == 0 {
		f.Items = append(f.Items, itemInput{Quantity: "1"})
	}
	render(w, r, "new.html", f)
}

// addCatalog persists one new šifrarnik value (added from the invoice form's
// modal). Idempotent via the UNIQUE(kind, value) constraint; returns 204.
func addCatalog(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		kind := r.FormValue("kind")
		value := strings.TrimSpace(r.FormValue("value"))
		if value == "" || (kind != "unit" && kind != "note") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if _, err := db.Exec(`INSERT OR IGNORE INTO catalog (kind, value) VALUES (?, ?)`, kind, value); err != nil {
			httpErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// loadCatalogs reads the šifrarnik values grouped by kind for the form datalists.
func loadCatalogs(db *sql.DB) (catalogs, error) {
	rows, err := db.Query(`SELECT kind, value FROM catalog ORDER BY value`)
	if err != nil {
		return catalogs{}, err
	}
	defer rows.Close()
	var c catalogs
	for rows.Next() {
		var kind, value string
		if err := rows.Scan(&kind, &value); err != nil {
			return catalogs{}, err
		}
		switch kind {
		case "unit":
			c.Units = append(c.Units, value)
		case "note":
			c.Notes = append(c.Notes, value)
		}
	}
	return c, nil
}

// articleForm is the data the article create/edit template renders. ID is 0 for
// a new article; Price is the editable euro string.
type articleForm struct {
	ID     int64
	Name   string
	Unit   string
	Price  string
	Errors map[string]string
	Units  []string
}

func listArticles(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		arts, err := loadArticles(db)
		if err != nil {
			httpErr(w, err)
			return
		}
		render(w, r, "articles.html", map[string]any{"Articles": arts})
	}
}

// renderArticleForm loads the unit suggestions and renders the create/edit form.
func renderArticleForm(w http.ResponseWriter, r *http.Request, db *sql.DB, f articleForm) {
	cat, err := loadCatalogs(db)
	if err != nil {
		httpErr(w, err)
		return
	}
	f.Units = cat.Units
	render(w, r, "article_form.html", f)
}

func newArticle(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		renderArticleForm(w, r, db, articleForm{})
	}
}

func createArticle(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f := parseArticleForm(r)
		if errs := validateArticle(f, langOf(r)); len(errs) > 0 {
			f.Errors = errs
			w.WriteHeader(http.StatusUnprocessableEntity)
			renderArticleForm(w, r, db, f)
			return
		}
		_, err := db.Exec(`INSERT INTO articles (name, unit, unit_price_cents) VALUES (?, ?, ?)`,
			f.Name, f.Unit, euroToCents(f.Price))
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				f.Errors = map[string]string{"name": tr(langOf(r), "err_article_exists")}
				w.WriteHeader(http.StatusUnprocessableEntity)
				renderArticleForm(w, r, db, f)
				return
			}
			httpErr(w, err)
			return
		}
		http.Redirect(w, r, "/articles", http.StatusSeeOther)
	}
}

func editArticle(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		f := articleForm{ID: id}
		var cents int64
		err = db.QueryRow(`SELECT name, unit, unit_price_cents FROM articles WHERE id = ?`, id).
			Scan(&f.Name, &f.Unit, &cents)
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		} else if err != nil {
			httpErr(w, err)
			return
		}
		f.Price = fmt.Sprintf("%.2f", float64(cents)/100)
		renderArticleForm(w, r, db, f)
	}
}

func updateArticle(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		f := parseArticleForm(r)
		f.ID = id
		if errs := validateArticle(f, langOf(r)); len(errs) > 0 {
			f.Errors = errs
			w.WriteHeader(http.StatusUnprocessableEntity)
			renderArticleForm(w, r, db, f)
			return
		}
		_, err = db.Exec(`UPDATE articles SET name = ?, unit = ?, unit_price_cents = ? WHERE id = ?`,
			f.Name, f.Unit, euroToCents(f.Price), id)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				f.Errors = map[string]string{"name": tr(langOf(r), "err_article_exists")}
				w.WriteHeader(http.StatusUnprocessableEntity)
				renderArticleForm(w, r, db, f)
				return
			}
			httpErr(w, err)
			return
		}
		http.Redirect(w, r, "/articles", http.StatusSeeOther)
	}
}

func deleteArticle(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if _, err := db.Exec(`DELETE FROM articles WHERE id = ?`, id); err != nil {
			httpErr(w, err)
			return
		}
		http.Redirect(w, r, "/articles", http.StatusSeeOther)
	}
}

func parseArticleForm(r *http.Request) articleForm {
	return articleForm{
		Name:  strings.TrimSpace(r.FormValue("name")),
		Unit:  strings.TrimSpace(r.FormValue("unit")),
		Price: strings.TrimSpace(r.FormValue("price")),
	}
}

func validateArticle(f articleForm, lang string) map[string]string {
	errs := map[string]string{}
	if f.Name == "" {
		errs["name"] = tr(lang, "err_required")
	}
	return errs
}

// loadArticles returns all articles ordered by name (for suggestions/management).
func loadArticles(db *sql.DB) ([]Article, error) {
	rows, err := db.Query(`SELECT id, name, unit, unit_price_cents FROM articles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var as []Article
	for rows.Next() {
		var a Article
		if err := rows.Scan(&a.ID, &a.Name, &a.Unit, &a.PriceCents); err != nil {
			return nil, err
		}
		as = append(as, a)
	}
	return as, nil
}

// customerForm is the data the customer create/edit template renders. ID is 0
// for a new customer.
type customerForm struct {
	ID      int64
	Name    string
	OIB     string
	Address string
	Errors  map[string]string
}

func listCustomers(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		custs, err := allCustomers(db)
		if err != nil {
			httpErr(w, err)
			return
		}
		render(w, r, "customers.html", map[string]any{
			"Customers": custs,
			"InUse":     r.URL.Query().Get("err") == "inuse",
		})
	}
}

func newCustomer(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		render(w, r, "customer_form.html", customerForm{})
	}
}

func createCustomer(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f := parseCustomerForm(r)
		if errs := validateCustomer(f, langOf(r)); len(errs) > 0 {
			f.Errors = errs
			w.WriteHeader(http.StatusUnprocessableEntity)
			render(w, r, "customer_form.html", f)
			return
		}
		if _, err := insertCustomer(db, f); err != nil {
			httpErr(w, err)
			return
		}
		http.Redirect(w, r, "/customers", http.StatusSeeOther)
	}
}

// quickCustomer inserts a customer from the invoice-form modal and returns the
// new row as JSON so the page can add and select it without reloading.
func quickCustomer(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f := parseCustomerForm(r)
		if f.Name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		id, err := insertCustomer(db, f)
		if err != nil {
			httpErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": id, "name": f.Name})
	}
}

func editCustomer(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		f := customerForm{ID: id}
		err = db.QueryRow(`SELECT name, oib, address FROM customers WHERE id = ?`, id).
			Scan(&f.Name, &f.OIB, &f.Address)
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		} else if err != nil {
			httpErr(w, err)
			return
		}
		render(w, r, "customer_form.html", f)
	}
}

func updateCustomer(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		f := parseCustomerForm(r)
		f.ID = id
		if errs := validateCustomer(f, langOf(r)); len(errs) > 0 {
			f.Errors = errs
			w.WriteHeader(http.StatusUnprocessableEntity)
			render(w, r, "customer_form.html", f)
			return
		}
		if _, err := db.Exec(`UPDATE customers SET name = ?, oib = ?, address = ? WHERE id = ?`,
			f.Name, f.OIB, f.Address, id); err != nil {
			httpErr(w, err)
			return
		}
		http.Redirect(w, r, "/customers", http.StatusSeeOther)
	}
}

func deleteCustomer(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		// A customer referenced by an invoice can't be deleted (FK); tell the user
		// instead of 500-ing.
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM invoices WHERE customer_id = ?`, id).Scan(&n); err != nil {
			httpErr(w, err)
			return
		}
		if n > 0 {
			http.Redirect(w, r, "/customers?err=inuse", http.StatusSeeOther)
			return
		}
		if _, err := db.Exec(`DELETE FROM customers WHERE id = ?`, id); err != nil {
			httpErr(w, err)
			return
		}
		http.Redirect(w, r, "/customers", http.StatusSeeOther)
	}
}

func parseCustomerForm(r *http.Request) customerForm {
	return customerForm{
		Name:    strings.TrimSpace(r.FormValue("name")),
		OIB:     strings.TrimSpace(r.FormValue("oib")),
		Address: strings.TrimSpace(r.FormValue("address")),
	}
}

func validateCustomer(f customerForm, lang string) map[string]string {
	errs := map[string]string{}
	if f.Name == "" {
		errs["name"] = tr(lang, "err_required")
	}
	return errs
}

func insertCustomer(db *sql.DB, f customerForm) (int64, error) {
	res, err := db.Exec(`INSERT INTO customers (name, oib, address) VALUES (?, ?, ?)`,
		f.Name, f.OIB, f.Address)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func newInvoice(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		renderNew(w, r, db, invoiceForm{IssueDate: time.Now().Format("2006-01-02")})
	}
}

// duplicateInvoice opens the new-invoice form prefilled from an existing invoice
// (same customer, items, note) with today's date. Saving runs the normal create
// flow, so the copy gets a fresh id, number, and date.
func duplicateInvoice(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		var customerID int64
		var note string
		err = db.QueryRow(`SELECT customer_id, note FROM invoices WHERE id = ?`, id).Scan(&customerID, &note)
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		} else if err != nil {
			httpErr(w, err)
			return
		}
		rows, err := db.Query(`SELECT description, unit, quantity, unit_price_cents, discount_pct
			FROM invoice_items WHERE invoice_id = ? ORDER BY id`, id)
		if err != nil {
			httpErr(w, err)
			return
		}
		defer rows.Close()
		f := invoiceForm{
			IssueDate:  time.Now().Format("2006-01-02"),
			CustomerID: strconv.FormatInt(customerID, 10),
			Note:       note,
		}
		f.Items = scanItemInputs(rows, &err)
		if err != nil {
			httpErr(w, err)
			return
		}
		renderNew(w, r, db, f)
	}
}

// scanItemInputs reads item rows (description, unit, quantity, unit_price_cents)
// into form inputs, formatting numbers back to editable strings.
func scanItemInputs(rows *sql.Rows, outErr *error) []itemInput {
	var items []itemInput
	for rows.Next() {
		var desc, uom string
		var qty, pct float64
		var price int64
		if err := rows.Scan(&desc, &uom, &qty, &price, &pct); err != nil {
			*outErr = err
			return items
		}
		disc := ""
		if pct != 0 {
			disc = strconv.FormatFloat(pct, 'f', -1, 64)
		}
		items = append(items, itemInput{
			Description: desc,
			Unit:        uom,
			Quantity:    strconv.FormatFloat(qty, 'f', -1, 64),
			UnitPrice:   fmt.Sprintf("%.2f", float64(price)/100),
			Discount:    disc,
		})
	}
	return items
}

// parseInvoiceForm reads the submitted invoice fields and item rows.
func parseInvoiceForm(r *http.Request) invoiceForm {
	f := invoiceForm{
		IssueDate:     r.FormValue("issue_date"),
		IssueTime:     strings.TrimSpace(r.FormValue("issue_time")),
		DeliveryDate:  r.FormValue("delivery_date"),
		DueDate:       r.FormValue("due_date"),
		PaymentMethod: strings.TrimSpace(r.FormValue("payment_method")),
		Poziv:         strings.TrimSpace(r.FormValue("poziv")),
		CustomerID:    r.FormValue("customer_id"),
		Note:          r.FormValue("note"),
	}
	descs := r.Form["description"]
	units := r.Form["unit"]
	qtys := r.Form["quantity"]
	prices := r.Form["unit_price"]
	discounts := r.Form["discount"]
	for i := range descs {
		f.Items = append(f.Items, itemInput{
			Description: descs[i],
			Unit:        at(units, i),
			Quantity:    at(qtys, i),
			UnitPrice:   at(prices, i),
			Discount:    at(discounts, i),
		})
	}
	return f
}

// validateInvoice checks the form and returns the parsed issue date, customer id,
// and a per-field error map (empty when valid).
func validateInvoice(f invoiceForm, lang string) (time.Time, int64, map[string]string) {
	errs := map[string]string{}
	issued, dateErr := time.Parse("2006-01-02", f.IssueDate)
	if f.IssueDate == "" {
		errs["issue_date"] = tr(lang, "err_date_required")
	} else if dateErr != nil {
		errs["issue_date"] = tr(lang, "err_date_invalid")
	}
	customerID, _ := strconv.ParseInt(f.CustomerID, 10, 64)
	if customerID == 0 {
		errs["customer_id"] = tr(lang, "err_customer_required")
	}
	validItems := 0
	for _, it := range f.Items {
		if strings.TrimSpace(it.Description) != "" {
			validItems++
		}
	}
	if validItems == 0 {
		errs["items"] = tr(lang, "err_items_required")
	}
	return issued, customerID, errs
}

// insertItems writes the form's item rows (skipping blank-description rows).
func insertItems(tx *sql.Tx, invoiceID int64, items []itemInput) error {
	for _, it := range items {
		if strings.TrimSpace(it.Description) == "" {
			continue
		}
		qty := parseFloat(it.Quantity, 1)
		price := euroToCents(it.UnitPrice)
		pct := parseFloat(it.Discount, 0)
		line := int64(math.Round(qty * float64(price) * (1 - pct/100)))
		if _, err := tx.Exec(`INSERT INTO invoice_items
			(invoice_id, description, unit, quantity, unit_price_cents, discount_pct, line_total_cents)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, invoiceID, it.Description, it.Unit, qty, price, pct, line); err != nil {
			return err
		}
	}
	return nil
}

func createInvoice(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		f := parseInvoiceForm(r)
		issued, customerID, errs := validateInvoice(f, langOf(r))
		if len(errs) > 0 {
			f.Errors = errs
			w.WriteHeader(http.StatusUnprocessableEntity)
			renderNew(w, r, db, f)
			return
		}

		tx, err := db.Begin()
		if err != nil {
			httpErr(w, err)
			return
		}
		defer tx.Rollback()

		// Sequential number per calendar year (resets Jan 1), format "seq/month/year"
		// to match the example spreadsheet. ponytail: count-based seq is fine at this
		// scale; the UNIQUE constraint on number catches any race, and single-writer
		// SQLite serializes writes anyway.
		var seq int
		if err := tx.QueryRow(
			`SELECT COUNT(*) + 1 FROM invoices WHERE substr(issue_date, 1, 4) = ?`,
			strconv.Itoa(issued.Year())).Scan(&seq); err != nil {
			httpErr(w, err)
			return
		}
		number := fmt.Sprintf("%d/%d/%d", seq, int(issued.Month()), issued.Year())

		res, err := tx.Exec(`INSERT INTO invoices
			(number, issue_date, customer_id, note, issue_time, delivery_date, due_date, payment_method, poziv)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			number, f.IssueDate, customerID, f.Note, f.IssueTime, f.DeliveryDate, f.DueDate, f.PaymentMethod, f.Poziv)
		if err != nil {
			httpErr(w, err)
			return
		}
		invID, _ := res.LastInsertId()

		if err := insertItems(tx, invID, f.Items); err != nil {
			httpErr(w, err)
			return
		}
		if err := recordRevision(tx, invID, "created"); err != nil {
			httpErr(w, err)
			return
		}
		if err := tx.Commit(); err != nil {
			httpErr(w, err)
			return
		}
		http.Redirect(w, r, "/invoices/"+strconv.FormatInt(invID, 10), http.StatusSeeOther)
	}
}

// editInvoice opens the form prefilled from an existing (non-deleted) invoice,
// keeping its id and immutable number so saving updates it in place.
func editInvoice(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		f := invoiceForm{ID: id}
		err = db.QueryRow(`SELECT number, issue_date, customer_id, note,
			issue_time, delivery_date, due_date, payment_method, poziv
			FROM invoices WHERE id = ? AND deleted_at IS NULL`, id).
			Scan(&f.Number, &f.IssueDate, &f.CustomerID, &f.Note,
				&f.IssueTime, &f.DeliveryDate, &f.DueDate, &f.PaymentMethod, &f.Poziv)
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		} else if err != nil {
			httpErr(w, err)
			return
		}
		rows, err := db.Query(`SELECT description, unit, quantity, unit_price_cents, discount_pct
			FROM invoice_items WHERE invoice_id = ? ORDER BY id`, id)
		if err != nil {
			httpErr(w, err)
			return
		}
		defer rows.Close()
		f.Items = scanItemInputs(rows, &err)
		if err != nil {
			httpErr(w, err)
			return
		}
		renderNew(w, r, db, f)
	}
}

// updateInvoice saves edits to an existing invoice and records a revision. The
// number is never regenerated: an issued invoice number is immutable by law, so
// only the customer, date, note, and items change.
func updateInvoice(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		f := parseInvoiceForm(r)
		f.ID = id
		// number is needed to redisplay the edit form on a validation error
		if err := db.QueryRow(`SELECT number FROM invoices WHERE id = ? AND deleted_at IS NULL`, id).
			Scan(&f.Number); errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		} else if err != nil {
			httpErr(w, err)
			return
		}
		_, customerID, errs := validateInvoice(f, langOf(r))
		if len(errs) > 0 {
			f.Errors = errs
			w.WriteHeader(http.StatusUnprocessableEntity)
			renderNew(w, r, db, f)
			return
		}

		tx, err := db.Begin()
		if err != nil {
			httpErr(w, err)
			return
		}
		defer tx.Rollback()

		if _, err := tx.Exec(`UPDATE invoices SET issue_date = ?, customer_id = ?, note = ?,
			issue_time = ?, delivery_date = ?, due_date = ?, payment_method = ?, poziv = ? WHERE id = ?`,
			f.IssueDate, customerID, f.Note, f.IssueTime, f.DeliveryDate, f.DueDate, f.PaymentMethod, f.Poziv, id); err != nil {
			httpErr(w, err)
			return
		}
		if _, err := tx.Exec(`DELETE FROM invoice_items WHERE invoice_id = ?`, id); err != nil {
			httpErr(w, err)
			return
		}
		if err := insertItems(tx, id, f.Items); err != nil {
			httpErr(w, err)
			return
		}
		if err := recordRevision(tx, id, "edited"); err != nil {
			httpErr(w, err)
			return
		}
		if err := tx.Commit(); err != nil {
			httpErr(w, err)
			return
		}
		http.Redirect(w, r, "/invoices/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
	}
}

// deleteInvoice soft-deletes: it records a final revision then stamps deleted_at,
// so the invoice drops out of the list but its history (and view) remain.
func deleteInvoice(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		tx, err := db.Begin()
		if err != nil {
			httpErr(w, err)
			return
		}
		defer tx.Rollback()

		var exists int
		if err := tx.QueryRow(`SELECT 1 FROM invoices WHERE id = ? AND deleted_at IS NULL`, id).
			Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		} else if err != nil {
			httpErr(w, err)
			return
		}
		if err := recordRevision(tx, id, "deleted"); err != nil {
			httpErr(w, err)
			return
		}
		if _, err := tx.Exec(`UPDATE invoices SET deleted_at = datetime('now') WHERE id = ?`, id); err != nil {
			httpErr(w, err)
			return
		}
		if err := tx.Commit(); err != nil {
			httpErr(w, err)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// revSnap is the JSON shape stored per revision: a self-contained snapshot of the
// invoice (including the computed totals and VAT status) so the audit log never
// depends on current data.
type revSnap struct {
	IssueDate string `json:"issue_date"`
	Customer  string `json:"customer"`
	Note      string `json:"note"`
	Items     []Item `json:"items"`
	Totals    Totals `json:"totals"`
	VatExempt bool   `json:"vat_exempt"`
}

// Revision is a decoded history entry for rendering.
type Revision struct {
	Action    string
	ChangedAt string
	Snap      revSnap
}

// recordRevision snapshots the invoice's current state (within the caller's tx)
// and appends it to invoice_revisions with the given action.
func recordRevision(tx *sql.Tx, invoiceID int64, action string) error {
	var snap revSnap
	var vatExempt int
	if err := tx.QueryRow(`SELECT i.issue_date, c.name, i.note, co.vat_exempt
		FROM invoices i JOIN customers c ON c.id = i.customer_id, company co
		WHERE i.id = ? AND co.id = 1`, invoiceID).
		Scan(&snap.IssueDate, &snap.Customer, &snap.Note, &vatExempt); err != nil {
		return err
	}
	snap.VatExempt = vatExempt != 0
	rows, err := tx.Query(`SELECT description, unit, quantity, unit_price_cents, discount_pct, line_total_cents
		FROM invoice_items WHERE invoice_id = ? ORDER BY id`, invoiceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.Description, &it.Unit, &it.Quantity, &it.UnitPriceCents, &it.DiscountPct, &it.LineTotalCents); err != nil {
			return err
		}
		snap.Items = append(snap.Items, it)
	}
	snap.Totals = computeTotals(snap.Items, snap.VatExempt)
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO invoice_revisions (invoice_id, action, snapshot) VALUES (?, ?, ?)`,
		invoiceID, action, string(b))
	return err
}

// invoiceHistory renders the audit trail for an invoice (deleted ones included).
func invoiceHistory(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		var number string
		if err := db.QueryRow(`SELECT number FROM invoices WHERE id = ?`, id).Scan(&number); errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		} else if err != nil {
			httpErr(w, err)
			return
		}
		rows, err := db.Query(`SELECT action, changed_at, snapshot
			FROM invoice_revisions WHERE invoice_id = ? ORDER BY id`, id)
		if err != nil {
			httpErr(w, err)
			return
		}
		defer rows.Close()
		var revs []Revision
		for rows.Next() {
			var rev Revision
			var snap string
			if err := rows.Scan(&rev.Action, &rev.ChangedAt, &snap); err != nil {
				httpErr(w, err)
				return
			}
			if err := json.Unmarshal([]byte(snap), &rev.Snap); err != nil {
				httpErr(w, err)
				return
			}
			revs = append(revs, rev)
		}
		render(w, r, "history.html", map[string]any{"ID": id, "Number": number, "Revisions": revs})
	}
}

// importResult summarizes one import run for display.
type importResult struct {
	Company   bool
	Invoices  int
	Customers int
	Err       string
}

func importPage(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		render(w, r, "import.html", map[string]any{})
	}
}

// runImport reads company data from an uploaded invoice or Plavi ured workbook
// (xlsx/xlsm/ods/pdf, Croatian or English) and, for Plavi ured workbooks, also
// imports the invoices and customers from the BAZA sheet.
func runImport(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lang := langOf(r)
		res := &importResult{}
		fail := func(key string) {
			res.Err = tr(lang, key)
			render(w, r, "import.html", map[string]any{"Result": res})
		}
		file, hdr, err := r.FormFile("file")
		if err != nil {
			fail("import_no_file")
			return
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			httpErr(w, err)
			return
		}
		lines, err := readDocLines(hdr.Filename, data)
		if err != nil {
			fail("import_error")
			return
		}
		// Company data (best-effort): a Plavi ured PODACI sheet or an invoice header.
		fields := extractCompany(lines)
		if fields["name"] != "" && (fields["iban"] != "" || fields["owner"] != "" || fields["oib"] != "") {
			if err := saveCompanyFields(db, fields); err != nil {
				httpErr(w, err)
				return
			}
			res.Company = true
		}
		// Invoices + customers from the BAZA sheet (Plavi ured xlsx/xlsm only).
		switch strings.ToLower(filepath.Ext(hdr.Filename)) {
		case ".xlsx", ".xlsm":
			res.Invoices, res.Customers, err = importBaza(db, data)
			if err != nil {
				httpErr(w, err)
				return
			}
		}
		if !res.Company && res.Invoices == 0 {
			fail("import_no_company")
			return
		}
		render(w, r, "import.html", map[string]any{"Result": res})
	}
}

// importBaza imports invoices (and the customers they reference) from a Plavi
// ured workbook's BAZA sheet. It is idempotent: rows whose invoice number
// already exists are skipped, so re-importing the same file is safe. Returns the
// number of invoices and new customers created.
func importBaza(db *sql.DB, data []byte) (int, int, error) {
	xl, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return 0, 0, err
	}
	defer xl.Close()
	hasBaza := false
	for _, s := range xl.GetSheetList() {
		if strings.EqualFold(s, "BAZA") {
			hasBaza = true
		}
	}
	if !hasBaza {
		return 0, 0, nil
	}
	// RawCellValue so dates arrive as Excel serial numbers (deterministic) rather
	// than the sheet's locale-formatted rendering.
	rows, err := xl.GetRows("BAZA", excelize.Options{RawCellValue: true})
	if err != nil {
		return 0, 0, err
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	invoices, customers := 0, 0
	for i, row := range rows {
		if i == 0 {
			continue // header
		}
		cell := func(idx int) string {
			if idx < len(row) {
				return strings.TrimSpace(row[idx])
			}
			return ""
		}
		number, name := cell(0), cell(1)
		issue := serialToDate(cell(5))
		// skip the "primjer" example, the legend row, blank template rows, and any
		// row missing the essentials (customer + a valid issue date).
		if number == "" || strings.EqualFold(number, "primjer") || name == "" || issue == "" {
			continue
		}
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM invoices WHERE number = ?`, number).Scan(&exists); err != nil {
			return invoices, customers, err
		}
		if exists > 0 {
			continue
		}
		// up to 5 line-item groups: (description, JM, qty, cijena, rabat, iznos)
		var items []itemInput
		for g := 0; g < 5; g++ {
			b := 9 + g*6
			if cell(b) == "" {
				continue
			}
			items = append(items, itemInput{
				Description: cell(b),
				Unit:        cell(b + 1),
				Quantity:    cell(b + 2),
				UnitPrice:   cell(b + 3),
				Discount:    cell(b + 4),
			})
		}
		if len(items) == 0 {
			continue
		}
		custID, created, err := upsertCustomer(tx, name, cell(4), joinAddr(cell(2), cell(3)))
		if err != nil {
			return invoices, customers, err
		}
		if created {
			customers++
		}
		res, err := tx.Exec(`INSERT INTO invoices
			(number, issue_date, customer_id, note, issue_time, delivery_date, due_date, payment_method, poziv,
			 paid_date, paid_cash_cents, paid_bank_cents, payment_ref)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			number, issue, custID, cell(40), serialToTime(cell(7)), serialToDate(cell(6)), serialToDate(cell(8)), "", "",
			serialToDate(cell(45)), euroToCents(cell(42)), euroToCents(cell(43)), cell(44))
		if err != nil {
			return invoices, customers, err
		}
		invID, _ := res.LastInsertId()
		if err := insertItems(tx, invID, items); err != nil {
			return invoices, customers, err
		}
		if err := recordRevision(tx, invID, "created"); err != nil {
			return invoices, customers, err
		}
		invoices++
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return invoices, customers, nil
}

// upsertCustomer finds a customer by OIB (or by name when no OIB) and inserts one
// if none matches. Returns the id and whether a new row was created.
func upsertCustomer(tx *sql.Tx, name, oib, address string) (int64, bool, error) {
	var id int64
	var err error
	if oib != "" {
		err = tx.QueryRow(`SELECT id FROM customers WHERE oib = ?`, oib).Scan(&id)
	} else {
		err = tx.QueryRow(`SELECT id FROM customers WHERE name = ?`, name).Scan(&id)
	}
	if err == nil {
		return id, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	res, err := tx.Exec(`INSERT INTO customers (name, oib, address) VALUES (?, ?, ?)`, name, oib, address)
	if err != nil {
		return 0, false, err
	}
	id, _ = res.LastInsertId()
	return id, true, nil
}

// serialToDate converts an Excel date serial ("45658") to an ISO date; "" if the
// value is blank or not a serial.
func serialToDate(s string) string {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f <= 0 {
		return ""
	}
	t, err := excelize.ExcelDateToTime(f, false)
	if err != nil {
		return ""
	}
	return t.Format("2006-01-02")
}

// serialToTime converts an Excel time serial (a day fraction like 0.5) to HH:MM.
func serialToTime(s string) string {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f <= 0 {
		return ""
	}
	t, err := excelize.ExcelDateToTime(f, false)
	if err != nil {
		return ""
	}
	return t.Format("15:04")
}

// joinAddr combines the BAZA street and city columns into one address line.
func joinAddr(street, city string) string {
	street, city = strings.TrimSpace(street), strings.TrimSpace(city)
	switch {
	case street == "":
		return city
	case city == "":
		return street
	default:
		return street + ", " + city
	}
}

// readDocLines returns ordered, non-empty text lines from an uploaded document,
// dispatching on file extension. All three formats reduce to the same shape (an
// invoice's issuer header, or the PODACI key/value sheet), so one extractor
// (extractCompany) handles them uniformly.
func readDocLines(name string, data []byte) ([]string, error) {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".xlsx", ".xlsm":
		return xlsxLines(data)
	case ".ods":
		return odsLines(data)
	case ".pdf":
		return pdfLines(data)
	}
	return nil, fmt.Errorf("unsupported file type: %s", name)
}

// xlsxLines flattens one sheet into ordered cell text: the PODACI sheet if the
// workbook has one (Plavi ured), otherwise the first sheet (a plain invoice).
func xlsxLines(data []byte) ([]string, error) {
	xl, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer xl.Close()
	sheets := xl.GetSheetList()
	if len(sheets) == 0 {
		return nil, nil
	}
	target := sheets[0]
	for _, s := range sheets {
		if strings.EqualFold(s, "PODACI") {
			target = s
		}
	}
	rows, err := xl.GetRows(target)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, row := range rows {
		for _, c := range row {
			if s := strings.TrimSpace(c); s != "" {
				lines = append(lines, s)
			}
		}
	}
	return lines, nil
}

// odsLines extracts each text paragraph from an OpenDocument spreadsheet's
// content.xml in document order (excelize cannot read ods).
func odsLines(data []byte) ([]string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	var rc io.ReadCloser
	for _, f := range zr.File {
		if f.Name == "content.xml" {
			if rc, err = f.Open(); err != nil {
				return nil, err
			}
			break
		}
	}
	if rc == nil {
		return nil, fmt.Errorf("no content.xml in ods")
	}
	defer rc.Close()
	dec := xml.NewDecoder(rc)
	var lines []string
	var buf strings.Builder
	depth := 0 // inside a <text:p> paragraph
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "p" {
				depth++
			}
		case xml.CharData:
			if depth > 0 {
				buf.Write(t)
			}
		case xml.EndElement:
			if t.Name.Local == "p" {
				depth--
				if depth == 0 {
					if s := strings.TrimSpace(buf.String()); s != "" {
						lines = append(lines, s)
					}
					buf.Reset()
				}
			}
		}
	}
	return lines, nil
}

// pdfLines extracts text lines from a PDF by bucketing characters on their Y
// position (GetTextByRow collapses this invoice to a single row), then ordering
// each line left-to-right and inserting spaces on horizontal gaps.
func pdfLines(data []byte) ([]string, error) {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	var lines []string
	for i := 1; i <= r.NumPage(); i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		byY := map[int][]pdf.Text{}
		for _, t := range p.Content().Text {
			y := int(math.Round(t.Y))
			byY[y] = append(byY[y], t)
		}
		ys := make([]int, 0, len(byY))
		for y := range byY {
			ys = append(ys, y)
		}
		sort.Sort(sort.Reverse(sort.IntSlice(ys))) // top of page first
		for _, y := range ys {
			ts := byY[y]
			sort.Slice(ts, func(a, b int) bool { return ts[a].X < ts[b].X })
			var b strings.Builder
			var lastX float64
			for k, t := range ts {
				if k > 0 && t.X-lastX > 1.5 {
					b.WriteByte(' ')
				}
				b.WriteString(t.S)
				lastX = t.X + t.W
			}
			if s := strings.TrimSpace(b.String()); s != "" {
				lines = append(lines, s)
			}
		}
	}
	return lines, nil
}

var (
	ibanRe = regexp.MustCompile(`[A-Z]{2}\d{2}[A-Z0-9]{2}[A-Z0-9 ]{6,26}`)
	oibRe  = regexp.MustCompile(`\b\d{11}\b`)
)

// extractCompany maps ordered document lines onto company columns. It matches
// bilingual labels (Croatian PODACI sheet plus Croatian/English invoice
// headers), falls back to positional issuer-header parsing (name / "Vl." owner /
// address / place), and pattern-matches IBAN and OIB.
func extractCompany(lines []string) map[string]string {
	f := map[string]string{}
	set := func(k, v string) {
		if v != "" && f[k] == "" {
			f[k] = v
		}
	}
	for i, line := range lines {
		label, val := line, ""
		if idx := strings.Index(line, ":"); idx >= 0 {
			label, val = line[:idx], strings.TrimSpace(line[idx+1:])
		}
		if val == "" && i+1 < len(lines) {
			val = strings.TrimSpace(lines[i+1])
		}
		switch low := strings.ToLower(strings.TrimSpace(label)); {
		case strings.HasPrefix(low, "naziv obrta"):
			set("name", val)
		case strings.HasPrefix(low, "adresa obavljanja"):
			set("address", val)
		case strings.HasPrefix(low, "adresa vlasnika"):
			set("owner_address", val)
		case strings.HasPrefix(low, "ime i prezime vlasnika") || low == "owner":
			set("owner", val)
		case low == "oib":
			set("oib", val)
		case low == "iban":
			set("iban", collapseSpaces(val))
		case low == "bank" || strings.HasPrefix(low, "banka"):
			// ods packs both into one cell: "Banka, swift banke:" -> "name, SWIFT".
			parts := strings.SplitN(val, ",", 2)
			set("bank", strings.TrimSpace(parts[0]))
			if len(parts) == 2 {
				set("swift", strings.TrimSpace(parts[1]))
			}
		case strings.Contains(low, "swift"):
			set("swift", val)
		}
	}
	// Invoice-header fallback: the issuer block has no labels.
	if len(lines) > 0 {
		set("name", lines[0])
	}
	for i, line := range lines {
		ll := strings.ToLower(line)
		if strings.HasPrefix(ll, "vl.") || strings.HasPrefix(ll, "vl ") {
			set("owner", strings.TrimSpace(line[3:]))
			if i+1 < len(lines) && !isLabelLine(lines[i+1]) {
				set("address", lines[i+1])
			}
			if i+2 < len(lines) && !isLabelLine(lines[i+2]) {
				set("place", lines[i+2])
			}
			break
		}
	}
	if f["iban"] == "" {
		if m := ibanRe.FindString(strings.Join(lines, "\n")); m != "" {
			f["iban"] = collapseSpaces(m)
		}
	}
	if f["oib"] == "" {
		if m := oibRe.FindString(strings.Join(lines, "\n")); m != "" {
			f["oib"] = m
		}
	}
	return f
}

func isLabelLine(s string) bool { return strings.Contains(s, ":") || ibanRe.MatchString(s) }

func collapseSpaces(s string) string { return strings.Join(strings.Fields(s), " ") }

// saveCompanyFields updates the single company row. Column names are a fixed
// whitelist, never file-derived, so the dynamic UPDATE is injection-safe.
func saveCompanyFields(db *sql.DB, fields map[string]string) error {
	allowed := map[string]bool{
		"name": true, "owner": true, "oib": true, "address": true,
		"owner_address": true, "place": true, "iban": true, "bank": true, "swift": true,
	}
	var sets []string
	var args []any
	for col, val := range fields {
		if !allowed[col] {
			continue
		}
		sets = append(sets, col+" = ?")
		args = append(args, val)
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, 1)
	_, err := db.Exec("UPDATE company SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	return err
}

func viewInvoice(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		var v Invoice
		var deletedAt sql.NullString
		err = db.QueryRow(`SELECT i.id, i.number, i.issue_date, i.issue_time, i.delivery_date,
			i.due_date, i.payment_method, i.poziv, i.paid_date, i.paid_cash_cents, i.paid_bank_cents,
			i.payment_ref, c.name, c.address, c.oib, i.note, i.deleted_at
			FROM invoices i JOIN customers c ON c.id = i.customer_id WHERE i.id = ?`, id).
			Scan(&v.ID, &v.Number, &v.IssueDate, &v.IssueTime, &v.DeliveryDate, &v.DueDate,
				&v.PaymentMethod, &v.Poziv, &v.PaidDate, &v.PaidCashCents, &v.PaidBankCents,
				&v.PaymentRef, &v.Customer, &v.CustomerAddress, &v.CustomerOIB, &v.Note, &deletedAt)
		v.Deleted = deletedAt.Valid
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		} else if err != nil {
			httpErr(w, err)
			return
		}
		company, err := loadCompany(db)
		if err != nil {
			httpErr(w, err)
			return
		}
		rows, err := db.Query(`SELECT description, unit, quantity, unit_price_cents, discount_pct, line_total_cents
			FROM invoice_items WHERE invoice_id = ? ORDER BY id`, id)
		if err != nil {
			httpErr(w, err)
			return
		}
		defer rows.Close()
		var items []Item
		for rows.Next() {
			var it Item
			if err := rows.Scan(&it.Description, &it.Unit, &it.Quantity, &it.UnitPriceCents, &it.DiscountPct, &it.LineTotalCents); err != nil {
				httpErr(w, err)
				return
			}
			items = append(items, it)
		}
		totals := computeTotals(items, company.VatExempt)
		v.TotalCents = totals.TotalCents
		render(w, r, "view.html", map[string]any{
			"Invoice":     v,
			"Items":       items,
			"Company":     company,
			"Totals":      totals,
			"Today":       time.Now().Format("2006-01-02"),
			"PaidDefault": fmt.Sprintf("%.2f", float64(totals.TotalCents)/100),
			"PayErr":      r.URL.Query().Get("payerr") != "",
		})
	}
}

// recordPayment stores a collection (naplata) against an invoice: amount, date,
// and method (gotovina -> cash column, virmanski -> bank column).
func recordPayment(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		date := r.FormValue("paid_date")
		method := r.FormValue("method")
		amount := euroToCents(r.FormValue("amount"))
		dest := "/invoices/" + strconv.FormatInt(id, 10)
		if _, e := time.Parse("2006-01-02", date); e != nil || amount <= 0 || (method != "gotovina" && method != "virmanski") {
			http.Redirect(w, r, dest+"?payerr=1", http.StatusSeeOther)
			return
		}
		var cash, bank int64
		if method == "gotovina" {
			cash = amount
		} else {
			bank = amount
		}
		if _, err := db.Exec(`UPDATE invoices SET paid_date = ?, paid_cash_cents = ?, paid_bank_cents = ?, payment_ref = ?
			WHERE id = ? AND deleted_at IS NULL`,
			date, cash, bank, strings.TrimSpace(r.FormValue("payment_ref")), id); err != nil {
			httpErr(w, err)
			return
		}
		http.Redirect(w, r, dest, http.StatusSeeOther)
	}
}

// clearPayment removes a recorded collection, returning the invoice to unpaid.
func clearPayment(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if _, err := db.Exec(`UPDATE invoices SET paid_date = '', paid_cash_cents = 0, paid_bank_cents = 0, payment_ref = '' WHERE id = ?`, id); err != nil {
			httpErr(w, err)
			return
		}
		http.Redirect(w, r, "/invoices/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
	}
}

// kprRow is one line of the Knjiga prometa (turnover ledger).
type kprRow struct {
	No         int
	Date       string // paid_date (ISO)
	Ref        string // broj temeljnice (broj izvoda/uplatnice)
	Number     string // broj računa
	CashCents  int64
	BankCents  int64
	TotalCents int64
}

// kprPage renders the Knjiga prometa for one year: every collected payment as a
// ledger row (ordered by collection date) plus cash/bank/total sums. The book is
// per calendar year, matching the Plavi ured KPR form.
func kprPage(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		company, err := loadCompany(db)
		if err != nil {
			httpErr(w, err)
			return
		}
		// years that actually have collected payments, newest first
		yrows, err := db.Query(`SELECT DISTINCT substr(paid_date, 1, 4) y
			FROM invoices WHERE paid_date != '' AND deleted_at IS NULL ORDER BY y DESC`)
		if err != nil {
			httpErr(w, err)
			return
		}
		defer yrows.Close()
		var years []string
		for yrows.Next() {
			var y string
			if err := yrows.Scan(&y); err != nil {
				httpErr(w, err)
				return
			}
			years = append(years, y)
		}
		year := r.URL.Query().Get("year")
		if year == "" {
			if len(years) > 0 {
				year = years[0]
			} else {
				year = time.Now().Format("2006")
			}
		}
		rows, err := db.Query(`SELECT number, paid_date, payment_ref, paid_cash_cents, paid_bank_cents
			FROM invoices
			WHERE paid_date != '' AND deleted_at IS NULL AND substr(paid_date, 1, 4) = ?
			ORDER BY paid_date, id`, year)
		if err != nil {
			httpErr(w, err)
			return
		}
		defer rows.Close()
		var list []kprRow
		var cashTotal, bankTotal int64
		for rows.Next() {
			var k kprRow
			if err := rows.Scan(&k.Number, &k.Date, &k.Ref, &k.CashCents, &k.BankCents); err != nil {
				httpErr(w, err)
				return
			}
			k.No = len(list) + 1
			k.TotalCents = k.CashCents + k.BankCents
			cashTotal += k.CashCents
			bankTotal += k.BankCents
			list = append(list, k)
		}
		render(w, r, "kpr.html", map[string]any{
			"Company":    company,
			"Rows":       list,
			"Year":       year,
			"Years":      years,
			"CashTotal":  cashTotal,
			"BankTotal":  bankTotal,
			"GrandTotal": cashTotal + bankTotal,
		})
	}
}

func loadCompany(db *sql.DB) (Company, error) {
	var c Company
	var vatExempt int
	err := db.QueryRow(`SELECT name, owner, oib, address, place, iban, bank, swift, owner_address, vat_exempt FROM company WHERE id = 1`).
		Scan(&c.Name, &c.Owner, &c.OIB, &c.Address, &c.Place, &c.IBAN, &c.Bank, &c.Swift, &c.OwnerAddress, &vatExempt)
	c.VatExempt = vatExempt != 0
	return c, err
}

// settingsPage renders the company (obrt) master-data form. ?saved=1 shows a
// confirmation after a successful save (post/redirect/get).
func settingsPage(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := loadCompany(db)
		if err != nil {
			httpErr(w, err)
			return
		}
		render(w, r, "settings.html", map[string]any{
			"Company": c,
			"Errors":  map[string]string{},
			"Saved":   r.URL.Query().Get("saved") != "",
		})
	}
}

// saveSettings validates and updates the single company row.
func saveSettings(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		c := Company{
			Name:         strings.TrimSpace(r.FormValue("name")),
			Owner:        strings.TrimSpace(r.FormValue("owner")),
			OIB:          strings.TrimSpace(r.FormValue("oib")),
			Address:      strings.TrimSpace(r.FormValue("address")),
			OwnerAddress: strings.TrimSpace(r.FormValue("owner_address")),
			Place:        strings.TrimSpace(r.FormValue("place")),
			IBAN:         strings.TrimSpace(r.FormValue("iban")),
			Bank:         strings.TrimSpace(r.FormValue("bank")),
			Swift:        strings.TrimSpace(r.FormValue("swift")),
			VatExempt:    r.FormValue("vat_exempt") != "",
		}
		lang := langOf(r)
		errs := map[string]string{}
		// OIB is optional (foreign sole traders and some obrti have none).
		for field, val := range map[string]string{"name": c.Name, "owner": c.Owner} {
			if val == "" {
				errs[field] = tr(lang, "err_required")
			}
		}
		if len(errs) > 0 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			render(w, r, "settings.html", map[string]any{"Company": c, "Errors": errs})
			return
		}
		vatExempt := 0
		if c.VatExempt {
			vatExempt = 1
		}
		if _, err := db.Exec(`UPDATE company SET name=?, owner=?, oib=?, address=?, owner_address=?, place=?, iban=?, bank=?, swift=?, vat_exempt=? WHERE id = 1`,
			c.Name, c.Owner, c.OIB, c.Address, c.OwnerAddress, c.Place, c.IBAN, c.Bank, c.Swift, vatExempt); err != nil {
			httpErr(w, err)
			return
		}
		http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
	}
}

func allCustomers(db *sql.DB) ([]Customer, error) {
	rows, err := db.Query("SELECT id, name, oib, address FROM customers ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cs []Customer
	for rows.Next() {
		var c Customer
		if err := rows.Scan(&c.ID, &c.Name, &c.OIB, &c.Address); err != nil {
			return nil, err
		}
		cs = append(cs, c)
	}
	return cs, nil
}

// hrDate formats a stored ISO date ("2006-01-02") in the Croatian numeric style
// ("05.07.2026."). Anything unparseable is returned as-is.
func hrDate(iso string) string {
	t, err := time.Parse("2006-01-02", iso)
	if err != nil {
		return iso
	}
	return t.Format("02.01.2006.")
}

// hrDateTime formats a stored UTC timestamp ("2006-01-02 15:04:05", from
// datetime('now')) as Croatian date + time. This is the no-JS fallback; the
// browser converts data-utc attributes to the viewer's local timezone.
func hrDateTime(iso string) string {
	t, err := time.Parse("2006-01-02 15:04:05", iso)
	if err != nil {
		return hrDate(iso)
	}
	return t.Format("02.01.2006. 15:04")
}

// isoUTC turns a stored SQLite UTC timestamp into an ISO-8601 instant with a Z
// suffix, so the browser parses it as UTC and can localise it.
func isoUTC(iso string) string {
	t, err := time.Parse("2006-01-02 15:04:05", iso)
	if err != nil {
		return ""
	}
	return t.Format("2006-01-02T15:04:05Z")
}

// euroToCents parses "12.34" / "12,34" into integer cents.
func euroToCents(s string) int64 {
	f := parseFloat(s, 0)
	return int64(math.Round(f * 100))
}

func parseFloat(s string, def float64) float64 {
	if s == "" {
		return def
	}
	// accept comma decimals
	for i := range s {
		if s[i] == ',' {
			s = s[:i] + "." + s[i+1:]
			break
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return def
	}
	return f
}

func at(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return ""
}

func render(w http.ResponseWriter, r *http.Request, name string, data any) {
	t := tmpls[langOf(r)]
	if t == nil {
		t = tmpls[defaultLang]
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		log.Print(err)
	}
}

func httpErr(w http.ResponseWriter, err error) {
	log.Print(err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
