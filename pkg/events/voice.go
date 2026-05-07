package events

// Schema-version constants for voice.* payloads. See doc.go.
//
// Voice events are forward-looking placeholders for Phase 4 of the
// multimodal/voice IO work tracked under Idea 18. The realtime IO transport
// (nexus.io.realtime) carries them between connected clients and the bus
// today; nexus.io.voice will consume input chunks (run VAD + ASR) and
// produce output chunks (TTS) once that plugin lands.
const (
	VoiceAudioInputChunkVersion  = 1
	VoiceAudioOutputChunkVersion = 1
)

// VoiceAudioInputChunk carries a chunk of microphone-captured audio from a
// realtime client. Phase 4 (nexus.io.voice) consumes these, runs voice-
// activity detection, and hands a final segment to ASR. The audio frame is
// base64-encoded so the chunk can ride a JSON envelope without any binary
// framing concerns; MIME type is supplied by the producer (typically
// "audio/webm;codecs=opus" from a browser MediaRecorder, or "audio/wav"
// from a native client).
//
// Bus event type: "voice.audio.input.chunk".
type VoiceAudioInputChunk struct {
	SchemaVersion int `json:"_schema_version"`

	// TurnID groups consecutive chunks belonging to the same utterance.
	TurnID string
	// Sequence is the 0-based ordinal of this chunk within the turn.
	Sequence int
	// AudioBase64 is the base64-encoded raw audio frame. Decoders should
	// trust MimeType for the container/codec; this field is opaque.
	AudioBase64 string
	// MimeType identifies the container/codec, e.g. "audio/webm;codecs=opus".
	MimeType string
	// Final marks the last chunk of a turn. Consumers should treat any
	// chunks emitted after Final=true as the start of a new turn.
	Final bool
}

// VoiceAudioOutputChunk is emitted by Phase 4 (nexus.io.voice) when a TTS
// stream produces audio for a given assistant response. The realtime IO
// plugin forwards these to connected clients for playback.
//
// Bus event type: "voice.audio.output.chunk".
type VoiceAudioOutputChunk struct {
	SchemaVersion int `json:"_schema_version"`

	// TurnID correlates the output with the assistant turn it speaks.
	TurnID string
	// Sequence is the 0-based ordinal of this chunk within the turn.
	Sequence int
	// AudioBase64 is the base64-encoded raw audio frame.
	AudioBase64 string
	// MimeType identifies the container/codec the producer chose.
	MimeType string
	// Final marks the last chunk of a turn.
	Final bool
}
