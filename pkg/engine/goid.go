package engine

import (
	"runtime"
	"strconv"
	"strings"
)

// goroutineID parses the calling goroutine's ID from runtime.Stack. The
// runtime exposes no public accessor; the standard parse-the-first-line
// trick is the same one widely-used logging libraries rely on.
//
// Cost: ~1µs on modern hardware. The bus calls this twice per dispatched
// event (push + pop the per-goroutine seq stack) which is dwarfed by JSON
// marshaling and disk I/O in the journal write path.
func goroutineID() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	s := string(buf[:n])
	const prefix = "goroutine "
	if !strings.HasPrefix(s, prefix) {
		return 0
	}
	s = s[len(prefix):]
	if i := strings.IndexByte(s, ' '); i > 0 {
		s = s[:i]
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return id
}
