// Package voice provides the nexus.io.voice plugin: a bus-driven voice IO
// transport that consumes microphone audio chunks (voice.audio.input.chunk),
// runs simple energy-based VAD + ASR (OpenAI Whisper API) to emit io.input,
// and consumes llm.response to drive TTS (OpenAI /audio/speech) chunks back
// out (voice.audio.output.chunk).
//
// Local-model providers are out of scope for this PR — see #92. Configuring
// asr.provider or tts.provider with a local-model id (e.g. local_whisper,
// faster_whisper, kokoro) makes Init return a clear error pointing operators
// at #92.
//
// Barge-in: while a TTS turn is in flight (i.e. between llm.response and the
// final voice.audio.output.chunk), if any input chunk's RMS exceeds the
// barge-in threshold, we emit cancel.request{Source: "voice"} so the
// existing cooperative-cancellation chain terminates the in-flight LLM/turn.
// The new utterance is then processed normally.
package voice

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.io.voice"

// asrConfig holds resolved ASR settings.
type asrConfig struct {
	provider string
	apiKey   string
	model    string
	endpoint string
}

// ttsConfig holds resolved TTS settings.
type ttsConfig struct {
	provider   string
	apiKey     string
	model      string
	voice      string
	endpoint   string
	chunkBytes int
}

// vadConfig holds resolved VAD settings.
type vadConfig struct {
	threshold float64
	silenceMS int
}

// bargeInConfig holds resolved barge-in settings.
type bargeInConfig struct {
	enabled   bool
	threshold float64
}

// turnBuffer accumulates audio chunks for a single utterance.
type turnBuffer struct {
	turnID         string
	mimeType       string
	audio          []byte
	lastChunkAt    time.Time
	lastSpeechAt   time.Time
	hasSpeech      bool
	flushed        bool
	flushTimer     *time.Timer
	expectedSeqNxt int // next expected sequence number; -1 = unknown
}

// Plugin implements engine.Plugin for nexus.io.voice.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace
	unsubs  []func()

	// Config (resolved at Init).
	asr      asrConfig
	tts      ttsConfig
	vad      vadConfig
	bargeIn  bargeInConfig
	textPass bool

	// Test/HTTP injection.
	httpClient *http.Client

	// State.
	mu      sync.Mutex
	buffers map[string]*turnBuffer // turnID -> buffer
	// ttsActive tracks the turnID of the in-flight TTS operation. Empty
	// when no TTS is currently in flight. Set when llm.response handler
	// kicks off synthesis; cleared when the final output chunk has been
	// emitted (or the operation is cancelled).
	ttsActive    string
	ttsCancel    context.CancelFunc
	bargeInFired bool
}

// New returns a new voice IO plugin instance.
func New() engine.Plugin {
	return &Plugin{
		buffers: make(map[string]*turnBuffer),
	}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "Voice IO" }
func (p *Plugin) Version() string                   { return "0.1.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "voice.audio.input.chunk", Priority: 50},
		{EventType: "llm.response", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"io.input",
		"voice.audio.output.chunk",
		"cancel.request",
	}
}

// Init parses config and wires bus subscriptions.
func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session

	if err := p.parseConfig(ctx.Config); err != nil {
		return err
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("voice.audio.input.chunk", p.handleInputChunk, engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.response", p.handleLLMResponse, engine.WithSource(pluginID)),
	)

	p.logger.Info("voice IO plugin initialized",
		"asr_provider", p.asr.provider,
		"asr_model", p.asr.model,
		"tts_provider", p.tts.provider,
		"tts_model", p.tts.model,
		"tts_voice", p.tts.voice,
		"vad_threshold", p.vad.threshold,
		"vad_silence_ms", p.vad.silenceMS,
		"barge_in_enabled", p.bargeIn.enabled,
	)
	return nil
}

func (p *Plugin) Ready() error { return nil }

// Shutdown cancels any in-flight TTS and unsubscribes.
func (p *Plugin) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	if p.ttsCancel != nil {
		p.ttsCancel()
		p.ttsCancel = nil
	}
	for _, b := range p.buffers {
		if b.flushTimer != nil {
			b.flushTimer.Stop()
		}
	}
	p.buffers = nil
	p.mu.Unlock()

	for _, unsub := range p.unsubs {
		unsub()
	}
	return nil
}

// parseConfig resolves the plugin config map into typed structs and validates
// the provider settings (incl. the #92 local-model rejection).
func (p *Plugin) parseConfig(cfg map[string]any) error {
	// ASR.
	asr, _ := cfg["asr"].(map[string]any)
	if asr == nil {
		asr = map[string]any{}
	}
	p.asr.provider = stringOrDefault(asr, "provider", "openai_whisper")
	if err := rejectLocalASR(p.asr.provider); err != nil {
		return err
	}
	p.asr.model = stringOrDefault(asr, "model", "whisper-1")
	p.asr.endpoint = stringOrDefault(asr, "endpoint", defaultASREndpoint)
	p.asr.apiKey = resolveAPIKey(asr, "OPENAI_API_KEY")
	if p.asr.apiKey == "" && !isTestEndpoint(p.asr.endpoint) {
		return fmt.Errorf("voice: ASR API key not found (set asr.api_key, asr.api_key_env, or OPENAI_API_KEY)")
	}

	// TTS.
	tts, _ := cfg["tts"].(map[string]any)
	if tts == nil {
		tts = map[string]any{}
	}
	p.tts.provider = stringOrDefault(tts, "provider", "openai")
	if err := rejectLocalTTS(p.tts.provider); err != nil {
		return err
	}
	p.tts.model = stringOrDefault(tts, "model", "tts-1")
	p.tts.voice = stringOrDefault(tts, "voice", "alloy")
	p.tts.endpoint = stringOrDefault(tts, "endpoint", defaultTTSEndpoint)
	p.tts.apiKey = resolveAPIKey(tts, "OPENAI_API_KEY")
	if p.tts.apiKey == "" && !isTestEndpoint(p.tts.endpoint) {
		return fmt.Errorf("voice: TTS API key not found (set tts.api_key, tts.api_key_env, or OPENAI_API_KEY)")
	}
	p.tts.chunkBytes = intOrDefault(tts, "chunk_bytes", defaultTTSChunkBytes)
	if p.tts.chunkBytes <= 0 {
		p.tts.chunkBytes = defaultTTSChunkBytes
	}

	// VAD.
	vad, _ := cfg["vad"].(map[string]any)
	if vad == nil {
		vad = map[string]any{}
	}
	p.vad.threshold = floatOrDefault(vad, "threshold", 0.02)
	p.vad.silenceMS = intOrDefault(vad, "silence_ms", 600)

	// Barge-in.
	bi, _ := cfg["barge_in"].(map[string]any)
	if bi == nil {
		bi = map[string]any{}
	}
	p.bargeIn.enabled = boolOrDefault(bi, "enabled", true)
	p.bargeIn.threshold = floatOrDefault(bi, "threshold", p.vad.threshold)

	// Text fallback.
	p.textPass = boolOrDefault(cfg, "text_fallback", true)

	return nil
}

// rejectLocalASR returns the #92 error when the configured provider is one of
// the known local-model ids. Schema accepts these strings so configs migrate
// once #92 lands; Init refuses to run with them today.
func rejectLocalASR(provider string) error {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "local_whisper", "faster_whisper", "distil_whisper":
		return errors.New("local-model voice providers tracked under #92; not available in this PR — use openai_whisper / openai for ASR/TTS")
	case "openai_whisper", "":
		return nil
	default:
		return fmt.Errorf("voice: unsupported asr.provider %q (only openai_whisper is wired in this PR)", provider)
	}
}

// rejectLocalTTS mirrors rejectLocalASR for TTS providers.
func rejectLocalTTS(provider string) error {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "kokoro" || strings.HasPrefix(p, "local_") || strings.HasSuffix(p, "_local") {
		return errors.New("local-model voice providers tracked under #92; not available in this PR — use openai_whisper / openai for ASR/TTS")
	}
	switch p {
	case "openai", "":
		return nil
	default:
		return fmt.Errorf("voice: unsupported tts.provider %q (only openai is wired in this PR)", provider)
	}
}

// resolveAPIKey reads api_key (literal) then api_key_env (env var) from the
// supplied sub-config; falls back to the supplied default env var when
// api_key_env is unset.
func resolveAPIKey(cfg map[string]any, defaultEnv string) string {
	if v, ok := cfg["api_key"].(string); ok && v != "" {
		return v
	}
	envVar, _ := cfg["api_key_env"].(string)
	if envVar == "" {
		envVar = defaultEnv
	}
	return os.Getenv(envVar)
}

// isTestEndpoint reports whether the configured endpoint points at an
// httptest server (loopback). Used to relax the api-key requirement so unit
// tests don't have to set OPENAI_API_KEY just to exercise plumbing.
func isTestEndpoint(url string) bool {
	return strings.Contains(url, "127.0.0.1") || strings.Contains(url, "localhost")
}

// -- handlers ---------------------------------------------------------------

func (p *Plugin) handleInputChunk(e engine.Event[any]) {
	chunk, ok := e.Payload.(events.VoiceAudioInputChunk)
	if !ok {
		// Allow pointer payloads too in case future emitters use *VoiceAudioInputChunk.
		if ptr, ok := e.Payload.(*events.VoiceAudioInputChunk); ok && ptr != nil {
			chunk = *ptr
		} else {
			return
		}
	}

	audio, err := base64.StdEncoding.DecodeString(chunk.AudioBase64)
	if err != nil {
		p.logger.Warn("voice: failed to decode input chunk audio",
			"turn_id", chunk.TurnID, "error", err)
		return
	}

	rms := computeRMS(audio, chunk.MimeType)

	// Barge-in: if a TTS is in flight and the new chunk shows speech-level
	// energy, cancel the active turn before processing.
	p.maybeBargeIn(rms)

	p.appendChunk(chunk, audio, rms)
}

// maybeBargeIn emits cancel.request{Source: "voice"} when the configured
// barge-in policy fires. Only fires once per active TTS turn.
func (p *Plugin) maybeBargeIn(rms float64) {
	if !p.bargeIn.enabled {
		return
	}
	p.mu.Lock()
	active := p.ttsActive
	already := p.bargeInFired
	if active == "" || already {
		p.mu.Unlock()
		return
	}
	if rms < p.bargeIn.threshold {
		p.mu.Unlock()
		return
	}
	p.bargeInFired = true
	cancel := p.ttsCancel
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	p.logger.Info("voice: barge-in detected; cancelling active TTS", "turn_id", active, "rms", rms)
	_ = p.bus.Emit("cancel.request", events.CancelRequest{
		SchemaVersion: events.CancelRequestVersion,
		TurnID:        active,
		Source:        "voice",
	})
}

// appendChunk merges a chunk into the per-turn buffer, updates speech state,
// and either schedules a silence-flush timer or force-flushes immediately
// when chunk.Final is set.
func (p *Plugin) appendChunk(chunk events.VoiceAudioInputChunk, audio []byte, rms float64) {
	p.mu.Lock()

	buf, ok := p.buffers[chunk.TurnID]
	if !ok {
		buf = &turnBuffer{turnID: chunk.TurnID, mimeType: chunk.MimeType}
		p.buffers[chunk.TurnID] = buf
	}
	if buf.flushed {
		// Late chunks for an already-flushed turn — start a new buffer
		// under the same turn id (treating Final=true as "end of utterance",
		// not "end of session"). This matches the realtime IO contract that
		// chunks emitted after Final=true are a new turn.
		buf = &turnBuffer{turnID: chunk.TurnID, mimeType: chunk.MimeType}
		p.buffers[chunk.TurnID] = buf
	}
	if buf.mimeType == "" {
		buf.mimeType = chunk.MimeType
	}
	buf.audio = append(buf.audio, audio...)
	now := time.Now()
	buf.lastChunkAt = now
	speaking := rms >= p.vad.threshold
	if speaking {
		buf.hasSpeech = true
		buf.lastSpeechAt = now
	}

	// Cancel any pending silence-flush timer; we'll re-arm below.
	if buf.flushTimer != nil {
		buf.flushTimer.Stop()
		buf.flushTimer = nil
	}

	if chunk.Final {
		// Mark flushed inline so a concurrent timer can't double-fire.
		buf.flushed = true
		audioCopy := append([]byte(nil), buf.audio...)
		mime := buf.mimeType
		hasSpeech := buf.hasSpeech || rms >= p.vad.threshold
		p.mu.Unlock()
		go p.flush(chunk.TurnID, audioCopy, mime, hasSpeech)
		return
	}

	if buf.hasSpeech {
		// Arm a silence flush. Will fire after silence_ms of no further chunks.
		turnID := chunk.TurnID
		buf.flushTimer = time.AfterFunc(time.Duration(p.vad.silenceMS)*time.Millisecond, func() {
			p.silenceFlush(turnID)
		})
	}
	p.mu.Unlock()
}

// silenceFlush is invoked when the silence timer fires for the given turn.
// It pulls the buffered audio and runs ASR.
func (p *Plugin) silenceFlush(turnID string) {
	p.mu.Lock()
	buf, ok := p.buffers[turnID]
	if !ok || buf.flushed || !buf.hasSpeech {
		p.mu.Unlock()
		return
	}
	buf.flushed = true
	audio := append([]byte(nil), buf.audio...)
	mime := buf.mimeType
	p.mu.Unlock()
	p.flush(turnID, audio, mime, true)
}

// flush runs ASR on the buffered audio and emits io.input with the result.
// Empty utterances and transcribe failures are logged but not fatal.
func (p *Plugin) flush(turnID string, audio []byte, mime string, hasSpeech bool) {
	if !hasSpeech || len(audio) == 0 {
		return
	}

	client := &asrClient{
		endpoint: p.asr.endpoint,
		apiKey:   p.asr.apiKey,
		model:    p.asr.model,
		http:     p.httpClient,
	}

	// Use a fresh context per ASR call. Bound it to a generous timeout so a
	// hung server doesn't pin the goroutine forever.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	text, err := client.transcribe(ctx, audio, mime)
	if err != nil {
		p.logger.Error("voice: ASR transcribe failed",
			"turn_id", turnID, "error", err)
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		p.logger.Debug("voice: ASR returned empty text; suppressing io.input", "turn_id", turnID)
		return
	}

	sessionID := ""
	if p.session != nil {
		sessionID = p.session.ID
	}
	input := events.UserInput{
		SchemaVersion: events.UserInputVersion,
		Content:       text,
		SessionID:     sessionID,
	}
	if veto, err := p.bus.EmitVetoable("before:io.input", &input); err == nil && veto.Vetoed {
		return
	}
	_ = p.bus.Emit("io.input", input)
}

// -- TTS ---------------------------------------------------------------------

func (p *Plugin) handleLLMResponse(e engine.Event[any]) {
	resp, ok := e.Payload.(events.LLMResponse)
	if !ok {
		if ptr, ok := e.Payload.(*events.LLMResponse); ok && ptr != nil {
			resp = *ptr
		} else {
			return
		}
	}
	text := strings.TrimSpace(resp.Content)
	if text == "" {
		return
	}

	turnID := ""
	if resp.Metadata != nil {
		if tid, ok := resp.Metadata["turn_id"].(string); ok {
			turnID = tid
		}
	}
	if turnID == "" {
		turnID = engine.GenerateID()
	}

	go p.runTTS(turnID, text)
}

// runTTS performs the TTS HTTP call and emits chunked audio events.
func (p *Plugin) runTTS(turnID, text string) {
	ctx, cancel := context.WithCancel(context.Background())

	p.mu.Lock()
	// If a previous TTS is somehow still active for the same turn, cancel it.
	if p.ttsCancel != nil {
		p.ttsCancel()
	}
	p.ttsActive = turnID
	p.ttsCancel = cancel
	p.bargeInFired = false
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		if p.ttsActive == turnID {
			p.ttsActive = ""
			p.ttsCancel = nil
		}
		p.mu.Unlock()
		cancel()
	}()

	client := &ttsClient{
		endpoint: p.tts.endpoint,
		apiKey:   p.tts.apiKey,
		model:    p.tts.model,
		voice:    p.tts.voice,
		http:     p.httpClient,
	}

	data, mime, err := client.synthesize(ctx, text)
	if err != nil {
		// Cancellation is the expected path for barge-in; degrade quietly.
		if errors.Is(ctx.Err(), context.Canceled) {
			p.logger.Debug("voice: TTS cancelled", "turn_id", turnID)
			return
		}
		p.logger.Error("voice: TTS synthesize failed", "turn_id", turnID, "error", err)
		return
	}
	if mime == "" {
		mime = "audio/mpeg"
	}

	chunkSize := p.tts.chunkBytes
	if chunkSize <= 0 {
		chunkSize = defaultTTSChunkBytes
	}

	total := len(data)
	if total == 0 {
		// Emit a single final chunk so consumers see a clean turn boundary.
		_ = p.bus.Emit("voice.audio.output.chunk", events.VoiceAudioOutputChunk{
			SchemaVersion: events.VoiceAudioOutputChunkVersion,
			TurnID:        turnID,
			Sequence:      0,
			MimeType:      mime,
			Final:         true,
		})
		return
	}

	seq := 0
	for off := 0; off < total; off += chunkSize {
		end := off + chunkSize
		if end > total {
			end = total
		}
		final := end == total
		// Honor barge-in / shutdown cancel between frames so a long MP3
		// doesn't keep flooding the bus after the user has interrupted.
		if ctx.Err() != nil {
			return
		}
		_ = p.bus.Emit("voice.audio.output.chunk", events.VoiceAudioOutputChunk{
			SchemaVersion: events.VoiceAudioOutputChunkVersion,
			TurnID:        turnID,
			Sequence:      seq,
			AudioBase64:   base64.StdEncoding.EncodeToString(data[off:end]),
			MimeType:      mime,
			Final:         final,
		})
		seq++
	}
}

// -- config helpers ----------------------------------------------------------

func stringOrDefault(m map[string]any, key, def string) string {
	if v, ok := m[key].(string); ok && v != "" {
		return v
	}
	return def
}

func intOrDefault(m map[string]any, key string, def int) int {
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return def
}

func floatOrDefault(m map[string]any, key string, def float64) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return def
}

func boolOrDefault(m map[string]any, key string, def bool) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return def
}
