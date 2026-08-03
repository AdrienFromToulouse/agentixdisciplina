package contract

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/AdrienFromToulouse/agentixdisciplina/internal/detect"
	cueeng "github.com/AdrienFromToulouse/agentixdisciplina/internal/engine/cue"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/engine/judge"
	regoeng "github.com/AdrienFromToulouse/agentixdisciplina/internal/engine/rego"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/episode"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/verdict"
)

type Class = verdict.Class

const (
	ClassDeterministic = verdict.Deterministic
	ClassProbabilistic = verdict.Probabilistic
)

// SkipError tells the runner a clause could not be evaluated. It exists so a
// clause can decide *at runtime* that its inputs are absent: an invariant
// whose operand never appeared in the trace is skipped, never vacuously
// passed (ADR-003 §4).
type SkipError struct{ Reasons []string }

func (e *SkipError) Error() string { return strings.Join(e.Reasons, "; ") }

// EvalContext is what a clause evaluator sees. The Episode is marshalled and
// values are bound once per run, then shared across clauses.
type EvalContext struct {
	Ctx         context.Context
	Episode     *episode.Episode
	EpisodeJSON map[string]any
	Clause      Clause
	Bindings    map[string]Binding
	Evidence    verdict.EvidenceMode
	Rego        *regoeng.Engine
	CUE         *cueeng.Evaluator
	Judge       *judge.Judge
	// ClaimsExtractor names the extractor that produced Episode.Claims.
	ClaimsExtractor string
	// Provenance is a per-clause scratch map the runner copies onto the
	// verdict. Probabilistic clauses write their model id, prompt hash,
	// and effort here.
	Provenance map[string]string
}

// Kind is a registered clause definition.
//
// Every kind is a *violation predicate*: Eval returns what is wrong, and an
// empty result is a pass. Aliases carry polarity (`expose_pii` resolves to
// `content.no_pii`), so v0 does not implement the general must_not inversion
// described in ADR-003 §3: Positions guards nonsense placements instead.
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
	// ReadsClaims marks a kind whose verdict inherits the claim extractor's
	// provenance. With the llm extractor that makes a failure deterministic
	// (the quote was verified) and a pass probabilistic (the extractor may
	// have missed something) — ADR-008 §2.
	ReadsClaims bool
	Eval        func(EvalContext) ([]verdict.Finding, error)
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

// regoClause adapts an embedded Rego rule to the clause interface.
func regoClause(query string) func(EvalContext) ([]verdict.Finding, error) {
	return func(ec EvalContext) ([]verdict.Finding, error) {
		found, err := ec.Rego.Eval(ec.Ctx, query, regoeng.Input{
			Episode: ec.EpisodeJSON,
			Params:  ec.Clause.Params,
		})
		if err != nil {
			return nil, err
		}
		out := make([]verdict.Finding, 0, len(found))
		for _, f := range found {
			out = append(out, verdict.Finding{
				Message: f.Message,
				Evidence: verdict.Evidence{
					TraceID: f.TraceID, SpanID: f.SpanID, Path: f.Path,
				},
			})
		}
		return out, nil
	}
}

func init() {
	// ------------------------------------------------------------- Rego
	// What the agent *did*: permissions and sequencing over the tool log.

	register(&Kind{
		Name: "tool.allowlist", Aliases: []string{"allowed_tools"},
		Engine: "rego:axda.tool.allowlist_violation", Class: ClassDeterministic,
		Reads: "tool_calls[].name", DefaultSeverity: "critical",
		Positions: []string{"spec", "must"}, PrefixDecidable: true,
		RequiredParams: []string{"tools"},
		Eval:           regoClause("data.axda.tool.allowlist_violation"),
	})

	register(&Kind{
		Name: "tool.denylist", Aliases: []string{"denied_tools"},
		Engine: "rego:axda.tool.denylist_violation", Class: ClassDeterministic,
		Reads: "tool_calls[].name", DefaultSeverity: "critical",
		Positions: []string{"spec", "must", "must_not"}, PrefixDecidable: true,
		RequiredParams: []string{"tools"},
		Eval:           regoClause("data.axda.tool.denylist_violation"),
	})

	register(&Kind{
		Name: "tool.call_limit", Engine: "rego:axda.tool.call_limit_violation",
		Class: ClassDeterministic,
		Reads: "tool_calls[]", DefaultSeverity: "major",
		Positions: []string{"must"}, PrefixDecidable: true,
		RequiredParams: []string{"max"},
		Eval:           regoClause("data.axda.tool.call_limit_violation"),
	})

	register(&Kind{
		Name: "order.requires_precondition", Aliases: []string{"verify_identity_before_action"},
		Engine: "rego:axda.order.requires_precondition_violation", Class: ClassDeterministic,
		Reads: "tool_calls[] (ordered)", DefaultSeverity: "critical",
		Positions: []string{"must"}, PrefixDecidable: true,
		RequiredParams: []string{"action", "precondition"},
		Eval:           regoClause("data.axda.order.requires_precondition_violation"),
	})

	register(&Kind{
		Name: "order.before", Engine: "rego:axda.order.before_violation",
		Class: ClassDeterministic,
		Reads: "tool_calls[] (ordered)", DefaultSeverity: "major",
		Positions: []string{"must"}, PrefixDecidable: true,
		RequiredParams: []string{"first", "then"},
		Eval:           regoClause("data.axda.order.before_violation"),
	})

	// -------------------------------------------------------------- CUE
	// What the agent *believed*: value consistency under unification.

	register(&Kind{
		Name: "invariant", Engine: "cue", Class: ClassDeterministic,
		Reads: "declared values (spec.values)", DefaultSeverity: "critical",
		Positions: []string{"spec"}, PrefixDecidable: true,
		RequiredParams: []string{"expr"},
		Eval:           evalInvariant,
	})

	register(&Kind{
		Name: "tool.args_match", Engine: "cue", Class: ClassDeterministic,
		Requires: []string{episode.HasToolArgs},
		Reads:    "tool_calls[].arguments", DefaultSeverity: "major",
		Positions: []string{"must"}, PrefixDecidable: true,
		RequiredParams: []string{"tool", "schema"},
		Eval:           evalArgsMatch,
	})

	// ---------------------------------------------------------- builtin
	// Detection that is not expressible as policy: regex and Luhn.

	register(&Kind{
		Name: "content.no_pii", Aliases: []string{"expose_pii"},
		Engine: "builtin:axda.pii", Class: ClassDeterministic,
		Requires: []string{episode.HasMessageContent},
		Reads:    "turns[].text, tool_calls[].arguments", DefaultSeverity: "critical",
		Positions: []string{"must", "must_not"}, PrefixDecidable: true,
		Eval: evalNoPII,
	})

	register(&Kind{
		Name: "content.deny_patterns", Engine: "builtin:axda.content", Class: ClassDeterministic,
		Requires: []string{episode.HasMessageContent},
		Reads:    "turns[].text", DefaultSeverity: "major",
		Positions: []string{"must", "must_not"}, PrefixDecidable: true,
		RequiredParams: []string{"patterns"},
		Eval:           evalDenyPatterns,
	})

	// ----------------------------------------------------------- metric

	budget := func(name, reads string, requires []string, get func(episode.Metrics) int64, unit string) *Kind {
		return &Kind{
			Name: name, Engine: "metric", Class: ClassDeterministic,
			Requires: requires, Reads: reads, DefaultSeverity: "major",
			Positions: []string{"must"}, PrefixDecidable: true,
			RequiredParams: []string{"value"},
			Eval: func(ec EvalContext) ([]verdict.Finding, error) {
				limit := int64(intParam(ec.Clause.Params["value"], -1))
				actual := get(ec.Episode.Metrics)
				if limit >= 0 && actual > limit {
					return []verdict.Finding{{
						Message: fmt.Sprintf("%d %s exceeds limit of %d", actual, unit, limit),
						Evidence: verdict.Evidence{
							TraceID: ec.Episode.Meta.TraceID, Path: "metrics",
						},
					}}, nil
				}
				return nil, nil
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

// evalInvariant checks a CUE constraint over declared values, once per
// combination of `any`-cardinality bindings. Referenced names were resolved
// at compile time (Clause.Refs), so nothing is re-parsed here.
func evalInvariant(ec EvalContext) ([]verdict.Finding, error) {
	expr, _ := ec.Clause.Params["expr"].(string)
	names := ec.Clause.Refs

	// A missing operand makes the constraint unevaluable, not satisfied.
	var reasons []string
	for _, n := range names {
		if b, ok := ec.Bindings[n]; ok && b.Missing {
			reasons = append(reasons, b.Reason)
		}
	}
	if len(reasons) > 0 {
		return nil, &SkipError{Reasons: reasons}
	}

	// A cardinality mismatch is a failure in its own right: the contract
	// asserted a count and the trace disagreed.
	var out []verdict.Finding
	for _, n := range names {
		b := ec.Bindings[n]
		if b.CardinalityViolation != "" {
			out = append(out, verdict.Finding{
				Message: b.CardinalityViolation,
				Evidence: verdict.Evidence{
					TraceID: b.Span.TraceID, SpanID: b.Span.SpanID, Path: b.Span.Path,
				},
			})
		}
	}
	if len(out) > 0 {
		return out, nil
	}

	combos, err := Combinations(ec.Bindings, names)
	if err != nil {
		return nil, err
	}
	for _, c := range combos {
		ok, err := ec.CUE.Constraint(expr, c.Vars)
		if err != nil {
			return nil, err
		}
		if ok {
			continue
		}
		out = append(out, verdict.Finding{
			Message: fmt.Sprintf("invariant %q does not hold where %s", expr, formatVars(c.Vars, names)),
			Evidence: verdict.Evidence{
				TraceID: c.Span.TraceID, SpanID: c.Span.SpanID, Path: c.Span.Path,
			},
		})
	}
	return out, nil
}

func evalArgsMatch(ec EvalContext) ([]verdict.Finding, error) {
	toolPattern, _ := ec.Clause.Params["tool"].(string)
	schema, _ := ec.Clause.Params["schema"].(string)

	var out []verdict.Finding
	for _, tc := range ec.Episode.ToolCalls {
		if !matchName(toolPattern, tc.Name) || !tc.ArgsCaptured {
			continue
		}
		var args any
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			continue
		}
		if err := ec.CUE.UnifySchema(schema, args); err != nil {
			out = append(out, verdict.Finding{
				Message: fmt.Sprintf("arguments to %q do not match the declared schema: %s",
					tc.Name, firstLine(err.Error())),
				Evidence: verdict.Evidence{
					TraceID: tc.Span.TraceID, SpanID: tc.Span.SpanID, Path: tc.Span.Path,
				},
			})
		}
	}
	return out, nil
}

func evalNoPII(ec EvalContext) ([]verdict.Finding, error) {
	types := strSlice(ec.Clause.Params["types"])
	// Sending a card number to billing.charge is the job; policies express
	// where PII may travel, not merely whether it appears.
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
	return out, nil
}

func evalDenyPatterns(ec EvalContext) ([]verdict.Finding, error) {
	var out []verdict.Finding
	for _, p := range strSlice(ec.Clause.Params["patterns"]) {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w", p, err)
		}
		for _, t := range ec.Episode.Turns {
			loc := re.FindStringIndex(t.Text)
			if loc == nil {
				continue
			}
			m := detect.Match{Type: "pattern", Start: loc[0], End: loc[1], Raw: t.Text[loc[0]:loc[1]]}
			// Trailing context is dropped unless --evidence=full: the value
			// a pattern like `password\s*[:=]` catches sits after the match,
			// so printing it would leak the secret this clause exists to find.
			ex := ""
			switch ec.Evidence {
			case verdict.EvidenceFull:
				ex = detect.Excerpt(t.Text, m, false)
			case verdict.EvidenceMasked:
				ex = detect.ExcerptLeading(t.Text, m)
			}
			out = append(out, verdict.Finding{
				Message:  fmt.Sprintf("denied pattern %q matched in %s turn", p, t.Role),
				Evidence: ev(t.Span, ex),
			})
		}
	}
	return out, nil
}

func formatVars(vars map[string]any, names []string) string {
	parts := make([]string, 0, len(names))
	for _, n := range names {
		if v, ok := vars[n]; ok {
			parts = append(parts, fmt.Sprintf("%s=%v", n, v))
		}
	}
	return strings.Join(parts, ", ")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
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

// matchName supports exact names and a trailing/leading `*` wildcard. Kept in
// sync with data.axda.match in policy/match.rego.
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
