package cmd

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

	"github.com/esosaoh/dodo/internal/cache"
	"github.com/esosaoh/dodo/internal/classify"
	"github.com/esosaoh/dodo/internal/engine"
)

func cmdCheck(args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	cfg := engine.DefaultConfig()
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

	e := engine.NewEngine(cfg)
	if !*noCache {
		if store, err := cache.Open(""); err == nil {
			defer store.Close()
			e.Cache = store
		} else {
			fmt.Fprintf(os.Stderr, "warning: cache disabled: %v\n", err)
		}
	}
	if !*jsonOut {
		e.OnProgress = phasePrinter()
		e.OnLinkChecked = linkPrinter()
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

// phasePrinter announces each phase once, like a section header for the
// stream of per-link lines that follows.
func phasePrinter() engine.ProgressFunc {
	var mu sync.Mutex
	var last engine.Phase
	return func(p engine.Progress) {
		mu.Lock()
		defer mu.Unlock()
		if p.Phase == last {
			return
		}
		last = p.Phase
		switch p.Phase {
		case engine.PhaseCrawl:
			fmt.Fprintln(os.Stderr, "Crawling...")
		case engine.PhaseVerify:
			fmt.Fprintln(os.Stderr, "\nVerifying links...")
		}
	}
}

// linkPrinter streams one line per crawled page or verified link as it
// resolves, the way lychee and muffet do, instead of only an aggregate count.
func linkPrinter() engine.LinkCheckedFunc {
	var mu sync.Mutex
	return func(url string, class classify.Class, status int) {
		mu.Lock()
		defer mu.Unlock()
		statusStr := "   "
		if status != 0 {
			statusStr = fmt.Sprintf("%3d", status)
		}
		fmt.Fprintf(os.Stderr, "%s %s  %s\n", colorizeErr(classColor[class], linkMark(class)), statusStr, url)
	}
}

func linkMark(c classify.Class) string {
	switch c {
	case classify.ClassAlive:
		return "✓"
	case classify.ClassBlocked, classify.ClassUnknown:
		return "⚠"
	default:
		return "✗"
	}
}

var classLabels = []struct {
	class classify.Class
	label string
}{
	{classify.ClassDead, "DEAD"},
	{classify.ClassSoft404, "SOFT 404"},
	{classify.ClassMalformed, "MALFORMED (invalid href on page)"},
	{classify.ClassBlocked, "BLOCKED (could not verify: bot protection / auth)"},
	{classify.ClassUnknown, "UNKNOWN"},
}

func printReport(rep *engine.Report) {
	dur := rep.FinishedAt.Sub(rep.StartedAt).Round(time.Millisecond)
	fmt.Printf("\nScanned %s in %v\n", rep.Seed, dur)
	fmt.Printf("Pages crawled: %d · unique links: %d", rep.PagesCrawled, rep.TotalLinks)
	if rep.Cached > 0 {
		fmt.Printf(" (%d from cache)", rep.Cached)
	}
	fmt.Println()

	printChanges(rep)

	for _, cl := range classLabels {
		var group []engine.LinkResult
		for _, r := range rep.Results {
			if r.Class == cl.class {
				group = append(group, r)
			}
		}
		if len(group) == 0 {
			continue
		}
		fmt.Printf("\n%s (%d):\n", colorize(classColor[cl.class], cl.label), len(group))
		for _, r := range group {
			fmt.Printf("  %s %s\n", colorize(classColor[r.Class], "✗"), r.URL)
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

	fmt.Printf("\n%s %d alive · %s %d broken · %d blocked · %d unknown\n",
		colorize(colorGreen, "✓"), rep.Counts[classify.ClassAlive],
		colorize(colorRed, "✗"), rep.Broken,
		rep.Counts[classify.ClassBlocked], rep.Counts[classify.ClassUnknown])
}

func wasBroken(c classify.Class) bool {
	return c == classify.ClassDead || c == classify.ClassSoft404 || c == classify.ClassMalformed
}

func printChanges(rep *engine.Report) {
	var newBroken, fixed []engine.LinkResult
	for _, r := range rep.Results {
		if r.PrevClass == "" {
			continue
		}
		switch prevBroken := wasBroken(r.PrevClass); {
		case r.Broken() && !prevBroken:
			newBroken = append(newBroken, r)
		case !r.Broken() && prevBroken:
			fixed = append(fixed, r)
		}
	}
	if len(newBroken) == 0 && len(fixed) == 0 {
		return
	}

	fmt.Println(colorize(colorBold, "\nSINCE LAST SCAN"))
	if len(newBroken) > 0 {
		fmt.Printf("  %s (%d):\n", colorize(colorRed, "newly broken"), len(newBroken))
		for _, r := range newBroken {
			fmt.Printf("    %s %s (was %s)\n", colorize(colorRed, "✗"), r.URL, r.PrevClass)
		}
	}
	if len(fixed) > 0 {
		fmt.Printf("  %s (%d):\n", colorize(colorGreen, "fixed"), len(fixed))
		for _, r := range fixed {
			fmt.Printf("    %s %s (was %s)\n", colorize(colorGreen, "✓"), r.URL, r.PrevClass)
		}
	}
}

func printRefs(refs []engine.Ref) {
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
