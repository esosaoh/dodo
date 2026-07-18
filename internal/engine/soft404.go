package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"hash/fnv"
	"math/bits"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/PuerkitoBio/goquery"

	"github.com/esosaoh/dodo/internal/fetch"
	"github.com/esosaoh/dodo/internal/scheduler"
)

// max Hamming distance (of 64 bits) at which two pages count as the same template
const simhashThreshold = 8

// fingerprints detects soft 404s: probe each host with a URL that cannot
// exist; if it answers 200, that page is the host's "not found" template and
// any checked link serving a near-identical page is a soft 404.
type fingerprints struct {
	e     *Engine
	mu    sync.Mutex
	hosts map[string]*hostFP
}

type hostFP struct {
	once sync.Once
	ok   bool
	hash uint64

	mu      sync.Mutex
	checks  int
	matched int
}

func newFingerprints(e *Engine) *fingerprints {
	return &fingerprints{e: e, hosts: make(map[string]*hostFP)}
}

func (f *fingerprints) forHost(ctx context.Context, scheme, host string) *hostFP {
	key := scheme + "://" + host
	f.mu.Lock()
	fp, ok := f.hosts[key]
	if !ok {
		fp = &hostFP{}
		f.hosts[key] = fp
	}
	f.mu.Unlock()
	fp.once.Do(func() { fp.probe(ctx, f.e, scheme, host) })
	return fp
}

// minWordsForFingerprint: pages with less extracted text than this (e.g. a
// client-rendered shell whose only server-side content is "Loading...")
// can't tell a soft 404 apart from real content, so they never establish or
// match a fingerprint.
const minWordsForFingerprint = 8

func (fp *hostFP) probe(ctx context.Context, e *Engine, scheme, host string) {
	url1 := scheme + "://" + host + "/" + randomSlug()
	first := e.probeFetch(ctx, host, url1)
	if first == nil || first.Status != 200 || first.Body == nil {
		return // host 404s properly; no fingerprint needed
	}
	hash1, words1 := simhashHTML(first.Body, pathTokens(url1))
	if words1 < minWordsForFingerprint {
		return // too little text to mean anything, e.g. a client-rendered shell
	}

	// Compare to a second garbage URL, not the homepage: hosts that redirect
	// dead links home would otherwise falsely disable the fingerprint.
	url2 := scheme + "://" + host + "/" + randomSlug()
	second := e.probeFetch(ctx, host, url2)
	if second == nil || second.Status != 200 || second.Body == nil {
		return
	}
	hash2, words2 := simhashHTML(second.Body, pathTokens(url2))
	if words2 < minWordsForFingerprint {
		return
	}
	if hamming(hash1, hash2) > simhashThreshold {
		return // inconsistent responses to unknown paths; no stable template
	}

	// A redirect to the site's own root is real evidence of a not-found
	// pattern (dead links bounced home): the server recognized the fake
	// path and still lives on this host. But a redirect to a *different*
	// host entirely - discarding the original path - proves nothing: it
	// means every path on this domain, real or fake, collapses to the same
	// destination (a deprecated/consolidated domain), same as a client-
	// rendered shell serving identical content for every path. Confirm
	// against the homepage whenever we don't have that same-host-redirect
	// evidence; if it matches too, nothing here is distinguishable.
	sameHostRedirect := first.Redirected && second.Redirected &&
		hostOf(first.FinalURL) == host && hostOf(second.FinalURL) == host
	if !sameHostRedirect {
		rootURL := scheme + "://" + host + "/"
		root := e.probeFetch(ctx, host, rootURL)
		if root != nil && root.Status == 200 && root.Body != nil {
			rootHash, rootWords := simhashHTML(root.Body, pathTokens(rootURL))
			if rootWords >= minWordsForFingerprint && hamming(rootHash, hash1) <= simhashThreshold {
				return
			}
		}
	}

	fp.hash = hash1
	fp.ok = true
}

func (fp *hostFP) matches(body []byte, linkURL string) bool {
	if !fp.ok || len(body) == 0 {
		return false
	}
	hash, words := simhashHTML(body, pathTokens(linkURL))
	if words < minWordsForFingerprint {
		return false // too little text on this page to compare meaningfully
	}
	matched := hamming(hash, fp.hash) <= simhashThreshold
	fp.mu.Lock()
	fp.checks++
	if matched {
		fp.matched++
	}
	fp.mu.Unlock()
	return matched
}

// suspect: a fingerprint matching most of a host's 200s can't be trusted.
func (fp *hostFP) suspect() bool {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return fp.checks >= 10 && fp.matched*100 >= fp.checks*60
}

func (f *fingerprints) peek(scheme, host string) *hostFP {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hosts[scheme+"://"+host]
}

func pathTokens(rawURL string) []string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	fields := strings.FieldsFunc(strings.ToLower(u.Path+" "+u.RawQuery), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	var toks []string
	for _, f := range fields {
		if len(f) >= 3 {
			toks = append(toks, f)
		}
	}
	return toks
}

func (e *Engine) probeFetch(ctx context.Context, host, url string) *fetch.FetchResult {
	if err := e.sched.Acquire(ctx, host); err != nil {
		return nil
	}
	fr := e.fetcher.Fetch(ctx, url, fetch.FetchOpts{WantBody: true})
	fb, ra := scheduler.FeedbackFor(fr)
	e.sched.Release(host, fb, ra)
	if fr.Err != nil || !fr.IsHTML() {
		return nil
	}
	return fr
}

func randomSlug() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "dodo-probe-" + hex.EncodeToString(b)
}

var (
	scriptRe = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	tagRe    = regexp.MustCompile(`(?s)<[^>]*>`)

	notFoundTitleRe = regexp.MustCompile(`(?i)\b(404|not\s+found|page\s+(doesn'?t|does\s+not)\s+exist|no\s+longer\s+(available|exists))\b`)
)

func titleLooks404(title string) bool {
	return notFoundTitleRe.MatchString(title)
}

// mainText drops nav/header/footer/aside and their common ARIA-role
// equivalents before extracting text, so template boilerplate shared by
// every page on a site doesn't dominate the fingerprint over the part of
// the page that's actually unique per URL.
func mainText(body []byte) string {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return ""
	}
	doc.Find(`script, style, nav, header, footer, aside, noscript, form, iframe,
		[role="navigation"], [role="banner"], [role="contentinfo"], [role="complementary"]`).Remove()
	return doc.Text()
}

// simhashHTML hashes 3-word shingles of visible text; similar templates land
// within a few bits. Strip tokens (the URL's path words) and URL-ish words
// are dropped so pages that echo the requested URL hash identically. The
// returned word count lets callers refuse to trust a hash built from too
// little text (e.g. a client-rendered shell whose only text is "Loading...").
func simhashHTML(body []byte, strip []string) (hash uint64, words int) {
	text := mainText(body)
	if text == "" {
		text = scriptRe.ReplaceAllString(string(body), " ")
		text = tagRe.ReplaceAllString(text, " ")
	}
	all := strings.Fields(strings.ToLower(text))
	kept := all[:0]
	for _, w := range all {
		if strings.ContainsRune(w, '/') || containsAny(w, strip) {
			continue
		}
		kept = append(kept, w)
	}

	var vec [64]int
	addFeature := func(s string) {
		h := fnv.New64a()
		h.Write([]byte(s))
		v := h.Sum64()
		for b := 0; b < 64; b++ {
			if v>>uint(b)&1 == 1 {
				vec[b]++
			} else {
				vec[b]--
			}
		}
	}

	if len(kept) < 3 {
		addFeature(strings.Join(kept, " "))
	} else {
		for i := 0; i+2 < len(kept); i++ {
			addFeature(kept[i] + " " + kept[i+1] + " " + kept[i+2])
		}
	}

	var out uint64
	for b := 0; b < 64; b++ {
		if vec[b] > 0 {
			out |= 1 << uint(b)
		}
	}
	return out, len(kept)
}

func containsAny(w string, toks []string) bool {
	for _, t := range toks {
		if strings.Contains(w, t) {
			return true
		}
	}
	return false
}

func hamming(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}
