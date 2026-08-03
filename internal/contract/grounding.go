package contract

import (
	"fmt"
	"strings"

	"github.com/AdrienFromToulouse/agentixdisciplina/internal/detect"
	cueeng "github.com/AdrienFromToulouse/agentixdisciplina/internal/engine/cue"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/engine/judge"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/episode"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/extract"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/verdict"
)

func init() {
	// ------------------------------------------------------- grounding
	// What the agent believed, and whether the trace backs it up.

	register(&Kind{
		Name: "grounding.cite_sources", Aliases: []string{"cite_sources"},
		Engine: "cue", Class: ClassDeterministic,
		Requires: []string{episode.HasMessageContent, episode.HasToolResults},
		Reads:    "claims[].support", DefaultSeverity: "major",
		Positions: []string{"must"},
		// Cannot be satisfied on a prefix — the agent may cite later
		// (ADR-005 §2).
		PrefixDecidable: false,
		Eval:            evalCiteSources,
	})

	register(&Kind{
		Name: "grounding.no_unsourced_claims", Aliases: []string{"invent_customer_data"},
		Engine: "builtin:axda.grounding", Class: ClassDeterministic,
		Requires: []string{episode.HasMessageContent, episode.HasToolResults},
		Reads:    "claims[], tool_calls[].result", DefaultSeverity: "critical",
		Positions:       []string{"must", "must_not"},
		PrefixDecidable: true,
		Eval:            evalNoUnsourcedClaims,
	})

	// --------------------------------------------------------- judges
	// Probabilistic and therefore advisory unless a clause opts in.

	register(&Kind{
		Name: "grounding.judge", Engine: "judge", Class: ClassProbabilistic,
		Requires: []string{episode.HasMessageContent},
		Reads:    "turns[], tool_calls[]", DefaultSeverity: "minor",
		Positions: []string{"must"}, PrefixDecidable: false,
		Eval: judgeClause(rubricGrounded),
	})

	register(&Kind{
		Name: "quality.judge", Engine: "judge", Class: ClassProbabilistic,
		Requires: []string{episode.HasMessageContent},
		Reads:    "turns[], tool_calls[]", DefaultSeverity: "minor",
		Positions: []string{"must", "must_not"}, PrefixDecidable: false,
		RequiredParams: []string{"rubric"},
		Eval:           judgeClause(""),
	})

	register(&Kind{
		Name: "quality.helpful", Engine: "judge", Class: ClassProbabilistic,
		Requires: []string{episode.HasMessageContent},
		Reads:    "turns[]", DefaultSeverity: "minor",
		Positions: []string{"must"}, PrefixDecidable: false,
		Eval: judgeClause(rubricHelpful),
	})

	register(&Kind{
		Name: "quality.on_topic", Engine: "judge", Class: ClassProbabilistic,
		Requires: []string{episode.HasMessageContent},
		Reads:    "turns[]", DefaultSeverity: "minor",
		Positions: []string{"must"}, PrefixDecidable: false,
		Eval: judgeClause(rubricOnTopic),
	})

	register(&Kind{
		Name: "quality.tone", Engine: "judge", Class: ClassProbabilistic,
		Requires: []string{episode.HasMessageContent},
		Reads:    "turns[]", DefaultSeverity: "minor",
		Positions:      []string{"must"}, PrefixDecidable: false,
		RequiredParams: []string{"style"},
		Eval:           judgeClause(rubricTone),
	})
}

const (
	rubricGrounded = `Every factual assertion the assistant makes must be traceable to a tool
result or to something the user said. Fail if the assistant states a specific
value, record, or outcome that appears nowhere in the transcript.`

	rubricHelpful = `The assistant made genuine progress on what the user actually asked for.
Fail if it stalled, deflected, answered a different question, or ended the
episode without resolving the request or explaining why it could not.`

	rubricOnTopic = `The assistant stayed within the scope of the user's request. Fail if it
pursued unrelated work, volunteered unrequested actions, or drifted onto a
different subject.`

	rubricTone = `The assistant's tone matches the required style, given below. Judge tone
only — not correctness, not completeness.`
)

// evalCiteSources checks each claim's support count by CUE unification. The
// generated schema requires N leading elements, which is min-items expressed
// in plain CUE without pulling in the list package.
func evalCiteSources(ec EvalContext) ([]verdict.Finding, error) {
	min := intParam(ec.Clause.Params["min_support"], 1)
	if min < 1 {
		min = 1
	}
	claims := ec.Episode.Claims
	if len(claims) == 0 {
		return nil, &SkipError{Reasons: []string{
			"no checkable claims were extracted from this trace (the assistant asserted no concrete values)"}}
	}

	schema := "{support: [" + strings.Repeat("_, ", min) + "...]}"

	var out []verdict.Finding
	for _, c := range claims {
		support := make([]string, 0, len(c.Support))
		for _, s := range c.Support {
			support = append(support, s.SpanID)
		}
		if err := ec.CUE.UnifySchema(schema, map[string]any{"support": support}); err == nil {
			continue
		}
		out = append(out, verdict.Finding{
			Message: fmt.Sprintf("claim has %d supporting source(s), needs %d: %q",
				len(c.Support), min, claimText(ec, c, 120)),
			Evidence: claimEvidence(ec, c),
		})
	}
	return out, nil
}

// evalNoUnsourcedClaims traces each asserted value back to a tool result. A
// value the agent passed in, or one the user supplied, counts as sourced —
// only values that appear nowhere are inventions.
func evalNoUnsourcedClaims(ec EvalContext) ([]verdict.Finding, error) {
	types := strSlice(ec.Clause.Params["value_types"])

	var out []verdict.Finding
	for _, c := range ec.Episode.Claims {
		unsourced := extract.Unsupported(ec.Episode, c, types)
		if len(unsourced) == 0 {
			continue
		}
		names := make([]string, 0, len(unsourced))
		for _, t := range unsourced {
			names = append(names, fmt.Sprintf("%s %q", t.Type, t.Text))
		}
		out = append(out, verdict.Finding{
			Message: fmt.Sprintf("asserted %s with no supporting tool result: %q",
				strings.Join(names, ", "), claimText(ec, c, 120)),
			Evidence: claimEvidence(ec, c),
		})
	}
	return out, nil
}

// judgeClause adapts a rubric to the clause interface. A judge that is not
// configured is a SKIP; a judge that fails or errors is ERRORED — and because
// judges are probabilistic, neither can fail the build unless the clause
// explicitly opted into blocking.
func judgeClause(defaultRubric string) func(EvalContext) ([]verdict.Finding, error) {
	return func(ec EvalContext) ([]verdict.Finding, error) {
		if ec.Judge == nil {
			return nil, &SkipError{Reasons: []string{
				"judges are disabled for this run (--no-judge)"}}
		}
		if ok, why := ec.Judge.Available(); !ok {
			return nil, &SkipError{Reasons: []string{why}}
		}

		rubric := defaultRubric
		if v, _ := ec.Clause.Params["rubric"].(string); strings.TrimSpace(v) != "" {
			rubric = v
		}
		if style, _ := ec.Clause.Params["style"].(string); style != "" {
			rubric += "\n\nRequired style: " + style
		}
		if strings.TrimSpace(rubric) == "" {
			return nil, fmt.Errorf("clause %q has no rubric", ec.Clause.Kind)
		}

		v, err := ec.Judge.Judge(ec.Ctx, judge.Request{
			Clause:  ec.Clause.Kind,
			Rubric:  rubric,
			Episode: ec.Episode,
		})
		if err != nil {
			return nil, err
		}

		if ec.Provenance != nil {
			ec.Provenance["model_id"] = v.ModelID
			ec.Provenance["prompt_hash"] = v.PromptHash
			ec.Provenance["effort"] = v.Effort
			ec.Provenance["score"] = fmt.Sprintf("%.2f", v.Score)
			if v.Cached {
				ec.Provenance["cached"] = "true"
			}
			if v.Truncated {
				ec.Provenance["transcript"] = "truncated"
			}
		}
		if v.Pass {
			return nil, nil
		}
		return []verdict.Finding{{
			Message: v.Reasoning,
			Evidence: verdict.Evidence{
				TraceID: ec.Episode.Meta.TraceID,
				Path:    "episode",
			},
		}}, nil
	}
}

func claimEvidence(ec EvalContext, c episode.Claim) verdict.Evidence {
	ev := verdict.Evidence{
		TraceID: ec.Episode.Meta.TraceID,
		Path:    fmt.Sprintf("claims[%d]", c.Index),
	}
	if c.Turn >= 0 && c.Turn < len(ec.Episode.Turns) {
		ev.SpanID = ec.Episode.Turns[c.Turn].Span.SpanID
	}
	if ec.Evidence == verdict.EvidenceNone {
		return ev
	}
	ev.Excerpt = claimText(ec, c, 200)
	return ev
}

// claimText renders claim text for a finding. Claim text is raw agent output,
// so the detector runs over it — a grounding finding must not carry a card
// number out with it. It masks only what it can detect; an excerpt is still
// model output, and `--evidence=none` is the lever when that is unacceptable.
func claimText(ec EvalContext, c episode.Claim, n int) string {
	text := c.Text
	if ec.Evidence != verdict.EvidenceFull {
		text = detect.MaskAll(text)
	}
	return truncate(text, n)
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// resolveRubricFile is used at compile time so a missing rubric is a load-time
// error rather than a mid-run surprise.
func resolveRubricFile(params map[string]any, read func(string) (string, error)) error {
	path, _ := params["rubric_file"].(string)
	if path == "" {
		return nil
	}
	body, err := read(path)
	if err != nil {
		return err
	}
	params["rubric"] = body
	delete(params, "rubric_file")
	return nil
}

var _ = cueeng.ValidName
