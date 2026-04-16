package matcher

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

// minExtractedChars is the threshold below which we treat a PDF's
// extracted text as "effectively empty" and return an error. Image-
// only PDFs and scanned-document PDFs commonly extract to a few
// dozen characters of metadata-ish garbage; real job descriptions
// are well above this. Tuned from the expected minimum for a job
// description ("Senior engineer, Go, remote") which is ~30 chars
// — so 50 is safely above the floor without being too permissive.
const minExtractedChars = 50

// maxExtractedChars caps the extracted text length to prevent a
// surprise 200-page PDF from producing a multi-megabyte prompt
// that blows past Sonnet's context window. Sonnet 4.6 has a large
// context, but the matcher prompt is small and the candidate pool
// is small — if a job description is above ~20 KB there is almost
// certainly formatting garbage or the wrong document was attached.
// This is a soft limit: we truncate with a marker rather than
// erroring, so a user whose real job description exceeds the cap
// still gets a partial match instead of a hard failure.
const maxExtractedChars = 20_000

// ErrPDFEmpty is returned when a PDF is successfully parsed but
// produces no meaningful text. The most common cause is an
// image-only PDF (scanned document) or a PDF where the text is
// embedded as vector paths rather than actual characters. The
// error message is intentionally user-friendly because it will
// surface directly in the staffing-match UI.
var ErrPDFEmpty = errors.New("PDF has no extractable text (image-only or scanned document)")

// extractPDFText reads the given PDF file and returns its plain
// text content. It is a thin wrapper around ledongthuc/pdf's
// GetPlainText with three pieces of added behavior:
//
//  1. Sentinel error (ErrPDFEmpty) when extraction produces too
//     little content to be a real job description. This is the
//     single most common PDF failure mode — scanned/image-only
//     PDFs — and the caller needs a clean signal to surface a
//     meaningful error to the user.
//
//  2. UTF-8 validation. The underlying library does not
//     guarantee valid UTF-8 on all inputs (encrypted or
//     malformed PDFs can produce raw bytes). We replace invalid
//     sequences rather than erroring because the cost of
//     dropping invalid bytes is much lower than the cost of
//     refusing to process a PDF that is 99% clean.
//
//  3. Soft truncation at maxExtractedChars. Prevents
//     pathologically long PDFs from blowing up the prompt.
//
// The returned text is not otherwise normalized: page breaks,
// whitespace, and line endings are whatever the PDF encoded.
// Downstream the LLM is the ranker and it handles messy text
// better than any pre-processing we could do here.
func extractPDFText(path string) (string, error) {
	f, r, err := pdf.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening PDF: %w", err)
	}
	defer f.Close()

	textReader, err := r.GetPlainText()
	if err != nil {
		return "", fmt.Errorf("extracting text: %w", err)
	}

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(textReader); err != nil {
		return "", fmt.Errorf("reading extracted text: %w", err)
	}

	text := buf.String()

	// UTF-8 validation: replace any invalid byte sequences with
	// the Unicode replacement character. ledongthuc/pdf can emit
	// non-UTF-8 bytes on encrypted or malformed PDFs, and the
	// Anthropic API will reject the entire request if we send it
	// invalid UTF-8 downstream.
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "\uFFFD")
	}

	// Count meaningful (non-whitespace) characters for the
	// image-only detection threshold. A PDF with a lot of page
	// markers and whitespace but no real content is still
	// effectively empty.
	meaningfulChars := 0
	for _, r := range text {
		if !isWhitespace(r) {
			meaningfulChars++
		}
	}
	if meaningfulChars < minExtractedChars {
		return "", ErrPDFEmpty
	}

	// Soft truncate. Use the rune count, not the byte count, so
	// we do not cut in the middle of a multi-byte character.
	if utf8.RuneCountInString(text) > maxExtractedChars {
		runes := []rune(text)
		text = string(runes[:maxExtractedChars]) + "\n\n[truncated]"
	}

	return text, nil
}

// isWhitespace returns true for characters that should not count
// toward the image-only detection threshold. Deliberately does
// NOT use unicode.IsSpace because we want to count form feeds
// and other page-break markers as whitespace too — a PDF full of
// page breaks is still empty.
func isWhitespace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	}
	return false
}
