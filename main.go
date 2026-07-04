package main

import (
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

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
			"eur":  eur,
			"dict": dict,
			"T":    func(key string) string { return tr(l, key) },
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
	Customer        string
	CustomerAddress string
	CustomerOIB     string
	Note            string
	TotalCents      int64
	Deleted         bool
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
	ID   int64
	Name string
}

type Company struct {
	Name         string
	Owner        string
	OIB          string
	Address      string
	Place        string
	IBAN         string
	Bank         string
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
	mux.HandleFunc("GET /invoices/{id}/history", invoiceHistory(conn))
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
		rows, err := db.Query(`
			SELECT i.id, i.number, i.issue_date, c.name, i.note,
			       COALESCE(SUM(it.line_total_cents), 0)
			FROM invoices i
			JOIN customers c ON c.id = i.customer_id
			LEFT JOIN invoice_items it ON it.invoice_id = i.id
			WHERE i.deleted_at IS NULL
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
			if err := rows.Scan(&v.ID, &v.Number, &v.IssueDate, &v.Customer, &v.Note, &v.TotalCents); err != nil {
				httpErr(w, err)
				return
			}
			if vatExempt == 0 {
				v.TotalCents += int64(math.Round(float64(v.TotalCents) * vatRate))
			}
			invs = append(invs, v)
		}
		render(w, r, "list.html", invs)
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
	ID         int64
	Number     string
	IssueDate  string
	CustomerID string
	Note       string
	Items      []itemInput
	Errors     map[string]string
	Customers  []Customer
	Catalog    catalogs
	Articles   []Article
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
		IssueDate:  r.FormValue("issue_date"),
		CustomerID: r.FormValue("customer_id"),
		Note:       r.FormValue("note"),
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

		res, err := tx.Exec(`INSERT INTO invoices (number, issue_date, customer_id, note) VALUES (?, ?, ?, ?)`,
			number, f.IssueDate, customerID, f.Note)
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
		err = db.QueryRow(`SELECT number, issue_date, customer_id, note
			FROM invoices WHERE id = ? AND deleted_at IS NULL`, id).
			Scan(&f.Number, &f.IssueDate, &f.CustomerID, &f.Note)
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

		if _, err := tx.Exec(`UPDATE invoices SET issue_date = ?, customer_id = ?, note = ? WHERE id = ?`,
			f.IssueDate, customerID, f.Note, id); err != nil {
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
	Company bool
	Err     string
}

func importPage(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		render(w, r, "import.html", map[string]any{})
	}
}

// runImport reads company data from the PODACI sheet of a Plavi ured workbook.
// ponytail: invoice/customer import (BAZA sheet) will land with Knjiga prometa.
func runImport(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lang := langOf(r)
		res := &importResult{}
		file, _, err := r.FormFile("file")
		if err != nil {
			res.Err = tr(lang, "import_no_file")
			render(w, r, "import.html", map[string]any{"Result": res})
			return
		}
		defer file.Close()
		xl, err := excelize.OpenReader(file)
		if err != nil {
			res.Err = tr(lang, "import_error")
			render(w, r, "import.html", map[string]any{"Result": res})
			return
		}
		defer xl.Close()

		tx, err := db.Begin()
		if err != nil {
			httpErr(w, err)
			return
		}
		defer tx.Rollback()

		importCompany(tx, xl, res)
		if err := tx.Commit(); err != nil {
			httpErr(w, err)
			return
		}
		if !res.Company && res.Err == "" {
			res.Err = tr(lang, "import_no_company")
		}
		render(w, r, "import.html", map[string]any{"Result": res})
	}
}

// importCompany maps the PODACI key/value rows onto the (single) company row.
// Column names come from a fixed whitelist, never from the sheet, so the dynamic
// UPDATE is injection-safe.
func importCompany(tx *sql.Tx, xl *excelize.File, res *importResult) {
	rows, err := xl.GetRows("PODACI")
	if err != nil {
		return
	}
	fields := map[string]string{}
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		label := strings.ToLower(strings.TrimRight(strings.TrimSpace(row[0]), ":"))
		val := strings.TrimSpace(row[1])
		if val == "" {
			continue
		}
		switch {
		case strings.HasPrefix(label, "naziv obrta"):
			fields["name"] = val
		case strings.HasPrefix(label, "adresa obavljanja"):
			fields["address"] = val
		case strings.HasPrefix(label, "ime i prezime vlasnika"):
			fields["owner"] = val
		case strings.HasPrefix(label, "adresa vlasnika"):
			fields["owner_address"] = val
		case label == "oib":
			fields["oib"] = val
		case label == "iban":
			fields["iban"] = val
		case strings.HasPrefix(label, "banka"):
			fields["bank"] = val
		}
	}
	if len(fields) == 0 {
		return
	}
	var sets []string
	var args []any
	for col, val := range fields {
		sets = append(sets, col+" = ?")
		args = append(args, val)
	}
	args = append(args, 1)
	if _, err := tx.Exec("UPDATE company SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err == nil {
		res.Company = true
	}
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
		err = db.QueryRow(`SELECT i.id, i.number, i.issue_date, c.name, c.address, c.oib, i.note, i.deleted_at
			FROM invoices i JOIN customers c ON c.id = i.customer_id WHERE i.id = ?`, id).
			Scan(&v.ID, &v.Number, &v.IssueDate, &v.Customer, &v.CustomerAddress, &v.CustomerOIB, &v.Note, &deletedAt)
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
		render(w, r, "view.html", map[string]any{"Invoice": v, "Items": items, "Company": company, "Totals": totals})
	}
}

func loadCompany(db *sql.DB) (Company, error) {
	var c Company
	var vatExempt int
	err := db.QueryRow(`SELECT name, owner, oib, address, place, iban, bank, owner_address, vat_exempt FROM company WHERE id = 1`).
		Scan(&c.Name, &c.Owner, &c.OIB, &c.Address, &c.Place, &c.IBAN, &c.Bank, &c.OwnerAddress, &vatExempt)
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
			VatExempt:    r.FormValue("vat_exempt") != "",
		}
		lang := langOf(r)
		errs := map[string]string{}
		for field, val := range map[string]string{"name": c.Name, "owner": c.Owner, "oib": c.OIB} {
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
		if _, err := db.Exec(`UPDATE company SET name=?, owner=?, oib=?, address=?, owner_address=?, place=?, iban=?, bank=?, vat_exempt=? WHERE id = 1`,
			c.Name, c.Owner, c.OIB, c.Address, c.OwnerAddress, c.Place, c.IBAN, c.Bank, vatExempt); err != nil {
			httpErr(w, err)
			return
		}
		http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
	}
}

func allCustomers(db *sql.DB) ([]Customer, error) {
	rows, err := db.Query("SELECT id, name FROM customers ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cs []Customer
	for rows.Next() {
		var c Customer
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			return nil, err
		}
		cs = append(cs, c)
	}
	return cs, nil
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
