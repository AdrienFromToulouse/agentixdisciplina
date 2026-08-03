package evaluate_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AdrienFromToulouse/agentixdisciplina/internal/contract"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/episode"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/evaluate"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/extract"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/report"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/verdict"
)

// stubExtractor stands in for the LLM extractor so the class asymmetry can be
// tested without a model. It runs the real gate over canned rows, so a test
// row that fabricates a snippet is rejected exactly as it would be live.
type stubExtractor struct {
	rows string
	err  error
}

func (s stubExtractor) Name() string { return extract.ExtractorLLM }

func (s stubExtractor) Facts(_ context.Context, ep *episode.Episode) ([]extract.Fact, []extract.Rejected, error) {
	if s.err != nil {
		return nil, nil, s.err
	}
	return extract.Gate(ep, []byte(s.rows))
}

const groundingContract = `
apiVersion: axda.dev/v1
kind: AgentContract
metadata: {name: x}
spec:
  must_not:
    - kind: grounding.no_unsourced_claims
      value_types: [number]
`

// plan compiles a contract written inline. The existing helper in this
// package reads the example bundle, so these tests need their own.
func inlinePlan(t *testing.T, yaml string) *contract.Plan {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "contract.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := contract.Load(path)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return p
}

func verdictFor(t *testing.T, r *report.Report, clause string) verdict.Verdict {
	t.Helper()
	for _, v := range r.Verdicts {
		if v.Clause == clause {
			return v
		}
	}
	t.Fatalf("clause %q not in report", clause)
	return verdict.Verdict{}
}

func episodeFor(assistant, toolResult string) *episode.Episode {
	e := &episode.Episode{Meta: episode.Meta{TraceID: "t1", Adapter: "test"}}
	e.Coverage.HasMessageContent = true
	e.Coverage.HasToolResults = true
	e.ToolCalls = []episode.ToolCall{{
		Index: 0, Name: "crm.lookup", Result: toolResult, ResultCaptured: true,
		StartedAt: 100, EndedAt: 200,
		Span: episode.SpanRef{TraceID: "t1", SpanID: "sp-tool", Path: "tool_calls[0]"},
	}}
	e.Turns = []episode.Turn{{
		Index: 0, Role: "assistant", Text: assistant, Captured: true,
		StartedAt: 300,
		Span:      episode.SpanRef{TraceID: "t1", SpanID: "sp-a", Path: "turns[0].text"},
	}}
	return e
}

// The verbatim gate makes evidence deterministic but says nothing about
// recall, so the uncertainty is one-sided (ADR-008 §2).
func TestLLMExtractorClassAsymmetry(t *testing.T) {
	p := inlinePlan(t, groundingContract)

	t.Run("failure is deterministic and blocks", func(t *testing.T) {
		// 999 is quoted verbatim from the turn but appears in no tool
		// result, so the violation rests on verified evidence.
		e := episodeFor("Your refund of 999 has been issued.", `{"amount":240}`)
		r := evaluate.Run(p, e, evaluate.Options{Extractor: stubExtractor{
			rows: `[{"claim":"refund 999","source_id":"turn-0","snippet":"Your refund of 999 has been issued."}]`,
		}})
		v := verdictFor(t, r, "must_not.grounding.no_unsourced_claims")
		if v.Status != verdict.Fail {
			t.Fatalf("status = %s, want fail", v.Status)
		}
		if v.Class != verdict.Deterministic {
			t.Errorf("class = %q, want deterministic: the quote was verified", v.Class)
		}
		if r.Gate != "fail" {
			t.Errorf("a verified violation must block, gate = %q", r.Gate)
		}
		if v.Provenance["extractor"] != extract.ExtractorLLM {
			t.Errorf("verdict should record the extractor, got %v", v.Provenance)
		}
	})

	t.Run("pass is probabilistic and advisory", func(t *testing.T) {
		e := episodeFor("Your refund of 240 has been issued.", `{"amount":240}`)
		r := evaluate.Run(p, e, evaluate.Options{Extractor: stubExtractor{
			rows: `[{"claim":"refund 240","source_id":"turn-0","snippet":"Your refund of 240 has been issued."}]`,
		}})
		v := verdictFor(t, r, "must_not.grounding.no_unsourced_claims")
		if v.Status != verdict.Pass {
			t.Fatalf("status = %s, want pass", v.Status)
		}
		if v.Class != verdict.Probabilistic {
			t.Errorf("class = %q, want probabilistic: the extractor may have missed a claim", v.Class)
		}
		if v.Blocking {
			t.Error("a pass that rests on extractor recall must not be blocking")
		}
		if !strings.Contains(v.Message, "advisory") {
			t.Errorf("the pass should say why it is advisory, got %q", v.Message)
		}
	})
}

// The structural extractor is deterministic in both directions, so a pass
// stays blocking there.
func TestStructuralExtractorKeepsBlockingPass(t *testing.T) {
	e := episodeFor("Your refund of 240 has been issued.", `{"amount":240}`)
	r := evaluate.Run(inlinePlan(t, groundingContract), e, evaluate.Options{})
	v := verdictFor(t, r, "must_not.grounding.no_unsourced_claims")
	if v.Class != verdict.Deterministic || !v.Blocking {
		t.Fatalf("structural pass should stay deterministic and blocking, got %q blocking=%t",
			v.Class, v.Blocking)
	}
}

// A rejected row is the extractor misbehaving, not the agent. It must be
// visible, and it must not become a violation.
func TestRejectedRowsAreDegradedNotViolations(t *testing.T) {
	e := episodeFor("Your refund of 240 has been issued.", `{"amount":240}`)
	r := evaluate.Run(inlinePlan(t, groundingContract), e, evaluate.Options{Extractor: stubExtractor{
		rows: `[{"claim":"invented","source_id":"turn-0","snippet":"a sentence that is not in the trace"}]`,
	}})
	v := verdictFor(t, r, "must_not.grounding.no_unsourced_claims")
	if v.Status == verdict.Fail {
		t.Fatal("an extractor rejection must not be charged to the agent")
	}
	joined := strings.Join(r.Episode.Degraded, " ")
	if !strings.Contains(joined, "rejected") {
		t.Fatalf("the rejection should be visible in coverage, got %v", r.Episode.Degraded)
	}
}

// An extractor that cannot run makes the clause unevaluable. It must never
// fall back to structural and report as though the better one had run.
func TestExtractorFailureSkipsRatherThanFallsBack(t *testing.T) {
	e := episodeFor("Your refund of 999 has been issued.", `{"amount":240}`)
	r := evaluate.Run(inlinePlan(t, groundingContract), e, evaluate.Options{
		Extractor: stubExtractor{err: errors.New("no credentials")},
	})
	v := verdictFor(t, r, "must_not.grounding.no_unsourced_claims")
	if v.Status != verdict.Skipped {
		t.Fatalf("status = %s, want skipped", v.Status)
	}
	if !strings.Contains(strings.Join(v.MissingCoverage, " "), "no credentials") {
		t.Errorf("skip should name the reason, got %v", v.MissingCoverage)
	}
	// Structural would have caught this one; the point is that it must not
	// silently stand in.
	if v.Status == verdict.Fail {
		t.Error("must not fall back to the structural extractor")
	}
}
