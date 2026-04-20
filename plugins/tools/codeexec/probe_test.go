package codeexec

import (
	"testing"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

// TestProbe_YaegiPackages exercises each candidate stdlib package we plan to
// whitelist. Any failure here means the package is listed in yaegi/stdlib
// but does not actually compile/run under the current yaegi version — we
// must remove it from defaultAllowedStdlib.
//
// Kept as a regression guard so upgrading yaegi surfaces breakage.
func TestProbe_YaegiPackages(t *testing.T) {
	cases := map[string]string{
		"bytes": `package main
import "bytes"
func main() { var b bytes.Buffer; b.WriteString("x") }`,

		"regexp": `package main
import "regexp"
func main() { _ = regexp.MustCompile("abc") }`,

		"encoding/base64": `package main
import "encoding/base64"
func main() { _ = base64.StdEncoding.EncodeToString([]byte("x")) }`,

		"encoding/hex": `package main
import "encoding/hex"
func main() { _ = hex.EncodeToString([]byte{1, 2}) }`,

		"encoding/csv": `package main
import (
	"encoding/csv"
	"strings"
)
func main() { _ = csv.NewReader(strings.NewReader("a,b")) }`,

		"encoding/xml": `package main
import "encoding/xml"
func main() { _, _ = xml.Marshal(map[string]string{"k":"v"}) }`,

		"encoding/binary": `package main
import "encoding/binary"
func main() { _ = binary.LittleEndian }`,

		"encoding/pem": `package main
import "encoding/pem"
func main() { _, _ = pem.Decode(nil) }`,

		"crypto/sha256": `package main
import "crypto/sha256"
func main() { _ = sha256.Sum256([]byte("x")) }`,

		"crypto/sha512": `package main
import "crypto/sha512"
func main() { _ = sha512.Sum512([]byte("x")) }`,

		"crypto/sha1": `package main
import "crypto/sha1"
func main() { _ = sha1.Sum([]byte("x")) }`,

		"crypto/md5": `package main
import "crypto/md5"
func main() { _ = md5.Sum([]byte("x")) }`,

		"crypto/hmac": `package main
import (
	"crypto/hmac"
	"crypto/sha256"
)
func main() { _ = hmac.New(sha256.New, []byte("k")) }`,

		"crypto/rand": `package main
import "crypto/rand"
func main() {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
}`,

		"crypto/subtle": `package main
import "crypto/subtle"
func main() { _ = subtle.ConstantTimeCompare([]byte("a"), []byte("a")) }`,

		"math/big": `package main
import "math/big"
func main() { _ = big.NewInt(1) }`,

		"math/rand": `package main
import "math/rand"
func main() { _ = rand.New(rand.NewSource(1)) }`,

		"math/rand/v2": `package main
import "math/rand/v2"
func main() { _ = rand.Int() }`,

		"math/bits": `package main
import "math/bits"
func main() { _ = bits.LeadingZeros64(0) }`,

		"bufio": `package main
import (
	"bufio"
	"bytes"
)
func main() { _ = bufio.NewWriter(&bytes.Buffer{}) }`,

		"io": `package main
import (
	"bytes"
	"io"
)
func main() { _, _ = io.ReadAll(&bytes.Buffer{}) }`,

		"hash": `package main
import (
	"crypto/sha256"
	"hash"
)
func main() {
	var _ hash.Hash = sha256.New()
}`,

		"hash/crc32": `package main
import "hash/crc32"
func main() { _ = crc32.ChecksumIEEE([]byte("x")) }`,

		"hash/crc64": `package main
import "hash/crc64"
func main() { _ = crc64.MakeTable(crc64.ISO) }`,

		"hash/fnv": `package main
import "hash/fnv"
func main() { _ = fnv.New64() }`,

		"hash/adler32": `package main
import "hash/adler32"
func main() { _ = adler32.Checksum([]byte("x")) }`,

		"container/heap": `package main
import "container/heap"
func main() { var _ = heap.Init }`,

		"container/list": `package main
import "container/list"
func main() { _ = list.New() }`,

		"container/ring": `package main
import "container/ring"
func main() { _ = ring.New(3) }`,

		"unicode": `package main
import "unicode"
func main() { _ = unicode.IsDigit('1') }`,

		"unicode/utf8": `package main
import "unicode/utf8"
func main() { _ = utf8.RuneCountInString("abc") }`,

		"unicode/utf16": `package main
import "unicode/utf16"
func main() { _ = utf16.Encode([]rune{'a'}) }`,
	}

	var broken []string
	for name, script := range cases {
		name, script := name, script
		t.Run(name, func(t *testing.T) {
			i := interp.New(interp.Options{})
			if err := i.Use(stdlib.Symbols); err != nil {
				t.Fatalf("stdlib use: %v", err)
			}
			if _, err := i.Eval(script); err != nil {
				t.Logf("package %s unusable in yaegi: %v", name, err)
				broken = append(broken, name+": "+err.Error())
				t.Fail()
			}
		})
	}
	if len(broken) > 0 {
		t.Logf("Yaegi-incompatible packages (remove from defaultAllowedStdlib):")
		for _, b := range broken {
			t.Logf("  %s", b)
		}
	}
}
