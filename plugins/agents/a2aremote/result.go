package a2aremote

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/a2a/a2aclient"
	"github.com/frankbardon/nexus/pkg/engine"
)

// maxPartBytes bounds one rendered part. A remote is under no obligation to be
// terse, and an artifact that is a megabyte of JSON would displace the caller's
// entire conversation from its context window. Truncation is announced in the
// element's attributes so the model can tell a cut-off document from a complete
// one and ask for the rest rather than reasoning over a fragment it believes is
// whole.
const maxPartBytes = 16 * 1024

// outcome is one delegated call's terminal result, in the two forms the tool
// layer needs: the folded text handed to the calling model, and the clean error
// string that makes the tool result a failure. Both may be set — a task that
// failed part way still produced whatever it produced, and withholding that
// from the model would make the failure less actionable, not more.
type outcome struct {
	// output is the XML-folded result document. See fold.
	output string
	// err is empty on success. When set it is the tool.result Error: a
	// sentence the calling model can act on, never a Go error dump.
	err string

	// taskID and contextID identify the remote task, empty for a
	// message-only exchange that produced none.
	taskID    string
	contextID string
	// state is the terminal task state observed, empty for a message reply.
	state a2a.TaskState
}

// failed reports whether the outcome carries an error, which is what keeps it
// out of the result cache.
func (o outcome) failed() bool { return o.err != "" }

// remoteResult is the binding-independent view of what a remote answered with.
// Both the streaming and the blocking path normalize into it so the folding
// below has exactly one input shape.
type remoteResult struct {
	taskID    string
	contextID string
	state     a2a.TaskState

	// reply is the direct agent message text, for an exchange that produced no
	// task.
	reply string
	// statusText is the text of the message attached to the terminal status:
	// a completion note, a failure explanation, or an INPUT_REQUIRED question.
	statusText string
	// artifacts are the task's outputs, chunk-reassembled.
	artifacts []a2a.Artifact
}

// fromStream normalizes a streaming call's accumulated result.
func fromStream(res a2aclient.StreamResult) remoteResult {
	out := remoteResult{
		taskID:     res.TaskID,
		contextID:  res.ContextID,
		state:      res.State,
		statusText: res.StatusText(),
		artifacts:  res.Artifacts,
	}
	if res.Message != nil {
		out.reply = messageText(res.Message)
	}
	return out
}

// fromSendMessage normalizes a blocking call's single response.
func fromSendMessage(resp *a2a.SendMessageResponse) remoteResult {
	var out remoteResult
	if resp == nil {
		return out
	}
	if resp.Message != nil {
		out.reply = messageText(resp.Message)
		out.contextID = resp.Message.ContextID
		out.taskID = resp.Message.TaskID
		return out
	}
	if resp.Task == nil {
		return out
	}
	task := resp.Task
	out.taskID = task.ID
	out.contextID = task.ContextID
	out.state = task.Status.State
	out.artifacts = task.Artifacts
	if task.Status.Message != nil {
		out.statusText = messageText(task.Status.Message)
	}
	return out
}

// messageText concatenates a message's text parts.
func messageText(m *a2a.Message) string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	for _, part := range m.Parts {
		text, ok := part.TextValue()
		if !ok {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(text)
	}
	return b.String()
}

// fold renders a remote's answer as the tool output the calling model reads.
//
// A2A splits an answer across two places — the terminal status message and the
// task's artifacts (specification section 3.7) — and a remote is free to put
// its whole answer in either. Concatenating them would produce an undelimited
// blob in which the model cannot tell the agent's summary from a tool's raw
// output, or one artifact from the next. So the fold is XML-tagged, per the
// house convention for anything injected into a prompt: every section is a
// named element, every artifact is its own element carrying its identity, and
// remote-authored text rides inside CDATA so a remote that emits angle brackets
// cannot break the framing the model is reading.
func fold(agentName string, r remoteResult) string {
	var b strings.Builder

	attrs := []string{"name", agentName}
	if r.state != "" {
		attrs = append(attrs, "state", string(r.state))
	}
	if r.taskID != "" {
		attrs = append(attrs, "task_id", r.taskID)
	}
	if r.contextID != "" {
		attrs = append(attrs, "context_id", r.contextID)
	}
	engine.XMLTag(&b, "remote_agent", attrs...)

	if final := strings.TrimSpace(firstNonEmpty(r.reply, r.statusText)); final != "" {
		engine.XMLTag(&b, "final_response")
		b.WriteString(engine.XMLCDATA(final))
		b.WriteByte('\n')
		engine.XMLClose(&b, "final_response")
	}

	rendered := renderArtifacts(&b, r.artifacts)
	if rendered == 0 && strings.TrimSpace(firstNonEmpty(r.reply, r.statusText)) == "" {
		// A terminal task with nothing in it is a real answer — "I did the
		// thing, there is no output" — and saying so beats an empty element the
		// model has to interpret.
		engine.XMLTag(&b, "final_response")
		b.WriteString(engine.XMLCDATA("The remote agent produced no output."))
		b.WriteByte('\n')
		engine.XMLClose(&b, "final_response")
	}

	engine.XMLClose(&b, "remote_agent")
	return b.String()
}

// renderArtifacts writes the artifacts section and returns how many artifacts
// it rendered. Telemetry parts carried by the Nexus A2A extension are dropped:
// they are observability, not output, and dumping a thinking step or a token
// count into the calling model's context would be noise it has to reason past.
func renderArtifacts(b *strings.Builder, artifacts []a2a.Artifact) int {
	type kept struct {
		artifact a2a.Artifact
		parts    []a2a.Part
	}
	var keep []kept
	for _, artifact := range artifacts {
		var parts []a2a.Part
		for _, part := range artifact.Parts {
			if _, isExtension, _ := a2a.NexusEventFromPart(part); isExtension {
				continue
			}
			parts = append(parts, part)
		}
		if len(parts) == 0 {
			continue
		}
		keep = append(keep, kept{artifact: artifact, parts: parts})
	}
	if len(keep) == 0 {
		return 0
	}

	engine.XMLTag(b, "artifacts", "count", strconv.Itoa(len(keep)))
	for _, k := range keep {
		attrs := []string{"id", k.artifact.ArtifactID}
		if k.artifact.Name != "" {
			attrs = append(attrs, "name", k.artifact.Name)
		}
		if k.artifact.Description != "" {
			attrs = append(attrs, "description", k.artifact.Description)
		}
		engine.XMLTag(b, "artifact", attrs...)
		for _, part := range k.parts {
			renderPart(b, part)
		}
		engine.XMLClose(b, "artifact")
	}
	engine.XMLClose(b, "artifacts")
	return len(keep)
}

// renderPart writes one part. Text and structured data are rendered inline;
// binary content and external URLs are described rather than inlined, because
// base64 in a prompt costs tokens and tells the model nothing it can use.
func renderPart(b *strings.Builder, part a2a.Part) {
	switch part.Kind() {
	case a2a.PartKindText:
		text, _ := part.TextValue()
		writeContent(b, "text", part, text)

	case a2a.PartKindData:
		writeContent(b, "data", part, formatJSON(part.Data))

	case a2a.PartKindURL:
		u, _ := part.URLValue()
		attrs := []string{"url", u}
		if part.MediaType != "" {
			attrs = append(attrs, "media_type", part.MediaType)
		}
		if part.Filename != "" {
			attrs = append(attrs, "filename", part.Filename)
		}
		writeEmpty(b, "url", attrs...)

	case a2a.PartKindRaw:
		attrs := []string{"bytes", strconv.Itoa(len(part.Raw))}
		if part.MediaType != "" {
			attrs = append(attrs, "media_type", part.MediaType)
		}
		if part.Filename != "" {
			attrs = append(attrs, "filename", part.Filename)
		}
		writeEmpty(b, "binary", attrs...)
	}
}

// writeContent writes an inline part element, truncating oversized content and
// saying so in the element's attributes.
func writeContent(b *strings.Builder, tag string, part a2a.Part, content string) {
	attrs := []string{}
	if part.MediaType != "" {
		attrs = append(attrs, "media_type", part.MediaType)
	}
	if part.Filename != "" {
		attrs = append(attrs, "filename", part.Filename)
	}
	if len(content) > maxPartBytes {
		attrs = append(attrs,
			"truncated", "true",
			"original_bytes", strconv.Itoa(len(content)))
		content = content[:maxPartBytes]
	}
	engine.XMLTag(b, tag, attrs...)
	b.WriteString(engine.XMLCDATA(content))
	b.WriteByte('\n')
	engine.XMLClose(b, tag)
}

// writeEmpty writes a self-closing element describing content that is not
// inlined.
func writeEmpty(b *strings.Builder, tag string, attrs ...string) {
	b.WriteByte('<')
	b.WriteString(tag)
	for i := 0; i+1 < len(attrs); i += 2 {
		fmt.Fprintf(b, " %s=%q", attrs[i], engine.XMLEscape(attrs[i+1]))
	}
	b.WriteString("/>\n")
}

// formatJSON re-indents a data part so the model reads structure rather than
// one long line. An undecodable payload is passed through verbatim: it is still
// the remote's answer, and mangling it would be worse than showing it raw.
func formatJSON(raw json.RawMessage) string {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		return string(raw)
	}
	return pretty.String()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
