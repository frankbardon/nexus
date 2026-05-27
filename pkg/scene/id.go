package scene

import (
	"crypto/rand"
	"encoding/hex"
)

// shortID returns a 12-character hex random ID used as the suffix of scene
// IDs. Sufficient entropy for per-session collision avoidance (the space is
// 2^48 and scene counts per session are tiny).
func shortID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
