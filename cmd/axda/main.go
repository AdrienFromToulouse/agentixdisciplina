// Command axda evaluates a recorded agent trace against a contract.
//
// Exit codes (ADR-001 §6): 0 pass, 1 blocking violation, 2 bundle/trace error.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/AdrienFromToulouse/agentixdisciplina/internal/adapter"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/contract"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/engine/judge"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/episode"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/evaluate"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/fetch"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/verdict"
)

var version = "0.1.0-dev"

const usage = `axda: out-of-band evaluation for AI agents

usage:
  axda evaluate   --contract FILE (--trace FILE | --from cloudwatch --session ID)
  axda explain    --contract FILE
  axda trace      fetch --from cloudwatch (--session ID | --trace-id ID) [--out FILE] [--raw]
  axda version

evaluate flags:
  --contract FILE        AgentContract to evaluate against
  --trace FILE           trace file ("-" for stdin); OTLP JSON or an axda trace envelope
  --from cloudwatch      fetch the trace from CloudWatch instead of reading a file
  --session ID           AgentCore runtime session id (with --from)
  --trace-id ID          trace id (with --from)
  --evidence MODE        masked (default) | full | none
  --fail-on-skipped      treat unevaluable clauses as failures
  --json                 emit the machine-readable report
  --no-color             disable ANSI colour

judge flags (LLM judges are advisory: they never fail the build unless a
clause sets blocking: true, and they SKIP when no credentials are found):
  --judge                run judges even if ANTHROPIC_API_KEY is not set
  --no-judge             skip every judge clause
  --judge-model M        default claude-opus-5
  --judge-effort E       low (default) | medium | high | xhigh | max
  --no-judge-cache       do not read or write .axda/judge-cache.json

fetch flags:
  --region R             AWS region (defaults to the ambient config)
  --log-group G          span log group (default aws/spans)
  --since D              lookback window (default 2h)
  --wait D               how long to wait for the span set to settle (default 30s)
  --raw                  dump raw log records instead of a trace envelope
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "evaluate", "eval":
		err = cmdEvaluate(os.Args[2:])
	case "explain":
		err = cmdExplain(os.Args[2:])
	case "trace":
		err = cmdTrace(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("axda %s (episode/%s, report %s)\n", version, "v1", "axda.dev/report/v1")
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
}

type flags struct {
	contract      string
	trace         string
	from          string
	session       string
	traceID       string
	region        string
	logGroup      string
	since         time.Duration
	wait          time.Duration
	out           string
	evidence      string
	raw           bool
	failOnSkipped bool
	jsonOut       bool
	noColor       bool
	judgeOn       bool
	judgeOff      bool
	judgeModel    string
	judgeEffort   string
	noJudgeCache  bool
}

// parse is a small hand-rolled flag reader so `axda trace fetch` reads as a
// two-word subcommand without a flag-package workaround.
func parse(args []string) (*flags, error) {
	f := &flags{
		evidence: string(verdict.EvidenceMasked),
		since:    2 * time.Hour,
		wait:     30 * time.Second,
		logGroup: fetch.DefaultLogGroup,
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", a)
			}
			i++
			return args[i], nil
		}
		var err error
		var v string
		switch a {
		case "--contract", "-c":
			v, err = next()
			f.contract = v
		case "--trace", "-t":
			v, err = next()
			f.trace = v
		case "--from":
			v, err = next()
			f.from = v
		case "--session":
			v, err = next()
			f.session = v
		case "--trace-id":
			v, err = next()
			f.traceID = v
		case "--region":
			v, err = next()
			f.region = v
		case "--log-group":
			v, err = next()
			f.logGroup = v
		case "--out", "-o":
			v, err = next()
			f.out = v
		case "--evidence":
			v, err = next()
			f.evidence = v
		case "--since":
			if v, err = next(); err == nil {
				f.since, err = time.ParseDuration(v)
			}
		case "--wait":
			if v, err = next(); err == nil {
				f.wait, err = time.ParseDuration(v)
			}
		case "--judge-model":
			v, err = next()
			f.judgeModel = v
		case "--judge-effort":
			v, err = next()
			f.judgeEffort = v
		case "--judge":
			f.judgeOn = true
		case "--no-judge":
			f.judgeOff = true
		case "--no-judge-cache":
			f.noJudgeCache = true
		case "--raw":
			f.raw = true
		case "--fail-on-skipped":
			f.failOnSkipped = true
		case "--json":
			f.jsonOut = true
		case "--no-color":
			f.noColor = true
		default:
			return nil, fmt.Errorf("unknown flag %q", a)
		}
		if err != nil {
			return nil, err
		}
	}
	switch f.evidence {
	case "full", "masked", "none":
	default:
		return nil, fmt.Errorf("--evidence must be full, masked, or none")
	}
	return f, nil
}

func cmdExplain(args []string) error {
	f, err := parse(args)
	if err != nil {
		return err
	}
	if f.contract == "" {
		return fmt.Errorf("--contract is required")
	}
	plan, err := contract.Load(f.contract)
	if err != nil {
		return err
	}
	fmt.Print(plan.Explain())
	return nil
}

func cmdEvaluate(args []string) error {
	f, err := parse(args)
	if err != nil {
		return err
	}
	if f.contract == "" {
		return fmt.Errorf("--contract is required")
	}
	plan, err := contract.Load(f.contract)
	if err != nil {
		return err
	}

	ep, err := loadEpisode(context.Background(), f)
	if err != nil {
		return err
	}

	opts := evaluate.Options{
		Evidence:      verdict.EvidenceMode(f.evidence),
		FailOnSkipped: f.failOnSkipped,
	}
	if plan.NeedsJudge() && !f.judgeOff {
		if f.judgeEffort != "" && !judge.ValidEffort(f.judgeEffort) {
			return fmt.Errorf("--judge-effort must be low, medium, high, xhigh, or max")
		}
		j := judge.New(judge.Config{
			Model:   f.judgeModel,
			Effort:  f.judgeEffort,
			NoCache: f.noJudgeCache,
			Enabled: f.judgeOn,
		})
		opts.Judge = j
		// Persisting the cache is best-effort: the report is already
		// correct without it.
		defer func() {
			if err := j.Flush(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not write judge cache: %v\n", err)
			}
		}()
	}

	rep := evaluate.Run(plan, ep, opts)

	if f.jsonOut {
		if err := rep.JSON(os.Stdout); err != nil {
			return err
		}
	} else {
		color := !f.noColor && os.Getenv("NO_COLOR") == "" && isTTY(os.Stdout)
		if err := rep.Human(os.Stdout, color); err != nil {
			return err
		}
	}
	os.Exit(evaluate.ExitCode(rep))
	return nil
}

func cmdTrace(args []string) error {
	if len(args) == 0 || args[0] != "fetch" {
		return fmt.Errorf("usage: axda trace fetch --from cloudwatch (--session ID | --trace-id ID)")
	}
	f, err := parse(args[1:])
	if err != nil {
		return err
	}
	if f.from != "cloudwatch" {
		return fmt.Errorf("--from cloudwatch is required (only source in v0)")
	}

	res, err := fetchRecords(context.Background(), f)
	if err != nil {
		return err
	}

	var payload []byte
	if f.raw {
		payload, err = json.MarshalIndent(res.Records, "", "  ")
	} else {
		payload, err = json.MarshalIndent(envelope{
			AxdaTrace: "v1",
			Source:    adapter.AdapterCloudWatch,
			TraceID:   res.TraceID,
			Stable:    res.Stable,
			Records:   res.Records,
		}, "", "  ")
	}
	if err != nil {
		return err
	}

	if f.out == "" || f.out == "-" {
		os.Stdout.Write(append(payload, '\n'))
	} else if err := os.WriteFile(f.out, append(payload, '\n'), 0o600); err != nil {
		return err
	} else {
		fmt.Fprintf(os.Stderr, "wrote %d record(s) for trace %s to %s\n", len(res.Records), res.TraceID, f.out)
	}
	if !res.Stable {
		fmt.Fprintln(os.Stderr, "warning: span set was still growing when --wait expired; this trace may be partial")
	}
	return nil
}

type envelope struct {
	AxdaTrace string            `json:"axda_trace"`
	Source    string            `json:"source"`
	TraceID   string            `json:"trace_id"`
	Stable    bool              `json:"stable"`
	Records   []json.RawMessage `json:"records"`
}

func fetchRecords(ctx context.Context, f *flags) (*fetch.Result, error) {
	c, err := fetch.New(ctx, fetch.Options{
		Region: f.region, LogGroup: f.logGroup,
		Session: f.session, TraceID: f.traceID,
		Since: f.since, Wait: f.wait,
		Verbose: os.Stderr,
	})
	if err != nil {
		return nil, err
	}
	return c.Fetch(ctx)
}

func loadEpisode(ctx context.Context, f *flags) (*episode.Episode, error) {
	if f.from == "cloudwatch" {
		res, err := fetchRecords(ctx, f)
		if err != nil {
			return nil, err
		}
		spans, err := adapter.DecodeCloudWatchSpans(res.Records)
		if err != nil {
			return nil, err
		}
		return adapter.BuildEpisode(spans, adapter.AdapterCloudWatch)
	}
	if f.from != "" {
		return nil, fmt.Errorf("--from %q is not supported (use cloudwatch)", f.from)
	}
	if f.trace == "" {
		return nil, fmt.Errorf("--trace or --from cloudwatch is required")
	}

	var raw []byte
	var err error
	if f.trace == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(f.trace)
	}
	if err != nil {
		return nil, err
	}

	// Sniff the payload: an axda envelope holds CloudWatch span records,
	// anything else is treated as OTLP/JSON.
	var env envelope
	if json.Unmarshal(raw, &env) == nil && env.AxdaTrace != "" {
		spans, err := adapter.DecodeCloudWatchSpans(env.Records)
		if err != nil {
			return nil, err
		}
		return adapter.BuildEpisode(spans, adapter.AdapterCloudWatch)
	}
	spans, err := adapter.DecodeOTLP(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return adapter.BuildEpisode(spans, adapter.AdapterOTLP)
}

func isTTY(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}
