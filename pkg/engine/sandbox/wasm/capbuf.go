package wasm

// capBuf is a stdout/stderr capture buffer that drops bytes past a configured
// cap and reports the overflow. Writers see a successful Write either way.
type capBuf struct {
	cap       int
	buf       []byte
	truncated bool
}

func newCapBuf(cap int) *capBuf {
	return &capBuf{cap: cap, buf: make([]byte, 0, 1024)}
}

func (b *capBuf) Write(p []byte) (int, error) {
	n := len(p)
	if b.cap <= 0 {
		b.buf = append(b.buf, p...)
		return n, nil
	}
	remaining := b.cap - len(b.buf)
	if remaining <= 0 {
		b.truncated = true
		return n, nil
	}
	if len(p) > remaining {
		b.buf = append(b.buf, p[:remaining]...)
		b.truncated = true
		return n, nil
	}
	b.buf = append(b.buf, p...)
	return n, nil
}

func (b *capBuf) Bytes() []byte { return b.buf }
func (b *capBuf) Len() int      { return len(b.buf) }
