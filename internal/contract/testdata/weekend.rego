# A contract-declared custom clause, namespaced under the bundle's own prefix.
package acme.weekend

violation contains v if {
	some tc in input.episode.tool_calls
	tc.name == "billing.refund"
	tc.duration_ms > input.params.max_duration_ms
	v := {
		"message": sprintf("refund %q took %dms", [tc.name, tc.duration_ms]),
		"trace_id": tc.span.trace_id,
		"span_id": tc.span.span_id,
		"path": tc.span.path,
	}
}
