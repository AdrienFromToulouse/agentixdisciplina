package adapter

import (
	"encoding/json"
	"fmt"
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
