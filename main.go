package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "check":
		cmdCheck(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `dodo — a fast, low-false-positive dead link checker

Usage:
  dodo check <url> [flags]   scan a site and report broken links

Exit codes: 0 no broken links, 1 broken links found, 2 error.
Run 'dodo check -h' for flags.`)
}

func cmdCheck(args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	cfg := DefaultConfig()
	fs.IntVar(&cfg.MaxDepth, "depth", cfg.MaxDepth, "max crawl depth on the seed site")
	fs.IntVar(&cfg.MaxPages, "max-pages", cfg.MaxPages, "max pages to crawl")
	fs.IntVar(&cfg.Workers, "workers", cfg.Workers, "global worker count")
	fs.IntVar(&cfg.PerHostMax, "per-host", cfg.PerHostMax, "max concurrent requests per host")
	fs.DurationVar(&cfg.Timeout, "timeout", cfg.Timeout, "per-request timeout")
	fs.BoolVar(&cfg.CheckExternal, "external", cfg.CheckExternal, "check external links")
	fs.BoolVar(&cfg.RespectRobots, "robots", cfg.RespectRobots, "respect robots.txt when crawling the seed site")
	fs.BoolVar(&cfg.Soft404, "soft404", cfg.Soft404, "detect soft 404s via response fingerprinting")
	fs.BoolVar(&cfg.CheckFragments, "fragments", cfg.CheckFragments, "validate #fragment anchors")
	fs.IntVar(&cfg.MaxRetries, "retries", cfg.MaxRetries, "retry rounds for transient failures")
	fs.DurationVar(&cfg.CacheTTL, "cache-ttl", cfg.CacheTTL, "skip re-checking links verified healthy within this window")
	noCache := fs.Bool("no-cache", false, "disable the local link-state cache")
	jsonOut := fs.Bool("json", false, "emit the full report as JSON")
	fs.Parse(args)

	seed := fs.Arg(0)
	if seed == "" {
		fmt.Fprintln(os.Stderr, "usage: dodo check <url> [flags]")
		os.Exit(2)
	}
	// flag stops at the first positional arg; re-parse anything after the URL
	// so `check <url> -depth 2` works too.
	if fs.NArg() > 1 {
		fs.Parse(fs.Args()[1:])
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	e := newEngine(cfg)
	if !*noCache {
		if cache, err := newFileCache(""); err == nil {
			e.Cache = cache
		} else {
			fmt.Fprintf(os.Stderr, "warning: cache disabled: %v\n", err)
		}
	}
	if !*jsonOut {
		e.OnProgress = progressPrinter()
	}

	rep, err := e.Run(ctx, seed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
	if !*jsonOut {
		fmt.Fprintln(os.Stderr)
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(rep)
	} else {
		printReport(rep)
	}
	if rep.Broken > 0 {
		os.Exit(1)
	}
}

func progressPrinter() ProgressFunc {
	var mu sync.Mutex
	var last time.Time
	return func(p Progress) {
		mu.Lock()
		defer mu.Unlock()
		if p.Phase != PhaseDone && time.Since(last) < 300*time.Millisecond {
			return
		}
		last = time.Now()
		fmt.Fprintf(os.Stderr, "\r[%-6s] pages %-4d links %-5d checked %d/%d · broken %d   ",
			p.Phase, p.PagesCrawled, p.LinksFound, p.LinksChecked, p.LinksTotal, p.Broken)
	}
}

var classLabels = []struct {
	class Class
	label string
}{
	{ClassDead, "DEAD"},
	{ClassSoft404, "SOFT 404"},
	{ClassBlocked, "BLOCKED (could not verify: bot protection / auth)"},
	{ClassUnknown, "UNKNOWN"},
}

func printReport(rep *Report) {
	dur := rep.FinishedAt.Sub(rep.StartedAt).Round(time.Millisecond)
	fmt.Printf("\nScanned %s in %v\n", rep.Seed, dur)
	fmt.Printf("Pages crawled: %d · unique links: %d", rep.PagesCrawled, rep.TotalLinks)
	if rep.Cached > 0 {
		fmt.Printf(" (%d from cache)", rep.Cached)
	}
	fmt.Println()

	for _, cl := range classLabels {
		var group []LinkResult
		for _, r := range rep.Results {
			if r.Class == cl.class {
				group = append(group, r)
			}
		}
		if len(group) == 0 {
			continue
		}
		fmt.Printf("\n%s (%d):\n", cl.label, len(group))
		for _, r := range group {
			fmt.Printf("  ✗ %s\n", r.URL)
			detail := fmt.Sprintf("    %s · confidence %.0f%%", r.Reason, r.Confidence*100)
			if r.Status != 0 {
				detail = fmt.Sprintf("    HTTP %d · %s · confidence %.0f%%", r.Status, r.Reason, r.Confidence*100)
			}
			if r.Attempts > 1 {
				detail += fmt.Sprintf(" · %d attempts", r.Attempts)
			}
			fmt.Println(detail)
			printRefs(r.Refs)
		}
	}

	fragIssues := 0
	for _, r := range rep.Results {
		if len(r.MissingFragments) == 0 {
			continue
		}
		if fragIssues == 0 {
			fmt.Println("\nMISSING ANCHORS (page is alive, #fragment target missing):")
		}
		fragIssues++
		fmt.Printf("  ⚠ %s — missing: #%s\n", r.URL, strings.Join(r.MissingFragments, ", #"))
		printRefs(r.Refs)
	}

	fmt.Printf("\n✓ %d alive · ✗ %d broken · %d blocked · %d unknown\n",
		rep.Counts[ClassAlive], rep.Broken,
		rep.Counts[ClassBlocked], rep.Counts[ClassUnknown])
}

func printRefs(refs []Ref) {
	shown := 0
	for _, ref := range refs {
		if ref.Page == "" {
			continue
		}
		if shown == 3 {
			fmt.Printf("      … and %d more\n", len(refs)-shown)
			return
		}
		line := "      on " + ref.Page
		if ref.Text != "" {
			line += fmt.Sprintf(" (%q)", ref.Text)
		}
		fmt.Println(line)
		shown++
	}
}
