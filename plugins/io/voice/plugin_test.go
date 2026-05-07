package voice

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// newPluginForTest wires a Plugin against a real engine.EventBus and a
// pair of httptest servers for ASR + TTS. It returns the plugin, the bus,
// and the two server URLs. Caller is responsible for invoking Shutdown.
func newPluginForTest(t *testing.T, asrURL, ttsURL string, mutate func(map[string]any)) (*Plugin, engine.EventBus) {
	t.Helper()

	bus := engine.NewEventBus()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := map[string]any{
		"asr": map[string]any{
			"endpoint": asrURL,
			"api_key":  "test",
		},
		"tts": map[string]any{
			"endpoint":    ttsURL,
			"api_key":     "test",
			"chunk_bytes": 16,
		},
		"vad": map[string]any{
			"threshold":  0.05,
			"silence_ms": 50,
		},
		"barge_in": map[string]any{
			"enabled":   true,
			"threshold": 0.05,
		},
	}
	if mutate != nil {
		mutate(cfg)
	}

	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{
		Config: cfg,
		Bus:    bus,
		Logger: logger,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
	})

	return p, bus
}

// makePCMChunk builds a base64-encoded little-endian int16 PCM buffer at the
// given amplitude (0..1). amp=0 produces silence, amp=0.5 produces ~half-
// scale energy.
func makePCMChunk(amp float64, samples int) string {
	buf := make([]byte, samples*2)
	val := int16(amp * 32767)
	for i := 0; i < samples; i++ {
		// Square wave, alternating sign — gives consistent RMS = amp.
		v := val
		if i%2 == 1 {
			v = -val
		}
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(v))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// startASRServer returns an httptest server that always returns the supplied
// transcription text for /v1/audio/transcriptions.
func startASRServer(t *testing.T, transcription string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Multipart parsing — best effort. We do not assert on the body to
		// keep the test focused on the plugin's plumbing.
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			t.Logf("server: parse multipart: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"text": transcription})
	})
	return httptest.NewServer(mux)
}

// startTTSServer returns an httptest server that returns the supplied audio
// bytes with content-type audio/mpeg.
func startTTSServer(t *testing.T, audio []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(audio)
	})
	return httptest.NewServer(mux)
}

// --- VAD tests --------------------------------------------------------------

func TestComputeRMS_PCMSilence(t *testing.T) {
	silent := make([]byte, 2048) // all zeros == silence
	rms := computeRMS(silent, "audio/wav")
	if rms != 0 {
		t.Fatalf("silence RMS expected 0, got %v", rms)
	}
}

func TestComputeRMS_PCMHalfScale(t *testing.T) {
	// 16384 (~half scale int16) signed alternating gives RMS ~ 0.5.
	buf := make([]byte, 4096)
	for i := 0; i < len(buf); i += 2 {
		var v int16 = 16384
		if (i/2)%2 == 1 {
			v = -16384
		}
		binary.LittleEndian.PutUint16(buf[i:], uint16(v))
	}
	rms := computeRMS(buf, "audio/wav")
	if math.Abs(rms-0.5) > 0.01 {
		t.Fatalf("expected ~0.5 RMS, got %v", rms)
	}
}

func TestComputeRMS_OpaqueByteHeuristic(t *testing.T) {
	// Bytes near 128 -> low energy; bytes near 0/255 -> high energy.
	low := []byte{128, 128, 128, 128, 129, 127, 128, 128}
	high := []byte{0, 255, 0, 255, 0, 255, 0, 255}

	lowRMS := computeRMS(low, "audio/webm")
	highRMS := computeRMS(high, "audio/webm")
	if lowRMS >= highRMS {
		t.Fatalf("expected low<high, got low=%v high=%v", lowRMS, highRMS)
	}
}

// --- VAD silence flush ------------------------------------------------------

// TestVAD_SilenceFlushTriggersASR feeds a chunk of speech-level audio, waits
// past silence_ms with no further chunks, and asserts io.input fires.
func TestVAD_SilenceFlushTriggersASR(t *testing.T) {
	asr := startASRServer(t, "hello world")
	defer asr.Close()
	tts := startTTSServer(t, []byte("audio"))
	defer tts.Close()

	_, bus := newPluginForTest(t, asr.URL, tts.URL, nil)

	var (
		mu        sync.Mutex
		gotInputs []events.UserInput
	)
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		if u, ok := e.Payload.(events.UserInput); ok {
			mu.Lock()
			gotInputs = append(gotInputs, u)
			mu.Unlock()
		}
	})

	// Speech-level chunk.
	if err := bus.Emit("voice.audio.input.chunk", events.VoiceAudioInputChunk{
		SchemaVersion: events.VoiceAudioInputChunkVersion,
		TurnID:        "t1",
		AudioBase64:   makePCMChunk(0.5, 256),
		MimeType:      "audio/wav",
	}); err != nil {
		t.Fatalf("emit chunk: %v", err)
	}

	// Wait for silence_ms (50ms) + ASR roundtrip + emit.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(gotInputs)
		mu.Unlock()
		if got > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gotInputs) != 1 {
		t.Fatalf("expected 1 io.input, got %d", len(gotInputs))
	}
	if gotInputs[0].Content != "hello world" {
		t.Fatalf("expected content 'hello world', got %q", gotInputs[0].Content)
	}
}

// TestVAD_FinalForcesFlush sends a single chunk with Final=true, verifies the
// flush fires immediately (not waiting for silence_ms).
func TestVAD_FinalForcesFlush(t *testing.T) {
	asr := startASRServer(t, "final flushed")
	defer asr.Close()
	tts := startTTSServer(t, []byte("audio"))
	defer tts.Close()

	// Use a long silence_ms to prove final overrides it.
	_, bus := newPluginForTest(t, asr.URL, tts.URL, func(cfg map[string]any) {
		cfg["vad"].(map[string]any)["silence_ms"] = 60000
	})

	gotCh := make(chan events.UserInput, 1)
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		if u, ok := e.Payload.(events.UserInput); ok {
			select {
			case gotCh <- u:
			default:
			}
		}
	})

	if err := bus.Emit("voice.audio.input.chunk", events.VoiceAudioInputChunk{
		SchemaVersion: events.VoiceAudioInputChunkVersion,
		TurnID:        "t-final",
		AudioBase64:   makePCMChunk(0.5, 256),
		MimeType:      "audio/wav",
		Final:         true,
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	select {
	case got := <-gotCh:
		if got.Content != "final flushed" {
			t.Fatalf("got %q", got.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("io.input never emitted on Final=true within 2s")
	}
}

// --- TTS roundtrip ----------------------------------------------------------

func TestTTS_EmitsChunkedAudio(t *testing.T) {
	asr := startASRServer(t, "")
	defer asr.Close()
	// 40 bytes of fake MP3 — at chunk_bytes=16 we expect ceil(40/16)=3 chunks.
	want := []byte("FAKEMP3FAKEMP3FAKEMP3FAKEMP3FAKEMP3FAKEM") // 40 bytes
	tts := startTTSServer(t, want)
	defer tts.Close()

	_, bus := newPluginForTest(t, asr.URL, tts.URL, nil)

	var (
		mu     sync.Mutex
		chunks []events.VoiceAudioOutputChunk
	)
	done := make(chan struct{})
	bus.Subscribe("voice.audio.output.chunk", func(e engine.Event[any]) {
		ch, ok := e.Payload.(events.VoiceAudioOutputChunk)
		if !ok {
			return
		}
		mu.Lock()
		chunks = append(chunks, ch)
		final := ch.Final
		mu.Unlock()
		if final {
			select {
			case <-done:
			default:
				close(done)
			}
		}
	})

	if err := bus.Emit("llm.response", events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       "speak this",
		Metadata:      map[string]any{"turn_id": "t-tts"},
	}); err != nil {
		t.Fatalf("emit llm.response: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("never received final voice.audio.output.chunk")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks (40 bytes / 16), got %d", len(chunks))
	}
	if !chunks[len(chunks)-1].Final {
		t.Fatal("last chunk should have Final=true")
	}
	// Reassemble decoded audio and compare to the server's response body.
	var out []byte
	for _, c := range chunks {
		if c.AudioBase64 == "" {
			continue
		}
		dec, err := base64.StdEncoding.DecodeString(c.AudioBase64)
		if err != nil {
			t.Fatalf("decode chunk: %v", err)
		}
		out = append(out, dec...)
	}
	if string(out) != string(want) {
		t.Fatalf("reassembled audio mismatch:\n got=%q\nwant=%q", out, want)
	}
	for _, c := range chunks {
		if c.MimeType != "audio/mpeg" {
			t.Fatalf("expected mime audio/mpeg, got %q", c.MimeType)
		}
		if c.TurnID != "t-tts" {
			t.Fatalf("expected turn id t-tts, got %q", c.TurnID)
		}
	}
}

// --- Barge-in ---------------------------------------------------------------

func TestBargeIn_EmitsCancelDuringTTS(t *testing.T) {
	asr := startASRServer(t, "post barge")
	defer asr.Close()

	// TTS server: hold the connection open until we close `release`. That
	// simulates a long-running synthesis so the barge-in chunk arrives while
	// it's still in flight.
	release := make(chan struct{})
	ttsHandler := http.NewServeMux()
	ttsHandler.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte("audio"))
	})
	tts := httptest.NewServer(ttsHandler)
	defer tts.Close()
	defer close(release)

	_, bus := newPluginForTest(t, asr.URL, tts.URL, nil)

	cancelCh := make(chan events.CancelRequest, 1)
	bus.Subscribe("cancel.request", func(e engine.Event[any]) {
		if c, ok := e.Payload.(events.CancelRequest); ok {
			select {
			case cancelCh <- c:
			default:
			}
		}
	})

	// Kick off TTS: the handler holds the connection so ttsActive stays set.
	if err := bus.Emit("llm.response", events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       "agent reply",
		Metadata:      map[string]any{"turn_id": "agent-1"},
	}); err != nil {
		t.Fatalf("emit llm.response: %v", err)
	}

	// Wait briefly for runTTS to register itself.
	time.Sleep(50 * time.Millisecond)

	// Now send a speech-energy chunk that should fire barge-in.
	if err := bus.Emit("voice.audio.input.chunk", events.VoiceAudioInputChunk{
		SchemaVersion: events.VoiceAudioInputChunkVersion,
		TurnID:        "user-2",
		AudioBase64:   makePCMChunk(0.5, 256),
		MimeType:      "audio/wav",
	}); err != nil {
		t.Fatalf("emit chunk: %v", err)
	}

	select {
	case got := <-cancelCh:
		if got.Source != "voice" {
			t.Fatalf("expected Source=voice, got %q", got.Source)
		}
		if got.TurnID != "agent-1" {
			t.Fatalf("expected TurnID=agent-1, got %q", got.TurnID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel.request never emitted on barge-in")
	}
}

// TestBargeIn_DisabledNoCancel asserts that turning barge-in off prevents the
// cancel.request emission even when speech-level audio arrives mid-TTS.
func TestBargeIn_DisabledNoCancel(t *testing.T) {
	asr := startASRServer(t, "")
	defer asr.Close()

	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte("audio"))
	})
	tts := httptest.NewServer(mux)
	defer tts.Close()
	defer close(release)

	_, bus := newPluginForTest(t, asr.URL, tts.URL, func(cfg map[string]any) {
		cfg["barge_in"].(map[string]any)["enabled"] = false
	})

	cancelCh := make(chan events.CancelRequest, 1)
	bus.Subscribe("cancel.request", func(e engine.Event[any]) {
		if c, ok := e.Payload.(events.CancelRequest); ok {
			select {
			case cancelCh <- c:
			default:
			}
		}
	})

	_ = bus.Emit("llm.response", events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       "agent reply",
		Metadata:      map[string]any{"turn_id": "agent-2"},
	})
	time.Sleep(50 * time.Millisecond)
	_ = bus.Emit("voice.audio.input.chunk", events.VoiceAudioInputChunk{
		SchemaVersion: events.VoiceAudioInputChunkVersion,
		TurnID:        "user-3",
		AudioBase64:   makePCMChunk(0.5, 256),
		MimeType:      "audio/wav",
	})

	select {
	case got := <-cancelCh:
		t.Fatalf("did not expect cancel.request, got %+v", got)
	case <-time.After(300 * time.Millisecond):
		// good
	}
}

// --- local-provider rejection ----------------------------------------------

func TestInit_RejectsLocalASRProvider(t *testing.T) {
	bus := engine.NewEventBus()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New().(*Plugin)
	err := p.Init(engine.PluginContext{
		Bus:    bus,
		Logger: logger,
		Config: map[string]any{
			"asr": map[string]any{"provider": "local_whisper"},
		},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "#92") {
		t.Fatalf("expected error to mention #92, got %v", err)
	}
}

func TestInit_RejectsLocalTTSProvider(t *testing.T) {
	bus := engine.NewEventBus()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New().(*Plugin)
	err := p.Init(engine.PluginContext{
		Bus:    bus,
		Logger: logger,
		Config: map[string]any{
			"asr": map[string]any{"endpoint": "http://localhost", "api_key": "x"},
			"tts": map[string]any{"provider": "kokoro"},
		},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "#92") {
		t.Fatalf("expected error to mention #92, got %v", err)
	}
}

// TestInit_RejectsUnknownASRProvider asserts unknown values are not silently
// accepted.
func TestInit_RejectsUnknownASRProvider(t *testing.T) {
	bus := engine.NewEventBus()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New().(*Plugin)
	err := p.Init(engine.PluginContext{
		Bus:    bus,
		Logger: logger,
		Config: map[string]any{
			"asr": map[string]any{"provider": "deepgram"},
		},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), "#92") {
		t.Fatalf("did not expect #92 hint for unknown provider, got %v", err)
	}
}
