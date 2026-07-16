package main

import (
	"fmt"
	"os"

	"github.com/glim-sh/cuttle/packages/cuttle-go/internal/cli"
	_ "github.com/glim-sh/cuttle/packages/cuttle-go/internal/serve"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "cuttle:", err)
		os.Exit(1)
	}
}
