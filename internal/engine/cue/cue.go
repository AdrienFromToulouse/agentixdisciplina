// Package cue evaluates value constraints.
//
// CUE is the belief-layer engine: is this set of extracted values internally
// consistent, and does it satisfy the declared constraints (ADR-003 §4)? CUE
// rather than a bespoke expression language because these are exactly value
// constraints under unification, and reusing it keeps one dependency doing one
// job.
package cue

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
)

// resultField holds the evaluated constraint. The `axda` prefix is reserved in
// value names so a contract cannot collide with it.
const resultField = "axdaResult"

// ReservedPrefix may not begin a declared value name.
const ReservedPrefix = "axda"

type Evaluator struct {
	ctx *cue.Context
}

func NewEvaluator() *Evaluator {
	return &Evaluator{ctx: cuecontext.New()}
}

// Constraint evaluates a boolean expression over the given values. Names may
// be dotted (`refund.amount`); they are nested into structs before evaluation
// so the expression reads as an ordinary CUE selector.
func (e *Evaluator) Constraint(expr string, vars map[string]any) (bool, error) {
	nested, err := nest(vars)
	if err != nil {
		return false, err
	}

	var b strings.Builder
	roots := make([]string, 0, len(nested))
	for k := range nested {
		roots = append(roots, k)
	}
	sort.Strings(roots) // deterministic source text
	for _, k := range roots {
		enc, err := json.Marshal(nested[k])
		if err != nil {
			return false, fmt.Errorf("encode value %q: %w", k, err)
		}
		fmt.Fprintf(&b, "%s: %s\n", k, enc)
	}
	fmt.Fprintf(&b, "%s: %s\n", resultField, expr)

	v := e.ctx.CompileString(b.String())
	if err := v.Err(); err != nil {
		return false, fmt.Errorf("invalid constraint %q: %w", expr, err)
	}
	res := v.LookupPath(cue.ParsePath(resultField))
	if err := res.Err(); err != nil {
		return false, fmt.Errorf("evaluate %q: %w", expr, err)
	}
	ok, err := res.Bool()
	if err != nil {
		return false, fmt.Errorf("constraint %q did not evaluate to a boolean: %w", expr, err)
	}
	return ok, nil
}

// UnifySchema validates data against a CUE schema, used by tool.args_match.
func (e *Evaluator) UnifySchema(schema string, data any) error {
	s := e.ctx.CompileString(schema)
	if err := s.Err(); err != nil {
		return fmt.Errorf("invalid schema: %w", err)
	}
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	d := e.ctx.CompileBytes(b)
	if err := d.Err(); err != nil {
		return fmt.Errorf("invalid data: %w", err)
	}
	return s.Unify(d).Validate(cue.Concrete(true))
}

var (
	identRe   = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*`)
	stringRe  = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'`)
	selectorR = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// reserved are CUE keywords and builtins that look like identifiers but are
// not value references.
var reserved = map[string]bool{
	"if": true, "for": true, "in": true, "let": true, "import": true,
	"package": true, "true": true, "false": true, "null": true,
	"and": true, "or": true, "div": true, "mod": true, "quo": true, "rem": true,
	"len": true, "close": true, "list": true, "struct": true,
	"string": true, "int": true, "float": true, "bool": true,
	"bytes": true, "number": true, "strings": true, "math": true, "regexp": true,
}

// RootIdentifiers returns the distinct root names an expression references.
//
// This is what makes an undeclared identifier a compile error rather than a
// runtime skip: the contract is checked against its own `values` block before
// any trace is read (ADR-003 §4).
func RootIdentifiers(expr string) []string {
	// Strip string literals first, or words inside them read as identifiers.
	cleaned := stringRe.ReplaceAllString(expr, `""`)

	seen := map[string]bool{}
	var out []string
	for _, m := range identRe.FindAllString(cleaned, -1) {
		root := m
		if i := strings.IndexByte(root, '.'); i >= 0 {
			root = root[:i]
		}
		if reserved[root] || seen[root] {
			continue
		}
		seen[root] = true
		out = append(out, root)
	}
	sort.Strings(out)
	return out
}

// ValidName reports whether a declared value name is usable: every dotted
// segment must be a plain CUE identifier, and the name must not shadow the
// engine's own result field.
func ValidName(name string) error {
	if name == "" {
		return fmt.Errorf("value name is empty")
	}
	for _, seg := range strings.Split(name, ".") {
		if !selectorR.MatchString(seg) {
			return fmt.Errorf("value name %q: segment %q is not a valid identifier", name, seg)
		}
	}
	if strings.HasPrefix(name, ReservedPrefix) {
		return fmt.Errorf("value name %q uses the reserved %q prefix", name, ReservedPrefix)
	}
	return nil
}

// nest expands dotted names into nested maps: {"refund.amount": 240} becomes
// {"refund": {"amount": 240}}.
func nest(vars map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for name, val := range vars {
		segs := strings.Split(name, ".")
		cur := out
		for i, seg := range segs {
			if i == len(segs)-1 {
				if _, exists := cur[seg]; exists {
					return nil, fmt.Errorf("value %q conflicts with another declared value", name)
				}
				cur[seg] = val
				break
			}
			next, ok := cur[seg]
			if !ok {
				m := map[string]any{}
				cur[seg] = m
				cur = m
				continue
			}
			m, ok := next.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("value %q conflicts with another declared value", name)
			}
			cur = m
		}
	}
	return out, nil
}
