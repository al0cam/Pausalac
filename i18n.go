package main

// UI strings keyed by language. Croatian ("hr") is the default and fallback.
// Add a language by adding a map with the same keys.
var messages = map[string]map[string]string{
	"hr": {
		"lang_code":             "hr",
		"invoices":              "Računi",
		"new_invoice":           "Novi račun",
		"number":                "Broj",
		"date":                  "Datum",
		"customer":              "Kupac",
		"customer_label":        "Kupac:",
		"amount_eur":            "Iznos (€)",
		"no_invoices":           "Nema računa.",
		"toggle_theme":          "Promijeni temu",
		"number_auto":           "Broj računa se generira automatski (redni-broj/mjesec/godina).",
		"choose_customer":       "-- odaberite kupca --",
		"note":                  "Napomena",
		"items":                 "Stavke",
		"add_item":              "Dodaj stavku",
		"description":           "Opis",
		"quantity":              "Količina",
		"qty_short":             "Kol.",
		"price_eur":             "Cijena (€)",
		"remove":                "Ukloni",
		"save_invoice":          "Spremi račun",
		"invoice":               "Račun",
		"place_date":            "Mjesto i datum izdavanja",
		"total":                 "Ukupno",
		"issued_by":             "Račun ispostavio",
		"owner_abbr":            "vl.",
		"err_date_required":     "Datum izdavanja je obavezan.",
		"err_date_invalid":      "Neispravan datum.",
		"err_customer_required": "Odaberite kupca.",
		"err_items_required":    "Unesite barem jednu stavku (opis).",
	},
	"en": {
		"lang_code":             "en",
		"invoices":              "Invoices",
		"new_invoice":           "New invoice",
		"number":                "Number",
		"date":                  "Date",
		"customer":              "Customer",
		"customer_label":        "Customer:",
		"amount_eur":            "Amount (€)",
		"no_invoices":           "No invoices.",
		"toggle_theme":          "Toggle theme",
		"number_auto":           "The invoice number is generated automatically (sequence/month/year).",
		"choose_customer":       "-- choose a customer --",
		"note":                  "Note",
		"items":                 "Items",
		"add_item":              "Add item",
		"description":           "Description",
		"quantity":              "Quantity",
		"qty_short":             "Qty",
		"price_eur":             "Price (€)",
		"remove":                "Remove",
		"save_invoice":          "Save invoice",
		"invoice":               "Invoice",
		"place_date":            "Place and date of issue",
		"total":                 "Total",
		"issued_by":             "Issued by",
		"owner_abbr":            "owner",
		"err_date_required":     "Issue date is required.",
		"err_date_invalid":      "Invalid date.",
		"err_customer_required": "Choose a customer.",
		"err_items_required":    "Enter at least one item (description).",
	},
}

const defaultLang = "hr"

// tr returns the string for key in lang, falling back to the default language,
// then to the key itself so a missing translation is visible rather than blank.
func tr(lang, key string) string {
	if m, ok := messages[lang]; ok {
		if s, ok := m[key]; ok {
			return s
		}
	}
	if s, ok := messages[defaultLang][key]; ok {
		return s
	}
	return key
}
