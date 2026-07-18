package cmd

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/esosaoh/dodo/internal/cache"
	"github.com/esosaoh/dodo/internal/engine"
)

func cmdTrend(args []string) {
	fs := flag.NewFlagSet("trend", flag.ExitOnError)
	fs.Parse(args)

	raw := fs.Arg(0)
	if raw == "" {
		fmt.Fprintln(os.Stderr, "usage: dodo trend <url>")
		os.Exit(2)
	}
	seed, err := engine.NormalizeURL(raw)
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

	scans, err := store.Trend(context.Background(), seed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
	if len(scans) == 0 {
		fmt.Printf("no recorded scans for %s\n", seed)
		return
	}

	fmt.Printf("Trend for %s:\n\n", seed)
	for _, sc := range scans {
		broken := fmt.Sprintf("%d broken", sc.Broken)
		if sc.Broken > 0 {
			broken = colorize(colorRed, broken)
		} else {
			broken = colorize(colorGreen, broken)
		}
		fmt.Printf("  %s  %d links  %s\n", sc.StartedAt.Local().Format("2006-01-02 15:04"), sc.TotalLinks, broken)
	}
}
