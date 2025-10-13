package internal

import (
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

type Parser struct{}

func NewParser() *Parser {
	return &Parser{}
}

func (p *Parser) ParseHTML(body io.Reader, baseURL string) (*Page, error) {
	doc, err := goquery.NewDocumentFromReader(body)
	if err != nil {
		return nil, fmt.Errorf("parse html: %w", err)
	}

	title := doc.Find("title").First().Text()
	if title == "" {
		title = "No Title"
	}

	content := doc.Find("body").Text()
	content = strings.TrimSpace(content)
	if len(content) > 500 {
		content = content[:500] + "..."
	}

	links := p.ExtractLinks(doc, baseURL)

	return &Page{
		URL:        baseURL,
		Title:      title,
		Content:    content,
		Links:      links,
		StatusCode: 200,
		CrawledAt:  time.Now(),
	}, nil
}

func (p *Parser) ExtractLinks(doc *goquery.Document, baseURL string) []string {
	var links []string
	seen := make(map[string]bool)

	doc.Find("a[href]").Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists {
			return
		}

		absoluteURL, err := NormalizeURL(href, baseURL)
		if err != nil {
			return
		}

		if !strings.HasPrefix(absoluteURL, "http://") && !strings.HasPrefix(absoluteURL, "https://") {
			return
		}

		// Skip non-HTML file extensions
		if isNonHTMLURL(absoluteURL) {
			return
		}

		//deduplicate
		if !seen[absoluteURL] {
			seen[absoluteURL] = true
			links = append(links, absoluteURL)
		}
	})

	return links
}

func isNonHTMLURL(urlStr string) bool {
	nonHTMLExts := []string{
		".jpg", ".jpeg", ".png", ".gif", ".webp", ".svg", ".ico",
		".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx",
		".zip", ".tar", ".gz", ".rar", ".7z",
		".mp3", ".mp4", ".avi", ".mov", ".wmv", ".flv",
		".css", ".js", ".json", ".xml",
		".exe", ".dmg", ".pkg", ".deb", ".rpm",
	}

	lowerURL := strings.ToLower(urlStr)
	for _, ext := range nonHTMLExts {
		if strings.HasSuffix(lowerURL, ext) {
			return true
		}
	}
	return false
}

func NormalizeURL(href, baseURL string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}

	u, err := url.Parse(href)
	if err != nil {
		return "", err
	}

	absolute := base.ResolveReference(u)

	absolute.Fragment = ""
	absolute.Scheme = strings.ToLower(absolute.Scheme)
	absolute.Host = strings.ToLower(absolute.Host)

	return absolute.String(), nil
}
