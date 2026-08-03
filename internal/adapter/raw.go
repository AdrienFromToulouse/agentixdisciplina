// Package adapter decodes traces into the normalized Episode model (ADR-002).
//
// Both supported sources (OTLP payloads and CloudWatch `aws/spans` records)
// carry the same GenAI semantic-convention vocabulary in different envelopes,
// so they converge on RawSpan and share all mapping below (ADR-007 §4).
package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/AdrienFromToulouse/agentixdisciplina/internal/episode"
)

// RawSpan is the envelope-independent span shape.
type RawSpan struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	Name         string
	StartNano    int64
	EndNano      int64
	Attrs        map[string]any
	Resource     map[string]any
	StatusCode   string // UNSET | OK | ERROR
	StatusMsg    string
}

// GenAI semantic-convention attribute keys (ADR-002 §3).
const (
	attrOperation  = "gen_ai.operation.name"
	attrAgentName  = "gen_ai.agent.name"
	attrToolName   = "gen_ai.tool.name"
	attrToolType   = "gen_ai.tool.type"
	attrToolArgs   = "gen_ai.tool.call.arguments"
	attrToolResult = "gen_ai.tool.call.result"
	attrInputMsgs  = "gen_ai.input.messages"
	attrOutputMsgs = "gen_ai.output.messages"
	attrReqModel   = "gen_ai.request.model"
	attrRespModel  = "gen_ai.response.model"
	attrProvider   = "gen_ai.provider.name"
	attrInTokens   = "gen_ai.usage.input_tokens"
	attrOutTokens  = "gen_ai.usage.output_tokens"
	attrSystem     = "gen_ai.system" // legacy provider key
	attrPromptDep  = "gen_ai.prompt"
	attrComplDep   = "gen_ai.completion"
	attrSessionID  = "session.id"
	attrAWSSession = "aws.bedrock.agentcore.session.id"
)

type message struct {
	Role  string `json:"role"`
	Parts []any  `json:"parts"`
	// Some instrumentations emit a flat content string instead of parts.
	Content any `json:"content"`
}

// BuildEpisode performs the shared mapping: total ordering, attribute
// decoding, coverage probing, and metric derivation.
func BuildEpisode(spans []RawSpan, adapterName string) (*episode.Episode, error) {
	if len(spans) == 0 {
		return nil, fmt.Errorf("no spans")
	}
	ordered := sortSpans(spans)

	ep := &episode.Episode{
		Meta: episode.Meta{
			TraceID:       ordered[0].TraceID,
			SchemaVersion: episode.SchemaVersion,
			Adapter:       adapterName,
		},
	}

	var (
		seenTurn        = map[string]bool{}
		modelDurations  []int64
		toolArgsTotal   int
		toolArgsMissing int
		toolResTotal    int
		toolResMissing  int
		anyContentAttr  bool
		modelSet        = map[string]bool{}
	)

	ep.Meta.StartedAt = ordered[0].StartNano
	for _, s := range ordered {
		if s.EndNano > ep.Meta.EndedAt {
			ep.Meta.EndedAt = s.EndNano
		}
		ep.Spans = append(ep.Spans, episode.SpanRef{
			TraceID: s.TraceID, SpanID: s.SpanID, Name: s.Name,
		})

		if v := str(s.Attrs[attrProvider]); v != "" {
			ep.Meta.Provider = v
		} else if v := str(s.Attrs[attrSystem]); v != "" && ep.Meta.Provider == "" {
			ep.Meta.Provider = v
		}
		for _, k := range []string{attrSessionID, attrAWSSession} {
			if v := str(s.Attrs[k]); v != "" && ep.Meta.SessionID == "" {
				ep.Meta.SessionID = v
			}
		}

		switch operation(s) {
		case "invoke_agent":
			ep.Coverage.HasAgentSpans = true
			name := str(s.Attrs[attrAgentName])
			if name == "" {
				name = suffixAfter(s.Name, "invoke_agent ")
			}
			if s.ParentSpanID == "" || ep.Meta.RootAgent == "" {
				ep.Meta.RootAgent = name
			}
			// A nested agent invocation is also an action: delegation is
			// governable by tool.allowlist (ADR-002 §6).
			if s.ParentSpanID != "" {
				ep.ToolCalls = append(ep.ToolCalls, toolCallFrom(s, name, "agent", &toolArgsTotal, &toolArgsMissing, &toolResTotal, &toolResMissing))
			}

		case "chat", "text_completion", "generate_content":
			ep.Metrics.ModelCalls++
			if s.EndNano > s.StartNano {
				modelDurations = append(modelDurations, (s.EndNano-s.StartNano)/1e6)
			}
			for _, k := range []string{attrReqModel, attrRespModel} {
				if v := str(s.Attrs[k]); v != "" {
					modelSet[v] = true
				}
			}
			if v, ok := num(s.Attrs[attrInTokens]); ok {
				ep.Metrics.InputTokens += v
				ep.Coverage.HasTokenUsage = true
			}
			if v, ok := num(s.Attrs[attrOutTokens]); ok {
				ep.Metrics.OutputTokens += v
				ep.Coverage.HasTokenUsage = true
			}

			in, okIn := decodeMessages(s.Attrs[attrInputMsgs])
			out, okOut := decodeMessages(s.Attrs[attrOutputMsgs])
			if !okIn {
				if v := str(s.Attrs[attrPromptDep]); v != "" {
					in, okIn = []message{{Role: "user", Content: v}}, true
					ep.Coverage.Degraded = appendOnce(ep.Coverage.Degraded,
						"trace uses deprecated gen_ai.prompt; mapped as user content")
				}
			}
			if !okOut {
				if v := str(s.Attrs[attrComplDep]); v != "" {
					out, okOut = []message{{Role: "assistant", Content: v}}, true
					ep.Coverage.Degraded = appendOnce(ep.Coverage.Degraded,
						"trace uses deprecated gen_ai.completion; mapped as assistant content")
				}
			}
			if okIn || okOut {
				anyContentAttr = true
			}
			// Later chat spans repeat earlier history in their input
			// messages; dedupe by (role, text) so the conversation
			// reconstructs without duplication.
			for _, m := range append(in, out...) {
				role, text := m.Role, messageText(m)
				if role == "" || text == "" {
					continue
				}
				key := role + "\x00" + text
				if seenTurn[key] {
					continue
				}
				seenTurn[key] = true
				ep.Turns = append(ep.Turns, episode.Turn{
					Index: len(ep.Turns), Role: role, Text: text, Captured: true,
					StartedAt: s.StartNano,
					Span: episode.SpanRef{TraceID: s.TraceID, SpanID: s.SpanID, Name: s.Name,
						Path: fmt.Sprintf("turns[%d].text", len(ep.Turns))},
				})
			}
			if !okIn && !okOut {
				ep.Turns = append(ep.Turns, episode.Turn{
					Index: len(ep.Turns), Role: "assistant", Captured: false,
					StartedAt: s.StartNano,
					Span: episode.SpanRef{TraceID: s.TraceID, SpanID: s.SpanID, Name: s.Name,
						Path: fmt.Sprintf("turns[%d]", len(ep.Turns))},
				})
			}

		case "execute_tool":
			name := str(s.Attrs[attrToolName])
			if name == "" {
				name = suffixAfter(s.Name, "execute_tool ")
			}
			kind := str(s.Attrs[attrToolType])
			if kind == "" {
				kind = "tool"
			}
			if strings.Contains(strings.ToLower(kind), "retriev") ||
				strings.Contains(strings.ToLower(name), "retriev") ||
				strings.Contains(strings.ToLower(name), "search") {
				ep.Coverage.HasRetrievalSpans = true
			}
			ep.ToolCalls = append(ep.ToolCalls, toolCallFrom(s, name, kind,
				&toolArgsTotal, &toolArgsMissing, &toolResTotal, &toolResMissing))
		}
	}

	for i := range ep.ToolCalls {
		ep.ToolCalls[i].Index = i
		ep.ToolCalls[i].Span.Path = fmt.Sprintf("tool_calls[%d]", i)
		if ep.ToolCalls[i].Error != "" {
			ep.Metrics.ToolErrors++
		}
	}
	for m := range modelSet {
		ep.Meta.Models = append(ep.Meta.Models, m)
	}
	sort.Strings(ep.Meta.Models)

	ep.Metrics.ToolCalls = len(ep.ToolCalls)
	ep.Metrics.Steps = ep.Metrics.ModelCalls + ep.Metrics.ToolCalls
	ep.Metrics.DurationMS = (ep.Meta.EndedAt - ep.Meta.StartedAt) / 1e6
	ep.Metrics.LatencyP50MS = percentile(modelDurations, 50)
	ep.Metrics.LatencyP95MS = percentile(modelDurations, 95)

	ep.Coverage.HasMessageContent = anyContentAttr
	ep.Coverage.HasToolArgs = toolArgsTotal > 0 && toolArgsMissing < toolArgsTotal
	ep.Coverage.HasToolResults = toolResTotal > 0 && toolResMissing < toolResTotal
	// No pricing table in v0, so cost is never derived rather than guessed.
	ep.Coverage.HasCost = false

	if toolArgsMissing > 0 {
		ep.Coverage.Degraded = append(ep.Coverage.Degraded,
			fmt.Sprintf("%d of %d tool calls have no captured arguments", toolArgsMissing, toolArgsTotal))
	}
	if toolResMissing > 0 {
		ep.Coverage.Degraded = append(ep.Coverage.Degraded,
			fmt.Sprintf("%d of %d tool calls have no captured result", toolResMissing, toolResTotal))
	}

	ep.Meta.EpisodeID = episodeID(ordered)
	return ep, nil
}

func toolCallFrom(s RawSpan, name, kind string, argsTotal, argsMissing, resTotal, resMissing *int) episode.ToolCall {
	args := str(s.Attrs[attrToolArgs])
	res := str(s.Attrs[attrToolResult])
	*argsTotal++
	if args == "" {
		*argsMissing++
	}
	*resTotal++
	if res == "" {
		*resMissing++
	}
	tc := episode.ToolCall{
		Name: name, Kind: kind,
		Arguments: args, Result: res,
		StartedAt: s.StartNano, EndedAt: s.EndNano,
		DurationMS:     (s.EndNano - s.StartNano) / 1e6,
		ArgsCaptured:   args != "",
		ResultCaptured: res != "",
		Span:           episode.SpanRef{TraceID: s.TraceID, SpanID: s.SpanID, Name: s.Name},
	}
	if s.StatusCode == "ERROR" {
		tc.Error = s.StatusMsg
		if tc.Error == "" {
			tc.Error = "error"
		}
	}
	return tc
}

// sortSpans applies the total order from ADR-002 §5: start time, then
// parent-before-child, then span id. Rule 3 is arbitrary but total, which is
// the only property determinism needs.
func sortSpans(spans []RawSpan) []RawSpan {
	byID := map[string]RawSpan{}
	for _, s := range spans {
		byID[s.SpanID] = s
	}
	depth := map[string]int{}
	var d func(string, int) int
	d = func(id string, guard int) int {
		if v, ok := depth[id]; ok {
			return v
		}
		if guard > 64 {
			return guard
		}
		s, ok := byID[id]
		if !ok || s.ParentSpanID == "" {
			depth[id] = 0
			return 0
		}
		v := d(s.ParentSpanID, guard+1) + 1
		depth[id] = v
		return v
	}
	out := append([]RawSpan(nil), spans...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StartNano != out[j].StartNano {
			return out[i].StartNano < out[j].StartNano
		}
		di, dj := d(out[i].SpanID, 0), d(out[j].SpanID, 0)
		if di != dj {
			return di < dj
		}
		return out[i].SpanID < out[j].SpanID
	})
	return out
}

func episodeID(spans []RawSpan) string {
	ids := make([]string, 0, len(spans))
	for _, s := range spans {
		ids = append(ids, s.SpanID)
	}
	sort.Strings(ids)
	h := sha256.New()
	h.Write([]byte(spans[0].TraceID))
	for _, id := range ids {
		h.Write([]byte(id))
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func operation(s RawSpan) string {
	if v := str(s.Attrs[attrOperation]); v != "" {
		return v
	}
	// v1.41 requires the tool name in the span name, so tool identity is
	// recoverable even from traces that captured nothing else.
	switch {
	case strings.HasPrefix(s.Name, "execute_tool"):
		return "execute_tool"
	case strings.HasPrefix(s.Name, "invoke_agent"):
		return "invoke_agent"
	case strings.HasPrefix(s.Name, "chat"):
		return "chat"
	}
	return ""
}

func decodeMessages(v any) ([]message, bool) {
	if v == nil {
		return nil, false
	}
	var raw []byte
	switch t := v.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, false
		}
		raw = []byte(t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return nil, false
		}
		raw = b
	}
	var msgs []message
	if err := json.Unmarshal(raw, &msgs); err == nil && len(msgs) > 0 {
		return msgs, true
	}
	var one message
	if err := json.Unmarshal(raw, &one); err == nil && (one.Role != "" || one.Content != nil) {
		return []message{one}, true
	}
	return nil, false
}

func messageText(m message) string {
	var b strings.Builder
	for _, p := range m.Parts {
		switch t := p.(type) {
		case string:
			b.WriteString(t)
		case map[string]any:
			for _, k := range []string{"content", "text"} {
				if s := str(t[k]); s != "" {
					b.WriteString(s)
				}
			}
		}
	}
	if b.Len() == 0 {
		switch t := m.Content.(type) {
		case string:
			b.WriteString(t)
		case []any:
			for _, p := range t {
				if mp, ok := p.(map[string]any); ok {
					for _, k := range []string{"text", "content"} {
						if s := str(mp[k]); s != "" {
							b.WriteString(s)
						}
					}
				}
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func percentile(v []int64, p int) int64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := (p * (len(s) - 1)) / 100
	return s[i]
}

func suffixAfter(s, prefix string) string {
	if strings.HasPrefix(s, prefix) {
		return strings.TrimSpace(strings.TrimPrefix(s, prefix))
	}
	return s
}

func appendOnce(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return fmt.Sprintf("%g", t)
	case bool:
		return fmt.Sprintf("%t", t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func num(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case int64:
		return t, true
	case json.Number:
		i, err := t.Int64()
		return i, err == nil
	case string:
		var f float64
		if _, err := fmt.Sscanf(t, "%g", &f); err == nil {
			return int64(f), true
		}
	}
	return 0, false
}
