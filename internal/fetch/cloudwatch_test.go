package fetch

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// An empty result over the shared log group has a third cause beyond the two
// the message used to name: the agent may deliver spans to its own log group.
// That message is the only place a user learns the per-agent destination
// exists, so assert it names the flag and the path shape (ADR-007 §3).
func TestEmptyHintNamesPerAgentLogGroup(t *testing.T) {
	c := &Client{opt: Options{LogGroup: DefaultLogGroup}}
	hint := c.emptyHint()

	for _, want := range []string{
		"Transaction Search",
		"--since",
		"--log-group",
		"/aws/bedrock-agentcore/runtimes/",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint for %s does not mention %q:\n%s", DefaultLogGroup, want, hint)
		}
	}
}

// The hint used to send users straight to `--log-stream spans`. That stream can
// exist on a per-agent group and still be empty, in which case scoping to it
// returns zero events and reads as "no data" — so the advice now says to verify
// the destination first rather than to scope blindly.
func TestEmptyHintDoesNotRecommendScopingUnverified(t *testing.T) {
	c := &Client{opt: Options{LogGroup: DefaultLogGroup}}
	hint := c.emptyHint()

	if strings.Contains(hint, "--log-stream spans") {
		t.Errorf("hint still recommends an unverified --log-stream spans:\n%s", hint)
	}
	if !strings.Contains(hint, "describe-log-streams") {
		t.Errorf("hint does not say how to verify the destination:\n%s", hint)
	}
}

// On an explicit log group the remaining causes differ: the caller already
// chose the group, but an empty `spans` stream there means the spans went to
// the shared group after all, which is worth naming.
func TestEmptyHintOnExplicitLogGroup(t *testing.T) {
	c := &Client{opt: Options{LogGroup: "/aws/bedrock-agentcore/runtimes/agent-DEFAULT"}}
	hint := c.emptyHint()

	for _, want := range []string{"Transaction Search", "--since", "--log-stream", DefaultLogGroup} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint does not mention %q:\n%s", want, hint)
		}
	}
}

func spanRecord(logGroup string) json.RawMessage {
	return json.RawMessage(`{"resource":{"attributes":{
		"aws.log.group.names":"` + logGroup + `",
		"aws.log.stream.names":"otel-rt-logs"}},
		"traceId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","spanId":"1111111111111111",
		"name":"chat model-placeholder"}`)
}

// --no-content must not reach the network. The nil api field is the assertion:
// a query attempt would panic.
func TestWithContentSkippedWhenDisabled(t *testing.T) {
	c := &Client{opt: Options{NoContent: true}}
	in := &Result{TraceID: "t", Records: []json.RawMessage{spanRecord("/aws/bedrock-agentcore/runtimes/agent-ENV")}}

	got := c.withContent(context.Background(), in)
	if len(got.ContentRecords) != 0 || got.ContentSource != "" {
		t.Errorf("--no-content still fetched content: %d record(s) from %q",
			len(got.ContentRecords), got.ContentSource)
	}
}

// A trace that does not name a content log group degrades to spans-only rather
// than guessing a group name or failing the fetch.
func TestWithContentDegradesWhenSourceUnknown(t *testing.T) {
	c := &Client{opt: Options{}}
	in := &Result{
		TraceID: "t",
		Records: []json.RawMessage{json.RawMessage(`{"traceId":"t","spanId":"s"}`)},
	}

	got := c.withContent(context.Background(), in)
	if len(got.ContentRecords) != 0 {
		t.Error("content was invented for a trace that names no source")
	}
	if got.TraceID != "t" || len(got.Records) != 1 {
		t.Error("degrading dropped the span set")
	}
}

// When the spans already came from the stream that would carry the content,
// there is nothing to join and no reason to pay for a second scan.
func TestWithContentSkipsRedundantQuery(t *testing.T) {
	group := "/aws/bedrock-agentcore/runtimes/agent-ENV"
	c := &Client{opt: Options{LogGroup: group, LogStream: "otel-rt-logs"}}
	in := &Result{TraceID: "t", Records: []json.RawMessage{spanRecord(group)}}

	got := c.withContent(context.Background(), in)
	if len(got.ContentRecords) != 0 {
		t.Error("re-queried the stream the spans already came from")
	}
}
