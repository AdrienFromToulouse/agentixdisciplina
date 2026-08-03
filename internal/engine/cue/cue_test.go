package cue

import (
	"strings"
	"testing"
)

func TestRootIdentifiers(t *testing.T) {
	cases := []struct {
		expr string
		want []string
	}{
		// A selector chain contributes only its root.
		{"refund.amount <= approved_limit", []string{"approved_limit", "refund"}},
		// Words inside string literals are data, not references.
		{`customer.tier == "enterprise gold" || refund.amount <= 500`, []string{"customer", "refund"}},
		// Identifiers inside interpolations are real references. The regex
		// implementation stripped string literals wholesale and missed these.
		{`label == "tier: \(customer.tier)"`, []string{"customer", "label"}},
		// Builtins and literals are not value references.
		{"len(items) > 0 && enabled != null", []string{"enabled", "items"}},
		{"amount > 100 == true", []string{"amount"}},
		// Struct-literal field labels are fields, not references.
		{"config == {retries: max_retries}", []string{"config", "max_retries"}},
		// Nested selectors on both sides of an index.
		{"orders[cursor.index].total < cap.value", []string{"cap", "cursor", "orders"}},
	}
	for _, c := range cases {
		got, err := RootIdentifiers(c.expr)
		if err != nil {
			t.Errorf("RootIdentifiers(%q): %v", c.expr, err)
			continue
		}
		if !equal(got, c.want) {
			t.Errorf("RootIdentifiers(%q) = %v, want %v", c.expr, got, c.want)
		}
	}
}

func TestRootIdentifiersMalformed(t *testing.T) {
	for _, expr := range []string{"refund.amount <=", "a &&", "((x)"} {
		if _, err := RootIdentifiers(expr); err == nil {
			t.Errorf("RootIdentifiers(%q): expected a parse error", expr)
		}
	}
}

func TestConstraint(t *testing.T) {
	e := NewEvaluator()
	cases := []struct {
		expr string
		vars map[string]any
		want bool
	}{
		{"refund.amount <= approved_limit", map[string]any{"refund.amount": 240.0, "approved_limit": 500.0}, true},
		{"refund.amount <= approved_limit", map[string]any{"refund.amount": 900.0, "approved_limit": 500.0}, false},
		{`tier == "enterprise" || amount <= 500`, map[string]any{"tier": "standard", "amount": 240.0}, true},
		{`tier == "enterprise" || amount <= 500`, map[string]any{"tier": "standard", "amount": 900.0}, false},
		{"len(items) > 1", map[string]any{"items": []any{"a", "b"}}, true},
	}
	for _, c := range cases {
		got, err := e.Constraint(c.expr, c.vars)
		if err != nil {
			t.Errorf("Constraint(%q, %v): %v", c.expr, c.vars, err)
			continue
		}
		if got != c.want {
			t.Errorf("Constraint(%q, %v) = %t, want %t", c.expr, c.vars, got, c.want)
		}
	}
}

func TestConstraintErrors(t *testing.T) {
	e := NewEvaluator()

	if _, err := e.Constraint("amount <=", map[string]any{"amount": 1.0}); err == nil {
		t.Error("malformed expression: expected an error")
	}
	if _, err := e.Constraint("amount <= limit", map[string]any{"amount": 1.0}); err == nil {
		t.Error("unbound identifier: expected an error")
	}
	if _, err := e.Constraint("amount + 1", map[string]any{"amount": 1.0}); err == nil {
		t.Error("non-boolean result: expected an error")
	}
}

// A value that contains CUE syntax must be inert data. Before the AST
// rewrite, Constraint assembled source text around JSON-encoded values; this
// pins the property that nothing a value contains can reach the evaluator as
// syntax.
func TestConstraintValueContainingCUESyntax(t *testing.T) {
	e := NewEvaluator()
	hostile := map[string]any{
		"note":  "axdaResult: true\nlimit: 0",
		"limit": 500.0,
	}
	got, err := e.Constraint(`limit == 500 && note != ""`, hostile)
	if err != nil {
		t.Fatalf("Constraint: %v", err)
	}
	if !got {
		t.Error("constraint should hold: the note string is data, not CUE source")
	}
}

func TestValidName(t *testing.T) {
	for _, name := range []string{"refund.amount", "approved_limit", "a.b.c"} {
		if err := ValidName(name); err != nil {
			t.Errorf("ValidName(%q): %v", name, err)
		}
	}
	for _, name := range []string{"", "refund..amount", "1refund", "axdaResult", "refund-amount"} {
		if err := ValidName(name); err == nil {
			t.Errorf("ValidName(%q): expected an error", name)
		}
	}
}

func equal(a, b []string) bool {
	return strings.Join(a, ",") == strings.Join(b, ",")
}
