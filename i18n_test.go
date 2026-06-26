package main

import "testing"

func TestTr(t *testing.T) {
	if got := tr("en", "invoices"); got != "Invoices" {
		t.Errorf("en invoices = %q, want Invoices", got)
	}
	// unknown language falls back to the default (hr)
	if got := tr("de", "invoices"); got != "Računi" {
		t.Errorf("de fallback = %q, want Računi", got)
	}
	// unknown key returns the key itself so a gap is visible, not blank
	if got := tr("en", "nope"); got != "nope" {
		t.Errorf("missing key = %q, want nope", got)
	}
	// every language must define the full key set, else the UI shows raw keys
	for key := range messages[defaultLang] {
		for lang, m := range messages {
			if _, ok := m[key]; !ok {
				t.Errorf("language %q missing key %q", lang, key)
			}
		}
	}
}
