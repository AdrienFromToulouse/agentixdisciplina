package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/AdrienFromToulouse/agentixdisciplina/internal/episode"
)

// Fact is one extracted claim that survived the verbatim gate.
//
// Snippet holds the *source's* bytes for the located span, never the model's
// retyping, which is what makes the citation verbatim by construction.
type Fact struct {
	Text     string
	SourceID string
	Snippet  string
	Span     episode.SpanRef
	Start    int
	End      int
	Turn     int
}

// Rejected is a row the gate refused. It says the extraction model quoted
// something that is not in the trace, which is a signal about the extractor,
// not about the agent under evaluation (ADR-008 §3).
type Rejected struct {
	Text     string
	SourceID string
	Snippet  string
	Reason   string
}

// Extractor produces facts from an Episode. The LLM implementation lives
// behind this so the runner can hold one without importing a model client.
type Extractor interface {
	Facts(ctx context.Context, ep *episode.Episode) ([]Fact, []Rejected, error)
	Name() string
}

// row is the shape the extraction model must return.
type row struct {
	Claim    string `json:"claim"`
	SourceID string `json:"source_id"`
	Snippet  string `json:"snippet"`
}

// Gate applies the verbatim check to raw extractor output. Exported so the
// gate can be tested without a model, and so any future extractor gets the
// identical check rather than its own approximation.
func Gate(ep *episode.Episode, raw []byte) ([]Fact, []Rejected, error) {
	var rows []row
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, nil, fmt.Errorf("extractor returned malformed rows: %w", err)
	}

	byID := map[string]Source{}
	turnOf := map[string]int{}
	for _, s := range Sources(ep) {
		byID[s.ID] = s
	}
	for _, t := range ep.Turns {
		turnOf[sourceID("turn", t.Index)] = t.Index
	}

	var facts []Fact
	var rejected []Rejected

	for _, r := range rows {
		src, ok := byID[r.SourceID]
		if !ok {
			rejected = append(rejected, Rejected{
				Text: r.Claim, SourceID: r.SourceID, Snippet: r.Snippet,
				Reason: "names a source that does not exist in this episode",
			})
			continue
		}
		m := Locate(src.Text, r.Snippet)
		if m == nil {
			// Deliberately not repaired: a snippet the model invented is
			// exactly the failure this gate exists to catch.
			rejected = append(rejected, Rejected{
				Text: r.Claim, SourceID: r.SourceID, Snippet: r.Snippet,
				Reason: "snippet is not a verbatim substring of " + r.SourceID,
			})
			continue
		}
		span := src.Span
		span.Path = fmt.Sprintf("%s[%d:%d]", src.ID, m.Start, m.End)
		facts = append(facts, Fact{
			Text:     r.Claim,
			SourceID: r.SourceID,
			Snippet:  m.Text, // the source's bytes, not the model's
			Span:     span,
			Start:    m.Start,
			End:      m.End,
			Turn:     turnOf[r.SourceID],
		})
	}
	return facts, rejected, nil
}

// ToClaims converts verified facts into Episode claims. Support is left for
// the grounding clauses to establish in code; the gate establishes only that
// the agent said this.
func ToClaims(facts []Fact) []episode.Claim {
	out := make([]episode.Claim, 0, len(facts))
	for i, f := range facts {
		out = append(out, episode.Claim{
			Index:     i,
			Text:      f.Snippet,
			Extractor: ExtractorLLM,
			Turn:      f.Turn,
		})
	}
	return out
}

// SourceView renders the addressable sources for the extraction prompt. Each
// region is fenced and labelled, so "quote from turn-3" is unambiguous.
func SourceView(ep *episode.Episode, max int) (string, bool) {
	var b strings.Builder
	truncated := false
	for _, s := range Sources(ep) {
		block := fmt.Sprintf("<source id=%q kind=%q role=%q>\n%s\n</source>\n\n",
			s.ID, s.Kind, s.Role, s.Text)
		if max > 0 && b.Len()+len(block) > max {
			truncated = true
			break
		}
		b.WriteString(block)
	}
	return b.String(), truncated
}
