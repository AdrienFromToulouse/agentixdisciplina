// Package evaluate runs a compiled plan against an Episode.
package evaluate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/AdrienFromToulouse/agentixdisciplina/internal/adapter"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/contract"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/engine/judge"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/episode"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/extract"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/report"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/verdict"
)

type Options struct {
	Evidence      verdict.EvidenceMode
	FailOnSkipped bool
	// Judge is nil when judges are disabled; clauses that need one then
	// report SKIPPED rather than failing.
	Judge *judge.Judge
}

func Run(plan *contract.Plan, ep *episode.Episode, opts Options) *report.Report {
	return RunContext(context.Background(), plan, ep, opts)
}

func RunContext(ctx context.Context, plan *contract.Plan, ep *episode.Episode, opts Options) *report.Report {
	if opts.Evidence == "" {
		opts.Evidence = verdict.EvidenceMasked
	}

	r := &report.Report{
		Schema:   report.Schema,
		Contract: plan.Name,
		PlanHash: plan.Hash,
		Episode: report.Summary{
			EpisodeID: ep.Meta.EpisodeID,
			TraceID:   ep.Meta.TraceID,
			SessionID: ep.Meta.SessionID,
			Adapter:   ep.Meta.Adapter,
			RootAgent: ep.Meta.RootAgent,
			Spans:     len(ep.Spans),
			ToolCalls: len(ep.ToolCalls),
			Turns:     len(ep.Turns),
			Degraded:  ep.Coverage.Degraded,
		},
	}
	if ep.Meta.Adapter == adapter.AdapterXRay {
		r.Notice = "episode reconstructed from X-Ray segments: attributes may be truncated and this report must not gate a build"
	}

	// Claims are inferred, not read, so they are extracted here rather than
	// by the adapter, and the structural extractor is deterministic, which
	// is what lets grounding clauses block (ADR-002 §4).
	if len(ep.Claims) == 0 {
		ep.Claims = extract.Structural(ep)
	}

	// The Episode is marshalled once and shared with every Rego clause;
	// values are bound once and shared with every invariant.
	epJSON, jsonErr := episodeJSON(ep)
	bindings := contract.Bind(ep, plan.Values)

	var scoreEarned, scoreTotal float64

	for _, e := range plan.Entries {
		v := verdict.Verdict{
			Clause:   e.Clause.Label,
			Kind:     e.Kind.Name,
			Engine:   e.Kind.Engine,
			Class:    e.Kind.Class,
			Severity: e.Clause.Severity,
			Blocking: e.Clause.Blocking,
		}

		// requires ⊄ coverage → SKIPPED, never PASS (ADR-003 §5).
		if missing := ep.Coverage.Missing(e.Kind.Requires); len(missing) > 0 {
			v.Status = verdict.Skipped
			v.MissingCoverage = missing
			r.Counts.Skipped++
			r.Verdicts = append(r.Verdicts, v)
			continue
		}

		provenance := map[string]string{}
		findings, err := runClause(ctx, e, ep, epJSON, bindings, plan, opts, provenance, jsonErr)
		if len(provenance) > 0 {
			v.Provenance = provenance
		}

		// A clause may decide at runtime that its inputs are absent.
		var skip *contract.SkipError
		if errors.As(err, &skip) {
			v.Status = verdict.Skipped
			v.MissingCoverage = skip.Reasons
			r.Counts.Skipped++
			r.Verdicts = append(r.Verdicts, v)
			continue
		}

		w := weight(e.Clause.Severity)
		scoreTotal += w

		switch {
		case err != nil:
			// A broken evaluator is a failed check, not an absent one
			// (ADR-004 §7).
			v.Status = verdict.Errored
			v.Message = err.Error()
			r.Counts.Errored++
		case len(findings) > 0:
			v.Status = verdict.Fail
			v.Findings = findings
			v.Message = fmt.Sprintf("%d violation(s)", len(findings))
			r.Counts.Fail++
		default:
			v.Status = verdict.Pass
			v.Message = passSummary(e, ep)
			r.Counts.Pass++
			scoreEarned += w
		}
		r.Verdicts = append(r.Verdicts, v)
	}

	// The score summarises; it never gates. The gate is the violation list
	// (ADR-001 trade-offs).
	r.ReliabilityScore = 1.0
	if scoreTotal > 0 {
		r.ReliabilityScore = scoreEarned / scoreTotal
	}

	r.Gate = "pass"
	for _, v := range r.Verdicts {
		if v.Blocks() {
			r.Gate = "fail"
			break
		}
	}
	if opts.FailOnSkipped && r.Counts.Skipped > 0 {
		r.Gate = "fail"
	}
	return r
}

// runClause isolates a panicking evaluator so a broken check is ERRORED, not
// a crash and not a pass (ADR-004 §7).
func runClause(
	ctx context.Context,
	e contract.Entry,
	ep *episode.Episode,
	epJSON map[string]any,
	bindings map[string]contract.Binding,
	plan *contract.Plan,
	opts Options,
	provenance map[string]string,
	jsonErr error,
) (out []verdict.Finding, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			out, err = nil, fmt.Errorf("evaluator panicked: %v", rec)
		}
	}()

	if jsonErr != nil {
		return nil, fmt.Errorf("encode episode for policy evaluation: %w", jsonErr)
	}
	return e.Kind.Eval(contract.EvalContext{
		Ctx:         ctx,
		Episode:     ep,
		EpisodeJSON: epJSON,
		Clause:      e.Clause,
		Bindings:    bindings,
		Evidence:    opts.Evidence,
		Rego:        plan.Rego,
		CUE:         plan.CUE,
		Judge:       opts.Judge,
		Provenance:  provenance,
	})
}

func episodeJSON(ep *episode.Episode) (map[string]any, error) {
	b, err := json.Marshal(ep)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func passSummary(e contract.Entry, ep *episode.Episode) string {
	switch e.Kind.Name {
	case "tool.allowlist", "tool.denylist":
		return fmt.Sprintf("%d tool call(s) checked", len(ep.ToolCalls))
	case "content.no_pii", "content.deny_patterns":
		return fmt.Sprintf("%d turn(s) scanned", len(ep.Turns))
	case "invariant":
		return "holds for every bound value"
	}
	return "ok"
}

func weight(severity string) float64 {
	switch severity {
	case "critical":
		return 3
	case "major":
		return 2
	default:
		return 1
	}
}

// ExitCode maps a report to the process exit code (ADR-001 §6).
func ExitCode(r *report.Report) int {
	if r.Gate == "fail" {
		return 1
	}
	return 0
}
