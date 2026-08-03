package contract_test

import (
	"os"
	"strings"
	"testing"

	"github.com/AdrienFromToulouse/agentixdisciplina/internal/contract"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/episode"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/evaluate"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/verdict"
)

// grounded builds an episode where the assistant asserts `claimText` after a
// tool call that returned `result`.
func grounded(result, claimText string) *episode.Episode {
	e := &episode.Episode{Meta: episode.Meta{TraceID: "t1", Adapter: "test"}}
	e.Coverage.HasMessageContent = true
	e.Coverage.HasToolResults = true

	if result != "" {
		e.ToolCalls = append(e.ToolCalls, episode.ToolCall{
			Index: 0, Name: "crm.lookup", Kind: "tool",
			Result: result, ResultCaptured: true,
			StartedAt: 100, EndedAt: 200,
			Span: episode.SpanRef{TraceID: "t1", SpanID: "sp-tool", Path: "tool_calls[0]"},
		})
	}
	e.Turns = append(e.Turns,
		episode.Turn{
			Index: 0, Role: "user", Text: "What is my refund?", Captured: true,
			StartedAt: 50,
			Span:      episode.SpanRef{TraceID: "t1", SpanID: "sp-u", Path: "turns[0].text"},
		},
		episode.Turn{
			Index: 1, Role: "assistant", Text: claimText, Captured: true,
			StartedAt: 300,
			Span:      episode.SpanRef{TraceID: "t1", SpanID: "sp-a", Path: "turns[1].text"},
		},
	)
	return e
}

const citeContract = `
apiVersion: axda.dev/v1
kind: AgentContract
metadata: {name: g}
spec:
  must:
    - kind: grounding.cite_sources
      min_support: 1
`

func TestCiteSourcesSupportedAndUnsupported(t *testing.T) {
	p := mustLoad(t, citeContract)

	// 240 appears in the tool result → grounded.
	ok := run(t, p, grounded(`{"refund":{"amount":240}}`, "Your refund of 240 has been issued."))
	if v := status(t, ok, "grounding.cite_sources"); v.Status != verdict.Pass {
		t.Fatalf("grounded claim should pass, got %s: %v", v.Status, v.Findings)
	}

	// 999 appears nowhere → ungrounded.
	bad := run(t, p, grounded(`{"refund":{"amount":240}}`, "Your refund of 999 has been issued."))
	v := status(t, bad, "grounding.cite_sources")
	if v.Status != verdict.Fail {
		t.Fatalf("ungrounded claim should fail, got %s", v.Status)
	}
	if v.Findings[0].Evidence.SpanID != "sp-a" {
		t.Errorf("evidence should point at the assistant turn, got %q", v.Findings[0].Evidence.SpanID)
	}
}

// A tool call that completed *after* the turn was written cannot be its
// source: otherwise a later call would retroactively ground an invention.
func TestGroundingIgnoresLaterToolCalls(t *testing.T) {
	e := grounded("", "Your refund of 240 has been issued.")
	e.ToolCalls = append(e.ToolCalls, episode.ToolCall{
		Index: 0, Name: "billing.refund", Kind: "tool",
		Result: `{"amount":240}`, ResultCaptured: true,
		StartedAt: 400, EndedAt: 500, // after the turn at 300
		Span: episode.SpanRef{TraceID: "t1", SpanID: "sp-late", Path: "tool_calls[0]"},
	})

	v := status(t, run(t, mustLoad(t, citeContract), e), "grounding.cite_sources")
	if v.Status != verdict.Fail {
		t.Fatalf("a tool result that arrived after the claim must not ground it, got %s", v.Status)
	}
}

// Sentences that assert no concrete value are not claims: otherwise every
// "Sure, let me look that up." would need a citation.
func TestNonAssertiveSentencesAreNotClaims(t *testing.T) {
	e := grounded(`{"ok":true}`, "I can help with that. Let me pull up your account.")
	v := status(t, run(t, mustLoad(t, citeContract), e), "grounding.cite_sources")
	if v.Status != verdict.Skipped {
		t.Fatalf("no checkable claims should skip, got %s (%v)", v.Status, v.Findings)
	}
}

func TestNoUnsourcedClaimsNamesTheValue(t *testing.T) {
	p := mustLoad(t, `
apiVersion: axda.dev/v1
kind: AgentContract
metadata: {name: u}
spec:
  must_not:
    - kind: grounding.no_unsourced_claims
      value_types: [number]
`)
	v := status(t, run(t, p, grounded(`{"refund":{"amount":240}}`,
		"Your refund of 999 was sent to account 12345.")), "must_not.grounding.no_unsourced_claims")
	if v.Status != verdict.Fail {
		t.Fatalf("expected fail, got %s", v.Status)
	}
	if !strings.Contains(v.Findings[0].Message, "999") {
		t.Errorf("message should name the invented value: %q", v.Findings[0].Message)
	}
}

// A grounding finding must not carry a card number out with it.
func TestClaimEvidenceIsMasked(t *testing.T) {
	const card = "4242 4242 4242 4242"
	e := grounded(`{"unrelated":1}`, "Refunded to Visa "+card+" today.")
	r := evaluate.Run(mustLoad(t, citeContract), e, evaluate.Options{})
	v := status(t, r, "grounding.cite_sources")
	if v.Status != verdict.Fail {
		t.Fatalf("expected a finding to inspect, got %s", v.Status)
	}
	if strings.Contains(v.Findings[0].Evidence.Excerpt, card) ||
		strings.Contains(v.Findings[0].Message, card) {
		t.Fatal("claim evidence leaked a card number")
	}
}

// Judges are probabilistic: with no credentials they SKIP, and a skip alone
// never fails the gate.
func TestJudgeSkipsWithoutCredentials(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	p := mustLoad(t, `
apiVersion: axda.dev/v1
kind: AgentContract
metadata: {name: j}
spec:
  must:
    - kind: quality.helpful
`)
	e := grounded(`{"ok":1}`, "Your refund of 240 has been issued.")

	r := evaluate.Run(p, e, evaluate.Options{}) // no judge configured
	v := status(t, r, "quality.helpful")
	if v.Status != verdict.Skipped {
		t.Fatalf("judge without credentials should skip, got %s", v.Status)
	}
	if v.Class != verdict.Probabilistic {
		t.Errorf("judge verdicts must be probabilistic, got %q", v.Class)
	}
	if v.Blocking {
		t.Error("judge clauses must be advisory by default")
	}
	if r.Gate != "pass" {
		t.Errorf("an advisory skip must not fail the gate, got %q", r.Gate)
	}
}

// Even opted into blocking, a probabilistic verdict cannot fail the build:
// Blocks() requires the deterministic class.
func TestProbabilisticNeverBlocksOnSkip(t *testing.T) {
	v := verdict.Verdict{
		Class: verdict.Probabilistic, Blocking: true, Status: verdict.Errored,
	}
	if v.Blocks() {
		t.Fatal("a probabilistic verdict must never block the build")
	}
	d := verdict.Verdict{
		Class: verdict.Deterministic, Blocking: true, Status: verdict.Errored,
	}
	if !d.Blocks() {
		t.Fatal("a deterministic errored verdict must block")
	}
}

func TestMissingRubricFileFailsAtLoad(t *testing.T) {
	_, err := load(t, `
apiVersion: axda.dev/v1
kind: AgentContract
metadata: {name: r}
spec:
  must:
    - kind: quality.judge
      rubric_file: does-not-exist.md
`)
	if err == nil || !strings.Contains(err.Error(), "does-not-exist") {
		t.Fatalf("a missing rubric must fail at load, got: %v", err)
	}
}

func TestRubricFileIsInlinedAtLoad(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/rubric.md", []byte("Be excellent."), 0o644); err != nil {
		t.Fatal(err)
	}
	path := dir + "/contract.yaml"
	if err := os.WriteFile(path, []byte(`
apiVersion: axda.dev/v1
kind: AgentContract
metadata: {name: r}
spec:
  must:
    - kind: quality.judge
      rubric_file: rubric.md
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.Load(path); err != nil {
		t.Fatalf("rubric_file should satisfy the required rubric param: %v", err)
	}
}
