package adapter

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/AdrienFromToulouse/agentixdisciplina/internal/episode"
)

// Fixtures below are hand-written to the shapes AgentCore actually emits. They
// carry no account, agent, session, or trace identifier and no real prompt: the
// structure is what the adapter is contracted to, the data is not.
const (
	testTraceID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	agentSpanID = "1111111111111111"
	chatSpanID  = "2222222222222222"
	toolSpanID  = "3333333333333333"
)

// spanRecords is the span tree as it arrives from the span log group: the
// trajectory, the token counts, and no message content whatsoever.
func spanRecords() []json.RawMessage {
	resource := `"resource":{"attributes":{
		"service.name":"agent.ENV",
		"aws.log.group.names":"/aws/bedrock-agentcore/runtimes/agent-ENV",
		"aws.log.stream.names":"otel-rt-logs"}}`
	return []json.RawMessage{
		json.RawMessage(`{` + resource + `,
			"traceId":"` + testTraceID + `","spanId":"` + agentSpanID + `",
			"name":"invoke_agent Strands Agents","kind":"SERVER",
			"startTimeUnixNano":1000000000,"endTimeUnixNano":9000000000,
			"attributes":{"gen_ai.operation.name":"invoke_agent",
				"gen_ai.agent.name":"Strands Agents",
				"session.id":"session-placeholder"},
			"status":{"code":"UNSET"}}`),
		json.RawMessage(`{` + resource + `,
			"traceId":"` + testTraceID + `","spanId":"` + chatSpanID + `",
			"parentSpanId":"` + agentSpanID + `",
			"name":"chat model-placeholder","kind":"CLIENT",
			"startTimeUnixNano":2000000000,"endTimeUnixNano":5000000000,
			"attributes":{"gen_ai.operation.name":"chat",
				"gen_ai.request.model":"model-placeholder",
				"gen_ai.usage.input_tokens":100,"gen_ai.usage.output_tokens":20},
			"status":{"code":"UNSET"}}`),
		json.RawMessage(`{` + resource + `,
			"traceId":"` + testTraceID + `","spanId":"` + toolSpanID + `",
			"parentSpanId":"` + agentSpanID + `",
			"name":"execute_tool lookup_order","kind":"INTERNAL",
			"startTimeUnixNano":5500000000,"endTimeUnixNano":6000000000,
			"attributes":{"gen_ai.operation.name":"execute_tool",
				"gen_ai.tool.name":"lookup_order"},
			"status":{"code":"UNSET"}}`),
	}
}

// contentRecords is the same trace's message bodies as they arrive from the
// agent's own log group: one JSON string nested inside the record's JSON.
func contentRecords() []json.RawMessage {
	return []json.RawMessage{
		json.RawMessage(`{"scope":{"name":"strands.telemetry.tracer"},
			"traceId":"` + testTraceID + `","spanId":"` + chatSpanID + `",
			"body":{
				"input":{"messages":[{"role":"user","content":{
					"content":"[{\"text\": \"where is order A1\"}]"}}]},
				"output":{"messages":[{"role":"assistant","content":{
					"finish_reason":"tool_use",
					"message":"[{\"reasoningContent\": {\"reasoningText\": {\"text\": \"looking it up\"}}}, {\"toolUse\": {\"toolUseId\": \"tooluse_PLACEHOLDER\", \"name\": \"lookup_order\", \"input\": {\"id\": \"A1\"}}}]"}}]}}}`),
		json.RawMessage(`{"scope":{"name":"strands.telemetry.tracer"},
			"traceId":"` + testTraceID + `","spanId":"` + toolSpanID + `",
			"body":{
				"input":{"messages":[{"role":"tool","content":{
					"role":"tool","content":"{\"id\": \"A1\"}","id":"tooluse_PLACEHOLDER"}}]},
				"output":{"messages":[{"role":"assistant","content":{
					"message":"[{\"text\": \"{\\\"status\\\": \\\"shipped\\\"}\"}]",
					"id":"tooluse_PLACEHOLDER"}}]}}}`),
		// Ordinary agent stdout shares the stream and must be ignored.
		json.RawMessage(`{"scope":{"name":"botocore.credentials"},
			"traceId":"","spanId":"","body":"Found credentials from IAM Role"}`),
	}
}

func buildFrom(t *testing.T, spanRecs, contentRecs []json.RawMessage) *episode.Episode {
	t.Helper()
	spans, err := DecodeCloudWatchSpans(spanRecs)
	if err != nil {
		t.Fatalf("decode spans: %v", err)
	}
	spans, _ = MergeContentRecords(spans, contentRecs)
	ep, err := BuildEpisode(spans, AdapterCloudWatch)
	if err != nil {
		t.Fatalf("build episode: %v", err)
	}
	return ep
}

// The span log group alone cannot support the content clauses. Asserting the
// absence explicitly is what keeps SKIP from drifting into a vacuous PASS
// (ADR-003 §5).
func TestSpansOnlyHasNoMessageContent(t *testing.T) {
	ep := buildFrom(t, spanRecords(), nil)

	if ep.Coverage.HasMessageContent {
		t.Error("spans without content records must not report has_message_content")
	}
	if ep.Coverage.HasToolArgs || ep.Coverage.HasToolResults {
		t.Errorf("spans alone reported tool args=%v results=%v, want both false",
			ep.Coverage.HasToolArgs, ep.Coverage.HasToolResults)
	}
	// The trajectory is still recoverable, which is why this path stays useful.
	if !ep.Coverage.HasAgentSpans || len(ep.ToolCalls) != 1 {
		t.Errorf("agent spans=%v tool calls=%d, want true and 1",
			ep.Coverage.HasAgentSpans, len(ep.ToolCalls))
	}
	if !ep.Coverage.HasTokenUsage {
		t.Error("token usage lives on the spans and should survive without content")
	}
}

// Joining the content log records is what makes the other half of the contract
// evaluable (ADR-007 §4).
func TestMergeContentRecordsLightsUpCoverage(t *testing.T) {
	ep := buildFrom(t, spanRecords(), contentRecords())

	if !ep.Coverage.HasMessageContent {
		t.Fatal("merged content did not set has_message_content")
	}
	if !ep.Coverage.HasToolArgs {
		t.Error("merged content did not set has_tool_args")
	}
	if !ep.Coverage.HasToolResults {
		t.Error("merged content did not set has_tool_results")
	}

	var got []string
	for _, turn := range ep.Turns {
		if !turn.Captured {
			t.Errorf("turn %d is uncaptured after a content merge", turn.Index)
		}
		got = append(got, turn.Role+":"+turn.Text)
	}
	for _, want := range []string{"user:where is order A1", "assistant:looking it up"} {
		if !slices.Contains(got, want) {
			t.Errorf("turns %q missing %q", got, want)
		}
	}
	// The tool-use block is not rendered as turn text: the call is its own
	// span, so doing that would report the same content twice.
	for _, turn := range ep.Turns {
		if strings.Contains(turn.Text, "toolUse") || strings.Contains(turn.Text, "tooluse_") {
			t.Errorf("turn %d leaked a tool-use block into text: %q", turn.Index, turn.Text)
		}
	}

	if len(ep.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(ep.ToolCalls))
	}
	tc := ep.ToolCalls[0]
	if tc.Arguments != `{"id": "A1"}` {
		t.Errorf("tool arguments = %q", tc.Arguments)
	}
	if tc.Result != `{"status": "shipped"}` {
		t.Errorf("tool result = %q", tc.Result)
	}
	if !tc.ArgsCaptured || !tc.ResultCaptured {
		t.Errorf("tool call capture flags: args=%v result=%v", tc.ArgsCaptured, tc.ResultCaptured)
	}
}

// Every finding needs a span, so merged content must stay anchored to the span
// it came from (ADR-001 §6).
func TestMergedContentKeepsSpanAnchors(t *testing.T) {
	ep := buildFrom(t, spanRecords(), contentRecords())

	for _, turn := range ep.Turns {
		if turn.Span.TraceID != testTraceID || turn.Span.SpanID != chatSpanID {
			t.Errorf("turn %d anchored to %s/%s, want %s/%s",
				turn.Index, turn.Span.TraceID, turn.Span.SpanID, testTraceID, chatSpanID)
		}
		if turn.Span.Path == "" {
			t.Errorf("turn %d has no evidence path", turn.Index)
		}
	}
	if ep.ToolCalls[0].Span.SpanID != toolSpanID {
		t.Errorf("tool call anchored to %s, want %s", ep.ToolCalls[0].Span.SpanID, toolSpanID)
	}
}

// CloudWatch does not promise an order, so the merge must not depend on one:
// determinism is a property of the whole input closure (ADR-001 §3).
func TestMergeContentRecordsIsOrderIndependent(t *testing.T) {
	forward := contentRecords()
	reversed := make([]json.RawMessage, len(forward))
	for i, r := range forward {
		reversed[len(forward)-1-i] = r
	}

	a, err := json.Marshal(buildFrom(t, spanRecords(), forward))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(buildFrom(t, spanRecords(), reversed))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Error("reordering the content records changed the Episode")
	}
}

// A span that carried its own content is the more canonical source; a log
// record must never silently replace it.
func TestMergeDoesNotOverwriteSpanContent(t *testing.T) {
	spans, err := DecodeCloudWatchSpans(spanRecords())
	if err != nil {
		t.Fatal(err)
	}
	for i := range spans {
		if spans[i].SpanID == chatSpanID {
			spans[i].Attrs[attrInputMsgs] = `[{"role":"user","parts":[{"text":"from the span"}]}]`
		}
	}

	merged, n := MergeContentRecords(spans, contentRecords())
	if n == 0 {
		t.Fatal("merge reported nothing enriched")
	}
	for _, s := range merged {
		if s.SpanID != chatSpanID {
			continue
		}
		if got := str(s.Attrs[attrInputMsgs]); !strings.Contains(got, "from the span") {
			t.Errorf("span-carried input messages were overwritten: %q", got)
		}
		// The output side was absent, so it should still have been filled in.
		if s.Attrs[attrOutputMsgs] == nil {
			t.Error("absent output messages were not filled from the content record")
		}
	}
}

// Span ids are only unique within a trace. Grafting one trace's content onto
// another would fabricate evidence, so disagreement is a skip, not a merge.
func TestMergeRejectsForeignTraceContent(t *testing.T) {
	foreign := []json.RawMessage{
		json.RawMessage(`{"scope":{"name":"strands.telemetry.tracer"},
			"traceId":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","spanId":"` + chatSpanID + `",
			"body":{"input":{"messages":[{"role":"user","content":{
				"content":"[{\"text\": \"other trace\"}]"}}]}}}`),
	}

	ep := buildFrom(t, spanRecords(), foreign)
	if ep.Coverage.HasMessageContent {
		t.Error("content from a different trace was merged in")
	}
	for _, turn := range ep.Turns {
		if strings.Contains(turn.Text, "other trace") {
			t.Errorf("foreign content reached turn %d", turn.Index)
		}
	}
}

// The content source is read off the trace, so no account-specific log group
// name has to be configured or hardcoded (ADR-007 §3).
func TestContentSourceDerivedFromSpans(t *testing.T) {
	group, stream := ContentSource(spanRecords())
	if group != "/aws/bedrock-agentcore/runtimes/agent-ENV" {
		t.Errorf("log group = %q", group)
	}
	if stream != "otel-rt-logs" {
		t.Errorf("log stream = %q", stream)
	}

	// A trace that does not say is not an error; the caller degrades.
	silent := []json.RawMessage{json.RawMessage(`{"traceId":"x","spanId":"y"}`)}
	if g, s := ContentSource(silent); g != "" || s != "" {
		t.Errorf("silent trace yielded %q/%q, want empty", g, s)
	}
}
