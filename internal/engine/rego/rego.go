// Package rego evaluates policy with an embedded OPA engine.
//
// Rego is the action-layer engine: permissions and sequencing over the tool
// log (ADR-003 §4). Built-in clauses and bundle-declared custom clauses run
// through the same engine, which is the point — one evaluation path, one set
// of performance characteristics, and an `axda explain` that does not lie
// about which engine ran.
package rego

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"sync"

	"github.com/open-policy-agent/opa/v1/rego"
)

//go:embed policy/*.rego
var policyFS embed.FS

// Finding is the shape every policy must produce. Keeping it fixed is what
// lets a third-party module drop into the same reporting path as a built-in.
type Finding struct {
	Message string `json:"message"`
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`
	Path    string `json:"path"`
}

// Input is what a policy sees. The Episode is marshalled once per evaluation
// run and shared across clauses.
type Input struct {
	Episode map[string]any `json:"episode"`
	Params  map[string]any `json:"params"`
}

type Engine struct {
	mu       sync.Mutex
	modules  map[string]string
	prepared map[string]*rego.PreparedEvalQuery
}

// New returns an engine with the built-in policy modules loaded.
func New() (*Engine, error) {
	e := &Engine{
		modules:  map[string]string{},
		prepared: map[string]*rego.PreparedEvalQuery{},
	}
	err := fs.WalkDir(policyFS, "policy", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := policyFS.ReadFile(path)
		if err != nil {
			return err
		}
		e.modules[path] = string(b)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("load built-in policy: %w", err)
	}
	return e, nil
}

// AddModule registers an additional Rego module, for contract-declared custom
// clauses. Adding a module invalidates prepared queries so later evaluations
// see it.
func (e *Engine) AddModule(name, src string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.modules[name] = src
	e.prepared = map[string]*rego.PreparedEvalQuery{}
}

// Eval runs a query (e.g. `data.axda.tool.allowlist_violation`) and returns
// the findings it produced.
func (e *Engine) Eval(ctx context.Context, query string, in Input) ([]Finding, error) {
	pq, err := e.prepare(ctx, query)
	if err != nil {
		return nil, err
	}
	if in.Params == nil {
		in.Params = map[string]any{}
	}

	rs, err := pq.Eval(ctx, rego.EvalInput(in))
	if err != nil {
		return nil, fmt.Errorf("eval %s: %w", query, err)
	}
	if len(rs) == 0 || len(rs[0].Expressions) == 0 {
		// An undefined result is an empty violation set, i.e. a pass.
		return nil, nil
	}

	items, ok := rs[0].Expressions[0].Value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: expected a set of findings, got %T", query, rs[0].Expressions[0].Value)
	}

	out := make([]Finding, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: finding is %T, want an object", query, it)
		}
		out = append(out, Finding{
			Message: str(m["message"]),
			TraceID: str(m["trace_id"]),
			SpanID:  str(m["span_id"]),
			Path:    str(m["path"]),
		})
	}

	// Rego produces a *set*, whose iteration order is not stable. Sorting
	// here is what keeps reports byte-identical across runs (ADR-001 §6) —
	// without it the determinism guarantee would silently depend on OPA's
	// internal hashing.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		if out[i].SpanID != out[j].SpanID {
			return out[i].SpanID < out[j].SpanID
		}
		return out[i].Message < out[j].Message
	})
	return out, nil
}

// Check compiles a query without evaluating it, so a broken custom policy is
// a load-time error rather than a mid-run surprise.
func (e *Engine) Check(ctx context.Context, query string) error {
	_, err := e.prepare(ctx, query)
	return err
}

func (e *Engine) prepare(ctx context.Context, query string) (*rego.PreparedEvalQuery, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if pq, ok := e.prepared[query]; ok {
		return pq, nil
	}

	opts := []func(*rego.Rego){rego.Query(query)}
	for name, src := range e.modules {
		opts = append(opts, rego.Module(name, src))
	}
	pq, err := rego.New(opts...).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("compile %s: %w", query, err)
	}
	e.prepared[query] = &pq
	return &pq, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
