package contract

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/AdrienFromToulouse/agentixdisciplina/internal/detect"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/episode"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/verdict"
)

type Class = verdict.Class

const (
	ClassDeterministic = verdict.Deterministic
	ClassProbabilistic = verdict.Probabilistic
)

// EvalContext is what a clause evaluator sees.
type EvalContext struct {
	Episode  *episode.Episode
	Clause   Clause
	Evidence verdict.EvidenceMode
}

// Kind is a registered clause definition.
//
// Every kind is a *violation predicate*: Eval returns what is wrong, and an
// empty result is a pass. Aliases carry polarity (`expose_pii` resolves to
// `content.no_pii`), so v0 does not implement the general must_not inversion
// described in ADR-003 §3 — Positions guards nonsense placements instead.
type Kind struct {
	Name            string
	Aliases         []string
	Engine          string
	Class           Class
	Requires        []string
	Reads           string
	DefaultSeverity string
	Positions       []string // spec | must | must_not
	PrefixDecidable bool
	RequiredParams  []string
	Eval            func(EvalContext) []verdict.Finding
}

func (k *Kind) allows(position string) bool {
	for _, p := range k.Positions {
		if p == position {
			return true
		}
	}
	return false
}

func (k *Kind) validate(c Clause) error {
	if !k.allows(c.Position) {
		return fmt.Errorf("clause %q cannot be used under %s (allowed: %s)",
			k.Name, c.Position, strings.Join(k.Positions, ", "))
	}
	for _, p := range k.RequiredParams {
		if _, ok := c.Params[p]; !ok {
			return fmt.Errorf("clause %q requires parameter %q\n  expand the shorthand, e.g.\n    - kind: %s\n      %s: <value>",
				k.Name, p, k.Name, p)
		}
	}
	return nil
}

var registry = map[string]*Kind{}
var aliases = map[string]string{}

func register(k *Kind) {
	registry[k.Name] = k
	for _, a := range k.Aliases {
		aliases[a] = k.Name
	}
}

func Lookup(name string) *Kind {
	if k, ok := registry[name]; ok {
		return k
	}
	if real, ok := aliases[name]; ok {
		return registry[real]
	}
	return nil
}

func KnownNames() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func init() {
	register(&Kind{
		Name: "tool.allowlist", Aliases: []string{"allowed_tools"},
		Engine: "builtin:axda.tools", Class: ClassDeterministic,
		Reads: "tool_calls[].name", DefaultSeverity: "critical",
		Positions: []string{"spec", "must"}, PrefixDecidable: true,
		RequiredParams: []string{"tools"},
		Eval: func(ec EvalContext) []verdict.Finding {
			allowed := strSlice(ec.Clause.Params["tools"])
			var out []verdict.Finding
			for _, tc := range ec.Episode.ToolCalls {
				if anyMatch(allowed, tc.Name) {
					continue
				}
				noun := "tool"
				if tc.Kind == "agent" {
					noun = "sub-agent delegation"
				}
				out = append(out, verdict.Finding{
					Message:  fmt.Sprintf("%s %q is not in the allowed set", noun, tc.Name),
					Evidence: ev(tc.Span, ""),
				})
			}
			return out
		},
	})

	register(&Kind{
		Name: "tool.denylist", Aliases: []string{"denied_tools"},
		Engine: "builtin:axda.tools", Class: ClassDeterministic,
		Reads: "tool_calls[].name", DefaultSeverity: "critical",
		Positions: []string{"spec", "must", "must_not"}, PrefixDecidable: true,
		RequiredParams: []string{"tools"},
		Eval: func(ec EvalContext) []verdict.Finding {
			denied := strSlice(ec.Clause.Params["tools"])
			var out []verdict.Finding
			for _, tc := range ec.Episode.ToolCalls {
				if anyMatch(denied, tc.Name) {
					out = append(out, verdict.Finding{
						Message:  fmt.Sprintf("tool %q is denied", tc.Name),
						Evidence: ev(tc.Span, ""),
					})
				}
			}
			return out
		},
	})

	register(&Kind{
		Name: "tool.call_limit", Engine: "builtin:axda.tools", Class: ClassDeterministic,
		Reads: "tool_calls[]", DefaultSeverity: "major",
		Positions: []string{"must"}, PrefixDecidable: true,
		RequiredParams: []string{"max"},
		Eval: func(ec EvalContext) []verdict.Finding {
			max := intParam(ec.Clause.Params["max"], -1)
			pattern, _ := ec.Clause.Params["tool"].(string)
			var count int
			var last episode.SpanRef
			for _, tc := range ec.Episode.ToolCalls {
				if pattern == "" || matchName(pattern, tc.Name) {
					count++
					last = tc.Span
				}
			}
			if max >= 0 && count > max {
				label := "tool calls"
				if pattern != "" {
					label = fmt.Sprintf("calls to %q", pattern)
				}
				return []verdict.Finding{{
					Message:  fmt.Sprintf("%d %s exceeds limit of %d", count, label, max),
					Evidence: ev(last, ""),
				}}
			}
			return nil
		},
	})

	register(&Kind{
		Name: "order.requires_precondition", Aliases: []string{"verify_identity_before_action"},
		Engine: "builtin:axda.order", Class: ClassDeterministic,
		Reads: "tool_calls[] (ordered)", DefaultSeverity: "critical",
		Positions: []string{"must"}, PrefixDecidable: true,
		RequiredParams: []string{"action", "precondition"},
		Eval: func(ec EvalContext) []verdict.Finding {
			action, _ := ec.Clause.Params["action"].(string)
			pre, _ := ec.Clause.Params["precondition"].(string)
			var out []verdict.Finding
			for _, tc := range ec.Episode.ToolCalls {
				if !matchName(action, tc.Name) {
					continue
				}
				if hasPrecondition(ec.Episode.ToolCalls, pre, tc) {
					continue
				}
				out = append(out, verdict.Finding{
					Message: fmt.Sprintf("%q ran with no completed %q before it", tc.Name, pre),
					Evidence: ev(tc.Span, ""),
				})
			}
			return out
		},
	})

	register(&Kind{
		Name: "content.no_pii", Aliases: []string{"expose_pii"},
		Engine: "builtin:axda.pii", Class: ClassDeterministic,
		Requires: []string{episode.HasMessageContent},
		Reads:    "turns[].text, tool_calls[].arguments", DefaultSeverity: "critical",
		Positions: []string{"must", "must_not"}, PrefixDecidable: true,
		Eval: func(ec EvalContext) []verdict.Finding {
			types := strSlice(ec.Clause.Params["types"])
			// Sending a card number to billing.charge is the job; policies
			// express where PII may travel, not merely whether it appears.
			allowIn := strSlice(ec.Clause.Params["allow_in_tool_args"])
			masked := ec.Evidence != verdict.EvidenceFull

			var out []verdict.Finding
			for _, t := range ec.Episode.Turns {
				for _, m := range detect.PII(t.Text, types) {
					out = append(out, verdict.Finding{
						Message:  fmt.Sprintf("%s exposed in %s turn", m.Type, t.Role),
						Evidence: ev(t.Span, excerpt(ec, t.Text, m, masked)),
					})
				}
			}
			for _, tc := range ec.Episode.ToolCalls {
				if tc.Arguments == "" || anyMatch(allowIn, tc.Name) {
					continue
				}
				for _, m := range detect.PII(tc.Arguments, types) {
					out = append(out, verdict.Finding{
						Message:  fmt.Sprintf("%s in arguments to %q", m.Type, tc.Name),
						Evidence: ev(tc.Span, excerpt(ec, tc.Arguments, m, masked)),
					})
				}
			}
			return out
		},
	})

	register(&Kind{
		Name: "content.deny_patterns", Engine: "builtin:axda.content", Class: ClassDeterministic,
		Requires: []string{episode.HasMessageContent},
		Reads:    "turns[].text", DefaultSeverity: "major",
		Positions: []string{"must", "must_not"}, PrefixDecidable: true,
		RequiredParams: []string{"patterns"},
		Eval: func(ec EvalContext) []verdict.Finding {
			var out []verdict.Finding
			for _, p := range strSlice(ec.Clause.Params["patterns"]) {
				re, err := regexp.Compile(p)
				if err != nil {
					continue // rejected at validate time in a later revision
				}
				for _, t := range ec.Episode.Turns {
					if loc := re.FindStringIndex(t.Text); loc != nil {
						m := detect.Match{Type: "pattern", Start: loc[0], End: loc[1], Raw: t.Text[loc[0]:loc[1]]}
						// Trailing context is dropped unless --evidence=full:
						// the value a pattern like `password\s*[:=]` catches
						// sits after the match, so printing it would leak the
						// secret this clause exists to find.
						ex := ""
						if ec.Evidence == verdict.EvidenceFull {
							ex = detect.Excerpt(t.Text, m, false)
						} else if ec.Evidence == verdict.EvidenceMasked {
							ex = detect.ExcerptLeading(t.Text, m)
						}
						out = append(out, verdict.Finding{
							Message:  fmt.Sprintf("denied pattern %q matched in %s turn", p, t.Role),
							Evidence: ev(t.Span, ex),
						})
					}
				}
			}
			return out
		},
	})

	budget := func(name, reads string, requires []string, get func(episode.Metrics) int64, unit string) *Kind {
		return &Kind{
			Name: name, Engine: "builtin:axda.metric", Class: ClassDeterministic,
			Requires: requires, Reads: reads, DefaultSeverity: "major",
			Positions: []string{"must"}, PrefixDecidable: true,
			RequiredParams: []string{"value"},
			Eval: func(ec EvalContext) []verdict.Finding {
				limit := int64(intParam(ec.Clause.Params["value"], -1))
				actual := get(ec.Episode.Metrics)
				if limit >= 0 && actual > limit {
					return []verdict.Finding{{
						Message: fmt.Sprintf("%d %s exceeds limit of %d", actual, unit, limit),
						Evidence: verdict.Evidence{
							TraceID: ec.Episode.Meta.TraceID, Path: "metrics",
						},
					}}
				}
				return nil
			},
		}
	}
	register(budget("budget.max_duration_ms", "metrics.duration_ms", nil,
		func(m episode.Metrics) int64 { return m.DurationMS }, "ms"))
	register(budget("budget.max_steps", "metrics.steps", nil,
		func(m episode.Metrics) int64 { return int64(m.Steps) }, "steps"))
	register(budget("budget.max_tokens", "metrics.*_tokens", []string{episode.HasTokenUsage},
		func(m episode.Metrics) int64 { return m.InputTokens + m.OutputTokens }, "tokens"))
	register(budget("budget.max_tool_errors", "metrics.tool_errors", nil,
		func(m episode.Metrics) int64 { return int64(m.ToolErrors) }, "tool errors"))
}

// hasPrecondition reports whether a completed, non-overlapping precondition
// call precedes the action. Overlapping spans count as unordered (ADR-003 §2),
// so no policy depends on the arbitrary span-id tie-break.
func hasPrecondition(calls []episode.ToolCall, pattern string, action episode.ToolCall) bool {
	for _, c := range calls {
		if !matchName(pattern, c.Name) || c.Error != "" {
			continue
		}
		if c.EndedAt > 0 && c.EndedAt <= action.StartedAt {
			return true
		}
	}
	return false
}

func excerpt(ec EvalContext, text string, m detect.Match, masked bool) string {
	if ec.Evidence == verdict.EvidenceNone {
		return ""
	}
	return detect.Excerpt(text, m, masked)
}

func ev(s episode.SpanRef, excerpt string) verdict.Evidence {
	return verdict.Evidence{TraceID: s.TraceID, SpanID: s.SpanID, Path: s.Path, Excerpt: excerpt}
}

// matchName supports exact names and a trailing/leading `*` wildcard.
func matchName(pattern, name string) bool {
	switch {
	case pattern == "" || pattern == "*":
		return true
	case strings.HasPrefix(pattern, "*") && strings.HasSuffix(pattern, "*") && len(pattern) > 2:
		return strings.Contains(name, pattern[1:len(pattern)-1])
	case strings.HasPrefix(pattern, "*"):
		return strings.HasSuffix(name, pattern[1:])
	case strings.HasSuffix(pattern, "*"):
		return strings.HasPrefix(name, pattern[:len(pattern)-1])
	}
	return pattern == name
}

func anyMatch(patterns []string, name string) bool {
	for _, p := range patterns {
		if matchName(p, name) {
			return true
		}
	}
	return false
}

func strSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{t}
	}
	return nil
}

func intParam(v any, def int) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	}
	return def
}
