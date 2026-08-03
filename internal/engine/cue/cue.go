// Package cue evaluates value constraints.
//
// CUE is the belief-layer engine: is this set of extracted values internally
// consistent, and does it satisfy the declared constraints (ADR-003 §4)? It
// also unifies schemas against data for tool.args_match: an action clause on
// this engine because schema validation *is* unification. CUE rather than a
// bespoke expression language because these are exactly value constraints
// under unification, and reusing it keeps one dependency doing one job.
package cue

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/ast"
	"cuelang.org/go/cue/cuecontext"
	"cuelang.org/go/cue/parser"
)

// ReservedPrefix may not begin a declared value name. The engine no longer
// injects fields of its own, but the prefix stays reserved as contract-surface
// namespace hygiene for future engine and report use.
const ReservedPrefix = "axda"

type Evaluator struct {
	ctx *cue.Context
}

func NewEvaluator() *Evaluator {
	return &Evaluator{ctx: cuecontext.New()}
}

// Constraint evaluates a boolean expression over the given values. Names may
// be dotted (`refund.amount`); they are nested into structs so the expression
// reads as an ordinary CUE selector.
//
// The expression is parsed and built against the values as a scope; no source
// text is assembled, so a value that happens to contain CUE syntax is inert
// data.
func (e *Evaluator) Constraint(expr string, vars map[string]any) (bool, error) {
	nested, err := nest(vars)
	if err != nil {
		return false, err
	}

	scope := e.ctx.Encode(nested)
	if err := scope.Err(); err != nil {
		return false, fmt.Errorf("encode values: %w", err)
	}

	x, err := parser.ParseExpr("constraint", expr)
	if err != nil {
		return false, fmt.Errorf("invalid constraint %q: %w", expr, err)
	}

	res := e.ctx.BuildExpr(x, cue.Scope(scope), cue.InferBuiltins(true))
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
	d := e.ctx.Encode(data)
	if err := d.Err(); err != nil {
		return fmt.Errorf("invalid data: %w", err)
	}
	return s.Unify(d).Validate(cue.Concrete(true))
}

var selectorR = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// predeclared are identifiers that resolve as CUE builtins rather than value
// references. Keywords (`if`, `for`, `let`, …) need no entry: they are
// grammar, not identifiers, once the expression is parsed.
var predeclared = map[string]bool{
	"true": true, "false": true, "null": true,
	"len": true, "close": true, "and": true, "or": true,
	"string": true, "int": true, "float": true, "bool": true,
	"bytes": true, "number": true, "top": true,
	"int8": true, "int16": true, "int32": true, "int64": true, "int128": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true, "uint128": true,
	"float32": true, "float64": true,
	"strings": true, "math": true, "list": true, "struct": true, "regexp": true,
	"_": true,
}

// RootIdentifiers returns the distinct root names an expression references,
// parsed from the CUE AST rather than approximated with regexes.
//
// This is what makes an undeclared identifier a compile error rather than a
// runtime skip: the contract is checked against its own `values` block before
// any trace is read (ADR-003 §4). A malformed expression is an error here,
// which the contract compiler surfaces at load.
func RootIdentifiers(expr string) ([]string, error) {
	x, err := parser.ParseExpr("constraint", expr)
	if err != nil {
		return nil, fmt.Errorf("invalid expression %q: %w", expr, err)
	}

	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if predeclared[name] || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}

	// Selector chains contribute only their root: `refund.amount` references
	// `refund`, and `amount` is a field, not an identifier. ast.Walk would
	// visit the field's Ident too, so the traversal descends into a
	// SelectorExpr's operand and skips its selector; struct-literal field
	// labels are skipped for the same reason.
	var before func(ast.Node) bool
	before = func(n ast.Node) bool {
		switch t := n.(type) {
		case *ast.SelectorExpr:
			ast.Walk(t.X, before, nil)
			return false
		case *ast.Field:
			ast.Walk(t.Value, before, nil)
			return false
		case *ast.Ident:
			add(t.Name)
		}
		return true
	}
	ast.Walk(x, before, nil)

	sort.Strings(out)
	return out, nil
}

// ValidName reports whether a declared value name is usable: every dotted
// segment must be a plain CUE identifier, and the name must not use the
// reserved prefix.
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
