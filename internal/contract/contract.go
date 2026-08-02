// Package contract parses an AgentContract and lowers it onto concrete
// evaluators (ADR-003).
//
// Clause names resolve against a closed registry. An unknown name is a compile
// error, never a prompt — a contract that reads like prose but is *understood*
// like prose would be a prompt with YAML syntax (ADR-003 §1).
package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

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
}

type Spec struct {
	AllowedTools []string    `yaml:"allowed_tools"`
	DeniedTools  []string    `yaml:"denied_tools"`
	Must         []yaml.Node `yaml:"must"`
	MustNot      []yaml.Node `yaml:"must_not"`
}

// Clause is one bound, registered predicate.
type Clause struct {
	Kind     string
	Position string // must | must_not | spec
	Params   map[string]any
	Severity string
	Blocking bool
	Source   string // location in the contract, for error messages
}

type Plan struct {
	Name    string
	Entries []Entry
	Hash    string
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
	return Compile(&doc)
}

func Compile(doc *Document) (*Plan, error) {
	p := &Plan{Name: doc.Metadata.Name}
	if p.Name == "" {
		p.Name = "contract"
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
	b, _ := json.Marshal(out)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])[:16]
}

// Explain renders the lowering. `axda explain` prints this; the whole
// "assertions at the right altitude" pitch dies if the descent from contract
// to check is a black box (ADR-003 §6).
func (p *Plan) Explain() string {
	var b strings.Builder
	fmt.Fprintf(&b, "plan: %s (contract %s, episode/v1)  hash=%s\n\n", p.Name, APIVersion, p.Hash)
	for _, e := range p.Entries {
		label := e.Clause.Kind
		if e.Clause.Position == "must_not" {
			label = "must_not." + e.Clause.Kind
		}
		fmt.Fprintf(&b, "  %s\n", label)
		fmt.Fprintf(&b, "    ├─ kind      %s %s\n", e.Kind.Name, formatParams(e.Clause.Params))
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
