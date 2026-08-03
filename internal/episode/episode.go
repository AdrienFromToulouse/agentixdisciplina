// Package episode defines the normalized Episode model (ADR-002).
//
// Evaluators never see raw spans. An adapter decodes a trace into an Episode,
// and every element carries a SpanRef so a finding can always be resolved back
// to a trace_id/span_id pair (ADR-001 §6).
package episode

const SchemaVersion = "episode/v1"

// Coverage flag names. A clause whose requirements exceed coverage reports
// SKIPPED, never PASS (ADR-003 §5).
const (
	HasMessageContent = "has_message_content"
	HasToolArgs       = "has_tool_args"
	HasToolResults    = "has_tool_results"
	HasTokenUsage     = "has_token_usage"
	HasCost           = "has_cost"
	HasRetrievalSpans = "has_retrieval_spans"
	HasAgentSpans     = "has_agent_spans"
)

// Episode is one normalized agent run.
type Episode struct {
	Meta      Meta       `json:"meta"`
	Turns     []Turn     `json:"turns"`
	ToolCalls []ToolCall `json:"tool_calls"`
	Claims    []Claim    `json:"claims"`
	Metrics   Metrics    `json:"metrics"`
	Coverage  Coverage   `json:"coverage"`
	Spans     []SpanRef  `json:"spans"`
}

type Meta struct {
	EpisodeID     string   `json:"episode_id"`
	TraceID       string   `json:"trace_id"`
	SchemaVersion string   `json:"schema_version"`
	RootAgent     string   `json:"root_agent,omitempty"`
	SessionID     string   `json:"session_id,omitempty"`
	Provider      string   `json:"provider,omitempty"`
	Models        []string `json:"models,omitempty"`
	StartedAt     int64    `json:"started_at_unix_nano"`
	EndedAt       int64    `json:"ended_at_unix_nano"`
	// Adapter records how this Episode was produced: "otlp/v1.41",
	// "cloudwatch-spans/v1", or "xray/v1" (ADR-007 §4).
	Adapter string `json:"adapter"`
}

// SpanRef locates evidence. Path is an accessor into the Episode, e.g.
// "turns[3].content".
type SpanRef struct {
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`
	Name    string `json:"name,omitempty"`
	Path    string `json:"path,omitempty"`
}

type Turn struct {
	Index     int    `json:"index"`
	Role      string `json:"role"` // user | assistant | system | tool
	AgentPath string `json:"agent_path,omitempty"`
	Text      string `json:"text"`
	// StartedAt is the start of the model call that produced this turn.
	// Grounding needs it to decide which tool results were available when
	// the turn was written.
	StartedAt int64   `json:"started_at_unix_nano"`
	Span      SpanRef `json:"span"`
	// Captured is false when the span existed but its content was not
	// recorded (GenAI content capture is opt-in; ADR-002 §1).
	Captured bool `json:"captured"`
}

type ToolCall struct {
	Index          int     `json:"index"`
	Name           string  `json:"name"`
	Kind           string  `json:"kind"` // tool | agent | retrieval | unknown
	Arguments      string  `json:"arguments,omitempty"`
	Result         string  `json:"result,omitempty"`
	Error          string  `json:"error,omitempty"`
	AgentPath      string  `json:"agent_path,omitempty"`
	StartedAt      int64   `json:"started_at_unix_nano"`
	EndedAt        int64   `json:"ended_at_unix_nano"`
	DurationMS     int64   `json:"duration_ms"`
	Span           SpanRef `json:"span"`
	ArgsCaptured   bool    `json:"args_captured"`
	ResultCaptured bool    `json:"result_captured"`
}

// Claim is the only inferred field in the Episode. Extractor provenance
// propagates to verdict class: a deterministic engine reading an llm-extracted
// claim still yields a probabilistic verdict (ADR-002 §4).
type Claim struct {
	Index     int       `json:"index"`
	Text      string    `json:"text"`
	Support   []SpanRef `json:"support,omitempty"`
	Extractor string    `json:"extractor"` // structural | llm | plugin
	Turn      int       `json:"turn"`
}

type Metrics struct {
	DurationMS   int64 `json:"duration_ms"`
	LatencyP50MS int64 `json:"latency_p50_ms"`
	LatencyP95MS int64 `json:"latency_p95_ms"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	ModelCalls   int   `json:"model_calls"`
	ToolCalls    int   `json:"tool_calls"`
	ToolErrors   int   `json:"tool_errors"`
	Steps        int   `json:"steps"`
}

// Coverage records what this trace can and cannot support. Absent data is
// absent *and* recorded, so an evaluator can distinguish "no tool calls
// happened" from "tool calls happened but were not captured".
type Coverage struct {
	HasMessageContent bool     `json:"has_message_content"`
	HasToolArgs       bool     `json:"has_tool_args"`
	HasToolResults    bool     `json:"has_tool_results"`
	HasTokenUsage     bool     `json:"has_token_usage"`
	HasCost           bool     `json:"has_cost"`
	HasRetrievalSpans bool     `json:"has_retrieval_spans"`
	HasAgentSpans     bool     `json:"has_agent_spans"`
	Degraded          []string `json:"degraded,omitempty"`
}

// Has reports whether a named coverage flag is satisfied.
func (c Coverage) Has(flag string) bool {
	switch flag {
	case HasMessageContent:
		return c.HasMessageContent
	case HasToolArgs:
		return c.HasToolArgs
	case HasToolResults:
		return c.HasToolResults
	case HasTokenUsage:
		return c.HasTokenUsage
	case HasCost:
		return c.HasCost
	case HasRetrievalSpans:
		return c.HasRetrievalSpans
	case HasAgentSpans:
		return c.HasAgentSpans
	}
	return false
}

// Missing returns the subset of required flags this coverage does not satisfy.
func (c Coverage) Missing(required []string) []string {
	var out []string
	for _, f := range required {
		if !c.Has(f) {
			out = append(out, f)
		}
	}
	return out
}
