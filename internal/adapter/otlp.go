package adapter

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

const AdapterOTLP = "otlp/v1.41"

// DecodeOTLP reads an OTLP/JSON trace payload (the shape produced by the
// collector's `otlp_json` marshaler and by `otel-cli`).
func DecodeOTLP(r io.Reader) ([]RawSpan, error) {
	var doc struct {
		ResourceSpans []struct {
			Resource struct {
				Attributes []otlpAttr `json:"attributes"`
			} `json:"resource"`
			ScopeSpans []struct {
				Spans []otlpSpan `json:"spans"`
			} `json:"scopeSpans"`
			// Older collectors emit instrumentationLibrarySpans.
			InstrumentationLibrarySpans []struct {
				Spans []otlpSpan `json:"spans"`
			} `json:"instrumentationLibrarySpans"`
		} `json:"resourceSpans"`
	}
	dec := json.NewDecoder(r)
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse otlp json: %w", err)
	}

	var out []RawSpan
	for _, rs := range doc.ResourceSpans {
		res := flattenOTLPAttrs(rs.Resource.Attributes)
		groups := rs.ScopeSpans
		if len(groups) == 0 {
			groups = rs.InstrumentationLibrarySpans
		}
		for _, ss := range groups {
			for _, s := range ss.Spans {
				out = append(out, RawSpan{
					TraceID:      s.TraceID,
					SpanID:       s.SpanID,
					ParentSpanID: s.ParentSpanID,
					Name:         s.Name,
					StartNano:    parseNano(s.StartTimeUnixNano),
					EndNano:      parseNano(s.EndTimeUnixNano),
					Attrs:        flattenOTLPAttrs(s.Attributes),
					Resource:     res,
					StatusCode:   statusName(s.Status.Code),
					StatusMsg:    s.Status.Message,
				})
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no spans found in otlp payload")
	}
	return out, nil
}

type otlpSpan struct {
	TraceID           string     `json:"traceId"`
	SpanID            string     `json:"spanId"`
	ParentSpanID      string     `json:"parentSpanId"`
	Name              string     `json:"name"`
	StartTimeUnixNano any        `json:"startTimeUnixNano"`
	EndTimeUnixNano   any        `json:"endTimeUnixNano"`
	Attributes        []otlpAttr `json:"attributes"`
	Status            struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	} `json:"status"`
}

type otlpAttr struct {
	Key   string `json:"key"`
	Value struct {
		StringValue *string  `json:"stringValue"`
		IntValue    any      `json:"intValue"`
		DoubleValue *float64 `json:"doubleValue"`
		BoolValue   *bool    `json:"boolValue"`
		ArrayValue  *struct {
			Values []json.RawMessage `json:"values"`
		} `json:"arrayValue"`
	} `json:"value"`
}

func flattenOTLPAttrs(attrs []otlpAttr) map[string]any {
	m := make(map[string]any, len(attrs))
	for _, a := range attrs {
		switch {
		case a.Value.StringValue != nil:
			m[a.Key] = *a.Value.StringValue
		case a.Value.BoolValue != nil:
			m[a.Key] = *a.Value.BoolValue
		case a.Value.DoubleValue != nil:
			m[a.Key] = *a.Value.DoubleValue
		case a.Value.IntValue != nil:
			if n, ok := num(a.Value.IntValue); ok {
				m[a.Key] = float64(n)
			}
		case a.Value.ArrayValue != nil:
			b, _ := json.Marshal(a.Value.ArrayValue.Values)
			m[a.Key] = string(b)
		}
	}
	return m
}

func parseNano(v any) int64 {
	switch t := v.(type) {
	case string:
		n, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			return 0
		}
		return n
	case float64:
		return int64(t)
	}
	return 0
}

func statusName(v any) string {
	switch t := v.(type) {
	case string:
		switch t {
		case "STATUS_CODE_ERROR", "ERROR":
			return "ERROR"
		case "STATUS_CODE_OK", "OK":
			return "OK"
		}
		return "UNSET"
	case float64:
		switch int(t) {
		case 2:
			return "ERROR"
		case 1:
			return "OK"
		}
	}
	return "UNSET"
}
