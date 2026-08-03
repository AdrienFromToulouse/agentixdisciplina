package extract

import (
	"strings"
	"testing"

	"github.com/AdrienFromToulouse/agentixdisciplina/internal/episode"
)

// The gate's whole job: a quote either exists in the source or the row dies.
func TestLocateAcceptsExactAndWrapped(t *testing.T) {
	src := "Your refund of 240 has been issued\nand a confirmation email is on its way."

	cases := []struct {
		name    string
		snippet string
	}{
		{"exact substring", "refund of 240 has been issued"},
		// A wrapped sentence cannot be quoted contiguously; holding that
		// against the model would discard real evidence.
		{"across a newline", "has been issued and a confirmation email"},
		{"collapsed whitespace", "refund   of    240"},
		{"whole source", src},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := Locate(src, c.snippet)
			if m == nil {
				t.Fatalf("Locate(%q) = nil, want a match", c.snippet)
			}
			if src[m.Start:m.End] != m.Text {
				t.Errorf("offsets do not agree with returned text")
			}
		})
	}
}

// A model that tidies a quote is rejected on the same footing as one that
// fabricates: both mean the citation is not what the source says.
func TestLocateRejectsAnythingButWhitespaceDrift(t *testing.T) {
	src := "Refunded 240 EUR to the Vitam1n B12 order on 2026-03-04."

	cases := []struct {
		name    string
		snippet string
	}{
		{"corrected typo", "Vitamin B12"},
		{"converted unit", "Refunded 240 USD"},
		{"paraphrase", "We refunded 240 euros"},
		{"invented outright", "Refunded 999 EUR"},
		{"dropped a word", "Refunded to the order"},
		{"reordered words", "240 Refunded EUR"},
		{"empty", "   "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if m := Locate(src, c.snippet); m != nil {
				t.Fatalf("Locate(%q) = %q, want rejection", c.snippet, m.Text)
			}
		})
	}
}

// The stored citation must be the source's bytes, not the model's retyping,
// so a quote is verbatim by construction rather than by diligence.
func TestLocateReturnsSourceBytes(t *testing.T) {
	src := "issued\n    240 EUR"
	m := Locate(src, "issued 240 EUR") // model collapsed the wrap
	if m == nil {
		t.Fatal("expected a match across the wrap")
	}
	if m.Text != src {
		t.Fatalf("stored %q, want the source's own bytes %q", m.Text, src)
	}
}

func ep(assistant, toolResult string) *episode.Episode {
	e := &episode.Episode{Meta: episode.Meta{TraceID: "t1"}}
	e.Coverage.HasMessageContent = true
	e.Coverage.HasToolResults = true
	e.ToolCalls = []episode.ToolCall{{
		Index: 0, Name: "crm.lookup", Result: toolResult, ResultCaptured: true,
		StartedAt: 100, EndedAt: 200,
		Span: episode.SpanRef{TraceID: "t1", SpanID: "sp-tool"},
	}}
	e.Turns = []episode.Turn{{
		Index: 0, Role: "assistant", Text: assistant, Captured: true,
		StartedAt: 300,
		Span:      episode.SpanRef{TraceID: "t1", SpanID: "sp-a"},
	}}
	return e
}

func TestGateAcceptsVerifiedRow(t *testing.T) {
	e := ep("Your refund of 240 has been issued.", `{"amount":240}`)
	facts, rejected, err := Gate(e, []byte(
		`[{"claim":"a refund of 240 was issued","source_id":"turn-0","snippet":"refund of 240 has been issued"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(rejected) != 0 {
		t.Fatalf("unexpected rejections: %+v", rejected)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(facts))
	}
	f := facts[0]
	if f.Snippet != "refund of 240 has been issued" {
		t.Errorf("snippet = %q", f.Snippet)
	}
	// Evidence resolves to a span, like every other finding (ADR-001 §6).
	if f.Span.SpanID != "sp-a" {
		t.Errorf("span = %q, want sp-a", f.Span.SpanID)
	}
	if !strings.Contains(f.Span.Path, "turn-0[") {
		t.Errorf("path should carry the located offsets, got %q", f.Span.Path)
	}
}

func TestGateRejectsFabricatedSnippet(t *testing.T) {
	e := ep("Your refund of 240 has been issued.", `{"amount":240}`)
	facts, rejected, err := Gate(e, []byte(
		`[{"claim":"a refund of 999 was issued","source_id":"turn-0","snippet":"refund of 999 has been issued"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 0 {
		t.Fatalf("a fabricated snippet must not become a fact: %+v", facts)
	}
	if len(rejected) != 1 || !strings.Contains(rejected[0].Reason, "verbatim") {
		t.Fatalf("expected one verbatim rejection, got %+v", rejected)
	}
}

// Quoting text that exists, but in a different source than the one named, is
// still a fabricated citation.
func TestGateRejectsSnippetFromAnotherSource(t *testing.T) {
	e := ep("Your refund has been issued.", `{"secret_note":"internal only"}`)
	_, rejected, err := Gate(e, []byte(
		`[{"claim":"note","source_id":"turn-0","snippet":"internal only"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(rejected) != 1 {
		t.Fatalf("expected rejection, got %+v", rejected)
	}
}

func TestGateRejectsUnknownSource(t *testing.T) {
	e := ep("Hello.", `{}`)
	_, rejected, err := Gate(e, []byte(
		`[{"claim":"x","source_id":"turn-42","snippet":"Hello"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(rejected) != 1 || !strings.Contains(rejected[0].Reason, "does not exist") {
		t.Fatalf("expected an unknown-source rejection, got %+v", rejected)
	}
}

func TestGateRejectsMalformedOutput(t *testing.T) {
	if _, _, err := Gate(ep("hi", "{}"), []byte(`not json`)); err == nil {
		t.Fatal("malformed extractor output must be an error, not silently empty")
	}
}

func TestSourcesAreAddressableAndExact(t *testing.T) {
	e := ep("Assistant said this.", `{"k":"v"}`)
	e.ToolCalls[0].Arguments = `{"q":1}`
	e.ToolCalls[0].ArgsCaptured = true

	got := map[string]string{}
	for _, s := range Sources(e) {
		got[s.ID] = s.Text
	}
	for id, want := range map[string]string{
		"turn-0":        "Assistant said this.",
		"tool_result-0": `{"k":"v"}`,
		"tool_args-0":   `{"q":1}`,
	} {
		if got[id] != want {
			t.Errorf("source %s = %q, want %q", id, got[id], want)
		}
	}
}
