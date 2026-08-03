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
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/extract"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/fetch"
	"github.com/AdrienFromToulouse/agentixdisciplina/internal/verdict"
	"github.com/spf13/cobra"
)

// traceFlags are shared by every command that can read a trace.
type traceFlags struct {
	trace    string
	from     string
	session  string
	traceID  string
	region   string
	logGroup string
	since    time.Duration
	wait     time.Duration
}

func (t *traceFlags) register(c *cobra.Command) {
	f := c.Flags()
	f.StringVarP(&t.trace, "trace", "t", "",
		`trace file ("-" for stdin); OTLP JSON or an axda trace envelope`)
	f.StringVar(&t.from, "from", "", "fetch the trace instead of reading a file (cloudwatch)")
	f.StringVar(&t.session, "session", "", "AgentCore runtime session id (with --from)")
	f.StringVar(&t.traceID, "trace-id", "", "trace id (with --from)")
	f.StringVar(&t.region, "region", "", "AWS region (defaults to the ambient config)")
	f.StringVar(&t.logGroup, "log-group", fetch.DefaultLogGroup, "span log group")
	f.DurationVar(&t.since, "since", 2*time.Hour, "lookback window")
	f.DurationVar(&t.wait, "wait", 30*time.Second, "how long to wait for the span set to settle")
}

type judgeFlags struct {
	on        bool
	off       bool
	model     string
	effort    string
	noCache   bool
	extractor string
}

func (j *judgeFlags) register(c *cobra.Command) {
	f := c.Flags()
	f.BoolVar(&j.on, "judge", false, "run judges even if ANTHROPIC_API_KEY is not set")
	f.BoolVar(&j.off, "no-judge", false, "skip every judge clause")
	f.StringVar(&j.model, "judge-model", "", "judge model (default "+judge.DefaultModel+")")
	f.StringVar(&j.effort, "judge-effort", "", "low (default) | medium | high | xhigh | max")
	f.BoolVar(&j.noCache, "no-judge-cache", false, "do not read or write "+judge.DefaultCachePath)
	f.StringVar(&j.extractor, "extractor", extract.ExtractorStructural,
		"claim extractor: structural (deterministic) | llm (verbatim-gated)")
}

func newEvaluateCmd() *cobra.Command {
	var (
		contractPath  string
		evidence      string
		failOnSkipped bool
		jsonOut       bool
		noColor       bool
		tf            traceFlags
		jf            judgeFlags
	)

	c := &cobra.Command{
		Use:     "evaluate",
		Aliases: []string{"eval"},
		Short:   "Evaluate a trace against a contract",
		Long: `Evaluate a recorded agent trace against an AgentContract.

Exit codes: 0 pass, 1 blocking violation, 2 contract or trace error.

LLM judges are advisory: they never fail the build unless a clause sets
blocking: true, and they SKIP when no credentials are found.`,
		Example: `  axda evaluate -c agent.yaml -t trace.json
  axda evaluate -c agent.yaml --from cloudwatch --session "$SESSION_ID"
  axda evaluate -c agent.yaml -t trace.json --extractor llm --fail-on-skipped`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mode := verdict.EvidenceMode(evidence)
			switch mode {
			case verdict.EvidenceFull, verdict.EvidenceMasked, verdict.EvidenceNone:
			default:
				return failf("--evidence must be full, masked, or none")
			}
			switch jf.extractor {
			case extract.ExtractorStructural, extract.ExtractorLLM:
			default:
				return failf("--extractor must be %s or %s",
					extract.ExtractorStructural, extract.ExtractorLLM)
			}

			plan, err := contract.Load(contractPath)
			if err != nil {
				return fail(err)
			}

			ctx := cmd.Context()
			ep, err := loadEpisode(ctx, &tf)
			if err != nil {
				return fail(err)
			}

			opts := evaluate.Options{Evidence: mode, FailOnSkipped: failOnSkipped}

			needJudge := plan.NeedsJudge() && !jf.off
			needExtractor := jf.extractor == extract.ExtractorLLM
			if needJudge || needExtractor {
				if jf.effort != "" && !judge.ValidEffort(jf.effort) {
					return failf("--judge-effort must be low, medium, high, xhigh, or max")
				}
				j := judge.New(judge.Config{
					Model:   jf.model,
					Effort:  jf.effort,
					NoCache: jf.noCache,
					Enabled: jf.on,
				})
				if needJudge {
					opts.Judge = j
				}
				if needExtractor {
					opts.Extractor = judge.NewExtractor(j)
				}
				// Persisting the cache is best-effort: the report is
				// already correct without it.
				defer func() {
					if err := j.Flush(); err != nil {
						fmt.Fprintf(os.Stderr, "warning: could not write judge cache: %v\n", err)
					}
				}()
			}

			rep := evaluate.RunContext(ctx, plan, ep, opts)

			out := cmd.OutOrStdout()
			if jsonOut {
				err = rep.JSON(out)
			} else {
				color := !noColor && os.Getenv("NO_COLOR") == "" && isTTY(os.Stdout)
				err = rep.Human(out, color)
			}
			if err != nil {
				return fail(err)
			}
			if evaluate.ExitCode(rep) != exitPass {
				return &exitError{code: exitGate, printed: true}
			}
			return nil
		},
	}

	f := c.Flags()
	f.StringVarP(&contractPath, "contract", "c", "", "AgentContract to evaluate against")
	f.StringVar(&evidence, "evidence", string(verdict.EvidenceMasked), "masked (default) | full | none")
	f.BoolVar(&failOnSkipped, "fail-on-skipped", false, "treat unevaluable clauses as failures")
	f.BoolVar(&jsonOut, "json", false, "emit the machine-readable report")
	f.BoolVar(&noColor, "no-color", false, "disable ANSI colour")
	tf.register(c)
	jf.register(c)
	_ = c.MarkFlagRequired("contract")
	return c
}

func newExplainCmd() *cobra.Command {
	var contractPath string
	c := &cobra.Command{
		Use:   "explain",
		Short: "Print how a contract lowers onto evaluators",
		Long: `Print the evaluation plan: every clause, the engine it lowers onto, its
verdict class, the coverage it requires, and whether it is enforceable inline.

The "assertions at the right altitude" pitch dies if the descent from contract
to check is a black box, so the lowering is always inspectable.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			plan, err := contract.Load(contractPath)
			if err != nil {
				return fail(err)
			}
			fmt.Fprint(cmd.OutOrStdout(), plan.Explain())
			return nil
		},
	}
	c.Flags().StringVarP(&contractPath, "contract", "c", "", "AgentContract to explain")
	_ = c.MarkFlagRequired("contract")
	return c
}

func newTraceCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "trace",
		Short: "Fetch traces from a runtime",
		Args:  cobra.NoArgs,
	}
	c.AddCommand(newTraceFetchCmd())
	return c
}

func newTraceFetchCmd() *cobra.Command {
	var (
		out string
		raw bool
		tf  traceFlags
	)
	c := &cobra.Command{
		Use:   "fetch",
		Short: "Fetch a trace from CloudWatch",
		Long: `Fetch a trace from the CloudWatch aws/spans log group by session or trace id,
polling until the span set stops growing.

AWS does not publish a stable schema for those records, so run --raw first to
see what your account actually emits.`,
		Example: `  axda trace fetch --from cloudwatch --session "$SESSION_ID" --out trace.json
  axda trace fetch --from cloudwatch --session "$SESSION_ID" --raw | head -50`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if tf.from != "cloudwatch" {
				return failf("--from cloudwatch is required (only source in v0)")
			}
			res, err := fetchRecords(cmd.Context(), &tf)
			if err != nil {
				return fail(err)
			}

			var payload []byte
			if raw {
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
				return fail(err)
			}

			if out == "" || out == "-" {
				cmd.OutOrStdout().Write(append(payload, '\n'))
			} else if err := os.WriteFile(out, append(payload, '\n'), 0o600); err != nil {
				return fail(err)
			} else {
				fmt.Fprintf(cmd.ErrOrStderr(), "wrote %d record(s) for trace %s to %s\n",
					len(res.Records), res.TraceID, out)
			}
			if !res.Stable {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"warning: span set was still growing when --wait expired; this trace may be partial")
			}
			return nil
		},
	}
	c.Flags().StringVarP(&out, "out", "o", "", `output file ("-" for stdout)`)
	c.Flags().BoolVar(&raw, "raw", false, "dump raw log records instead of a trace envelope")
	tf.register(c)
	return c
}

type envelope struct {
	AxdaTrace string            `json:"axda_trace"`
	Source    string            `json:"source"`
	TraceID   string            `json:"trace_id"`
	Stable    bool              `json:"stable"`
	Records   []json.RawMessage `json:"records"`
}

func fetchRecords(ctx context.Context, tf *traceFlags) (*fetch.Result, error) {
	c, err := fetch.New(ctx, fetch.Options{
		Region: tf.region, LogGroup: tf.logGroup,
		Session: tf.session, TraceID: tf.traceID,
		Since: tf.since, Wait: tf.wait,
		Verbose: os.Stderr,
	})
	if err != nil {
		return nil, err
	}
	return c.Fetch(ctx)
}

func loadEpisode(ctx context.Context, tf *traceFlags) (*episode.Episode, error) {
	if tf.from == "cloudwatch" {
		res, err := fetchRecords(ctx, tf)
		if err != nil {
			return nil, err
		}
		spans, err := adapter.DecodeCloudWatchSpans(res.Records)
		if err != nil {
			return nil, err
		}
		return adapter.BuildEpisode(spans, adapter.AdapterCloudWatch)
	}
	if tf.from != "" {
		return nil, fmt.Errorf("--from %q is not supported (use cloudwatch)", tf.from)
	}
	if tf.trace == "" {
		return nil, fmt.Errorf("--trace or --from cloudwatch is required")
	}

	var raw []byte
	var err error
	if tf.trace == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(tf.trace)
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
