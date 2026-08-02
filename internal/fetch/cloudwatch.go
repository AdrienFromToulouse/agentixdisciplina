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
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
)

const DefaultLogGroup = "aws/spans"

type Options struct {
	Region   string
	LogGroup string
	Session  string
	TraceID  string
	Since    time.Duration
	Wait     time.Duration
	// Settle is how long the span set must stop growing before the trace is
	// considered complete. Returning early would risk a partial episode that
	// evaluates as a pass.
	Settle  time.Duration
	Verbose io.Writer
}

type Result struct {
	TraceID string
	Records []json.RawMessage
	Stable  bool
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
		opt.Since = 2 * time.Hour
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
		recs, err := c.query(ctx, c.opt.Session)
		if err != nil {
			return nil, err
		}
		if len(recs) == 0 {
			return nil, fmt.Errorf("no spans found for session %q in %s over the last %s\n  check: is CloudWatch Transaction Search enabled, and is --since long enough?",
				c.opt.Session, c.opt.LogGroup, c.opt.Since)
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
		recs, err := c.query(ctx, traceID)
		if err != nil {
			return nil, err
		}
		switch {
		case len(recs) != last:
			last = len(recs)
			stableSince = time.Now()
			c.logf("  %d span record(s)…\n", last)
		case len(recs) > 0 && time.Since(stableSince) >= c.opt.Settle:
			return &Result{TraceID: traceID, Records: recs, Stable: true}, nil
		}

		if time.Now().After(deadline) {
			if len(recs) == 0 {
				return nil, fmt.Errorf("no spans found for trace %q in %s", traceID, c.opt.LogGroup)
			}
			// Report instability rather than silently returning a possibly
			// partial trace.
			return &Result{TraceID: traceID, Records: recs, Stable: false}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// query does a substring match on the raw log message. Filtering on the raw
// text rather than a parsed field keeps this working regardless of how AWS
// names fields in the span record.
func (c *Client) query(ctx context.Context, needle string) ([]json.RawMessage, error) {
	start := time.Now().Add(-c.opt.Since)
	in := &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName:  aws.String(c.opt.LogGroup),
		StartTime:     aws.Int64(start.UnixMilli()),
		FilterPattern: aws.String(fmt.Sprintf("%q", needle)),
		Limit:         aws.Int32(10000),
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
