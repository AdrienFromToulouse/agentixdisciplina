// Command axda evaluates a recorded agent trace against a contract.
//
// Exit codes (ADR-001 §6): 0 pass, 1 blocking violation, 2 contract/trace error.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		// Cobra has already printed the error for flag and usage problems;
		// exitError carries a code chosen by the command.
		var code int = exitUsage
		if ee, ok := err.(*exitError); ok {
			code = ee.code
			if ee.printed {
				os.Exit(code)
			}
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(code)
	}
}
