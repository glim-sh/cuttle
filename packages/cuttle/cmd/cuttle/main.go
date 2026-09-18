package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/glim-sh/cuttle/internal/cli"
	_ "github.com/glim-sh/cuttle/internal/serve"
)

func main() {
	if err := cli.Execute(); err != nil {
		// A passthrough verb's child owns the exit status and has already written
		// its own stderr, so mirror the code instead of collapsing it to 1.
		if ec, ok := errors.AsType[*cli.ExitCodeError](err); ok {
			os.Exit(ec.Code)
		}
		fmt.Fprintln(os.Stderr, "cuttle:", err)
		os.Exit(1)
	}
}
