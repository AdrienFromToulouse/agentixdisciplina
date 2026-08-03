package adapter

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	AdapterCloudWatch = "cloudwatch-spans/v1"
	// AdapterXRay marks an Episode reconstructed from X-Ray segment
	// documents. Degraded input must never gate a build (ADR-007 §4).
	AdapterXRay = "xray/v1"
)

// Content acquisition constants. AgentCore delivers GenAI message content as
// OTel *log records*, not span attributes: the span log group carries the tree
// and the agent's own log group carries `{input, output}` bodies keyed by span
// id. Spans alone therefore report `has_message_content = false` even when
// content capture is enabled, and the join is what makes the content clauses
// evaluable (ADR-007 §4).
const (
	// ContentLogGroupAttr and ContentLogStreamAttr are resource attributes the
	// spans carry about themselves, so the content source is derived from the
	// trace rather than configured. Nothing account-specific is hardcoded.
	ContentLogGroupAttr  = "aws.log.group.names"
	ContentLogStreamAttr = "aws.log.stream.names"
	// ContentScope is the instrumentation scope of the records that carry
	// message bodies. Every other record in that stream is ordinary agent
	// stdout and is skipped.
	ContentScope = "strands.telemetry.tracer"
)

// ContentSource reports where the message bodies for these span records live,
// read from the records' own resource attributes. Both results are empty when
// the trace does not say, which is not an error: the caller degrades to
// spans-only rather than guessing a log group name.
//
// This takes raw records rather than decoded spans so that acquisition can ask
// the question before decoding, without a second copy of the record shape.
func ContentSource(records []json.RawMessage) (logGroup, logStream string) {
	for _, rec := range records {
		var m struct {
			Resource struct {
				Attributes map[string]any `json:"attributes"`
			} `json:"resource"`
		}
		if json.Unmarshal(rec, &m) != nil {
			continue
		}
		if logGroup == "" {
			logGroup = str(m.Resource.Attributes[ContentLogGroupAttr])
		}
		if logStream == "" {
			logStream = str(m.Resource.Attributes[ContentLogStreamAttr])
		}
		if logGroup != "" && logStream != "" {
			break
		}
	}
	return logGroup, logStream
}

// contentBody is one decoded `{input, output}` record awaiting a span.
type contentBody struct {
	sortKey string
	traceID string
	input   any
	output  any
}

// MergeContentRecords writes message bodies from content log records onto the
// spans they belong to, joined on span id, and reports how many spans it
// enriched. Content lands in the same attributes an OTLP trace would have
// carried it in, so BuildEpisode, the evaluators, and the clause registry need
// no knowledge of this path.
//
// A span that already carries content keeps it: the span is the more canonical
// source, and a log record must never silently overwrite it.
func MergeContentRecords(spans []RawSpan, records []json.RawMessage) ([]RawSpan, int) {
	if len(spans) == 0 || len(records) == 0 {
		return spans, 0
	}

	bySpan := map[string][]contentBody{}
	for _, rec := range records {
		var m map[string]any
		if json.Unmarshal(rec, &m) != nil {
			continue
		}
		if sc, ok := m["scope"].(map[string]any); ok {
			if name := str(sc["name"]); name != "" && name != ContentScope {
				continue
			}
		}
		spanID := firstStr(m, "spanId", "span_id", "SpanId")
		body, ok := m["body"].(map[string]any)
		if spanID == "" || !ok {
			continue
		}
		if body["input"] == nil && body["output"] == nil {
			continue
		}
		bySpan[spanID] = append(bySpan[spanID], contentBody{
			// Records are keyed by their own bytes so that repeated records
			// for one span apply in a stable order regardless of the order
			// CloudWatch returned them. Determinism is a property of the whole
			// input closure (ADR-001 §3).
			sortKey: string(rec),
			traceID: normalizeTraceID(firstStr(m, "traceId", "trace_id", "TraceId")),
			input:   body["input"],
			output:  body["output"],
		})
	}
	for id := range bySpan {
		sort.Slice(bySpan[id], func(i, j int) bool {
			return bySpan[id][i].sortKey < bySpan[id][j].sortKey
		})
	}

	var enriched int
	out := append([]RawSpan(nil), spans...)
	for i := range out {
		bodies := bySpan[out[i].SpanID]
		if len(bodies) == 0 {
			continue
		}
		if out[i].Attrs == nil {
			out[i].Attrs = map[string]any{}
		}
		var applied bool
		for _, b := range bodies {
			// A span id collision across traces would silently graft one
			// trace's content onto another's, so require agreement when the
			// record says which trace it belongs to.
			if b.traceID != "" && out[i].TraceID != "" && b.traceID != out[i].TraceID {
				continue
			}
			if applyContent(&out[i], b) {
				applied = true
			}
		}
		if applied {
			enriched++
		}
	}
	return out, enriched
}

// applyContent maps one body onto one span. Tool spans carry their payload in
// the tool-call attributes and model spans in the message attributes, matching
// where each already gets read from.
func applyContent(s *RawSpan, b contentBody) bool {
	var set bool
	if operation(*s) == "execute_tool" {
		if args, id := strandsToolPayload(b.input); args != "" {
			set = setAttrIfAbsent(s, attrToolArgs, args) || set
			if id != "" {
				set = setAttrIfAbsent(s, attrToolCallID, id) || set
			}
		}
		if res, id := strandsToolPayload(b.output); res != "" {
			set = setAttrIfAbsent(s, attrToolResult, res) || set
			if id != "" {
				set = setAttrIfAbsent(s, attrToolCallID, id) || set
			}
		}
		return set
	}
	if msgs := strandsMessages(b.input); len(msgs) > 0 {
		set = setAttrIfAbsent(s, attrInputMsgs, msgs) || set
	}
	if msgs := strandsMessages(b.output); len(msgs) > 0 {
		set = setAttrIfAbsent(s, attrOutputMsgs, msgs) || set
	}
	return set
}

// setAttrIfAbsent refuses to overwrite anything already present and non-empty,
// so a span that carried its own content wins over a log record.
func setAttrIfAbsent(s *RawSpan, key string, v any) bool {
	if existing, ok := s.Attrs[key]; ok && existing != nil {
		if t, isStr := existing.(string); !isStr || strings.TrimSpace(t) != "" {
			return false
		}
	}
	s.Attrs[key] = v
	return true
}

// strandsMessages normalizes `{"messages":[{role, content:{…}}]}` into the
// semantic-convention message shape BuildEpisode already decodes.
func strandsMessages(v any) []any {
	wrapper, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	list, ok := wrapper["messages"].([]any)
	if !ok {
		return nil
	}
	var out []any
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := str(m["role"])
		payload, _ := strandsPayload(m["content"])
		parts := strandsParts(payload)
		if role == "" || len(parts) == 0 {
			continue
		}
		out = append(out, map[string]any{"role": role, "parts": parts})
	}
	return out
}

// strandsToolPayload returns the tool arguments or result carried by a
// `{"messages":[…]}` wrapper, plus the tool-use id that correlates the two.
func strandsToolPayload(v any) (payload, id string) {
	wrapper, ok := v.(map[string]any)
	if !ok {
		return "", ""
	}
	list, ok := wrapper["messages"].([]any)
	if !ok {
		return "", ""
	}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		raw, callID := strandsPayload(m["content"])
		if raw == "" {
			continue
		}
		// Prefer the text blocks when the payload is a content array (tool
		// results), and fall back to the payload itself when it is already the
		// bare JSON object a tool was called with.
		if text := joinParts(strandsParts(raw)); text != "" {
			return text, callID
		}
		return raw, callID
	}
	return "", ""
}

// strandsPayload digs the string payload out of a Strands content object. The
// body nests one JSON string inside the record's own JSON, so this is the
// single unwrap every shape needs.
func strandsPayload(v any) (payload, id string) {
	switch t := v.(type) {
	case string:
		return t, ""
	case map[string]any:
		id = str(t["id"])
		for _, k := range []string{"content", "message"} {
			if s := str(t[k]); s != "" {
				return s, id
			}
		}
		// Some blocks nest one level further before the payload appears.
		for _, k := range []string{"content", "message"} {
			if inner, ok := t[k].(map[string]any); ok {
				if s, innerID := strandsPayload(inner); s != "" {
					if id == "" {
						id = innerID
					}
					return s, id
				}
			}
		}
	}
	return "", id
}

// strandsParts turns one payload into message parts, reading the content-block
// kinds AgentCore emits. Tool-use blocks are deliberately not rendered as
// text: the call is its own span, and duplicating its arguments here would
// report the same content twice.
func strandsParts(payload string) []any {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return nil
	}
	var blocks []any
	if err := json.Unmarshal([]byte(payload), &blocks); err != nil {
		// Not a content array: the payload is the content.
		return []any{map[string]any{"text": payload}}
	}
	var out []any
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			if s := str(b); s != "" {
				out = append(out, map[string]any{"text": s})
			}
			continue
		}
		for _, text := range blockText(block) {
			out = append(out, map[string]any{"text": text})
		}
	}
	return out
}

func blockText(block map[string]any) []string {
	if s := str(block["text"]); s != "" {
		return []string{s}
	}
	if rc, ok := block["reasoningContent"].(map[string]any); ok {
		if rt, ok := rc["reasoningText"].(map[string]any); ok {
			if s := str(rt["text"]); s != "" {
				return []string{s}
			}
		}
	}
	if tr, ok := block["toolResult"].(map[string]any); ok {
		if inner, ok := tr["content"].([]any); ok {
			var out []string
			for _, c := range inner {
				if cm, ok := c.(map[string]any); ok {
					if s := str(cm["text"]); s != "" {
						out = append(out, s)
					}
				}
			}
			return out
		}
	}
	return nil
}

func joinParts(parts []any) string {
	var b strings.Builder
	for _, p := range parts {
		if pm, ok := p.(map[string]any); ok {
			if s := str(pm["text"]); s != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(s)
			}
		}
	}
	return b.String()
}

// DecodeCloudWatchSpans reads records from the CloudWatch `aws/spans` log
// group. Those records hold OTel spans in semantic-convention format with W3C
// trace ids, so the mapping is the same as OTLP's: only the envelope differs
// (ADR-007 §4).
//
// AWS does not publish a stable JSON schema for these records, so decoding is
// deliberately tolerant of naming variants. Use `axda trace fetch --raw` to
// dump what your account actually emits.
func DecodeCloudWatchSpans(records []json.RawMessage) ([]RawSpan, error) {
	var out []RawSpan
	var skipped int
	for _, rec := range records {
		var m map[string]any
		if err := json.Unmarshal(rec, &m); err != nil {
			skipped++
			continue
		}
		s, ok := cwRecordToSpan(m)
		if !ok {
			skipped++
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no spans decoded from %d records (re-run with --raw to inspect the record shape)", len(records))
	}
	return out, nil
}

func cwRecordToSpan(m map[string]any) (RawSpan, bool) {
	trace := firstStr(m, "traceId", "trace_id", "TraceId")
	span := firstStr(m, "spanId", "span_id", "SpanId", "id")
	if trace == "" || span == "" {
		return RawSpan{}, false
	}

	s := RawSpan{
		TraceID:      normalizeTraceID(trace),
		SpanID:       span,
		ParentSpanID: firstStr(m, "parentSpanId", "parent_span_id", "ParentSpanId", "parentId"),
		Name:         firstStr(m, "name", "Name", "operationName"),
		StartNano:    firstTime(m, "startTimeUnixNano", "start_time_unix_nano", "startTime", "StartTime"),
		EndNano:      firstTime(m, "endTimeUnixNano", "end_time_unix_nano", "endTime", "EndTime"),
		Attrs:        map[string]any{},
	}

	for _, key := range []string{"attributes", "Attributes", "spanAttributes"} {
		mergeAttrs(s.Attrs, m[key])
	}
	// Some emitters flatten gen_ai.* attributes onto the record root.
	for k, v := range m {
		if strings.HasPrefix(k, "gen_ai.") || strings.HasPrefix(k, "aws.") || k == "session.id" {
			if _, exists := s.Attrs[k]; !exists {
				s.Attrs[k] = v
			}
		}
	}
	if r, ok := m["resource"].(map[string]any); ok {
		s.Resource = map[string]any{}
		mergeAttrs(s.Resource, r["attributes"])
		if len(s.Resource) == 0 {
			mergeAttrs(s.Resource, r)
		}
	}

	switch st := m["status"].(type) {
	case map[string]any:
		s.StatusCode = normalizeStatus(str(st["code"]))
		s.StatusMsg = str(st["message"])
	case string:
		s.StatusCode = normalizeStatus(st)
	}
	if s.StatusCode == "" {
		s.StatusCode = normalizeStatus(firstStr(m, "statusCode", "status_code"))
	}

	if s.EndNano == 0 {
		if d := firstTime(m, "durationNano", "duration_nano"); d > 0 {
			s.EndNano = s.StartNano + d
		} else if dm, ok := num(m["durationMillis"]); ok {
			s.EndNano = s.StartNano + dm*1e6
		}
	}
	return s, true
}

// mergeAttrs accepts either a flat object or an OTLP-style
// [{key, value:{stringValue}}] array.
func mergeAttrs(dst map[string]any, v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			dst[k] = val
		}
	case []any:
		for _, item := range t {
			im, ok := item.(map[string]any)
			if !ok {
				continue
			}
			k := str(im["key"])
			if k == "" {
				continue
			}
			switch val := im["value"].(type) {
			case map[string]any:
				for _, vk := range []string{"stringValue", "intValue", "doubleValue", "boolValue"} {
					if raw, ok := val[vk]; ok && raw != nil {
						dst[k] = raw
						break
					}
				}
				if _, set := dst[k]; !set {
					dst[k] = str(val)
				}
			default:
				dst[k] = im["value"]
			}
		}
	}
}

// normalizeTraceID strips the X-Ray "1-<epoch>-<rand>" presentation back to a
// bare 32-hex W3C id when it appears in that form.
func normalizeTraceID(id string) string {
	if strings.HasPrefix(id, "1-") {
		parts := strings.SplitN(id, "-", 3)
		if len(parts) == 3 {
			return parts[1] + parts[2]
		}
	}
	return id
}

func normalizeStatus(v string) string {
	switch strings.ToUpper(v) {
	case "ERROR", "STATUS_CODE_ERROR", "2":
		return "ERROR"
	case "OK", "STATUS_CODE_OK", "1":
		return "OK"
	case "":
		return ""
	}
	return "UNSET"
}

func firstStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s := str(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// firstTime accepts unix nanos (number or string), RFC3339, and millisecond
// epochs, returning unix nanoseconds.
func firstTime(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case float64:
			return scaleToNano(int64(t))
		case string:
			if n, err := strconv.ParseInt(t, 10, 64); err == nil {
				return scaleToNano(n)
			}
			for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
				if ts, err := time.Parse(layout, t); err == nil {
					return ts.UnixNano()
				}
			}
		}
	}
	return 0
}

// scaleToNano promotes second / millisecond / microsecond epochs to nanos by
// magnitude. Values already in nanos pass through.
func scaleToNano(v int64) int64 {
	switch {
	case v == 0:
		return 0
	case v < 1e11: // seconds
		return v * 1e9
	case v < 1e14: // milliseconds
		return v * 1e6
	case v < 1e17: // microseconds
		return v * 1e3
	default:
		return v
	}
}
