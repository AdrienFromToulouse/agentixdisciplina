package extract

import (
	"regexp"
	"strings"

	"github.com/AdrienFromToulouse/agentixdisciplina/internal/episode"
)

// Source is one addressable, exactly-quotable region of an Episode.
//
// The extractor sees these and must quote from one by id. Text holds the
// source's own bytes, so a located snippet can be stored as the source wrote
// it rather than as the model retyped it.
type Source struct {
	ID   string
	Kind string // turn | tool_result | tool_args
	Role string
	Text string
	Span episode.SpanRef
}

// Match is a located snippet: the source's bytes for the span, plus where.
type Match struct {
	Text  string
	Start int
	End   int
}

// Sources renders an Episode as addressable regions in Episode order.
func Sources(ep *episode.Episode) []Source {
	var out []Source
	for _, t := range ep.Turns {
		if !t.Captured || strings.TrimSpace(t.Text) == "" {
			continue
		}
		out = append(out, Source{
			ID:   sourceID("turn", t.Index),
			Kind: "turn",
			Role: t.Role,
			Text: t.Text,
			Span: t.Span,
		})
	}
	for _, tc := range ep.ToolCalls {
		if tc.ResultCaptured && strings.TrimSpace(tc.Result) != "" {
			out = append(out, Source{
				ID:   sourceID("tool_result", tc.Index),
				Kind: "tool_result",
				Role: tc.Name,
				Text: tc.Result,
				Span: tc.Span,
			})
		}
		if tc.ArgsCaptured && strings.TrimSpace(tc.Arguments) != "" {
			out = append(out, Source{
				ID:   sourceID("tool_args", tc.Index),
				Kind: "tool_args",
				Role: tc.Name,
				Text: tc.Arguments,
				Span: tc.Span,
			})
		}
	}
	return out
}

func sourceID(kind string, index int) string {
	return kind + "-" + itoa(index)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}

// Locate finds a quoted snippet in a source, tolerating only its line wrapping.
//
// Every word must appear in order, spelled exactly as the source spells it, so
// a model that repairs a typo, converts a unit, or tidies a quote is rejected
// on the same footing as one that fabricates. Only the whitespace between
// words is flexible, because a snippet spanning a wrapped line cannot be
// quoted contiguously and holding that against the model would discard real
// evidence.
//
// Returns the source's text for the span, so callers store what the trace says
// rather than how the model retyped it (ADR-008 §1).
func Locate(source, snippet string) *Match {
	if strings.TrimSpace(snippet) == "" {
		return nil
	}
	if i := strings.Index(source, snippet); i >= 0 {
		return &Match{Text: source[i : i+len(snippet)], Start: i, End: i + len(snippet)}
	}

	words := strings.Fields(snippet)
	if len(words) == 0 {
		return nil
	}
	parts := make([]string, 0, len(words))
	for _, w := range words {
		parts = append(parts, regexp.QuoteMeta(w))
	}
	re, err := regexp.Compile(strings.Join(parts, `\s+`))
	if err != nil {
		return nil
	}
	loc := re.FindStringIndex(source)
	if loc == nil {
		return nil
	}
	return &Match{Text: source[loc[0]:loc[1]], Start: loc[0], End: loc[1]}
}
