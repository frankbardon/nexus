package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/frankbardon/nexus/pkg/events"
)

// pageImageProvider wires the fetch_page_image tool to an external
// screenshot-as-a-service provider. The plugin does not bundle its own
// renderer (would require a headless browser); operators bring their own
// (urlbox, screenshotapi.net, browserless, etc.) and configure the
// request shape via plugin config.
type pageImageProvider struct {
	url            string            // POST/GET endpoint
	method         string            // "POST" (default) or "GET"
	apiKeyEnv      string            // env var name; reads at request time
	urlParamName   string            // name of the URL field in the request body / query
	requestExtras  map[string]any    // arbitrary extra fields merged into the request body / query
	requestHeaders map[string]string // optional fixed headers (Authorization, etc.)
}

// configured reports whether the provider has the minimum config to make
// a request. Used by handleFetchPageImage to short-circuit with a clear
// error instead of attempting an empty POST.
func (p *pageImageProvider) configured() bool {
	return p != nil && strings.TrimSpace(p.url) != ""
}

// loadPageImageProviderConfig parses cfg["screenshot_provider"] into a
// pageImageProvider. Returns nil + nil err when the operator hasn't
// configured anything (tool stays disabled at runtime).
func loadPageImageProviderConfig(cfg map[string]any) (*pageImageProvider, error) {
	raw, ok := cfg["screenshot_provider"].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil, nil
	}
	p := &pageImageProvider{
		method:       "POST",
		urlParamName: "url",
	}
	if s, ok := raw["url"].(string); ok {
		p.url = strings.TrimSpace(s)
	}
	if s, ok := raw["method"].(string); ok && s != "" {
		m := strings.ToUpper(strings.TrimSpace(s))
		if m != http.MethodGet && m != http.MethodPost {
			return nil, fmt.Errorf("web: invalid screenshot_provider.method %q (want GET or POST)", s)
		}
		p.method = m
	}
	if s, ok := raw["api_key_env"].(string); ok {
		p.apiKeyEnv = strings.TrimSpace(s)
	}
	if s, ok := raw["url_param_name"].(string); ok && s != "" {
		p.urlParamName = s
	}
	if extras, ok := raw["request_template"].(map[string]any); ok {
		// Shallow copy so a later mutation of the parsed config map cannot
		// reach into our private state.
		p.requestExtras = make(map[string]any, len(extras))
		for k, v := range extras {
			p.requestExtras[k] = v
		}
	}
	if headers, ok := raw["headers"].(map[string]any); ok {
		p.requestHeaders = make(map[string]string, len(headers))
		for k, v := range headers {
			if s, ok := v.(string); ok {
				p.requestHeaders[k] = s
			}
		}
	}
	return p, nil
}

// handleFetchPageImage services a fetch_page_image tool call. The plugin
// itself does not render web pages — it forwards the request URL to the
// configured provider and routes the returned PNG bytes through the
// blob store like screenshot/fileio do.
func (p *Plugin) handleFetchPageImage(tc events.ToolCall) {
	if p.pageImage == nil || !p.pageImage.configured() {
		p.emitResultParts(tc, "", "fetch_page_image requires nexus.tool.web.screenshot_provider configuration; see docs", nil, nil)
		return
	}
	if p.blobStore == nil {
		p.emitResultParts(tc, "", "fetch_page_image is unavailable: blob store not initialised (no session workspace)", nil, nil)
		return
	}

	rawURL, _ := tc.Arguments["url"].(string)
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		p.emitResultParts(tc, "", "url argument is required", nil, nil)
		return
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		p.emitResultParts(tc, "", fmt.Sprintf("invalid url: %q", rawURL), nil, nil)
		return
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		p.emitResultParts(tc, "", fmt.Sprintf("only http(s) schemes are allowed, got %q", parsed.Scheme), nil, nil)
		return
	}
	host := strings.ToLower(parsed.Hostname())
	if !p.hostAllowed(host) {
		p.emitResultParts(tc, "", fmt.Sprintf("host %q is not allowed by policy", host), nil, nil)
		return
	}

	body, err := p.callPageImageProvider(parsed.String())
	if err != nil {
		p.emitResultParts(tc, "", err.Error(), nil, nil)
		return
	}
	if len(body) == 0 {
		p.emitResultParts(tc, "", "screenshot provider returned an empty body", nil, nil)
		return
	}

	const mime = "image/png"
	part := events.MessagePart{Type: "image", MimeType: mime}
	structured := map[string]any{
		"url":        parsed.String(),
		"media_type": mime,
		"size":       int64(len(body)),
	}
	if int64(len(body)) <= p.blobInlineCutoff {
		part.Data = body
	} else {
		h, err := p.blobStore.Put(body, mime)
		if err != nil {
			p.emitResultParts(tc, "", fmt.Sprintf("store blob: %s", err), nil, nil)
			return
		}
		if _, _, err := p.blobStore.Sweep(); err != nil {
			p.logger.Warn("web: blob store sweep failed", "error", err)
		}
		part.URI = h.URI()
		structured["blob_uri"] = h.URI()
	}

	summary := fmt.Sprintf("Captured %s as PNG (%d bytes)", parsed.String(), len(body))
	p.emitResultParts(tc, summary, "", structured, []events.MessagePart{part})
}

// callPageImageProvider performs the HTTP round-trip against the
// configured provider. Returns the raw response body bytes (expected to
// be PNG) on success.
func (p *Plugin) callPageImageProvider(target string) ([]byte, error) {
	prov := p.pageImage

	apiKey := ""
	if prov.apiKeyEnv != "" {
		apiKey = strings.TrimSpace(os.Getenv(prov.apiKeyEnv))
	}

	var req *http.Request
	switch prov.method {
	case http.MethodGet:
		u, err := url.Parse(prov.url)
		if err != nil {
			return nil, fmt.Errorf("invalid screenshot_provider.url: %w", err)
		}
		q := u.Query()
		q.Set(prov.urlParamName, target)
		for k, v := range prov.requestExtras {
			q.Set(k, fmt.Sprint(v))
		}
		if apiKey != "" {
			q.Set("api_key", apiKey)
		}
		u.RawQuery = q.Encode()
		req, err = http.NewRequest(http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
	default: // POST
		bodyMap := make(map[string]any, len(prov.requestExtras)+1)
		for k, v := range prov.requestExtras {
			bodyMap[k] = v
		}
		bodyMap[prov.urlParamName] = target
		bodyJSON, err := json.Marshal(bodyMap)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		req, err = http.NewRequest(http.MethodPost, prov.url, bytes.NewReader(bodyJSON))
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			// Most screenshot APIs accept either a header bearer token or a
			// query string param. We default to bearer for POST so the URL
			// doesn't leak in proxy logs; operators that need a different
			// shape can override via headers/request_template.
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	}
	for k, v := range prov.requestHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "image/png,image/*;q=0.9,*/*;q=0.5")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call screenshot provider: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read a small body sample so the error includes the provider's
		// own diagnostic text rather than just the HTTP status.
		sample, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("screenshot provider returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(sample)))
	}

	limited := io.LimitReader(resp.Body, p.maxSize+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read provider response: %w", err)
	}
	if int64(len(body)) > p.maxSize {
		return nil, fmt.Errorf("screenshot exceeds max_size (%d bytes)", p.maxSize)
	}
	return body, nil
}

// emitResultParts mirrors emitResult but threads OutputParts and
// OutputStructured for multimodal-shaped results. Stays in this file so
// the plain text path stays unchanged.
func (p *Plugin) emitResultParts(tc events.ToolCall, output, errMsg string, structured map[string]any, parts []events.MessagePart) {
	result := events.ToolResult{
		SchemaVersion:    events.ToolResultVersion,
		ID:               tc.ID,
		Name:             tc.Name,
		Output:           output,
		Error:            errMsg,
		OutputStructured: structured,
		OutputParts:      parts,
		TurnID:           tc.TurnID,
	}
	if veto, err := p.bus.EmitVetoable("before:tool.result", &result); err == nil && veto.Vetoed {
		p.logger.Info("tool.result vetoed", "tool", tc.Name, "reason", veto.Reason)
		return
	}
	_ = p.bus.Emit("tool.result", result)
}
