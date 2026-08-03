package contract

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	cueeng "github.com/AdrienFromToulouse/agentixdisciplina/internal/engine/cue"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/episode"
)

// ValueSpec declares how to extract a named value from an Episode.
//
// Invariants operate over these, never over identifiers guessed from the
// trace. An undeclared identifier is a compile error (ADR-003 §4).
type ValueSpec struct {
	From        string `yaml:"from"`   // tool_call | tool_result | metric | literal
	Tool        string `yaml:"tool"`   // tool name or wildcard
	Arg         string `yaml:"arg"`    // argument name (from: tool_call)
	Path        string `yaml:"path"`   // $.a.b[0].c into args or result
	Metric      string `yaml:"metric"` // metric field (from: metric)
	Literal     any    `yaml:"literal"`
	Cardinality string `yaml:"cardinality"` // any | first | last | exactly_one
	Default     any    `yaml:"default"`
	HasDefault  bool   `yaml:"-"`
}

// Bound is one extracted value with the span it came from.
type Bound struct {
	Value any
	Span  episode.SpanRef
}

// Binding is a value spec resolved against a specific Episode.
type Binding struct {
	Name string
	// Values holds every value that survived cardinality resolution. For
	// `any` this may be several; the constraint must hold for all of them.
	Values []Bound
	// Missing is set when nothing matched and no default was declared. The
	// clause then reports SKIPPED: never a vacuous pass (ADR-003 §4).
	Missing bool
	Reason  string
	// CardinalityViolation is set when `exactly_one` did not match exactly
	// once. That is a real failure, not a skip: the contract asserted a
	// count and the trace disagreed.
	CardinalityViolation string
	Span                 episode.SpanRef
	// Varies is set for `any` cardinality. Evidence for a failed constraint
	// should anchor to the value that iterates: the refund that was too
	// large, not the lookup that supplied the limit.
	Varies bool
}

const (
	CardAny        = "any"
	CardFirst      = "first"
	CardLast       = "last"
	CardExactlyOne = "exactly_one"
)

func validCardinality(c string) bool {
	switch c {
	case CardAny, CardFirst, CardLast, CardExactlyOne:
		return true
	}
	return false
}

// validateValueSpec checks a declaration at compile time, before any trace is
// read.
func validateValueSpec(name string, s ValueSpec) error {
	if !validCardinality(s.Cardinality) {
		return fmt.Errorf("value %q: cardinality must be one of any, first, last, exactly_one (got %q)\n"+
			"  cardinality is required, not defaulted: a policy that silently checks only the first of five\n"+
			"  refunds is the exact bug this field exists to prevent", name, s.Cardinality)
	}
	switch s.From {
	case "tool_call":
		if s.Tool == "" {
			return fmt.Errorf("value %q: from: tool_call requires `tool`", name)
		}
		if s.Arg == "" && s.Path == "" {
			return fmt.Errorf("value %q: from: tool_call requires `arg` or `path`", name)
		}
	case "tool_result":
		if s.Tool == "" {
			return fmt.Errorf("value %q: from: tool_result requires `tool`", name)
		}
		if s.Path == "" {
			return fmt.Errorf("value %q: from: tool_result requires `path`", name)
		}
	case "metric":
		if s.Metric == "" {
			return fmt.Errorf("value %q: from: metric requires `metric`", name)
		}
		if !knownMetric(s.Metric) {
			return fmt.Errorf("value %q: unknown metric %q (known: %s)",
				name, s.Metric, strings.Join(knownMetrics(), ", "))
		}
	case "literal":
		if s.Literal == nil && !s.HasDefault {
			return fmt.Errorf("value %q: from: literal requires `literal`", name)
		}
	case "":
		return fmt.Errorf("value %q: `from` is required (tool_call, tool_result, metric, literal)", name)
	default:
		return fmt.Errorf("value %q: unknown source %q", name, s.From)
	}
	return nil
}

// Bind resolves every declared value against an Episode.
func Bind(ep *episode.Episode, specs map[string]ValueSpec) map[string]Binding {
	out := make(map[string]Binding, len(specs))
	for name, spec := range specs {
		out[name] = bindOne(ep, name, spec)
	}
	return out
}

func bindOne(ep *episode.Episode, name string, spec ValueSpec) Binding {
	b := Binding{Name: name, Varies: spec.Cardinality == CardAny}
	var raw []Bound
	var uncaptured int

	switch spec.From {
	case "literal":
		raw = append(raw, Bound{Value: spec.Literal, Span: episode.SpanRef{
			TraceID: ep.Meta.TraceID, Path: "literal"}})

	case "metric":
		v, ok := metricValue(ep.Metrics, spec.Metric)
		if ok {
			raw = append(raw, Bound{Value: v, Span: episode.SpanRef{
				TraceID: ep.Meta.TraceID, Path: "metrics." + spec.Metric}})
		}

	case "tool_call":
		for _, tc := range ep.ToolCalls {
			if !matchName(spec.Tool, tc.Name) {
				continue
			}
			if !tc.ArgsCaptured {
				uncaptured++
				continue
			}
			if v, ok := extractFrom(tc.Arguments, spec.Arg, spec.Path); ok {
				raw = append(raw, Bound{Value: v, Span: tc.Span})
			}
		}

	case "tool_result":
		for _, tc := range ep.ToolCalls {
			if !matchName(spec.Tool, tc.Name) {
				continue
			}
			if !tc.ResultCaptured {
				uncaptured++
				continue
			}
			if v, ok := extractFrom(tc.Result, "", spec.Path); ok {
				raw = append(raw, Bound{Value: v, Span: tc.Span})
			}
		}
	}

	// Cardinality is applied before the missing/default check so
	// exactly_one can distinguish "none" from "several".
	switch spec.Cardinality {
	case CardFirst:
		if len(raw) > 1 {
			raw = raw[:1]
		}
	case CardLast:
		if len(raw) > 1 {
			raw = raw[len(raw)-1:]
		}
	case CardExactlyOne:
		if len(raw) > 1 {
			b.CardinalityViolation = fmt.Sprintf(
				"value %q declares cardinality exactly_one but matched %d times", name, len(raw))
			b.Span = raw[0].Span
			b.Values = raw[:1]
			return b
		}
	}

	if len(raw) == 0 {
		if spec.HasDefault {
			b.Values = []Bound{{Value: spec.Default, Span: episode.SpanRef{
				TraceID: ep.Meta.TraceID, Path: "default"}}}
			return b
		}
		b.Missing = true
		switch {
		case uncaptured > 0:
			b.Reason = fmt.Sprintf("value %q: %d matching call(s) but %s not captured in this trace",
				name, uncaptured, captureNoun(spec.From))
		default:
			b.Reason = fmt.Sprintf("value %q: nothing in this trace matched %s", name, describeSource(spec))
		}
		return b
	}

	b.Values = raw
	return b
}

func captureNoun(from string) string {
	if from == "tool_result" {
		return "results were"
	}
	return "arguments were"
}

func describeSource(s ValueSpec) string {
	switch s.From {
	case "tool_call":
		if s.Arg != "" {
			return fmt.Sprintf("tool_call(%s).arg(%s)", s.Tool, s.Arg)
		}
		return fmt.Sprintf("tool_call(%s).%s", s.Tool, s.Path)
	case "tool_result":
		return fmt.Sprintf("tool_result(%s).%s", s.Tool, s.Path)
	case "metric":
		return "metric." + s.Metric
	}
	return s.From
}

// extractFrom pulls a value out of a JSON document by argument name or path.
func extractFrom(doc, arg, path string) (any, bool) {
	if strings.TrimSpace(doc) == "" {
		return nil, false
	}
	var data any
	if err := json.Unmarshal([]byte(doc), &data); err != nil {
		return nil, false
	}
	if arg != "" {
		m, ok := data.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok := m[arg]
		return v, ok
	}
	return resolvePath(data, path)
}

// resolvePath walks a `$.a.b[0].c` selector. Deliberately small: a full
// JSONPath implementation would be another dependency for a feature whose
// whole point is being explicit and readable.
func resolvePath(data any, path string) (any, bool) {
	p := strings.TrimPrefix(strings.TrimPrefix(path, "$"), ".")
	if p == "" {
		return data, true
	}
	cur := data
	for _, seg := range strings.Split(p, ".") {
		name, indices := parseSegment(seg)
		if name != "" {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, false
			}
			cur, ok = m[name]
			if !ok {
				return nil, false
			}
		}
		for _, idx := range indices {
			arr, ok := cur.([]any)
			if !ok || idx < 0 || idx >= len(arr) {
				return nil, false
			}
			cur = arr[idx]
		}
	}
	return cur, true
}

func parseSegment(seg string) (string, []int) {
	name := seg
	var indices []int
	if i := strings.IndexByte(seg, '['); i >= 0 {
		name = seg[:i]
		rest := seg[i:]
		for len(rest) > 0 && rest[0] == '[' {
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				break
			}
			n, err := strconv.Atoi(rest[1:end])
			if err != nil {
				break
			}
			indices = append(indices, n)
			rest = rest[end+1:]
		}
	}
	return name, indices
}

func metricValue(m episode.Metrics, name string) (any, bool) {
	switch name {
	case "duration_ms":
		return m.DurationMS, true
	case "latency_p50_ms":
		return m.LatencyP50MS, true
	case "latency_p95_ms":
		return m.LatencyP95MS, true
	case "input_tokens":
		return m.InputTokens, true
	case "output_tokens":
		return m.OutputTokens, true
	case "total_tokens":
		return m.InputTokens + m.OutputTokens, true
	case "model_calls":
		return m.ModelCalls, true
	case "tool_calls":
		return m.ToolCalls, true
	case "tool_errors":
		return m.ToolErrors, true
	case "steps":
		return m.Steps, true
	}
	return nil, false
}

func knownMetrics() []string {
	return []string{"duration_ms", "latency_p50_ms", "latency_p95_ms",
		"input_tokens", "output_tokens", "total_tokens",
		"model_calls", "tool_calls", "tool_errors", "steps"}
}

func knownMetric(name string) bool {
	for _, m := range knownMetrics() {
		if m == name {
			return true
		}
	}
	return false
}

// ReferencedNames maps the root identifiers in an expression back to the
// declared value names that supply them.
//
// An expression says `refund.amount`, whose root is `refund`, but bindings are
// keyed by the full declared name. Resolving through the root is what connects
// the two.
func ReferencedNames(expr string, declared []string) []string {
	roots := map[string]bool{}
	for _, r := range cueeng.RootIdentifiers(expr) {
		roots[r] = true
	}
	var out []string
	for _, n := range declared {
		root := n
		if i := strings.IndexByte(root, '.'); i >= 0 {
			root = root[:i]
		}
		if roots[root] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// Combinations produces the cross product of bindings, so a constraint over an
// `any`-cardinality value is checked against every matching occurrence.
//
// Capped rather than unbounded: a contract binding three `any` values over a
// long episode would otherwise blow up silently.
const maxCombinations = 1024

type Combination struct {
	Vars map[string]any
	Span episode.SpanRef
}

func Combinations(bindings map[string]Binding, names []string) ([]Combination, error) {
	out := []Combination{{Vars: map[string]any{}}}
	for _, n := range names {
		b, ok := bindings[n]
		if !ok {
			return nil, fmt.Errorf("value %q is not bound", n)
		}
		var next []Combination
		for _, base := range out {
			for _, val := range b.Values {
				vars := make(map[string]any, len(base.Vars)+1)
				for k, v := range base.Vars {
					vars[k] = v
				}
				vars[n] = val.Value
				// Anchor evidence to the iterating value where there is one,
				// so a failed constraint points at the action that broke it
				// rather than at whichever operand happened to bind first.
				span := base.Span
				if b.Varies || span.SpanID == "" {
					span = val.Span
				}
				next = append(next, Combination{Vars: vars, Span: span})
			}
		}
		if len(next) > maxCombinations {
			return nil, fmt.Errorf("constraint expands to more than %d value combinations; "+
				"narrow a binding with cardinality first/last/exactly_one", maxCombinations)
		}
		out = next
	}
	return out, nil
}
