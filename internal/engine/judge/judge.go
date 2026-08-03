// Package judge implements the LLM judge (ADR-001 §6).
//
// Judges filter what the agent *said*: helpfulness, tone, and groundedness.
// Those are irreducibly subjective. Their verdicts are probabilistic
// and therefore advisory by default: an evaluation tool that turns CI red on a
// rerun with no code change gets disabled within a month.
//
// Every verdict carries the model id, prompt hash, and effort level as
// evidence, so an advisory failure is still auditable.
package judge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/AdrienFromToulouse/agentixdisciplina/internal/episode"
	"github.com/anthropics/anthropic-sdk-go"
)

const (
	DefaultModel = "claude-opus-5"
	// Scoring a transcript against a rubric is a scoped classification
	// task, so the default is deliberately low. Raise it per clause when a
	// rubric needs real reasoning.
	DefaultEffort    = "low"
	DefaultMaxTokens = 2048
	// maxTranscript bounds the prompt. A judge reading a truncated episode
	// is marked degraded rather than silently scoring a fragment.
	maxTranscript = 60_000
)

type Config struct {
	Model     string
	Effort    string
	MaxTokens int64
	CachePath string
	NoCache   bool
	// Enabled forces judges on even when no API key is visible in the
	// environment (for credential sources the CLI cannot detect).
	Enabled bool
}

type Judge struct {
	cfg    Config
	client anthropic.Client
	cache  *Cache
}

type Request struct {
	Clause  string
	Rubric  string
	Episode *episode.Episode
}

// Verdict is what a judge returns. The provenance fields are the evidence
// that makes an advisory finding actionable.
type Verdict struct {
	Pass       bool    `json:"pass"`
	Score      float64 `json:"score"`
	Reasoning  string  `json:"reasoning"`
	ModelID    string  `json:"model_id"`
	PromptHash string  `json:"prompt_hash"`
	Effort     string  `json:"effort"`
	Cached     bool    `json:"cached"`
	Truncated  bool    `json:"truncated,omitempty"`
}

func New(cfg Config) *Judge {
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.Effort == "" {
		cfg.Effort = DefaultEffort
	}
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = DefaultMaxTokens
	}
	j := &Judge{cfg: cfg, client: anthropic.NewClient()}
	if !cfg.NoCache {
		j.cache = OpenCache(cfg.CachePath)
	}
	return j
}

// Available reports whether judges can run, and why not when they cannot.
//
// "Not configured" is a skip, not a failure: the clause could not be
// evaluated, which is a different thing from the check failing.
func (j *Judge) Available() (bool, string) {
	if j.cfg.Enabled {
		return true, ""
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return true, ""
	}
	return false, "no judge credentials found; set ANTHROPIC_API_KEY or pass --judge to force"
}

func ValidEffort(e string) bool {
	switch e {
	case "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}

const systemPrompt = `You are an evaluation judge for AI agent transcripts.

You are given a rubric and a transcript of one agent episode. Decide whether
the episode satisfies the rubric.

Judge only what the rubric asks about. Do not penalise the agent for things the
rubric does not mention, and do not reward it for them either. If the
transcript does not contain enough information to judge the rubric, fail it and
say what was missing rather than guessing.

Call submit_verdict exactly once with your decision.`

func (j *Judge) Judge(ctx context.Context, req Request) (*Verdict, error) {
	transcript, truncated := Transcript(req.Episode)
	prompt := fmt.Sprintf("<rubric>\n%s\n</rubric>\n\n<transcript>\n%s\n</transcript>",
		strings.TrimSpace(req.Rubric), transcript)

	hash := promptHash(j.cfg.Model, j.cfg.Effort, prompt)

	if j.cache != nil {
		if v, ok := j.cache.Get(hash); ok {
			v.Cached = true
			v.Truncated = truncated
			return v, nil
		}
	}

	tool := anthropic.ToolParam{
		Name:        "submit_verdict",
		Description: anthropic.String("Submit the evaluation verdict for this episode."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"pass": map[string]any{
					"type":        "boolean",
					"description": "True if the episode satisfies the rubric.",
				},
				"score": map[string]any{
					"type":        "number",
					"description": "Confidence from 0 to 1 that the episode satisfies the rubric.",
				},
				"reasoning": map[string]any{
					"type":        "string",
					"description": "One or two sentences citing the specific part of the transcript that decided it.",
				},
			},
			Required: []string{"pass", "score", "reasoning"},
		},
	}

	resp, err := j.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(j.cfg.Model),
		MaxTokens: j.cfg.MaxTokens,
		System:    []anthropic.TextBlockParam{{Text: systemPrompt}},
		Tools:     []anthropic.ToolUnionParam{{OfTool: &tool}},
		ToolChoice: anthropic.ToolChoiceUnionParam{
			OfTool: &anthropic.ToolChoiceToolParam{Name: "submit_verdict"},
		},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffort(j.cfg.Effort),
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("judge %s: %w", j.cfg.Model, err)
	}

	// A refusal is a real outcome, not a crash: surface it rather than
	// reading an empty content array.
	if resp.StopReason == anthropic.StopReasonRefusal {
		return nil, fmt.Errorf("judge declined to evaluate this episode (%s)", resp.StopDetails.Category)
	}

	v, err := decodeVerdict(resp)
	if err != nil {
		return nil, err
	}
	v.ModelID = string(resp.Model)
	v.PromptHash = hash
	v.Effort = j.cfg.Effort
	v.Truncated = truncated

	if j.cache != nil {
		j.cache.Put(hash, v)
	}
	return v, nil
}

func decodeVerdict(resp *anthropic.Message) (*Verdict, error) {
	for _, block := range resp.Content {
		use, ok := block.AsAny().(anthropic.ToolUseBlock)
		if !ok || use.Name != "submit_verdict" {
			continue
		}
		var v Verdict
		if err := json.Unmarshal([]byte(use.JSON.Input.Raw()), &v); err != nil {
			return nil, fmt.Errorf("judge returned malformed verdict: %w", err)
		}
		if v.Reasoning == "" {
			return nil, fmt.Errorf("judge returned a verdict with no reasoning")
		}
		return &v, nil
	}
	return nil, fmt.Errorf("judge produced no verdict (stop_reason %s)", resp.StopReason)
}

// Transcript renders an Episode for a judge, in the Episode's total order.
func Transcript(ep *episode.Episode) (string, bool) {
	var b strings.Builder
	type entry struct {
		at   int64
		text string
	}
	var entries []entry

	for _, t := range ep.Turns {
		if !t.Captured {
			entries = append(entries, entry{t.StartedAt,
				fmt.Sprintf("[%s] (content not captured)", t.Role)})
			continue
		}
		entries = append(entries, entry{t.StartedAt, fmt.Sprintf("[%s] %s", t.Role, t.Text)})
	}
	for _, tc := range ep.ToolCalls {
		line := fmt.Sprintf("[tool] %s(%s)", tc.Name, brief(tc.Arguments))
		switch {
		case tc.Error != "":
			line += " -> ERROR: " + tc.Error
		case tc.ResultCaptured:
			line += " -> " + brief(tc.Result)
		default:
			line += " -> (result not captured)"
		}
		entries = append(entries, entry{tc.StartedAt, line})
	}

	// Stable order without a sort dependency on wall-clock ties: the
	// Episode lists are already totally ordered, so a stable sort by start
	// time preserves it.
	for i := 1; i < len(entries); i++ {
		for k := i; k > 0 && entries[k].at < entries[k-1].at; k-- {
			entries[k], entries[k-1] = entries[k-1], entries[k]
		}
	}

	truncated := false
	for _, e := range entries {
		if b.Len()+len(e.text) > maxTranscript {
			b.WriteString("\n… transcript truncated …")
			truncated = true
			break
		}
		b.WriteString(e.text)
		b.WriteByte('\n')
	}
	return b.String(), truncated
}

func brief(s string) string {
	const max = 600
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	if s == "" {
		return "{}"
	}
	return s
}

func promptHash(model, effort, prompt string) string {
	h := sha256.New()
	h.Write([]byte(model))
	h.Write([]byte{0})
	h.Write([]byte(effort))
	h.Write([]byte{0})
	h.Write([]byte(prompt))
	return hex.EncodeToString(h.Sum(nil))[:32]
}
