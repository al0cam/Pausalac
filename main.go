package main

import (
	"database/sql"
	"embed"
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

	"pausalac/db"
)

//go:embed templates/*.html
var assets embed.FS

// One parsed template set per language; the "T" func is bound to that language
// at parse time, so templates just call {{T "key"}} with no per-request cost.
var tmpls = parseTemplates()

func parseTemplates() map[string]*template.Template {
	eur := func(cents int64) string { return strconv.FormatFloat(float64(cents)/100, 'f', 2, 64) }
	m := map[string]*template.Template{}
	for lang := range messages {
		l := lang
		m[lang] = template.Must(template.New("").Funcs(template.FuncMap{
			"eur": eur,
			"T":   func(key string) string { return tr(l, key) },
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
}

type Item struct {
	Description    string
	Quantity       float64
	UnitPriceCents int64
	LineTotalCents int64
}

type Customer struct {
	ID   int64
	Name string
}

type Company struct {
	Name    string
	Owner   string
	OIB     string
	Address string
	Place   string
	IBAN    string
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
			GROUP BY i.id
			ORDER BY i.issue_date DESC, i.id DESC`)
		if err != nil {
			httpErr(w, err)
			return
		}
		defer rows.Close()
		var invs []Invoice
		for rows.Next() {
			var v Invoice
			if err := rows.Scan(&v.ID, &v.Number, &v.IssueDate, &v.Customer, &v.Note, &v.TotalCents); err != nil {
				httpErr(w, err)
				return
			}
			invs = append(invs, v)
		}
		render(w, r, "list.html", invs)
	}
}

// itemInput holds an item row exactly as submitted, so a failed form can be
// redisplayed with the user's values intact.
type itemInput struct{ Description, Quantity, UnitPrice string }

// invoiceForm is the data the new-invoice template renders: submitted values,
// the customer list, and per-field validation errors (empty on a fresh form).
type invoiceForm struct {
	IssueDate  string
	CustomerID string
	Note       string
	Items      []itemInput
	Errors     map[string]string
	Customers  []Customer
}

// renderNew loads customers, ensures at least one item row, and renders the form.
func renderNew(w http.ResponseWriter, r *http.Request, db *sql.DB, f invoiceForm) {
	custs, err := allCustomers(db)
	if err != nil {
		httpErr(w, err)
		return
	}
	f.Customers = custs
	if len(f.Items) == 0 {
		f.Items = append(f.Items, itemInput{Quantity: "1"})
	}
	render(w, r, "new.html", f)
}

func newInvoice(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		renderNew(w, r, db, invoiceForm{IssueDate: time.Now().Format("2006-01-02")})
	}
}

func createInvoice(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		f := invoiceForm{
			IssueDate:  r.FormValue("issue_date"),
			CustomerID: r.FormValue("customer_id"),
			Note:       r.FormValue("note"),
		}
		descs := r.Form["description"]
		qtys := r.Form["quantity"]
		prices := r.Form["unit_price"]
		for i := range descs {
			f.Items = append(f.Items, itemInput{descs[i], at(qtys, i), at(prices, i)})
		}

		// Validate; collect per-field errors and redisplay the form on any failure.
		lang := langOf(r)
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

		for _, it := range f.Items {
			if strings.TrimSpace(it.Description) == "" {
				continue
			}
			qty := parseFloat(it.Quantity, 1)
			unit := euroToCents(it.UnitPrice)
			line := int64(math.Round(qty * float64(unit)))
			if _, err := tx.Exec(`INSERT INTO invoice_items
				(invoice_id, description, quantity, unit_price_cents, line_total_cents)
				VALUES (?, ?, ?, ?, ?)`, invID, it.Description, qty, unit, line); err != nil {
				httpErr(w, err)
				return
			}
		}
		if err := tx.Commit(); err != nil {
			httpErr(w, err)
			return
		}
		http.Redirect(w, r, "/invoices/"+strconv.FormatInt(invID, 10), http.StatusSeeOther)
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
		err = db.QueryRow(`SELECT i.id, i.number, i.issue_date, c.name, c.address, c.oib, i.note
			FROM invoices i JOIN customers c ON c.id = i.customer_id WHERE i.id = ?`, id).
			Scan(&v.ID, &v.Number, &v.IssueDate, &v.Customer, &v.CustomerAddress, &v.CustomerOIB, &v.Note)
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
		rows, err := db.Query(`SELECT description, quantity, unit_price_cents, line_total_cents
			FROM invoice_items WHERE invoice_id = ? ORDER BY id`, id)
		if err != nil {
			httpErr(w, err)
			return
		}
		defer rows.Close()
		var items []Item
		for rows.Next() {
			var it Item
			if err := rows.Scan(&it.Description, &it.Quantity, &it.UnitPriceCents, &it.LineTotalCents); err != nil {
				httpErr(w, err)
				return
			}
			v.TotalCents += it.LineTotalCents
			items = append(items, it)
		}
		render(w, r, "view.html", map[string]any{"Invoice": v, "Items": items, "Company": company})
	}
}

func loadCompany(db *sql.DB) (Company, error) {
	var c Company
	err := db.QueryRow(`SELECT name, owner, oib, address, place, iban FROM company WHERE id = 1`).
		Scan(&c.Name, &c.Owner, &c.OIB, &c.Address, &c.Place, &c.IBAN)
	return c, err
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
