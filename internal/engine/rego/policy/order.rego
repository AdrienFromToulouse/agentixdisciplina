# Ordering clauses over the total order established in ADR-002 §5.
package axda.order

import data.axda.match

# "Verify identity before refunding" cannot be *satisfied* on a prefix — the
# agent might verify later — but it can be *violated*, exactly when the action
# fires with no completed precondition behind it. That asymmetry is what makes
# this enforceable inline (ADR-005 §2).
requires_precondition_violation contains v if {
	some tc in input.episode.tool_calls
	match.matches(input.params.action, tc.name)
	not has_precondition(tc)
	v := {
		"message": sprintf("%q ran with no completed %q before it", [tc.name, input.params.precondition]),
		"trace_id": tc.span.trace_id,
		"span_id": tc.span.span_id,
		"path": tc.span.path,
	}
}

# Overlapping spans count as unordered, so no policy depends on the arbitrary
# span-id tie-break (ADR-003 §2).
has_precondition(action) if {
	some c in input.episode.tool_calls
	match.matches(input.params.precondition, c.name)
	not c.error
	c.ended_at_unix_nano > 0
	c.ended_at_unix_nano <= action.started_at_unix_nano
}

# ------------------------------------------------------------------- before

before_violation contains v if {
	some later in input.episode.tool_calls
	match.matches(input.params.first, later.name)

	some earlier in input.episode.tool_calls
	match.matches(input.params.then, earlier.name)

	earlier.ended_at_unix_nano > 0
	earlier.ended_at_unix_nano <= later.started_at_unix_nano

	v := {
		"message": sprintf("%q completed before %q, violating the required order", [earlier.name, later.name]),
		"trace_id": earlier.span.trace_id,
		"span_id": earlier.span.span_id,
		"path": earlier.span.path,
	}
}
