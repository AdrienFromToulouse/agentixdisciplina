// Package extract derives Claims from an Episode (ADR-002 §4).
//
// Claims are the only inferred field in the Episode, so the extractor that
// produced them is recorded: a deterministic engine reading an llm-extracted
// claim still yields a probabilistic verdict. The structural extractor here is
// deterministic, which is why grounding clauses can block a build by default.
package extract

import (
	"regexp"
	"strings"

	"github.com/AdrienFromToulouse/agentixdisciplina/internal/episode"
)

const (
	ExtractorStructural = "structural"
	ExtractorLLM        = "llm"
)

// Token types a claim can assert. A sentence with none of these asserts
// nothing checkable and is not a claim.
const (
	TokenNumber = "number"
	TokenMoney  = "money"
	TokenDate   = "date"
	TokenID     = "id"
)

type Token struct {
	Type string
	Text string
	// Norm is the comparison form: digits only for numbers and money,
	// lowercase for everything else.
	Norm string
}

var (
	sentenceSplit = regexp.MustCompile(`(?m)(?:[.!?]+\s+|\n+)`)
	citationRe    = regexp.MustCompile(`\[\^?\d+\]|\(source:[^)]+\)|\bhttps?://\S+`)

	moneyRe  = regexp.MustCompile(`(?i)(?:[$€£]\s?\d[\d,]*(?:\.\d+)?)|(?:\b\d[\d,]*(?:\.\d+)?\s?(?:usd|eur|gbp|dollars?|euros?)\b)`)
	dateRe   = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b|\b\d{1,2}/\d{1,2}/\d{2,4}\b`)
	idRe     = regexp.MustCompile(`\b(?:[A-Za-z]+[-_]?\d[\w-]*|\d[\w-]*[A-Za-z][\w-]*)\b`)
	numberRe = regexp.MustCompile(`\b\d[\d,]*(?:\.\d+)?\b`)

	digits = regexp.MustCompile(`[^0-9.]`)
)

// Structural extracts claims deterministically: a sentence in an assistant
// turn that asserts at least one concrete value.
//
// Support is established by value provenance: a claim is supported when the
// concrete values it asserts appear in a tool result that completed before the
// turn was produced. Explicit citation markers count too.
func Structural(ep *episode.Episode) []episode.Claim {
	var out []episode.Claim

	for _, turn := range ep.Turns {
		if turn.Role != "assistant" || !turn.Captured || turn.Text == "" {
			continue
		}
		for _, sentence := range splitSentences(turn.Text) {
			toks := Tokens(sentence)
			hasCitation := citationRe.MatchString(sentence)
			if len(toks) == 0 && !hasCitation {
				continue // asserts nothing checkable
			}

			claim := episode.Claim{
				Index:     len(out),
				Text:      sentence,
				Extractor: ExtractorStructural,
				Turn:      turn.Index,
			}
			for _, tc := range supportingCalls(ep, turn, toks) {
				claim.Support = append(claim.Support, tc)
			}
			// A citation marker is support the agent asserted for itself.
			if hasCitation && len(claim.Support) == 0 {
				claim.Support = append(claim.Support, turn.Span)
			}
			out = append(out, claim)
		}
	}
	return out
}

// Unsupported returns the tokens in a claim that appear in no prior tool
// result, restricted to the requested types.
func Unsupported(ep *episode.Episode, claim episode.Claim, types []string) []Token {
	if len(types) == 0 {
		types = []string{TokenNumber, TokenMoney, TokenDate, TokenID}
	}
	want := map[string]bool{}
	for _, t := range types {
		want[t] = true
	}

	turn := turnByIndex(ep, claim.Turn)
	var out []Token
	for _, tok := range Tokens(claim.Text) {
		if !want[tok.Type] {
			continue
		}
		if !valueAppearsBefore(ep, turn, tok) {
			out = append(out, tok)
		}
	}
	return out
}

// Tokens returns the concrete values a sentence asserts. Each span of text is
// claimed by the most specific matcher, so "$240" is money rather than also
// counting as a bare number.
func Tokens(s string) []Token {
	claimed := make([]bool, len(s)+1)
	var out []Token

	take := func(re *regexp.Regexp, typ string) {
		for _, loc := range re.FindAllStringIndex(s, -1) {
			if overlaps(claimed, loc[0], loc[1]) {
				continue
			}
			text := s[loc[0]:loc[1]]
			out = append(out, Token{Type: typ, Text: text, Norm: normalize(typ, text)})
			for i := loc[0]; i < loc[1]; i++ {
				claimed[i] = true
			}
		}
	}

	take(moneyRe, TokenMoney)
	take(dateRe, TokenDate)
	take(idRe, TokenID)
	take(numberRe, TokenNumber)
	return out
}

func supportingCalls(ep *episode.Episode, turn episode.Turn, toks []Token) []episode.SpanRef {
	if len(toks) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []episode.SpanRef
	for _, tc := range ep.ToolCalls {
		if !completedBefore(tc, turn) || !tc.ResultCaptured {
			continue
		}
		hay := strings.ToLower(tc.Result)
		for _, tok := range toks {
			if tok.Norm == "" || !strings.Contains(hay, tok.Norm) {
				continue
			}
			if seen[tc.Span.SpanID] {
				break
			}
			seen[tc.Span.SpanID] = true
			out = append(out, tc.Span)
			break
		}
	}
	return out
}

func valueAppearsBefore(ep *episode.Episode, turn episode.Turn, tok Token) bool {
	if tok.Norm == "" {
		return true // nothing comparable; do not accuse
	}
	for _, tc := range ep.ToolCalls {
		if !completedBefore(tc, turn) {
			continue
		}
		if tc.ResultCaptured && strings.Contains(strings.ToLower(tc.Result), tok.Norm) {
			return true
		}
		// A value the agent itself passed in is sourced from the
		// conversation, not invented.
		if tc.ArgsCaptured && strings.Contains(strings.ToLower(tc.Arguments), tok.Norm) {
			return true
		}
	}
	// A value the user supplied is not invented either.
	for _, t := range ep.Turns {
		if t.Role == "user" && t.Index < turn.Index &&
			strings.Contains(strings.ToLower(t.Text), tok.Norm) {
			return true
		}
	}
	return false
}

// completedBefore reports whether a tool call finished before the model call
// that produced this turn began. Overlapping spans are unordered, matching the
// rule the Rego ordering clauses use (ADR-003 §2).
//
// A turn can only be grounded in results that existed when it was written:
// without this check, a claim would count a *later* tool call as its source.
func completedBefore(tc episode.ToolCall, turn episode.Turn) bool {
	if tc.EndedAt == 0 || turn.StartedAt == 0 {
		return false
	}
	return tc.EndedAt <= turn.StartedAt
}

func turnByIndex(ep *episode.Episode, i int) episode.Turn {
	if i >= 0 && i < len(ep.Turns) {
		return ep.Turns[i]
	}
	return episode.Turn{Index: i}
}

func splitSentences(text string) []string {
	var out []string
	for _, s := range sentenceSplit.Split(text, -1) {
		s = strings.TrimSpace(s)
		if len(s) > 0 {
			out = append(out, s)
		}
	}
	return out
}

func overlaps(claimed []bool, start, end int) bool {
	for i := start; i < end && i < len(claimed); i++ {
		if claimed[i] {
			return true
		}
	}
	return false
}

func normalize(typ, text string) string {
	switch typ {
	case TokenNumber, TokenMoney:
		d := digits.ReplaceAllString(text, "")
		return strings.TrimSuffix(strings.TrimSuffix(d, "."), ".0")
	default:
		return strings.ToLower(strings.TrimSpace(text))
	}
}
