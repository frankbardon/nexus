package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// defaultTTSEndpoint is OpenAI's audio speech (TTS) endpoint.
const defaultTTSEndpoint = "https://api.openai.com/v1/audio/speech"

// defaultTTSChunkBytes is the default frame size for emitted output chunks.
// Picked at ~8 KB so each frame is small enough to stream over a websocket
// without head-of-line blocking but large enough to avoid event-bus chatter.
const defaultTTSChunkBytes = 8192

// ttsClient wraps an OpenAI-compatible /audio/speech endpoint. The endpoint
// is overridable via config (tts.endpoint) so tests can spin up an httptest
// server returning canned audio bytes.
type ttsClient struct {
	endpoint string
	apiKey   string
	model    string
	voice    string
	http     *http.Client
}

// synthesize POSTs the request body and returns the full MP3 bytes plus the
// content-type the server claimed. The TTS endpoint streams MP3 by default;
// we read the entire body up front because the bus envelope (chunked output
// events) prefers a complete buffer to slice from.
func (c *ttsClient) synthesize(ctx context.Context, text string) ([]byte, string, error) {
	if text == "" {
		return nil, "", nil
	}
	payload := map[string]any{
		"model":           c.model,
		"voice":           c.voice,
		"input":           text,
		"response_format": "mp3",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("voice: marshal TTS request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("voice: build TTS request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "audio/mpeg")

	client := c.http
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("voice: TTS HTTP error: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("voice: read TTS body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, "", fmt.Errorf("voice: TTS status %d: %s", resp.StatusCode, string(data))
	}

	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = "audio/mpeg"
	}
	return data, mime, nil
}
