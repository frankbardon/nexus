package engine

import (
	"fmt"
	"strings"
)

// XMLTag writes an opening XML tag with optional attributes to a strings.Builder.
// Attributes are written as key="value" pairs.
func XMLTag(b *strings.Builder, tag string, attrs ...string) {
	b.WriteByte('<')
	b.WriteString(tag)
	for i := 0; i+1 < len(attrs); i += 2 {
		fmt.Fprintf(b, " %s=%q", attrs[i], attrs[i+1])
	}
	b.WriteString(">\n")
}

// XMLClose writes a closing XML tag to a strings.Builder.
func XMLClose(b *strings.Builder, tag string) {
	b.WriteString("</")
	b.WriteString(tag)
	b.WriteString(">\n")
}

// XMLWrap wraps content in an XML element with optional attributes.
// Returns empty string if content is empty.
func XMLWrap(tag, content string, attrs ...string) string {
	if content == "" {
		return ""
	}
	var b strings.Builder
	XMLTag(&b, tag, attrs...)
	b.WriteString(content)
	if !strings.HasSuffix(content, "\n") {
		b.WriteByte('\n')
	}
	XMLClose(&b, tag)
	return b.String()
}

// XMLCDATA wraps content in a CDATA section. Use for content that may contain
// characters that conflict with XML parsing (e.g. user input, LLM output).
func XMLCDATA(content string) string {
	// CDATA cannot contain the closing sequence "]]>". If present, split it.
	content = strings.ReplaceAll(content, "]]>", "]]]]><![CDATA[>")
	return "<![CDATA[" + content + "]]>"
}

// XMLEscape escapes XML special characters in a string.
func XMLEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
