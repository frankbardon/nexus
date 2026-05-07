package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
)

// defaultASREndpoint is OpenAI's audio transcription endpoint.
const defaultASREndpoint = "https://api.openai.com/v1/audio/transcriptions"

// asrClient wraps an OpenAI-compatible /audio/transcriptions endpoint. The
// endpoint is overridable via config (asr.endpoint) so tests can spin up an
// httptest server.
type asrClient struct {
	endpoint string
	apiKey   string
	model    string
	http     *http.Client
}

// transcribe POSTs the buffered audio as a multipart form with `file`,
// `model`, `response_format=json` fields and returns the parsed `text`.
//
// The provided mimeType is used to choose the upload filename's extension so
// the server-side decoder picks the right parser. We do not attempt any
// container-conversion locally — we ship the bytes as-is.
func (c *asrClient) transcribe(ctx context.Context, audio []byte, mimeType string) (string, error) {
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)

	// File field. Filename extension hints the server at the codec; default
	// to .wav since whisper's strongest path is PCM/WAV.
	fileName := "audio" + extForMime(mimeType)
	fw, err := mw.CreateFormFile("file", fileName)
	if err != nil {
		return "", fmt.Errorf("voice: build multipart form: %w", err)
	}
	if _, err := fw.Write(audio); err != nil {
		return "", fmt.Errorf("voice: write multipart audio: %w", err)
	}

	// Model field.
	if err := mw.WriteField("model", c.model); err != nil {
		return "", fmt.Errorf("voice: write multipart model: %w", err)
	}
	// Force JSON response so the response shape stays {"text": "..."}.
	if err := mw.WriteField("response_format", "json"); err != nil {
		return "", fmt.Errorf("voice: write multipart response_format: %w", err)
	}

	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("voice: close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, body)
	if err != nil {
		return "", fmt.Errorf("voice: build ASR request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	client := c.http
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("voice: ASR HTTP error: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("voice: ASR status %d: %s", resp.StatusCode, string(respBody))
	}

	var parsed struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("voice: parse ASR response: %w (body=%q)", err, string(respBody))
	}
	return parsed.Text, nil
}

// extForMime returns a likely filename extension for the given mime type.
// Whisper accepts wav/mp3/m4a/webm/ogg/flac; this map covers the common ones
// our realtime IO plugin produces. Unknown types default to .wav.
func extForMime(mime string) string {
	if mime == "" {
		return ".wav"
	}
	// Strip codec parameters.
	if i := indexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	switch mime {
	case "audio/wav", "audio/x-wav", "audio/wave", "audio/pcm", "audio/l16", "audio/x-l16":
		return ".wav"
	case "audio/webm":
		return ".webm"
	case "audio/ogg":
		return ".ogg"
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/mp4", "audio/m4a", "audio/x-m4a":
		return ".m4a"
	case "audio/flac":
		return ".flac"
	}
	return ".wav"
}

// indexByte is a tiny stand-in for strings.IndexByte without the import (the
// caller of extForMime is hot-path-adjacent and doesn't need the strings
// package elsewhere).
func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
