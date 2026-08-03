package contract_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/AdrienFromToulouse/agentixdisciplina/internal/contract"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/episode"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/evaluate"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/report"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/verdict"
)

// load writes a contract to a temp dir and compiles it. baseDir matters for
// custom clauses, which resolve `source` relative to the contract.
func load(t *testing.T, yaml string) (*contract.Plan, error) {
	t.Helper()
	dir := t.TempDir()
	// Custom-clause tests reference testdata/*.rego, so mirror it in.
	if entries, err := os.ReadDir("testdata"); err == nil {
		_ = os.Mkdir(filepath.Join(dir, "testdata"), 0o755)
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join("testdata", e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "testdata", e.Name()), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	path := filepath.Join(dir, "contract.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return contract.Load(path)
}

func mustLoad(t *testing.T, yaml string) *contract.Plan {
	t.Helper()
	p, err := load(t, yaml)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return p
}

// ep builds an Episode with the given refund amounts and an optional lookup
// limit, which is all the invariant tests need.
func ep(limit any, refunds ...float64) *episode.Episode {
	e := &episode.Episode{Meta: episode.Meta{TraceID: "t1", Adapter: "test"}}
	e.Coverage.HasToolArgs = true
	e.Coverage.HasToolResults = true

	if limit != nil {
		result := `{"customer":{"refund_limit":` + toJSON(limit) + `,"tier":"standard"}}`
		e.ToolCalls = append(e.ToolCalls, episode.ToolCall{
			Name: "crm.lookup", Kind: "tool", Result: result, ResultCaptured: true,
			Span: episode.SpanRef{TraceID: "t1", SpanID: "sp-lookup", Path: "tool_calls[0]"},
		})
	}
	for i, amt := range refunds {
		e.ToolCalls = append(e.ToolCalls, episode.ToolCall{
			Name: "billing.refund", Kind: "tool",
			Arguments:    `{"customer_id":"C-1","order_id":"o1","amount":` + toJSON(amt) + `}`,
			ArgsCaptured: true, DurationMS: 100,
			Span: episode.SpanRef{
				TraceID: "t1",
				SpanID:  fmt.Sprintf("sp-refund-%c", 'a'+i),
				Path:    fmt.Sprintf("tool_calls[%d]", len(e.ToolCalls)),
			},
		})
	}
	for i := range e.ToolCalls {
		e.ToolCalls[i].Index = i
	}
	e.Metrics.ToolCalls = len(e.ToolCalls)
	return e
}

func toJSON(v any) string {
	switch t := v.(type) {
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	}
	return "null"
}

func run(t *testing.T, p *contract.Plan, e *episode.Episode) *report.Report {
	t.Helper()
	return evaluate.Run(p, e, evaluate.Options{})
}

func status(t *testing.T, r *report.Report, clause string) verdict.Verdict {
	t.Helper()
	for _, v := range r.Verdicts {
		if v.Clause == clause {
			return v
		}
	}
	t.Fatalf("clause %q not in report", clause)
	return verdict.Verdict{}
}

const invariantContract = `
apiVersion: axda.dev/v1
kind: AgentContract
metadata: {name: inv}
spec:
  values:
    refund.amount:
      from: tool_call
      tool: billing.refund
      arg: amount
      cardinality: any
    approved_limit:
      from: tool_result
      tool: crm.lookup
      path: $.customer.refund_limit
      cardinality: last
  invariants:
    - "refund.amount <= approved_limit"
`

func TestInvariantHoldsAndFails(t *testing.T) {
	p := mustLoad(t, invariantContract)

	if v := status(t, run(t, p, ep(500.0, 240)), "invariants[0]"); v.Status != verdict.Pass {
		t.Fatalf("240 <= 500 should pass, got %s: %s", v.Status, v.Message)
	}

	v := status(t, run(t, p, ep(500.0, 900)), "invariants[0]")
	if v.Status != verdict.Fail {
		t.Fatalf("900 <= 500 should fail, got %s", v.Status)
	}
	// Evidence anchors to the refund that broke it, not the lookup that
	// supplied the limit.
	if got := v.Findings[0].Evidence.SpanID; got != "sp-refund-a" {
		t.Errorf("evidence span = %q, want the refund call sp-refund-a", got)
	}
	if !strings.Contains(v.Findings[0].Message, "900") {
		t.Errorf("message should name the offending value: %q", v.Findings[0].Message)
	}
}

// `cardinality: any` must check EVERY occurrence. A policy that silently
// checks only the first of several refunds is the bug the field prevents.
func TestCardinalityAnyChecksEveryOccurrence(t *testing.T) {
	p := mustLoad(t, invariantContract)
	v := status(t, run(t, p, ep(500.0, 100, 200, 900, 300)), "invariants[0]")
	if v.Status != verdict.Fail {
		t.Fatalf("a later out-of-range refund must fail, got %s", v.Status)
	}
	if len(v.Findings) != 1 {
		t.Fatalf("expected exactly the one bad refund, got %d findings", len(v.Findings))
	}
	if got := v.Findings[0].Evidence.SpanID; got != "sp-refund-c" {
		t.Errorf("evidence span = %q, want the third refund sp-refund-c", got)
	}
}

// A missing operand makes the constraint unevaluable. It must never pass.
func TestInvariantSkipsWhenValueMissing(t *testing.T) {
	p := mustLoad(t, invariantContract)

	// No crm.lookup at all, so approved_limit cannot bind.
	r := run(t, p, ep(nil, 900))
	v := status(t, r, "invariants[0]")
	if v.Status != verdict.Skipped {
		t.Fatalf("missing operand must skip, got %s: a vacuous pass is the worst outcome available", v.Status)
	}
	if r.Gate != "pass" {
		t.Errorf("a skip alone should not fail the gate, got %q", r.Gate)
	}
	if len(v.MissingCoverage) == 0 || !strings.Contains(strings.Join(v.MissingCoverage, " "), "approved_limit") {
		t.Errorf("skip reason should name the unbound value: %v", v.MissingCoverage)
	}

	// And --fail-on-skipped turns it into a gate failure.
	if got := evaluate.Run(p, ep(nil, 900), evaluate.Options{FailOnSkipped: true}); got.Gate != "fail" {
		t.Errorf("--fail-on-skipped should fail, got %q", got.Gate)
	}
}

func TestDefaultSubstitutesRatherThanSkipping(t *testing.T) {
	p := mustLoad(t, `
apiVersion: axda.dev/v1
kind: AgentContract
metadata: {name: d}
spec:
  values:
    refund.amount:
      from: tool_call
      tool: billing.refund
      arg: amount
      cardinality: any
    approved_limit:
      from: tool_result
      tool: crm.lookup
      path: $.customer.nonexistent
      cardinality: last
      default: 1000
  invariants:
    - "refund.amount <= approved_limit"
`)
	v := status(t, run(t, p, ep(500.0, 900)), "invariants[0]")
	if v.Status != verdict.Pass {
		t.Fatalf("declared default should bind and satisfy 900 <= 1000, got %s: %v", v.Status, v.MissingCoverage)
	}
}

func TestExactlyOneCardinality(t *testing.T) {
	p := mustLoad(t, `
apiVersion: axda.dev/v1
kind: AgentContract
metadata: {name: x}
spec:
  values:
    refund.amount:
      from: tool_call
      tool: billing.refund
      arg: amount
      cardinality: exactly_one
  invariants:
    - "refund.amount > 0"
`)
	if v := status(t, run(t, p, ep(nil, 240)), "invariants[0]"); v.Status != verdict.Pass {
		t.Fatalf("one refund should satisfy exactly_one, got %s", v.Status)
	}
	// Two matches is a failure, not a skip: the contract asserted a count.
	v := status(t, run(t, p, ep(nil, 240, 300)), "invariants[0]")
	if v.Status != verdict.Fail {
		t.Fatalf("two matches should fail exactly_one, got %s", v.Status)
	}
	if !strings.Contains(v.Findings[0].Message, "exactly_one") {
		t.Errorf("message should explain the cardinality mismatch: %q", v.Findings[0].Message)
	}
}

// The compile-time half of ADR-003 §4: operands must be declared, and the
// error must arrive before any trace is read.
func TestUndeclaredIdentifierIsCompileError(t *testing.T) {
	_, err := load(t, `
apiVersion: axda.dev/v1
kind: AgentContract
metadata: {name: u}
spec:
  values:
    refund.amount: {from: tool_call, tool: billing.refund, arg: amount, cardinality: any}
  invariants:
    - "refund.amount <= approved_limit"
`)
	if err == nil {
		t.Fatal("expected a compile error for the undeclared approved_limit")
	}
	if !strings.Contains(err.Error(), "approved_limit") {
		t.Errorf("error should name the undeclared value: %v", err)
	}
}

// A syntactically invalid expression is a load-time error naming its source
// location, not a mid-run surprise: RootIdentifiers parses the real CUE
// grammar, so malformed input fails where undeclared operands do.
func TestMalformedInvariantIsCompileError(t *testing.T) {
	_, err := load(t, `
apiVersion: axda.dev/v1
kind: AgentContract
metadata: {name: m}
spec:
  values:
    refund.amount: {from: tool_call, tool: billing.refund, arg: amount, cardinality: any}
  invariants:
    - "refund.amount <="
`)
	if err == nil {
		t.Fatal("expected a compile error for the malformed expression")
	}
	if !strings.Contains(err.Error(), "spec.invariants[0]") {
		t.Errorf("error should name the source location: %v", err)
	}
}

// String literals must not be mistaken for identifiers.
func TestStringLiteralsAreNotIdentifiers(t *testing.T) {
	if _, err := load(t, `
apiVersion: axda.dev/v1
kind: AgentContract
metadata: {name: s}
spec:
  values:
    customer.tier: {from: tool_result, tool: crm.lookup, path: $.customer.tier, cardinality: last, default: standard}
  invariants:
    - 'customer.tier == "enterprise"'
`); err != nil {
		t.Fatalf("the word inside a string literal must not read as an identifier: %v", err)
	}
}

func TestCardinalityIsRequired(t *testing.T) {
	_, err := load(t, `
apiVersion: axda.dev/v1
kind: AgentContract
metadata: {name: c}
spec:
  values:
    refund.amount: {from: tool_call, tool: billing.refund, arg: amount}
  invariants:
    - "refund.amount > 0"
`)
	if err == nil || !strings.Contains(err.Error(), "cardinality") {
		t.Fatalf("cardinality must be required, got: %v", err)
	}
}

func TestArgsMatchSchema(t *testing.T) {
	p := mustLoad(t, `
apiVersion: axda.dev/v1
kind: AgentContract
metadata: {name: a}
spec:
  must:
    - kind: tool.args_match
      tool: billing.refund
      schema: |
        {customer_id: string, order_id: string, amount: number & <=500}
`)
	if v := status(t, run(t, p, ep(nil, 240)), "tool.args_match"); v.Status != verdict.Pass {
		t.Fatalf("conforming args should pass, got %s: %v", v.Status, v.Findings)
	}
	if v := status(t, run(t, p, ep(nil, 900)), "tool.args_match"); v.Status != verdict.Fail {
		t.Fatalf("amount 900 violates <=500, got %s", v.Status)
	}
}

func TestCustomRegoClause(t *testing.T) {
	p := mustLoad(t, `
apiVersion: axda.dev/v1
kind: AgentContract
metadata: {name: custom}
clauses:
  - name: acme.slow_refund
    engine: rego
    source: testdata/weekend.rego
    query: data.acme.weekend.violation
    severity: major
spec:
  must:
    - kind: acme.slow_refund
      max_duration_ms: 50
`)
	v := status(t, run(t, p, ep(nil, 240)), "acme.slow_refund")
	if v.Status != verdict.Fail {
		t.Fatalf("a 100ms refund should trip a 50ms custom rule, got %s", v.Status)
	}
	if v.Findings[0].Evidence.SpanID == "" {
		t.Error("custom clause findings must carry a span like any other")
	}
}

func TestCustomClauseMustBeNamespaced(t *testing.T) {
	for _, name := range []string{"slow_refund", "tool.allowlist", "budget.whatever"} {
		_, err := load(t, `
apiVersion: axda.dev/v1
kind: AgentContract
metadata: {name: n}
clauses:
  - name: `+name+`
    engine: rego
    source: testdata/weekend.rego
    query: data.acme.weekend.violation
spec:
  allowed_tools: [a]
`)
		if err == nil {
			t.Errorf("custom clause %q should be rejected", name)
		}
	}
}

func TestBrokenCustomPolicyFailsAtLoad(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.rego"), []byte("package acme.bad\n\nthis is not rego"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "contract.yaml")
	if err := os.WriteFile(path, []byte(`
apiVersion: axda.dev/v1
kind: AgentContract
metadata: {name: b}
clauses:
  - name: acme.bad
    engine: rego
    source: bad.rego
    query: data.acme.bad.violation
spec:
  allowed_tools: [a]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.Load(path); err == nil {
		t.Fatal("a broken custom policy must fail at load, not mid-run")
	}
}

// Rego returns a set, whose iteration order is unspecified. Without an
// explicit sort the determinism guarantee would depend on OPA's hashing.
func TestRegoFindingsAreOrdered(t *testing.T) {
	p := mustLoad(t, `
apiVersion: axda.dev/v1
kind: AgentContract
metadata: {name: o}
spec:
  allowed_tools: [nothing]
`)
	e := ep(500.0, 100, 200, 300)
	var first []string
	for i := 0; i < 8; i++ {
		v := status(t, run(t, p, e), "tool.allowlist")
		var got []string
		for _, f := range v.Findings {
			got = append(got, f.Evidence.SpanID+"|"+f.Message)
		}
		if i == 0 {
			first = got
			if len(first) != 4 {
				t.Fatalf("expected 4 disallowed calls, got %d", len(first))
			}
			continue
		}
		if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("finding order changed on run %d:\n %v\n %v", i+1, first, got)
		}
	}
}
