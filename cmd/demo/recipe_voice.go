package main

import (
	"context"
	_ "embed"
	"fmt"
)

//go:embed recipe-voice.yaml
var voiceRecipeConfig []byte

// runVoiceRecipe boots a minimal engine with io/voice + io/realtime
// active. The voice plugin advertises its WebSocket endpoint via the
// realtime transport; clients connect, push raw audio frames, and the
// voice plugin runs Voice Activity Detection + ASR (OpenAI Whisper)
// to derive io.input events, which then drive the agent loop. TTS
// streams responses back as audio.
//
// What this recipe demonstrates:
//
//   - io/voice: VAD + ASR + TTS pipeline. Default providers are
//     OpenAI Whisper (ASR) and OpenAI TTS-1 (alloy voice). Both reuse
//     OPENAI_API_KEY from env.
//   - io/realtime: low-latency WebSocket transport carrying audio
//     frames + token deltas. Listens on 127.0.0.1:8890 by default.
//   - barge-in handling: speak over the agent and the in-flight TTS
//     turn cancels (configurable threshold).
//
// Voice plugin v1 only supports cloud ASR/TTS providers. Local-model
// integrations (Whisper local, kokoro, etc.) are accepted by the
// schema but rejected at Init pending follow-up work; the recipe's
// YAML uses the cloud defaults.
//
// Without an audio client, the recipe still boots — the WebSocket
// listener is up; clients connect at ws://127.0.0.1:8890/ . Use the
// Wails desktop app's voice mode (when wired) or any custom WS client
// to drive it.
//
// Required env: OPENAI_API_KEY (for both Whisper + TTS),
// ANTHROPIC_API_KEY (for the agent's LLM responses).
func runVoiceRecipe(ctx context.Context, args []string) error {
	eng, err := bootRecipeEngine(ctx, voiceRecipeConfig)
	if err != nil {
		return err
	}
	defer func() {
		_ = eng.Stop(context.Background())
	}()

	if err := eng.StartSession(); err != nil {
		return fmt.Errorf("start session: %w", err)
	}

	fmt.Println("Recipe: voice")
	fmt.Println("  Realtime WS:  ws://127.0.0.1:8890/")
	fmt.Println("  ASR:          openai_whisper (model whisper-1)")
	fmt.Println("  TTS:          openai (model tts-1, voice alloy)")
	fmt.Println("  Barge-in:     enabled (RMS threshold 0.02)")
	fmt.Println()
	fmt.Println("Connect a voice client to the WS endpoint and speak.")
	fmt.Println("Press Ctrl-C to stop the engine.")

	select {
	case <-eng.SessionEnded():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
