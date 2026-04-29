// Package main is the entry point for the aforo-loadgen CLI.
package main

import (
	"fmt"
	"os"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/cli"
)

func main() {
	if err := cli.NewRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
