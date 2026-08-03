# Tool clauses: what the agent *did* (ADR-003 §4).
package axda.tool

import data.axda.match

# ---------------------------------------------------------------- allowlist

allowlist_violation contains v if {
	some tc in input.episode.tool_calls
	not match.any_matches(input.params.tools, tc.name)
	v := {
		"message": sprintf("%s %q is not in the allowed set", [noun(tc), tc.name]),
		"trace_id": tc.span.trace_id,
		"span_id": tc.span.span_id,
		"path": tc.span.path,
	}
}

# Delegation to a sub-agent is an action, so it is governed by the same list
# (ADR-002 §6).
noun(tc) := "sub-agent delegation" if tc.kind == "agent"

noun(tc) := "tool" if tc.kind != "agent"

# ----------------------------------------------------------------- denylist

denylist_violation contains v if {
	some tc in input.episode.tool_calls
	match.any_matches(input.params.tools, tc.name)
	v := {
		"message": sprintf("tool %q is denied", [tc.name]),
		"trace_id": tc.span.trace_id,
		"span_id": tc.span.span_id,
		"path": tc.span.path,
	}
}

# --------------------------------------------------------------- call limit

limit_pattern := object.get(input.params, "tool", "*")

limit_label := "tool calls" if limit_pattern == "*"

limit_label := sprintf("calls to %q", [limit_pattern]) if limit_pattern != "*"

limit_matching contains tc if {
	some tc in input.episode.tool_calls
	match.matches(limit_pattern, tc.name)
}

call_limit_violation contains v if {
	n := count(limit_matching)
	n > input.params.max
	v := {
		"message": sprintf("%d %s exceeds limit of %d", [n, limit_label, input.params.max]),
		"trace_id": input.episode.meta.trace_id,
		"path": "tool_calls",
	}
}
