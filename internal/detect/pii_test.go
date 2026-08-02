package detect

import "testing"

func TestCardRequiresLuhn(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		{"valid visa test card", "refunded to 4242 4242 4242 4242 today", 1},
		{"valid mastercard", "card 5555555555554444 on file", 1},
		// The whole point of the Luhn gate: long identifiers are not cards.
		{"order number is not a card", "your order 1234567890123456 shipped", 0},
		{"tracking number", "tracking 9999999999999999999", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := len(PII(c.text, []string{"card"})); got != c.want {
				t.Fatalf("PII(%q) = %d matches, want %d", c.text, got, c.want)
			}
		})
	}
}

func TestMaskKeepsLastFour(t *testing.T) {
	m := PII("card 4242 4242 4242 4242", []string{"card"})
	if len(m) != 1 {
		t.Fatalf("expected 1 match, got %d", len(m))
	}
	got := Mask(m[0])
	if got != "[redacted:card ****4242]" {
		t.Fatalf("Mask = %q", got)
	}
}

// A masked excerpt must not contain the raw value it reports on: the tool
// must not re-leak what it was hired to detect (ADR-002 §7).
func TestMaskedExcerptDropsRawValue(t *testing.T) {
	text := "The refund went back to your Visa 4242 4242 4242 4242. Expect 3-5 days."
	m := PII(text, []string{"card"})[0]
	ex := Excerpt(text, m, true)
	if contains(ex, "4242 4242 4242 4242") {
		t.Fatalf("masked excerpt leaked the raw card: %q", ex)
	}
	if !contains(ex, "redacted:card") {
		t.Fatalf("masked excerpt missing marker: %q", ex)
	}
}

// For pattern hits the secret follows the match, so masked mode must stop at
// the match rather than print what comes after it.
func TestExcerptLeadingStopsAtMatch(t *testing.T) {
	text := "Your temporary portal password: Hunter2-Reset. Change it after signing in."
	m := Match{Type: "pattern", Start: 31, End: 32, Raw: ":"}
	ex := ExcerptLeading(text, m)
	if contains(ex, "Hunter2-Reset") {
		t.Fatalf("leading excerpt leaked the secret: %q", ex)
	}
}

func TestEmailAndSSN(t *testing.T) {
	text := "reach me at adrien@example.com or use 123-45-6789"
	if got := len(PII(text, []string{"email", "ssn"})); got != 2 {
		t.Fatalf("expected 2 matches, got %d", got)
	}
	// Phone is excluded from the defaults on purpose (false-positive rate).
	for _, ty := range DefaultPIITypes {
		if ty == "phone" {
			t.Fatal("phone must not be a default PII type")
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
