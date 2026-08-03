// Package contract parses an AgentContract and lowers it onto concrete
// evaluators (ADR-003).
//
// Clause names resolve against a closed registry. An unknown name is a compile
// error, never a prompt — a contract that reads like prose but is *understood*
// like prose would be a prompt with YAML syntax (ADR-003 §1).
package contract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	cueeng "github.com/AdrienFromToulouse/agentixdisciplina/internal/engine/cue"
	regoeng "github.com/AdrienFromToulouse/agentixdisciplina/internal/engine/rego"
	"gopkg.in/yaml.v3"
)

const APIVersion = "axda.dev/v1"

type Document struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec Spec `yaml:"spec"`
	// Clauses declares custom clause kinds. ADR-003 §7 puts these in bundle
	// metadata (axda.yaml); v0 has no bundles, so they live in the contract.
	Clauses []CustomClause `yaml:"clauses"`
}

type Spec struct {
	AllowedTools []string             `yaml:"allowed_tools"`
	DeniedTools  []string             `yaml:"denied_tools"`
	Values       map[string]yaml.Node `yaml:"values"`
	Invariants   []string             `yaml:"invariants"`
	Must         []yaml.Node          `yaml:"must"`
	MustNot      []yaml.Node          `yaml:"must_not"`
}

// CustomClause registers a bundle-supplied clause kind. It must be namespaced;
// the bare namespace is reserved so a contract cannot shadow `tool.allowlist`
// and change what an existing clause means (ADR-003 §7).
type CustomClause struct {
	Name     string   `yaml:"name"`
	Engine   string   `yaml:"engine"` // rego
	Source   string   `yaml:"source"` // .rego file, relative to the contract
	Query    string   `yaml:"query"`  // e.g. data.acme.kyc.violation
	Requires []string `yaml:"requires"`
	Severity string   `yaml:"severity"`
	Reads    string   `yaml:"reads"`
}

// Clause is one bound, registered predicate.
type Clause struct {
	Kind     string
	Label    string
	Position string // must | must_not | spec
	Params   map[string]any
	Severity string
	Blocking bool
	Source   string // location in the contract, for error messages
}

type Plan struct {
	Name    string
	Entries []Entry
	Values  map[string]ValueSpec
	Hash    string

	Rego *regoeng.Engine
	CUE  *cueeng.Evaluator
}

type Entry struct {
	Clause Clause
	Kind   *Kind
}

// Load reads and compiles a contract file.
func Load(path string) (*Plan, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc Document
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Kind != "" && doc.Kind != "AgentContract" {
		return nil, fmt.Errorf("%s: kind %q is not AgentContract", path, doc.Kind)
	}
	if doc.APIVersion != "" && doc.APIVersion != APIVersion {
		return nil, fmt.Errorf("%s: apiVersion %q is not %s", path, doc.APIVersion, APIVersion)
	}
	return Compile(&doc, filepath.Dir(path))
}

func Compile(doc *Document, baseDir string) (*Plan, error) {
	rg, err := regoeng.New()
	if err != nil {
		return nil, err
	}
	p := &Plan{
		Name:   doc.Metadata.Name,
		Values: map[string]ValueSpec{},
		Rego:   rg,
		CUE:    cueeng.NewEvaluator(),
	}
	if p.Name == "" {
		p.Name = "contract"
	}

	if err := p.registerCustom(doc.Clauses, baseDir); err != nil {
		return nil, err
	}
	if err := p.bindValues(doc.Spec.Values); err != nil {
		return nil, err
	}

	if len(doc.Spec.AllowedTools) > 0 {
		if err := p.add(Clause{
			Kind: "tool.allowlist", Position: "spec",
			Params: map[string]any{"tools": toAnySlice(doc.Spec.AllowedTools)},
			Source: "spec.allowed_tools",
		}); err != nil {
			return nil, err
		}
	}
	if len(doc.Spec.DeniedTools) > 0 {
		if err := p.add(Clause{
			Kind: "tool.denylist", Position: "spec",
			Params: map[string]any{"tools": toAnySlice(doc.Spec.DeniedTools)},
			Source: "spec.denied_tools",
		}); err != nil {
			return nil, err
		}
	}

	for i, expr := range doc.Spec.Invariants {
		src := fmt.Sprintf("spec.invariants[%d]", i)
		if err := p.checkInvariantIdentifiers(expr, src); err != nil {
			return nil, err
		}
		if err := p.add(Clause{
			Kind: "invariant", Position: "spec",
			Label:  fmt.Sprintf("invariants[%d]", i),
			Params: map[string]any{"expr": expr},
			Source: src,
		}); err != nil {
			return nil, err
		}
	}

	for i, n := range doc.Spec.Must {
		c, err := parseClause(n, "must", fmt.Sprintf("spec.must[%d]", i))
		if err != nil {
			return nil, err
		}
		if err := p.add(c); err != nil {
			return nil, err
		}
	}
	for i, n := range doc.Spec.MustNot {
		c, err := parseClause(n, "must_not", fmt.Sprintf("spec.must_not[%d]", i))
		if err != nil {
			return nil, err
		}
		if err := p.add(c); err != nil {
			return nil, err
		}
	}
	if len(p.Entries) == 0 {
		return nil, fmt.Errorf("contract declares no clauses")
	}
	p.Hash = p.computeHash()
	return p, nil
}

func (p *Plan) bindValues(nodes map[string]yaml.Node) error {
	names := make([]string, 0, len(nodes))
	for n := range nodes {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		if err := cueeng.ValidName(name); err != nil {
			return fmt.Errorf("spec.values: %w", err)
		}
		node := nodes[name]

		var spec ValueSpec
		if err := node.Decode(&spec); err != nil {
			return fmt.Errorf("spec.values.%s: %w", name, err)
		}
		// yaml cannot distinguish an absent `default` from a null one, and
		// the difference decides skip-versus-substitute.
		var raw map[string]any
		if err := node.Decode(&raw); err == nil {
			_, spec.HasDefault = raw["default"]
		}
		if err := validateValueSpec(name, spec); err != nil {
			return fmt.Errorf("spec.values: %w", err)
		}
		p.Values[name] = spec
	}
	return nil
}

// checkInvariantIdentifiers is the compile-time half of ADR-003 §4: an
// expression may only reference values the contract declared, and that is
// checked before any trace is read.
func (p *Plan) checkInvariantIdentifiers(expr, source string) error {
	declaredRoots := map[string]bool{}
	for name := range p.Values {
		root := name
		if i := strings.IndexByte(root, '.'); i >= 0 {
			root = root[:i]
		}
		declaredRoots[root] = true
	}

	var undeclared []string
	for _, id := range cueeng.RootIdentifiers(expr) {
		if !declaredRoots[id] {
			undeclared = append(undeclared, id)
		}
	}
	if len(undeclared) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s: invariant references undeclared value(s): %s\n",
		source, strings.Join(undeclared, ", "))
	b.WriteString("  every operand must say where it comes from, e.g.\n")
	fmt.Fprintf(&b, "    values:\n      %s:\n        from: tool_call\n        tool: <tool>\n        arg: <arg>\n        cardinality: any\n", undeclared[0])
	if len(p.Values) > 0 {
		fmt.Fprintf(&b, "  declared: %s", strings.Join(sortedKeys(p.Values), ", "))
	} else {
		b.WriteString("  this contract declares no values")
	}
	return fmt.Errorf("%s", b.String())
}

func (p *Plan) registerCustom(clauses []CustomClause, baseDir string) error {
	for i, cc := range clauses {
		src := fmt.Sprintf("clauses[%d]", i)
		if cc.Name == "" {
			return fmt.Errorf("%s: `name` is required", src)
		}
		if !strings.Contains(cc.Name, ".") {
			return fmt.Errorf("%s: custom clause %q must be namespaced (e.g. acme.%s)", src, cc.Name, cc.Name)
		}
		if reservedNamespace(cc.Name) {
			return fmt.Errorf("%s: %q shadows a reserved namespace; built-in clause meanings may not be redefined", src, cc.Name)
		}
		if Lookup(cc.Name) != nil {
			return fmt.Errorf("%s: clause %q is already registered", src, cc.Name)
		}
		if cc.Engine != "rego" {
			return fmt.Errorf("%s: engine %q is not supported for custom clauses (v0 supports: rego)", src, cc.Engine)
		}
		if cc.Source == "" || cc.Query == "" {
			return fmt.Errorf("%s: rego clauses require `source` and `query`", src)
		}

		body, err := os.ReadFile(filepath.Join(baseDir, cc.Source))
		if err != nil {
			return fmt.Errorf("%s: %w", src, err)
		}
		p.Rego.AddModule(cc.Source, string(body))
		// Compile now so a broken policy is a load-time error, not a
		// surprise halfway through a run.
		if err := p.Rego.Check(context.Background(), cc.Query); err != nil {
			return fmt.Errorf("%s: %w", src, err)
		}

		severity := cc.Severity
		if severity == "" {
			severity = "major"
		}
		reads := cc.Reads
		if reads == "" {
			reads = "episode"
		}
		register(&Kind{
			Name:   cc.Name,
			Engine: "rego:" + cc.Query,
			// A custom clause is deterministic because the Rego engine has
			// no clock, no network, and no randomness. When capability-
			// holding WASM plugins land (ADR-004 §5) that will no longer be
			// automatic.
			Class:           ClassDeterministic,
			Requires:        cc.Requires,
			Reads:           reads,
			DefaultSeverity: severity,
			Positions:       []string{"must", "must_not"},
			PrefixDecidable: true,
			Eval:            regoClause(cc.Query),
		})
	}
	return nil
}

func reservedNamespace(name string) bool {
	for _, ns := range []string{"tool.", "order.", "content.", "budget.", "grounding.", "quality.", "invariant"} {
		if strings.HasPrefix(name, ns) {
			return true
		}
	}
	return false
}

func (p *Plan) add(c Clause) error {
	k := Lookup(c.Kind)
	if k == nil {
		return unknownClauseError(c)
	}
	if err := k.validate(c); err != nil {
		return fmt.Errorf("%s: %w", c.Source, err)
	}
	// Severity and blocking defaults by position (ADR-003 §3).
	if c.Severity == "" {
		switch {
		case k.Class == ClassProbabilistic:
			c.Severity = "minor"
		case c.Position == "must_not", c.Kind == "tool.allowlist":
			c.Severity = "critical"
		default:
			c.Severity = k.DefaultSeverity
		}
	}
	if k.Class == ClassProbabilistic {
		// Probabilistic clauses are advisory unless explicitly opted in.
		if v, ok := c.Params["blocking"].(bool); ok {
			c.Blocking = v
		}
	} else {
		c.Blocking = true
		if v, ok := c.Params["blocking"].(bool); ok {
			c.Blocking = v
		}
	}
	if c.Label == "" {
		c.Label = c.Kind
		if c.Position == "must_not" {
			c.Label = "must_not." + c.Kind
		}
	}
	p.Entries = append(p.Entries, Entry{Clause: c, Kind: k})
	return nil
}

func parseClause(n yaml.Node, position, source string) (Clause, error) {
	c := Clause{Position: position, Source: source, Params: map[string]any{}}

	// Shorthand string form: `must_not: [expose_pii]`
	var s string
	if err := n.Decode(&s); err == nil && s != "" {
		c.Kind = s
		return c, nil
	}

	var m map[string]any
	if err := n.Decode(&m); err != nil {
		return c, fmt.Errorf("%s: clause must be a string or an object", source)
	}
	kind, _ := m["kind"].(string)
	if kind == "" {
		return c, fmt.Errorf("%s: clause object requires a `kind` field", source)
	}
	c.Kind = kind
	for k, v := range m {
		switch k {
		case "kind":
		case "severity":
			c.Severity, _ = v.(string)
		default:
			c.Params[k] = v
		}
	}
	return c, nil
}

func (p *Plan) computeHash() string {
	type ent struct {
		Kind     string         `json:"kind"`
		Position string         `json:"position"`
		Params   map[string]any `json:"params"`
		Severity string         `json:"severity"`
		Blocking bool           `json:"blocking"`
	}
	out := make([]ent, 0, len(p.Entries))
	for _, e := range p.Entries {
		out = append(out, ent{e.Kind.Name, e.Clause.Position, e.Clause.Params, e.Clause.Severity, e.Clause.Blocking})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return fmt.Sprint(out[i].Params) < fmt.Sprint(out[j].Params)
	})
	payload := struct {
		Entries []ent                `json:"entries"`
		Values  map[string]ValueSpec `json:"values"`
	}{out, p.Values}
	b, _ := json.Marshal(payload)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])[:16]
}

// Explain renders the lowering. `axda explain` prints this; the whole
// "assertions at the right altitude" pitch dies if the descent from contract
// to check is a black box (ADR-003 §6).
func (p *Plan) Explain() string {
	var b strings.Builder
	fmt.Fprintf(&b, "plan: %s (contract %s, episode/v1)  hash=%s\n\n", p.Name, APIVersion, p.Hash)

	if len(p.Values) > 0 {
		b.WriteString("  values\n")
		for _, name := range sortedKeys(p.Values) {
			s := p.Values[name]
			def := ""
			if s.HasDefault {
				def = fmt.Sprintf(" default=%v", s.Default)
			}
			fmt.Fprintf(&b, "    %-22s ← %s [%s]%s\n", name, describeSource(s), s.Cardinality, def)
		}
		b.WriteString("\n")
	}

	for _, e := range p.Entries {
		fmt.Fprintf(&b, "  %s\n", e.Clause.Label)
		if expr, ok := e.Clause.Params["expr"].(string); ok {
			fmt.Fprintf(&b, "    ├─ expr      %q\n", expr)
			for _, n := range ReferencedNames(expr, sortedKeys(p.Values)) {
				fmt.Fprintf(&b, "    ├─ binds     %-20s ← %s [%s]\n",
					n, describeSource(p.Values[n]), p.Values[n].Cardinality)
			}
		} else {
			fmt.Fprintf(&b, "    ├─ kind      %s %s\n", e.Kind.Name, formatParams(e.Clause.Params))
		}
		fmt.Fprintf(&b, "    ├─ engine    %s\n", e.Kind.Engine)
		fmt.Fprintf(&b, "    ├─ class     %-16s blocking: %-4t severity: %s\n",
			e.Kind.Class, e.Clause.Blocking, e.Clause.Severity)
		req := "—"
		if len(e.Kind.Requires) > 0 {
			req = strings.Join(e.Kind.Requires, ", ")
		}
		fmt.Fprintf(&b, "    ├─ requires  %s\n", req)
		fmt.Fprintf(&b, "    ├─ reads     %s\n", e.Kind.Reads)
		fmt.Fprintf(&b, "    └─ inline    %s\n\n", inlineLabel(e.Kind.PrefixDecidable))
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func inlineLabel(ok bool) string {
	if ok {
		return "yes (prefix-decidable)"
	}
	return "no (suffix-dependent)"
}

func formatParams(p map[string]any) string {
	if len(p) == 0 {
		return ""
	}
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: %v", k, p[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func unknownClauseError(c Clause) error {
	var b strings.Builder
	fmt.Fprintf(&b, "unknown clause %q (%s)\n", c.Kind, c.Source)
	b.WriteString("  axda does not interpret free-text clauses.\n")
	if s := suggest(c.Kind); s != "" {
		fmt.Fprintf(&b, "  did you mean: %s?\n", s)
	}
	b.WriteString("  known clauses: " + strings.Join(KnownNames(), ", "))
	return fmt.Errorf("%s", b.String())
}

func suggest(name string) string {
	best, bestScore := "", 1<<30
	for _, cand := range KnownNames() {
		d := editDistance(name, cand)
		if strings.Contains(cand, name) || strings.Contains(name, shortName(cand)) {
			d = 1
		}
		if d < bestScore {
			best, bestScore = cand, d
		}
	}
	if bestScore <= len(name)/2+2 {
		return best
	}
	return ""
}

func shortName(full string) string {
	if i := strings.IndexByte(full, '.'); i >= 0 {
		return full[i+1:]
	}
	return full
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

func toAnySlice(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}
