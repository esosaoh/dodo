// Package cmd wires up dodo's subcommands.
package cmd

import (
	"fmt"
	"os"
)

func Execute() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "check":
		cmdCheck(os.Args[2:])
	case "history":
		cmdHistory(os.Args[2:])
	case "trend":
		cmdTrend(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `dodo — a fast, low-false-positive dead link checker

Usage:
  dodo check <url> [flags]   scan a site and report broken links
  dodo history <url>         show a link's recorded status over past scans
  dodo trend <url>           show a site's broken-link count over past scans

Exit codes: 0 no broken links, 1 broken links found, 2 error.
Run 'dodo check -h' for flags.`)
}
