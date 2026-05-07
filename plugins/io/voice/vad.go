package voice

import (
	"encoding/binary"
	"math"
	"strings"
)

// computeRMS computes a normalized 0..1 RMS energy value for a chunk of audio
// bytes.
//
// For PCM container types ("audio/wav", "audio/pcm", "audio/l16") the bytes
// are interpreted as little-endian signed 16-bit samples and the result is
// the standard RMS divided by the int16 max (32767).
//
// For compressed containers (webm/opus, mpeg/mp3, etc.) the bytes are not
// PCM, so a true RMS is unavailable here. We fall back to a byte-level energy
// heuristic: average absolute deviation of each byte from 128 normalized to
// 0..1. This is approximate — it correlates loosely with audio energy because
// silence in compressed streams produces low-variance bytes — but it is good
// enough for a heuristic-grade barge-in / VAD signal until a proper decode is
// wired in. TODO(#91): decode webm/opus and mpeg before computing energy so
// VAD numbers match true loudness on browser MediaRecorder streams.
func computeRMS(audio []byte, mimeType string) float64 {
	if len(audio) == 0 {
		return 0
	}
	if isPCMMime(mimeType) {
		return rmsPCM16LE(audio)
	}
	return rmsByteHeuristic(audio)
}

// isPCMMime reports whether the given mime indicates raw 16-bit PCM-like
// content. WAV is included even though it has a header — the header is small
// relative to the body and the heuristic is robust to its inclusion.
func isPCMMime(mime string) bool {
	m := strings.ToLower(strings.TrimSpace(mime))
	if m == "" {
		return false
	}
	// Strip codec parameters: "audio/wav; codecs=1" -> "audio/wav".
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	switch m {
	case "audio/wav", "audio/x-wav", "audio/wave",
		"audio/pcm", "audio/l16", "audio/x-l16":
		return true
	}
	return false
}

// rmsPCM16LE treats audio as little-endian signed 16-bit samples.
func rmsPCM16LE(audio []byte) float64 {
	// Each sample is 2 bytes; if odd byte count, drop the trailing byte.
	n := len(audio) &^ 1
	if n == 0 {
		return 0
	}
	var sumSq float64
	count := 0
	for i := 0; i < n; i += 2 {
		s := int16(binary.LittleEndian.Uint16(audio[i : i+2]))
		f := float64(s) / 32768.0
		sumSq += f * f
		count++
	}
	if count == 0 {
		return 0
	}
	return math.Sqrt(sumSq / float64(count))
}

// rmsByteHeuristic returns the mean absolute deviation from byte midpoint
// (128) divided by 128 — a 0..1 energy proxy for opaque/compressed bytes.
func rmsByteHeuristic(audio []byte) float64 {
	var sum float64
	for _, b := range audio {
		d := float64(int(b) - 128)
		if d < 0 {
			d = -d
		}
		sum += d
	}
	return sum / float64(len(audio)) / 128.0
}
