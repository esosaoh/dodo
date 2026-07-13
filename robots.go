package main

import (
	"context"
	"strings"
	"sync"
)

// robots gates the crawl phase only: verifying a link is a single
// user-triggered fetch (like clicking it), so it is never blocked here.
type robotsCache struct {
	e     *Engine
	mu    sync.Mutex
	hosts map[string]*hostRobots
}

type hostRobots struct {
	once  sync.Once
	rules []robotsRule // nil means allow everything
}

type robotsRule struct {
	allow   bool
	pattern string
}

func newRobotsCache(e *Engine) *robotsCache {
	return &robotsCache{e: e, hosts: make(map[string]*hostRobots)}
}

func (r *robotsCache) allowed(ctx context.Context, scheme, host, path string) bool {
	key := scheme + "://" + host
	r.mu.Lock()
	hr, ok := r.hosts[key]
	if !ok {
		hr = &hostRobots{}
		r.hosts[key] = hr
	}
	r.mu.Unlock()

	hr.once.Do(func() {
		if err := r.e.sched.Acquire(ctx, host); err != nil {
			return
		}
		fr := r.e.fetcher.Fetch(ctx, key+"/robots.txt", FetchOpts{WantBody: true, AnyContentType: true})
		fb, ra := feedbackFor(fr)
		r.e.sched.Release(host, fb, ra)
		if fr.Err != nil || fr.Status != 200 || len(fr.Body) == 0 {
			return // no robots.txt (or unreachable): allow everything
		}
		hr.rules = parseRobots(string(fr.Body), r.e.cfg.UserAgent)
	})

	return robotsAllowed(hr.rules, path)
}

// parseRobots returns the rules of the most specific matching user-agent group
// (a token of our UA beats *).
func parseRobots(body, userAgent string) []robotsRule {
	uaToken := strings.ToLower(userAgent)
	if i := strings.IndexAny(uaToken, "/ "); i > 0 {
		uaToken = uaToken[:i]
	}

	var starRules, uaRules []robotsRule
	var currentAgents []string
	inGroup := false // false while collecting consecutive User-agent lines

	for _, line := range strings.Split(body, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)

		switch key {
		case "user-agent":
			if inGroup {
				currentAgents = currentAgents[:0]
				inGroup = false
			}
			currentAgents = append(currentAgents, strings.ToLower(val))
		case "allow", "disallow":
			inGroup = true
			if val == "" {
				continue // "Disallow:" (empty) allows everything
			}
			rule := robotsRule{allow: key == "allow", pattern: val}
			for _, agent := range currentAgents {
				switch {
				case agent == "*":
					starRules = append(starRules, rule)
				case strings.Contains(uaToken, agent) || strings.Contains(agent, uaToken):
					uaRules = append(uaRules, rule)
				}
			}
		}
	}
	if uaRules != nil {
		return uaRules
	}
	return starRules
}

// robotsAllowed applies longest-pattern-wins; allow wins ties.
func robotsAllowed(rules []robotsRule, path string) bool {
	if path == "" {
		path = "/"
	}
	bestLen := -1
	allowed := true
	for _, r := range rules {
		if !robotsPatternMatches(r.pattern, path) {
			continue
		}
		if len(r.pattern) > bestLen || (len(r.pattern) == bestLen && r.allow) {
			bestLen = len(r.pattern)
			allowed = r.allow
		}
	}
	return allowed
}

// robotsPatternMatches supports the * wildcard and $ end anchor.
func robotsPatternMatches(pattern, path string) bool {
	anchored := strings.HasSuffix(pattern, "$")
	if anchored {
		pattern = pattern[:len(pattern)-1]
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(path, parts[0]) {
		return false
	}
	idx := len(parts[0])
	for _, p := range parts[1:] {
		if p == "" {
			idx = len(path) // trailing * consumes the rest
			continue
		}
		j := strings.Index(path[idx:], p)
		if j < 0 {
			return false
		}
		idx += j + len(p)
	}
	return !anchored || idx == len(path)
}
