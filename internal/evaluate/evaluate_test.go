package evaluate_test

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/AdrienFromToulouse/agentixdisciplina/internal/adapter"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/contract"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/episode"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/evaluate"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/report"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/verdict"
)

const bundle = "../../examples/support-agent"

func plan(t *testing.T) *contract.Plan {
	t.Helper()
	p, err := contract.Load(filepath.Join(bundle, "contract.yaml"))
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}
	return p
}

func load(t *testing.T, name string) *episode.Episode {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(bundle, "testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	spans, err := adapter.DecodeOTLP(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	ep, err := adapter.BuildEpisode(spans, adapter.AdapterOTLP)
	if err != nil {
		t.Fatalf("build %s: %v", name, err)
	}
	return ep
}

func run(t *testing.T, name string, opts evaluate.Options) *report.Report {
	t.Helper()
	return evaluate.Run(plan(t), load(t, name), opts)
}

func TestCleanTracePasses(t *testing.T) {
	r := run(t, "clean.json", evaluate.Options{})
	if r.Gate != "pass" {
		t.Fatalf("gate = %q, want pass", r.Gate)
	}
	if evaluate.ExitCode(r) != 0 {
		t.Fatalf("exit = %d, want 0", evaluate.ExitCode(r))
	}
	if r.Counts.Fail != 0 || r.Counts.Errored != 0 {
		t.Fatalf("unexpected failures: %+v", r.Counts)
	}
}

func TestViolatingTraceFailsWithSpanEvidence(t *testing.T) {
	r := run(t, "violating.json", evaluate.Options{})
	if evaluate.ExitCode(r) != 1 {
		t.Fatalf("exit = %d, want 1", evaluate.ExitCode(r))
	}

	want := map[string]bool{
		"tool.allowlist":              false,
		"order.requires_precondition": false,
		"must_not.content.no_pii":     false,
	}
	for _, v := range r.Verdicts {
		if v.Status != verdict.Fail {
			continue
		}
		if _, ok := want[v.Clause]; ok {
			want[v.Clause] = true
		}
		// No finding without a span (ADR-001 §6).
		for _, f := range v.Findings {
			if f.Evidence.SpanID == "" && f.Evidence.Path == "" {
				t.Errorf("%s: finding with no evidence locator: %q", v.Clause, f.Message)
			}
		}
	}
	for clause, found := range want {
		if !found {
			t.Errorf("expected %s to fail, it did not", clause)
		}
	}
}

// The invariant that matters most: a clause whose data is missing must report
// SKIPPED, never PASS (ADR-003 §5). Asserted against a clause that genuinely
// fails when the same trace does carry content.
func TestMissingCoverageSkipsRatherThanPasses(t *testing.T) {
	withContent := run(t, "violating.json", evaluate.Options{})
	withoutContent := run(t, "no-content.json", evaluate.Options{})

	status := func(r *report.Report, clause string) verdict.Status {
		for _, v := range r.Verdicts {
			if v.Clause == clause {
				return v.Status
			}
		}
		t.Fatalf("clause %q not in report", clause)
		return ""
	}

	if got := status(withContent, "must_not.content.no_pii"); got != verdict.Fail {
		t.Fatalf("with content: no_pii = %q, want fail", got)
	}
	if got := status(withoutContent, "must_not.content.no_pii"); got != verdict.Skipped {
		t.Fatalf("without content: no_pii = %q, want skipped, a skip must never become a pass", got)
	}
}

func TestFailOnSkippedGatesOnCoverage(t *testing.T) {
	ep := load(t, "clean.json")
	// Strip message content the way a default-configured runtime would.
	ep.Coverage.HasMessageContent = false

	if r := evaluate.Run(plan(t), ep, evaluate.Options{}); r.Gate != "pass" {
		t.Fatalf("default gate = %q, want pass", r.Gate)
	}
	if r := evaluate.Run(plan(t), ep, evaluate.Options{FailOnSkipped: true}); r.Gate != "fail" {
		t.Fatalf("--fail-on-skipped gate = %q, want fail", r.Gate)
	}
}

// Same trace, same contract, byte-identical report (ADR-001 verification).
func TestDeterministicReport(t *testing.T) {
	var first []byte
	for i := 0; i < 10; i++ {
		var buf bytes.Buffer
		if err := run(t, "violating.json", evaluate.Options{}).JSON(&buf); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = append([]byte(nil), buf.Bytes()...)
			continue
		}
		if !bytes.Equal(first, buf.Bytes()) {
			t.Fatalf("report differed on run %d", i+1)
		}
	}
}

// OTLP guarantees no span ordering, so the Episode must impose a total order
// of its own (ADR-002 §5) or index-based evidence is meaningless.
func TestSpanOrderIndependence(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(bundle, "testdata", "violating.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	spans := doc["resourceSpans"].([]any)[0].(map[string]any)["scopeSpans"].([]any)[0].(map[string]any)["spans"].([]any)
	rng := rand.New(rand.NewSource(7))
	rng.Shuffle(len(spans), func(i, j int) { spans[i], spans[j] = spans[j], spans[i] })

	shuffled, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := adapter.DecodeOTLP(bytes.NewReader(shuffled))
	if err != nil {
		t.Fatal(err)
	}
	ep, err := adapter.BuildEpisode(decoded, adapter.AdapterOTLP)
	if err != nil {
		t.Fatal(err)
	}

	var a, b bytes.Buffer
	if err := evaluate.Run(plan(t), ep, evaluate.Options{}).JSON(&a); err != nil {
		t.Fatal(err)
	}
	if err := run(t, "violating.json", evaluate.Options{}).JSON(&b); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("shuffling span order changed the report")
	}
}

func TestEvidenceModes(t *testing.T) {
	const rawCard = "4242 4242 4242 4242"

	masked := mustJSON(t, run(t, "violating.json", evaluate.Options{Evidence: verdict.EvidenceMasked}))
	if bytes.Contains(masked, []byte(rawCard)) {
		t.Error("masked report leaked the raw card number")
	}

	full := mustJSON(t, run(t, "violating.json", evaluate.Options{Evidence: verdict.EvidenceFull}))
	if !bytes.Contains(full, []byte(rawCard)) {
		t.Error("--evidence=full should include the raw value")
	}

	none := mustJSON(t, run(t, "violating.json", evaluate.Options{Evidence: verdict.EvidenceNone}))
	if bytes.Contains(none, []byte("redacted:card")) || bytes.Contains(none, []byte(rawCard)) {
		t.Error("--evidence=none should emit no excerpt at all")
	}
}

func mustJSON(t *testing.T, r *report.Report) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := r.JSON(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
