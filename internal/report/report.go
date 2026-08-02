// Package report renders evaluation results.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/AdrienFromToulouse/agentixdisciplina/internal/episode"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/verdict"
)

const Schema = "axda.dev/report/v1"

type Report struct {
	Schema           string            `json:"schema"`
	Contract         string            `json:"contract"`
	PlanHash         string            `json:"plan_hash"`
	Episode          Summary           `json:"episode"`
	ReliabilityScore float64           `json:"reliability_score"`
	Gate             string            `json:"gate"` // pass | fail
	Counts           Counts            `json:"counts"`
	Verdicts         []verdict.Verdict `json:"verdicts"`
	// Notice carries a banner for degraded input, e.g. an xray/v1 Episode.
	Notice string `json:"notice,omitempty"`
}

type Summary struct {
	EpisodeID string   `json:"episode_id"`
	TraceID   string   `json:"trace_id"`
	SessionID string   `json:"session_id,omitempty"`
	Adapter   string   `json:"adapter"`
	RootAgent string   `json:"root_agent,omitempty"`
	Spans     int      `json:"spans"`
	ToolCalls int      `json:"tool_calls"`
	Turns     int      `json:"turns"`
	Degraded  []string `json:"degraded,omitempty"`
}

type Counts struct {
	Pass    int `json:"pass"`
	Fail    int `json:"fail"`
	Skipped int `json:"skipped"`
	Errored int `json:"errored"`
}

// CoverageHint maps a missing coverage flag to the instrumentation change that
// would satisfy it, so a skip is an actionable setup step rather than a dead
// end (ADR-002 trade-offs).
func CoverageHint(flag string) string {
	switch flag {
	case episode.HasMessageContent:
		return "set OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true on the agent runtime"
	case episode.HasToolArgs, episode.HasToolResults:
		return "enable tool argument/result capture in your instrumentation (opt-in for privacy)"
	case episode.HasTokenUsage:
		return "no gen_ai.usage.* attributes in this trace; check the model instrumentation"
	case episode.HasCost:
		return "cost is not derived in v0 (no pricing table)"
	case episode.HasRetrievalSpans:
		return "no retrieval spans found in this trace"
	case episode.HasAgentSpans:
		return "no invoke_agent spans found in this trace"
	}
	return ""
}

func (r *Report) JSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Human renders the default terminal report. Skipped clauses print above
// passes so they cannot be scrolled past (ADR-003 §5).
func (r *Report) Human(w io.Writer, color bool) error {
	c := palette(color)
	total := len(r.Verdicts)

	gate := c.green("PASS")
	if r.Gate == "fail" {
		gate = c.red("FAIL")
	}
	fmt.Fprintf(w, "\n%s · %d clauses · score %.2f · %s\n",
		c.bold(r.Contract), total, r.ReliabilityScore, gate)
	fmt.Fprintf(w, "%s\n\n", c.dim(fmt.Sprintf(
		"episode %s · adapter %s · %d spans · %d tool calls · %d turns",
		short(r.Episode.EpisodeID), r.Episode.Adapter,
		r.Episode.Spans, r.Episode.ToolCalls, r.Episode.Turns)))

	if r.Notice != "" {
		fmt.Fprintf(w, "  %s %s\n\n", c.yellow("!"), r.Notice)
	}

	order := map[verdict.Status]int{
		verdict.Fail: 0, verdict.Errored: 1, verdict.Skipped: 2, verdict.Pass: 3,
	}
	vs := append([]verdict.Verdict(nil), r.Verdicts...)
	sort.SliceStable(vs, func(i, j int) bool { return order[vs[i].Status] < order[vs[j].Status] })

	for _, v := range vs {
		var tag string
		switch v.Status {
		case verdict.Fail:
			tag = c.red("FAIL")
		case verdict.Errored:
			tag = c.red("ERR ")
		case verdict.Skipped:
			tag = c.yellow("SKIP")
		default:
			tag = c.green("PASS")
		}
		note := v.Message
		if v.Status == verdict.Skipped {
			note = "needs: " + strings.Join(v.MissingCoverage, ", ")
		}
		advisory := ""
		if v.Class == verdict.Probabilistic {
			advisory = c.dim(" (advisory)")
		}
		fmt.Fprintf(w, "  %s  %-34s %-9s %s%s\n", tag, v.Clause, v.Severity, note, advisory)

		for _, f := range v.Findings {
			fmt.Fprintf(w, "        %s %s\n", c.dim("└"), f.Message)
			if f.Evidence.Excerpt != "" {
				fmt.Fprintf(w, "          %s\n", c.dim(f.Evidence.Excerpt))
			}
			// Every violation resolves to a span (ADR-001 §6), so the
			// locator is always printed.
			var loc []string
			if f.Evidence.Path != "" {
				loc = append(loc, f.Evidence.Path)
			}
			if f.Evidence.SpanID != "" {
				loc = append(loc, "span "+f.Evidence.SpanID)
			}
			if len(loc) > 0 {
				fmt.Fprintf(w, "          %s\n", c.dim(strings.Join(loc, " · ")))
			}
		}
		if v.Status == verdict.Skipped {
			for _, flag := range v.MissingCoverage {
				if h := CoverageHint(flag); h != "" {
					fmt.Fprintf(w, "        %s %s\n", c.dim("└"), c.dim(h))
				}
			}
		}
	}

	fmt.Fprintf(w, "\n  %d passed · %d failed · %d skipped · %d errored\n",
		r.Counts.Pass, r.Counts.Fail, r.Counts.Skipped, r.Counts.Errored)
	if r.Counts.Skipped > 0 {
		fmt.Fprintf(w, "  %s\n", c.yellow(fmt.Sprintf(
			"%d clause(s) could not be evaluated against this trace and are NOT passes; use --fail-on-skipped to gate on coverage",
			r.Counts.Skipped)))
	}
	for _, d := range r.Episode.Degraded {
		fmt.Fprintf(w, "  %s %s\n", c.dim("·"), c.dim(d))
	}
	fmt.Fprintln(w)
	return nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

type pal struct{ enabled bool }

func palette(on bool) pal { return pal{on} }

func (p pal) wrap(code, s string) string {
	if !p.enabled {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}
func (p pal) red(s string) string    { return p.wrap("31", s) }
func (p pal) green(s string) string  { return p.wrap("32", s) }
func (p pal) yellow(s string) string { return p.wrap("33", s) }
func (p pal) dim(s string) string    { return p.wrap("2", s) }
func (p pal) bold(s string) string   { return p.wrap("1", s) }
