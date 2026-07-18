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

func progressPrinter() engine.ProgressFunc {
	var mu sync.Mutex
	var last time.Time
	var linesPrinted int
	throttle := 150 * time.Millisecond
	if !stderrLive {
		throttle = 2 * time.Second // plain log lines: don't flood the output
	}

	return func(p engine.Progress) {
		mu.Lock()
		defer mu.Unlock()
		if p.Phase != engine.PhaseDone && time.Since(last) < throttle {
			return
		}
		last = time.Now()
		lines := progressLines(p)

		if !stderrLive {
			for _, line := range lines {
				fmt.Fprintln(os.Stderr, line)
			}
			return
		}
		if linesPrinted > 0 {
			fmt.Fprintf(os.Stderr, "\033[%dA", linesPrinted)
		}
		for _, line := range lines {
			fmt.Fprintf(os.Stderr, "\033[2K%s\n", line)
		}
		linesPrinted = len(lines)
	}
}

func progressLines(p engine.Progress) []string {
	if p.Phase == engine.PhaseCrawl {
		return []string{fmt.Sprintf("Crawling: %d pages, %d links found", p.PagesCrawled, p.LinksFound)}
	}
	pct := 0
	if p.LinksTotal > 0 {
		pct = 100 * p.LinksChecked / p.LinksTotal
	}
	return []string{
		fmt.Sprintf("Verifying %s %3d%%  %d/%d links",
			progressBar(p.LinksChecked, p.LinksTotal, 24), pct, p.LinksChecked, p.LinksTotal),
		fmt.Sprintf("  %s %d alive   %s %d broken   %d blocked",
			colorizeErr(colorGreen, "✓"), p.Alive, colorizeErr(colorRed, "✗"), p.Broken, p.Blocked),
	}
}

func progressBar(current, total, width int) string {
	if total <= 0 {
		return strings.Repeat("░", width)
	}
	filled := min(width*current/total, width)
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
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
