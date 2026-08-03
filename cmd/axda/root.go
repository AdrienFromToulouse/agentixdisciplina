package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

var version = "0.1.0-dev"

// Exit codes are part of the contract with CI (ADR-001 §6).
const (
	exitPass  = 0
	exitGate  = 1
	exitUsage = 2
)

// exitError lets a command choose its exit code. printed marks output the
// command already wrote, so main does not repeat it.
type exitError struct {
	code    int
	err     error
	printed bool
}

func (e *exitError) Error() string {
	if e.err == nil {
		return "failed"
	}
	return e.err.Error()
}

func (e *exitError) Unwrap() error { return e.err }

func failf(format string, args ...any) error {
	return &exitError{code: exitUsage, err: fmt.Errorf(format, args...)}
}

func fail(err error) error {
	return &exitError{code: exitUsage, err: err}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "axda",
		Short: "Out-of-band evaluation for AI agents",
		Long: `axda reads a recorded agent trace, checks it against a contract, and emits a
reliability score, a violation list, and span-anchored evidence.

The agent never knows it exists: the only coupling is the OpenTelemetry trace
it already emits.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}
	root.SetVersionTemplate(
		fmt.Sprintf("axda %s (episode/v1, report axda.dev/report/v1)\n", version))

	root.AddCommand(
		newEvaluateCmd(),
		newExplainCmd(),
		newTraceCmd(),
	)
	return root
}
