// Package verdict defines evaluation results.
//
// Two invariants from the ADRs are encoded here: a check that could not run
// (Skipped) or that broke (Errored) is never a pass (ADR-003 §5, ADR-004 §7),
// and only deterministic verdicts block a build (ADR-001 §6).
package verdict

type Status string

const (
	Pass    Status = "pass"
	Fail    Status = "fail"
	Skipped Status = "skipped"
	Errored Status = "errored"
)

type Class string

const (
	Deterministic Class = "deterministic"
	Probabilistic Class = "probabilistic"
)

// EvidenceMode controls how much of a match reaches the report. Masked is the
// default: the highest-value finding this tool produces is "your agent leaked
// a card number", and writing that number into an archived CI log would
// reproduce the exact harm being reported (ADR-002 §7).
type EvidenceMode string

const (
	EvidenceFull   EvidenceMode = "full"
	EvidenceMasked EvidenceMode = "masked"
	EvidenceNone   EvidenceMode = "none"
)

// Evidence anchors a violation to a span. A finding without it is not
// shippable output (ADR-001 §6).
type Evidence struct {
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`
	Path    string `json:"path,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
}

// Finding is one violation produced by a clause evaluator.
type Finding struct {
	Message  string   `json:"message"`
	Evidence Evidence `json:"evidence"`
}

type Verdict struct {
	Clause          string    `json:"clause"`
	Kind            string    `json:"kind"`
	Engine          string    `json:"engine"`
	Status          Status    `json:"status"`
	Class           Class     `json:"class"`
	Severity        string    `json:"severity"`
	Blocking        bool      `json:"blocking"`
	Message         string    `json:"message,omitempty"`
	Findings        []Finding `json:"findings,omitempty"`
	MissingCoverage []string  `json:"missing_coverage,omitempty"`
}

// Blocks reports whether this verdict should fail the build. Only
// deterministic, blocking clauses in a failed or errored state do.
func (v Verdict) Blocks() bool {
	if !v.Blocking || v.Class != Deterministic {
		return false
	}
	return v.Status == Fail || v.Status == Errored
}
