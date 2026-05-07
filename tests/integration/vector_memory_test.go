//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestVectorMemory_Boot validates the memory.vector plugin activates
// alongside its required capabilities (embeddings.provider, vector.store)
// and emits no errors during a minimal two-input run.
func TestVectorMemory_Boot(t *testing.T) {
	h := testharness.New(t, "configs/test-vector-memory.yaml",
		testharness.WithTimeout(30*time.Second))
	h.Run()

	h.AssertBooted(
		"nexus.io.test",
		"nexus.agent.react",
		"nexus.memory.vector",
		"nexus.embeddings.mock",
		"nexus.vectorstore.chromem",
	)
	h.AssertEventEmitted("io.session.start")
	h.AssertEventEmitted("io.session.end")
	h.AssertEventCount("io.input", 2, 2)
	h.AssertNoSystemOutput()
}

// TestVectorMemory_EmitsEmbeddingsRequest verifies the recall path: on
// io.input the vector memory plugin emits embeddings.request to embed
// the user's query before searching the vector store.
func TestVectorMemory_EmitsEmbeddingsRequest(t *testing.T) {
	h := testharness.New(t, "configs/test-vector-memory.yaml",
		testharness.WithTimeout(30*time.Second))
	h.Run()

	h.AssertEventEmitted("embeddings.request")
}
