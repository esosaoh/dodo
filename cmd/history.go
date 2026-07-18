package cmd

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/esosaoh/dodo/internal/cache"
	"github.com/esosaoh/dodo/internal/engine"
)

func cmdHistory(args []string) {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	fs.Parse(args)

	raw := fs.Arg(0)
	if raw == "" {
		fmt.Fprintln(os.Stderr, "usage: dodo history <url>")
		os.Exit(2)
	}
	url, err := engine.NormalizeURL(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}

	store, err := cache.Open("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
	defer store.Close()

	hist, err := store.History(context.Background(), url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
	if len(hist) == 0 {
		fmt.Printf("no recorded history for %s\n", url)
		return
	}

	fmt.Printf("History for %s:\n\n", url)
	for _, st := range hist {
		class := colorize(classColor[st.Class], string(st.Class))
		line := fmt.Sprintf("  %s  %s", st.CheckedAt.Local().Format("2006-01-02 15:04"), class)
		if st.Status != 0 {
			line += fmt.Sprintf("  HTTP %d", st.Status)
		}
		fmt.Println(line)
	}
}
