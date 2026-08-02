// Package evaluate runs a compiled plan against an Episode.
package evaluate

import (
	"fmt"

	"github.com/AdrienFromToulouse/agentixdisciplina/internal/adapter"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/contract"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/episode"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/report"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/verdict"
)

type Options struct {
	Evidence      verdict.EvidenceMode
	FailOnSkipped bool
}

func Run(plan *contract.Plan, ep *episode.Episode, opts Options) *report.Report {
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

	var scoreEarned, scoreTotal float64

	for _, e := range plan.Entries {
		v := verdict.Verdict{
			Clause:   clauseLabel(e.Clause),
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

		findings := runClause(e, ep, opts.Evidence, &v)
		w := weight(e.Clause.Severity)
		scoreTotal += w

		switch {
		case v.Status == verdict.Errored:
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
func runClause(e contract.Entry, ep *episode.Episode, mode verdict.EvidenceMode, v *verdict.Verdict) (out []verdict.Finding) {
	defer func() {
		if rec := recover(); rec != nil {
			v.Status = verdict.Errored
			v.Message = fmt.Sprintf("evaluator panicked: %v", rec)
			out = nil
		}
	}()
	return e.Kind.Eval(contract.EvalContext{Episode: ep, Clause: e.Clause, Evidence: mode})
}

func clauseLabel(c contract.Clause) string {
	if c.Position == "must_not" {
		return "must_not." + c.Kind
	}
	return c.Kind
}

func passSummary(e contract.Entry, ep *episode.Episode) string {
	switch e.Kind.Name {
	case "tool.allowlist", "tool.denylist":
		return fmt.Sprintf("%d tool call(s) checked", len(ep.ToolCalls))
	case "content.no_pii", "content.deny_patterns":
		return fmt.Sprintf("%d turn(s) scanned", len(ep.Turns))
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
