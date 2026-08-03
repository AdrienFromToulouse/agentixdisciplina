package judge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/AdrienFromToulouse/agentixdisciplina/internal/episode"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/extract"
	"github.com/anthropics/anthropic-sdk-go"
)

// maxSourceView bounds the extraction prompt. Truncation is reported rather
// than hidden, because a partial view lowers recall and the caller needs to
// know that before trusting a clean result.
const maxSourceView = 80_000

// Extractor is the LLM claim extractor behind the verbatim gate (ADR-008).
//
// It reuses the judge's client, credentials, and cache: an extraction is the
// same shape of call, and caching by prompt hash makes repeated runs over an
// unchanged trace stable and free.
type Extractor struct {
	j *Judge
}

func NewExtractor(j *Judge) *Extractor { return &Extractor{j: j} }

func (e *Extractor) Name() string { return extract.ExtractorLLM }

func (e *Extractor) Available() (bool, string) {
	if e == nil || e.j == nil {
		return false, "the llm extractor is not configured"
	}
	return e.j.Available()
}

const extractSystem = `You extract factual assertions from an AI agent transcript.

You are given the transcript as a set of <source> blocks, each with an id. Find
every factual assertion the ASSISTANT makes: a statement about the world, the
user, an account, an amount, a status, or an outcome that could in principle be
checked against a source.

Rules:
- Ignore pleasantries, offers to help, questions, and restatements of what the
  user just said. Those assert nothing.
- Report an assertion even when it looks obviously true. Judging it is not your
  job.
- Every row must include a "snippet" copied CHARACTER-FOR-CHARACTER from the
  source you name. Reproduce it exactly, including any typo, odd spacing, or
  wrong unit the source contains. Never clean up, translate, correct, or
  paraphrase a snippet. A snippet you cannot copy exactly is a snippet you must
  not report.
- The snippet must come from the source whose id you put in "source_id".
- A snippet may span several lines if the sentence wraps.

Call submit_facts exactly once.`

func (e *Extractor) Facts(ctx context.Context, ep *episode.Episode) ([]extract.Fact, []extract.Rejected, error) {
	if ok, why := e.Available(); !ok {
		return nil, nil, fmt.Errorf("%s", why)
	}

	view, truncated := extract.SourceView(ep, maxSourceView)
	if strings.TrimSpace(view) == "" {
		return nil, nil, nil
	}
	prompt := "<transcript>\n" + view + "</transcript>"
	hash := promptHash(e.j.cfg.Model, e.j.cfg.Effort, "extract:"+prompt)

	var raw []byte
	if cached, ok := e.j.cacheRaw(hash); ok {
		raw = cached
	} else {
		tool := anthropic.ToolParam{
			Name:        "submit_facts",
			Description: anthropic.String("Submit every factual assertion found, each with a verbatim snippet."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"facts": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"claim": map[string]any{
									"type":        "string",
									"description": "The assertion, in your own words.",
								},
								"source_id": map[string]any{
									"type":        "string",
									"description": "The id of the <source> the snippet is copied from.",
								},
								"snippet": map[string]any{
									"type":        "string",
									"description": "Text copied character-for-character from that source.",
								},
							},
							"required": []string{"claim", "source_id", "snippet"},
						},
					},
				},
				Required: []string{"facts"},
			},
		}

		resp, err := e.j.client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.Model(e.j.cfg.Model),
			MaxTokens: 8192,
			System:    []anthropic.TextBlockParam{{Text: extractSystem}},
			Tools:     []anthropic.ToolUnionParam{{OfTool: &tool}},
			ToolChoice: anthropic.ToolChoiceUnionParam{
				OfTool: &anthropic.ToolChoiceToolParam{Name: "submit_facts"},
			},
			OutputConfig: anthropic.OutputConfigParam{
				Effort: anthropic.OutputConfigEffort(e.j.cfg.Effort),
			},
			Messages: []anthropic.MessageParam{
				anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
			},
		})
		if err != nil {
			return nil, nil, fmt.Errorf("extractor %s: %w", e.j.cfg.Model, err)
		}
		if resp.StopReason == anthropic.StopReasonRefusal {
			return nil, nil, fmt.Errorf("extractor declined this episode (%s)", resp.StopDetails.Category)
		}
		raw, err = factRows(resp)
		if err != nil {
			return nil, nil, err
		}
		e.j.putRaw(hash, raw)
	}

	facts, rejected, err := extract.Gate(ep, raw)
	if err != nil {
		return nil, nil, err
	}
	if truncated {
		rejected = append(rejected, extract.Rejected{
			Reason: "source view was truncated, so extraction did not see the whole episode",
		})
	}
	return facts, rejected, nil
}

// factRows pulls the raw `facts` array out of the tool call, so the gate can
// parse it with the same code path a test would use.
func factRows(resp *anthropic.Message) ([]byte, error) {
	for _, block := range resp.Content {
		use, ok := block.AsAny().(anthropic.ToolUseBlock)
		if !ok || use.Name != "submit_facts" {
			continue
		}
		var wrapper struct {
			Facts json.RawMessage `json:"facts"`
		}
		if err := json.Unmarshal([]byte(use.JSON.Input.Raw()), &wrapper); err != nil {
			return nil, fmt.Errorf("extractor returned malformed output: %w", err)
		}
		if len(wrapper.Facts) == 0 {
			return []byte("[]"), nil
		}
		return wrapper.Facts, nil
	}
	return nil, fmt.Errorf("extractor produced no facts call (stop_reason %s)", resp.StopReason)
}
