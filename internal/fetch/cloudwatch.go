// Package fetch retrieves traces from CloudWatch (ADR-007 Path A).
//
// Spans in the `aws/spans` log group are OTel spans in semantic-convention
// format with W3C trace ids, ingested at 100%. That is enough fidelity to gate
// on, which is why this path is first-class rather than a fallback.
package fetch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"

	"github.com/AdrienFromToulouse/agentixdisciplina/internal/adapter"
)

const DefaultLogGroup = "aws/spans"

type Options struct {
	Region   string
	LogGroup string
	// LogStream scopes the query to one stream. AgentCore's per-agent span
	// destination puts spans in the `spans` stream of a log group it shares
	// with the agent's stdout and its OTEL structured logs, so scoping cuts
	// what CloudWatch bills for scanning (ADR-007 §3).
	LogStream string
	Session   string
	TraceID   string
	// NoContent skips the second query that recovers message bodies. The
	// bodies are the input to the content clauses, so skipping them means
	// those clauses SKIP rather than run.
	NoContent bool
	Since     time.Duration
	Wait      time.Duration
	// Settle is how long the span set must stop growing before the trace is
	// considered complete. Returning early would risk a partial episode that
	// evaluates as a pass.
	Settle  time.Duration
	Verbose io.Writer
}

type Result struct {
	TraceID string
	Records []json.RawMessage
	// ContentRecords hold the GenAI message bodies, which AgentCore delivers
	// as OTel log records in the agent's own log group rather than as span
	// attributes (ADR-007 §4). Empty is a normal outcome, not an error.
	ContentRecords []json.RawMessage
	// ContentSource names the group and stream the bodies came from, for the
	// verbose log and for the degraded-coverage note when they came from
	// nowhere.
	ContentSource string
	Stable        bool
}

type Client struct {
	api *cloudwatchlogs.Client
	opt Options
}

func New(ctx context.Context, opt Options) (*Client, error) {
	if opt.LogGroup == "" {
		opt.LogGroup = DefaultLogGroup
	}
	if opt.Since == 0 {
		// Hours were too short to reach a trace worth reviewing days later. A
		// --trace-id narrows this to the trace's own window anyway (windows).
		opt.Since = 24 * time.Hour
	}
	if opt.Settle == 0 {
		opt.Settle = 3 * time.Second
	}
	var loadOpts []func(*config.LoadOptions) error
	if opt.Region != "" {
		loadOpts = append(loadOpts, config.WithRegion(opt.Region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("no AWS region configured (set --region or AWS_REGION)")
	}
	return &Client{api: cloudwatchlogs.NewFromConfig(cfg), opt: opt}, nil
}

// Fetch resolves a trace by session id or trace id and polls until the span
// set is stable.
func (c *Client) Fetch(ctx context.Context) (*Result, error) {
	traceID := c.opt.TraceID

	if traceID == "" {
		if c.opt.Session == "" {
			return nil, fmt.Errorf("one of --session or --trace-id is required")
		}
		// Not every span in a trace necessarily carries the session
		// attribute, so resolve session → trace id first, then pull the
		// whole trace by id.
		recs, err := c.query(ctx, c.opt.LogGroup, c.opt.LogStream, c.opt.Session)
		if err != nil {
			return nil, err
		}
		if len(recs) == 0 {
			return nil, fmt.Errorf("no spans found for session %q in %s over the last %s\n%s",
				c.opt.Session, c.opt.LogGroup, c.opt.Since, c.emptyHint())
		}
		traceID = firstTraceID(recs)
		if traceID == "" {
			return &Result{Records: recs}, fmt.Errorf("matched %d record(s) for the session but found no trace id field; re-run with --raw to inspect", len(recs))
		}
		c.logf("resolved session %s → trace %s\n", c.opt.Session, traceID)
	}

	deadline := time.Now().Add(c.opt.Wait)
	var last int
	var stableSince time.Time

	for {
		recs, err := c.query(ctx, c.opt.LogGroup, c.opt.LogStream, traceID)
		if err != nil {
			return nil, err
		}
		switch {
		case len(recs) != last:
			last = len(recs)
			stableSince = time.Now()
			c.logf("  %d span record(s)…\n", last)
		case len(recs) > 0 && time.Since(stableSince) >= c.opt.Settle:
			return c.withContent(ctx, &Result{TraceID: traceID, Records: recs, Stable: true}), nil
		}

		if time.Now().After(deadline) {
			if len(recs) == 0 {
				return nil, fmt.Errorf("no spans found for trace %q in %s\n%s",
					traceID, c.opt.LogGroup, c.emptyHint())
			}
			// Report instability rather than silently returning a possibly
			// partial trace.
			return c.withContent(ctx, &Result{TraceID: traceID, Records: recs, Stable: false}), nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// withContent adds the message bodies to a settled span set. AgentCore
// delivers GenAI content as OTel log records in the agent's own log group, not
// as span attributes, so recovering it takes a second bounded query joined on
// span id (ADR-007 §4).
//
// Every failure here is non-fatal. Content is what the content clauses read, so
// its absence has to surface as those clauses SKIPPING on a real Episode, never
// as a failed fetch and never as a vacuous pass.
func (c *Client) withContent(ctx context.Context, r *Result) *Result {
	if c.opt.NoContent || len(r.Records) == 0 {
		return r
	}
	group, stream := adapter.ContentSource(r.Records)
	if group == "" {
		c.logf("  no content log group named in the spans; message content unavailable\n")
		return r
	}
	if group == c.opt.LogGroup && stream == c.opt.LogStream {
		return r
	}

	recs, err := c.query(ctx, group, stream, r.TraceID)
	if err != nil {
		// A missing or unreadable content group is common enough (a CI role
		// scoped to the span group only) that it must not fail the fetch.
		c.logf("  content lookup in %s failed: %v\n", group, err)
		return r
	}
	r.ContentRecords = recs
	r.ContentSource = group
	if stream != "" {
		r.ContentSource += " (" + stream + ")"
	}
	c.logf("  %d content record(s) from %s\n", len(recs), r.ContentSource)
	return r
}

// window is a bounded scan range. Bounding is mandatory rather than polite:
// CloudWatch Logs bills by data scanned (ADR-007 §3).
type window struct {
	start time.Time
	end   time.Time
}

// windows returns the scan ranges to try, in order. An AWS-generated trace id
// carries the trace's start time in its first 8 hex characters, so a lookup by
// id can bound the scan to the minutes around the trace instead of sweeping
// `--since`. The `--since` range is kept as a fallback: the embedded timestamp
// is a convention, not a guarantee, and a wrong guess must degrade to a wider
// search rather than to "no spans found".
func (c *Client) windows(traceID string) []window {
	now := time.Now()
	since := window{start: now.Add(-c.opt.Since), end: now}
	if t, ok := traceStart(traceID); ok {
		bounded := window{start: t.Add(-30 * time.Minute), end: t.Add(2 * time.Hour)}
		if bounded.end.After(now) {
			bounded.end = now
		}
		return []window{bounded, since}
	}
	return []window{since}
}

// traceStart decodes the epoch seconds AWS embeds in an X-Ray-compatible trace
// id. Implausible values are rejected rather than trusted: a non-AWS id would
// otherwise bound the scan to a window that cannot contain the trace.
func traceStart(traceID string) (time.Time, bool) {
	id := strings.TrimPrefix(traceID, "1-")
	id = strings.ReplaceAll(id, "-", "")
	if len(id) < 8 {
		return time.Time{}, false
	}
	secs, err := strconv.ParseInt(id[:8], 16, 64)
	if err != nil {
		return time.Time{}, false
	}
	t := time.Unix(secs, 0)
	// CloudWatch retention tops out well inside a year, and a trace cannot
	// start in the future.
	if t.Before(time.Now().AddDate(-1, 0, 0)) || t.After(time.Now().Add(time.Hour)) {
		return time.Time{}, false
	}
	return t, true
}

// query does a substring match on the raw log message, trying each candidate
// window until one yields records. Filtering on the raw text rather than a
// parsed field keeps this working regardless of how AWS names fields in the
// span record.
func (c *Client) query(ctx context.Context, logGroup, logStream, needle string) ([]json.RawMessage, error) {
	for _, w := range c.windows(needle) {
		out, err := c.queryWindow(ctx, logGroup, logStream, needle, w)
		if err != nil || len(out) > 0 {
			return out, err
		}
	}
	return nil, nil
}

func (c *Client) queryWindow(ctx context.Context, logGroup, logStream, needle string, w window) ([]json.RawMessage, error) {
	in := &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName:  aws.String(logGroup),
		StartTime:     aws.Int64(w.start.UnixMilli()),
		EndTime:       aws.Int64(w.end.UnixMilli()),
		FilterPattern: aws.String(fmt.Sprintf("%q", needle)),
		Limit:         aws.Int32(10000),
	}
	if logStream != "" {
		in.LogStreamNames = []string{logStream}
	}

	var out []json.RawMessage
	p := cloudwatchlogs.NewFilterLogEventsPaginator(c.api, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("filter %s: %w", c.opt.LogGroup, err)
		}
		for _, e := range page.Events {
			if e.Message == nil {
				continue
			}
			msg := strings.TrimSpace(*e.Message)
			if !strings.HasPrefix(msg, "{") {
				continue
			}
			out = append(out, json.RawMessage(msg))
		}
		if len(out) >= 10000 {
			break
		}
	}
	return out, nil
}

// emptyHint lists the reasons a bounded query over a span log group comes back
// empty, in the order worth checking. Which group an agent writes to depends on
// its creation date, its region, an environment variable, and its ADOT version,
// and querying the wrong one returns zero events rather than an error, so the
// destination can only be named here, not inferred (ADR-007 §3).
//
// `--since` leads because it is the most common cause and the cheapest to rule
// out: the default window is hours, and a trace worth investigating is often
// days old.
func (c *Client) emptyHint() string {
	if c.opt.LogGroup != DefaultLogGroup {
		// A per-agent group carries the agent's stdout and OTEL logs alongside
		// spans, and its `spans` stream can exist while staying empty when the
		// agent in fact delivers to the shared group. Scoping to a stream that
		// was never written returns zero events, so name that trap.
		return "  check: is --since long enough; is CloudWatch Transaction Search enabled;\n" +
			"  if you passed --log-stream, does that stream actually carry spans? An empty\n" +
			"  `spans` stream on a per-agent group means this agent delivers to " + DefaultLogGroup + ",\n" +
			"  so drop --log-stream and pass --log-group " + DefaultLogGroup + "."
	}
	return "  check: is --since long enough; is CloudWatch Transaction Search enabled; does this\n" +
		"  agent deliver spans to its own log group rather than the shared one? Newly created\n" +
		"  AgentCore agents do, in which case pass:\n" +
		"    --log-group /aws/bedrock-agentcore/runtimes/<agent-id>-<endpoint>\n" +
		"  Verify the destination before scoping to a stream, since a per-agent group can hold\n" +
		"  an empty `spans` stream while the spans themselves go to " + DefaultLogGroup + ":\n" +
		"    aws logs describe-log-streams --log-group-name <group> --query 'logStreams[].[logStreamName,lastEventTimestamp]'"
}

func (c *Client) logf(format string, args ...any) {
	if c.opt.Verbose != nil {
		fmt.Fprintf(c.opt.Verbose, format, args...)
	}
}

func firstTraceID(recs []json.RawMessage) string {
	for _, r := range recs {
		var m map[string]any
		if json.Unmarshal(r, &m) != nil {
			continue
		}
		for _, k := range []string{"traceId", "trace_id", "TraceId"} {
			if v, ok := m[k].(string); ok && v != "" {
				return v
			}
		}
	}
	return ""
}
