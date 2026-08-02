// Package detect implements the built-in content detectors.
package detect

import (
	"regexp"
	"strings"
)

type Match struct {
	Type  string
	Start int
	End   int
	Raw   string
}

// DefaultPIITypes excludes phone deliberately: loose phone patterns produce
// enough false positives to train users into disabling the check.
var DefaultPIITypes = []string{"card", "email", "ssn"}

var patterns = map[string]*regexp.Regexp{
	"card":  regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`),
	"email": regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`),
	"ssn":   regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`),
	"phone": regexp.MustCompile(`\b\+?\d{1,3}[ .\-]?\(?\d{2,4}\)?[ .\-]?\d{3,4}[ .\-]?\d{3,4}\b`),
	"iban":  regexp.MustCompile(`\b[A-Z]{2}\d{2}[A-Z0-9]{11,30}\b`),
}

// PII scans text for the given types. Card matches are Luhn-validated, so a
// long order number does not read as a credit card.
func PII(text string, types []string) []Match {
	if len(types) == 0 {
		types = DefaultPIITypes
	}
	var out []Match
	for _, t := range types {
		re, ok := patterns[t]
		if !ok {
			continue
		}
		for _, loc := range re.FindAllStringIndex(text, -1) {
			raw := text[loc[0]:loc[1]]
			if t == "card" && !luhn(raw) {
				continue
			}
			out = append(out, Match{Type: t, Start: loc[0], End: loc[1], Raw: raw})
		}
	}
	return out
}

// KnownPIITypes lists every supported detector, for validation messages.
func KnownPIITypes() []string {
	return []string{"card", "email", "ssn", "phone", "iban"}
}

func luhn(s string) bool {
	var digits []int
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits = append(digits, int(r-'0'))
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum, double := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		d := digits[i]
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// Mask replaces a matched value with a type-labelled placeholder, keeping the
// last four characters for card numbers so a finding stays actionable.
func Mask(m Match) string {
	switch m.Type {
	case "card":
		digits := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, m.Raw)
		if len(digits) >= 4 {
			return "[redacted:card ****" + digits[len(digits)-4:] + "]"
		}
		return "[redacted:card]"
	case "email":
		if i := strings.IndexByte(m.Raw, '@'); i > 0 {
			return "[redacted:email @" + m.Raw[i+1:] + "]"
		}
	}
	return "[redacted:" + m.Type + "]"
}

// Excerpt renders a bounded window of text around a match, applying the
// requested masking. Excerpts are capped at 256 bytes (ADR-002 §7).
func Excerpt(text string, m Match, masked bool) string {
	return excerpt(text, m, masked, true)
}

// ExcerptLeading renders context up to the end of the match and stops there.
//
// Used for pattern matches, where the sensitive value typically *follows* the
// match ("password: hunter2"): masking the match itself would hide the label
// and print the secret. Dropping the trailing context is the only way to
// report the hit without reproducing what it found.
func ExcerptLeading(text string, m Match) string {
	return excerpt(text, m, false, false)
}

func excerpt(text string, m Match, masked, trailing bool) string {
	const window = 48
	const maxLen = 256

	start := m.Start - window
	if start < 0 {
		start = 0
	}
	end := m.End
	if trailing {
		end = m.End + window
		if end > len(text) {
			end = len(text)
		}
	}

	value := m.Raw
	if masked {
		value = Mask(m)
	}
	out := text[start:m.Start] + value + text[m.End:end]
	out = strings.Join(strings.Fields(out), " ")
	if len(out) > maxLen {
		out = out[:maxLen] + "…"
	}
	if start > 0 {
		out = "…" + out
	}
	if end < len(text) {
		out += "…"
	}
	return out
}
