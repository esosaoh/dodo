package engine

import (
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

type RawLink struct {
	URL      string // absolute, fragment stripped
	Fragment string
	Text     string
}

type PageData struct {
	Title     string
	Links     []RawLink
	Malformed []RawLink // href is in URL, unparseable as a URL
	IDs       map[string]struct{}
}

func ParsePage(body io.Reader, baseURL string) (*PageData, error) {
	doc, err := goquery.NewDocumentFromReader(body)
	if err != nil {
		return nil, fmt.Errorf("parse html: %w", err)
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base url: %w", err)
	}

	pd := &PageData{
		Title: strings.TrimSpace(doc.Find("title").First().Text()),
		IDs:   make(map[string]struct{}),
	}

	seen := make(map[string]bool)
	doc.Find("a[href]").Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		text := strings.Join(strings.Fields(s.Text()), " ")
		if len(text) > 80 {
			text = text[:80]
		}
		u, err := url.Parse(strings.TrimSpace(href))
		if err == nil && u.Host == "" && u.Scheme != "" && u.Opaque == "" && !isBenignScheme(u.Scheme) {
			err = fmt.Errorf("scheme %q with no host", u.Scheme)
		}
		if err != nil {
			if !seen["!"+href] {
				seen["!"+href] = true
				pd.Malformed = append(pd.Malformed, RawLink{URL: href, Text: text})
			}
			return
		}
		abs := base.ResolveReference(u)
		if abs.Scheme != "http" && abs.Scheme != "https" {
			return
		}
		frag := abs.Fragment
		normalized := normalizeURL(abs)

		key := normalized + "#" + frag
		if seen[key] {
			return
		}
		seen[key] = true
		pd.Links = append(pd.Links, RawLink{URL: normalized, Fragment: frag, Text: text})
	})

	doc.Find("[id]").Each(func(_ int, s *goquery.Selection) {
		if id, ok := s.Attr("id"); ok && id != "" {
			pd.IDs[id] = struct{}{}
		}
	})
	doc.Find("a[name]").Each(func(_ int, s *goquery.Selection) {
		if name, ok := s.Attr("name"); ok && name != "" {
			pd.IDs[name] = struct{}{}
		}
	})

	return pd, nil
}

// mailto:, javascript: and friends are intentional non-web links, not authoring bugs
func isBenignScheme(scheme string) bool {
	switch strings.ToLower(scheme) {
	case "mailto", "tel", "sms", "javascript", "data", "ftp", "file", "irc", "magnet", "webcal", "matrix", "xmpp":
		return true
	}
	return false
}

func normalizeURL(u *url.URL) string {
	c := *u
	c.Fragment = ""
	c.Scheme = strings.ToLower(c.Scheme)
	c.Host = strings.ToLower(c.Host)
	if c.Scheme == "http" {
		c.Host = strings.TrimSuffix(c.Host, ":80")
	} else if c.Scheme == "https" {
		c.Host = strings.TrimSuffix(c.Host, ":443")
	}
	if c.Path == "" {
		c.Path = "/"
	}
	return c.String()
}

// NormalizeURL canonicalizes a loosely-typed URL (default scheme, lowercase
// host, default port stripped) to the form dodo stores link states under.
func NormalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if u.Scheme == "" {
		u, err = url.Parse("https://" + strings.TrimSpace(raw))
		if err != nil {
			return "", err
		}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("missing host in %q", raw)
	}
	return normalizeURL(u), nil
}

func pathQueryOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "/"
	}
	pq := u.Path
	if u.RawQuery != "" {
		pq += "?" + u.RawQuery
	}
	return pq
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// isNonHTMLURL filters URLs that can't be HTML; they're still verified as
// links, just never crawled.
func isNonHTMLURL(urlStr string) bool {
	exts := []string{
		".jpg", ".jpeg", ".png", ".gif", ".webp", ".svg", ".ico",
		".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx",
		".zip", ".tar", ".gz", ".rar", ".7z",
		".mp3", ".mp4", ".avi", ".mov", ".wmv", ".flv",
		".css", ".js", ".json", ".xml",
		".exe", ".dmg", ".pkg", ".deb", ".rpm",
	}
	lower := strings.ToLower(urlStr)
	if i := strings.IndexAny(lower, "?"); i >= 0 {
		lower = lower[:i]
	}
	for _, ext := range exts {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}
