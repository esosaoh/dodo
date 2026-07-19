package cmd

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/esosaoh/dodo/internal/cache"
	"github.com/esosaoh/dodo/internal/classify"
	"github.com/esosaoh/dodo/internal/engine"
)

func cmdCheck(args []string) {
	os.Exit(runCheck(args))
}

func runCheck(args []string) int {
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
	progressMode := fs.String("progress", "errors", "live stream verbosity: all, errors, none")
	fs.Parse(args)

	seed := fs.Arg(0)
	if seed == "" {
		fmt.Fprintln(os.Stderr, "usage: dodo check <url> [flags]")
		return 2
	}
	// flag stops at the first positional arg; re-parse anything after the URL
	// so `check <url> -depth 2` works too.
	if fs.NArg() > 1 {
		fs.Parse(fs.Args()[1:])
	}

	switch *progressMode {
	case "all", "errors", "none":
	default:
		fmt.Fprintf(os.Stderr, "invalid -progress value %q (want all, errors, or none)\n", *progressMode)
		return 2
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
	if !*jsonOut && *progressMode != "none" {
		e.OnProgress = phasePrinter()
		e.OnLinkChecked = linkPrinter(*progressMode == "errors")
	}

	rep, err := e.Run(ctx, seed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
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
	if hardBrokenCount(rep) > 0 {
		return 1
	}
	return 0
}

// hardBrokenCount excludes ERRORING (transient-reason dead) results:
// they're explicitly hedged as "may be transient" and shouldn't fail CI
// on their own.
func hardBrokenCount(rep *engine.Report) int {
	n := 0
	for _, r := range rep.Results {
		if r.Broken() && !(r.Class == classify.ClassDead && isTransientReason(r.Reason)) {
			n++
		}
	}
	return n
}

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

func linkPrinter(errorsOnly bool) engine.LinkCheckedFunc {
	var mu sync.Mutex
	return func(url string, class classify.Class, status int, reason string) {
		if errorsOnly && class == classify.ClassAlive {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		statusStr := "   "
		switch {
		case status != 0:
			statusStr = fmt.Sprintf("%3d", status)
		case reason != "":
			statusStr = reason
		}
		fmt.Fprintf(os.Stderr, "%s %s  %s\n", colorizeErr(classColor[class], linkMark(class)), statusStr, hyperlinkErr(url))
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

// BLOCKED gets a rollup instead (printBlockedRollup): nobody acts on an
// individual blocked stackoverflow/HN link.
var classLabels = []struct {
	class classify.Class
	label string
}{
	{classify.ClassDead, "DEAD"},
	{classify.ClassSoft404, "SOFT 404"},
	{classify.ClassMalformed, "MALFORMED (invalid href on page)"},
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
	printSummary(rep)

	printChanges(rep)

	seedHost := hostOfURL(rep.Seed)
	var erroring []engine.LinkResult
	for _, cl := range classLabels {
		var group []engine.LinkResult
		for _, r := range rep.Results {
			if r.Class != cl.class {
				continue
			}
			if cl.class == classify.ClassDead && isTransientReason(r.Reason) {
				erroring = append(erroring, r)
				continue
			}
			group = append(group, r)
		}
		if len(group) == 0 {
			continue
		}
		fmt.Printf("\n%s (%d):\n", colorize(classColor[cl.class], cl.label), len(group))
		for _, r := range group {
			printResultDetail(r, seedHost)
		}
	}

	if len(erroring) > 0 {
		fmt.Printf("\n%s (%d, may be transient):\n", colorize(colorYellow, "ERRORING"), len(erroring))
		for _, r := range erroring {
			printResultDetail(r, seedHost)
		}
	}

	printBlockedRollup(rep)

	fragIssues := 0
	for _, r := range rep.Results {
		if len(r.MissingFragments) == 0 {
			continue
		}
		if fragIssues == 0 {
			fmt.Println("\nMISSING ANCHORS (page is alive, #fragment target missing):")
		}
		fragIssues++
		line := fmt.Sprintf("  ⚠ %s — missing: #%s", hyperlink(r.URL), strings.Join(r.MissingFragments, ", #"))
		if refs := refsSummary(r.Refs, seedHost); refs != "" {
			line += "  (" + refs + ")"
		}
		fmt.Println(line)
	}
}

func printResultDetail(r engine.LinkResult, seedHost string) {
	reason := r.Reason
	if r.Status != 0 {
		reason = fmt.Sprintf("%d %s", r.Status, r.Reason)
	}
	if r.Attempts > 1 {
		reason += fmt.Sprintf(", %d attempts", r.Attempts)
	}
	line := fmt.Sprintf("  %s %s  %s", colorize(classColor[r.Class], linkMark(r.Class)), hyperlink(r.URL), reason)
	if refs := refsSummary(r.Refs, seedHost); refs != "" {
		line += "  (" + refs + ")"
	}
	fmt.Println(line)
}

// isTransientReason: a single bad response shouldn't read the same as a
// permanently dead link - these flip back and forth across scans.
func isTransientReason(reason string) bool {
	return reason == "server_error" || reason == "connection_refused"
}

func printSummary(rep *engine.Report) {
	erroring := 0
	for _, r := range rep.Results {
		if r.Class == classify.ClassDead && isTransientReason(r.Reason) {
			erroring++
		}
	}
	fmt.Printf("%s %d alive · %s %d broken",
		colorize(colorGreen, "✓"), rep.Counts[classify.ClassAlive],
		colorize(colorRed, "✗"), hardBrokenCount(rep))
	if erroring > 0 {
		fmt.Printf(" · %d erroring", erroring)
	}
	fmt.Printf(" · %d blocked · %d unknown\n", rep.Counts[classify.ClassBlocked], rep.Counts[classify.ClassUnknown])
}

func printBlockedRollup(rep *engine.Report) {
	counts := make(map[string]int)
	total := 0
	for _, r := range rep.Results {
		if r.Class != classify.ClassBlocked {
			continue
		}
		counts[hostOfURL(r.URL)]++
		total++
	}
	if total == 0 {
		return
	}

	type hostCount struct {
		host  string
		count int
	}
	hosts := make([]hostCount, 0, len(counts))
	for h, c := range counts {
		hosts = append(hosts, hostCount{h, c})
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].count > hosts[j].count })

	fmt.Printf("\n%s (%d links on %d hosts — could not verify, not counted as broken):\n  ",
		colorize(classColor[classify.ClassBlocked], "BLOCKED"), total, len(hosts))
	parts := make([]string, len(hosts))
	for i, h := range hosts {
		parts[i] = fmt.Sprintf("%s (%d)", h.host, h.count)
	}
	fmt.Println(strings.Join(parts, " · "))
}

func hostOfURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Host
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
		case r.Broken() && !prevBroken && !isTransientReason(r.Reason):
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
			fmt.Printf("    %s %s (was %s)\n", colorize(colorRed, "✗"), hyperlink(r.URL), classLabel(r.PrevClass))
		}
	}
	if len(fixed) > 0 {
		fmt.Printf("  %s (%d):\n", colorize(colorGreen, "fixed"), len(fixed))
		for _, r := range fixed {
			fmt.Printf("    %s %s (was %s)\n", colorize(colorGreen, "✓"), hyperlink(r.URL), classLabel(r.PrevClass))
		}
	}
}

func classLabel(c classify.Class) string {
	if c == classify.ClassSoft404 {
		return "soft 404"
	}
	return string(c)
}

// refsSummary compacts every referencing page into one "(/a, /b, +2 more)"
// clause instead of a line per ref - anchor text stays in -json only.
func refsSummary(refs []engine.Ref, seedHost string) string {
	seen := make(map[string]bool)
	var pages []string
	for _, ref := range refs {
		if ref.Page == "" {
			continue
		}
		key := strings.TrimSuffix(ref.Page, "/")
		if seen[key] {
			continue
		}
		seen[key] = true
		pages = append(pages, refPageLabel(ref.Page, seedHost))
	}
	if len(pages) == 0 {
		return ""
	}
	const maxShown = 2
	if len(pages) > maxShown {
		return strings.Join(pages[:maxShown], ", ") + fmt.Sprintf(", +%d more", len(pages)-maxShown)
	}
	return strings.Join(pages, ", ")
}

// refPageLabel keeps the hyperlink target as the full URL even when the
// displayed text is shortened to a same-host path.
func refPageLabel(page, seedHost string) string {
	display := page
	if hostOfURL(page) == seedHost {
		if u, err := url.Parse(page); err == nil {
			display = u.RequestURI()
			if u.Fragment != "" {
				display += "#" + u.Fragment
			}
		}
	}
	return hyperlinkAs(page, display)
}
