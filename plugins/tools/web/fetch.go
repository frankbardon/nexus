package web

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"codeberg.org/readeck/go-readability/v2"
	"golang.org/x/net/html"

	"github.com/frankbardon/nexus/pkg/events"
)

// handleFetch services a web_fetch tool call. It respects the allow/block
// domain lists, enforces max response size, and runs the configured
// extraction strategy. On readability failure it returns an error rather
// than silently degrading — the agent can always re-call with extract='raw'.
func (p *Plugin) handleFetch(tc events.ToolCall) {
	rawURL, _ := tc.Arguments["url"].(string)
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		p.emitResult(tc, "", "url argument is required")
		return
	}

	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		p.emitResult(tc, "", fmt.Sprintf("invalid url: %q", rawURL))
		return
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		p.emitResult(tc, "", fmt.Sprintf("only http(s) schemes are allowed, got %q", parsed.Scheme))
		return
	}

	host := strings.ToLower(parsed.Hostname())
	if !p.hostAllowed(host) {
		p.emitResult(tc, "", fmt.Sprintf("host %q is not allowed by policy", host))
		return
	}

	extract := p.defaultExtract
	if s, ok := tc.Arguments["extract"].(string); ok && s != "" {
		if s != "readability" && s != "raw" {
			p.emitResult(tc, "", fmt.Sprintf("invalid extract mode %q (want readability|raw)", s))
			return
		}
		extract = s
	}

	cacheKey := extract + "|" + parsed.String()
	if hit, ok := p.cache.get(cacheKey); ok {
		p.emitResult(tc, hit, "")
		return
	}

	body, finalURL, err := p.doFetch(parsed.String())
	if err != nil {
		p.emitResult(tc, "", err.Error())
		return
	}

	var output string
	switch extract {
	case "readability":
		article, err := readability.FromReader(bytes.NewReader(body), mustParseURL(finalURL))
		if err != nil {
			p.emitResult(tc, "", fmt.Sprintf("readability extraction failed for %s: %s", finalURL, err))
			return
		}
		var buf bytes.Buffer
		if err := article.RenderText(&buf); err != nil {
			p.emitResult(tc, "", fmt.Sprintf("readability found no article content at %s (try extract='raw')", finalURL))
			return
		}
		text := strings.TrimSpace(buf.String())
		if text == "" {
			p.emitResult(tc, "", fmt.Sprintf("readability found no article content at %s (try extract='raw')", finalURL))
			return
		}
		output = renderArticle(article, finalURL, text)
	case "raw":
		text, err := extractRawText(bytes.NewReader(body))
		if err != nil {
			p.emitResult(tc, "", fmt.Sprintf("raw extraction failed for %s: %s", finalURL, err))
			return
		}
		output = renderRaw(finalURL, text)
	}

	p.cache.set(cacheKey, output)
	p.emitResult(tc, output, "")
}

func (p *Plugin) hostAllowed(host string) bool {
	for _, blocked := range p.blockedDomains {
		if domainMatch(host, blocked) {
			return false
		}
	}
	if len(p.allowedDomains) == 0 {
		return true
	}
	for _, allowed := range p.allowedDomains {
		if domainMatch(host, allowed) {
			return true
		}
	}
	return false
}

// domainMatch treats `example.com` as matching both itself and any subdomain.
func domainMatch(host, pattern string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "" {
		return false
	}
	if host == pattern {
		return true
	}
	return strings.HasSuffix(host, "."+pattern)
}

func (p *Plugin) doFetch(target string) ([]byte, string, error) {
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		return nil, "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", p.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch %s: %w", target, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("fetch %s: HTTP %d", target, resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if ct != "" && !looksLikeHTML(ct) {
		return nil, "", fmt.Errorf("fetch %s: unsupported content-type %q (only html/text accepted)", target, ct)
	}

	// Enforce a hard size cap even if Content-Length is missing or lies.
	limited := io.LimitReader(resp.Body, p.maxSize+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > p.maxSize {
		return nil, "", fmt.Errorf("fetch %s: response exceeds max_size (%d bytes)", target, p.maxSize)
	}

	final := target
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return body, final, nil
}

func looksLikeHTML(contentType string) bool {
	ct := strings.ToLower(contentType)
	return strings.Contains(ct, "html") || strings.Contains(ct, "xml") ||
		strings.HasPrefix(ct, "text/") || strings.Contains(ct, "application/xhtml")
}

func mustParseURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		return &url.URL{}
	}
	return u
}

// extractRawText walks the full HTML tree and emits visible text. Used when
// readability would strip too much (docs sites, reference tables, forums).
func extractRawText(r io.Reader) (string, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n == nil {
			return
		}
		if n.Type == html.ElementNode {
			name := strings.ToLower(n.Data)
			if name == "script" || name == "style" || name == "noscript" || name == "template" {
				return
			}
		}
		if n.Type == html.TextNode {
			t := strings.TrimSpace(n.Data)
			if t != "" {
				b.WriteString(t)
				b.WriteByte(' ')
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode {
			switch strings.ToLower(n.Data) {
			case "p", "div", "br", "li", "h1", "h2", "h3", "h4", "h5", "h6", "tr":
				b.WriteByte('\n')
			}
		}
	}
	walk(doc)
	return collapseBlankLines(b.String()), nil
}

func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if !blank {
				out = append(out, "")
			}
			blank = true
			continue
		}
		out = append(out, trimmed)
		blank = false
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func renderArticle(article readability.Article, finalURL, text string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "URL: %s\n", finalURL)
	if title := strings.TrimSpace(article.Title()); title != "" {
		fmt.Fprintf(&b, "Title: %s\n", title)
	}
	if byline := strings.TrimSpace(article.Byline()); byline != "" {
		fmt.Fprintf(&b, "Byline: %s\n", byline)
	}
	if excerpt := strings.TrimSpace(article.Excerpt()); excerpt != "" {
		fmt.Fprintf(&b, "Summary: %s\n", excerpt)
	}
	if site := strings.TrimSpace(article.SiteName()); site != "" {
		fmt.Fprintf(&b, "Site: %s\n", site)
	}
	if published, err := article.PublishedTime(); err == nil && !published.IsZero() {
		fmt.Fprintf(&b, "Published: %s\n", published.Format("2006-01-02"))
	}
	b.WriteString("Extract: readability\n\n")
	b.WriteString(text)
	return b.String()
}

func renderRaw(finalURL, text string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "URL: %s\n", finalURL)
	b.WriteString("Extract: raw\n\n")
	b.WriteString(text)
	return b.String()
}
