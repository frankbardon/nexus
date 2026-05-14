// Fake MCP server used by tests/integration/mcp_client_test.go. Exposes a
// small, deterministic surface so the Nexus mcp.client plugin can be exercised
// end-to-end over stdio:
//
//   tools:     echo (string -> string), add (a+b -> int)
//   resources: readme (text), tiny-image (binary)
//   template:  doc://{name}
//   prompts:   greet (no args), review (one required arg)
//
// Not built as part of normal go build — the integration test compiles it on
// demand via `go build`. Lives outside the main module path so it doesn't
// pollute production binaries.
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// 1x1 transparent PNG, base64-decoded at runtime.
const tinyPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII="

func main() {
	s := mcpserver.NewMCPServer("nexus.mcp.fake", "0.1.0",
		mcpserver.WithToolCapabilities(true),
		mcpserver.WithResourceCapabilities(true, true),
		mcpserver.WithPromptCapabilities(true),
	)

	s.AddTool(mcp.NewTool("echo",
		mcp.WithDescription("Echo back the supplied text."),
		mcp.WithString("text", mcp.Required(), mcp.Description("Text to echo.")),
	), func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		text := r.GetString("text", "")
		return mcp.NewToolResultText(text), nil
	})

	s.AddTool(mcp.NewTool("add",
		mcp.WithDescription("Sum two integers."),
		mcp.WithNumber("a", mcp.Required()),
		mcp.WithNumber("b", mcp.Required()),
	), func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		a := r.GetInt("a", 0)
		b := r.GetInt("b", 0)
		return mcp.NewToolResultText(fmt.Sprintf("%d", a+b)), nil
	})

	s.AddResource(mcp.NewResource("doc://readme", "readme",
		mcp.WithMIMEType("text/plain"),
		mcp.WithResourceDescription("Test readme content."),
	), func(ctx context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		return []mcp.ResourceContents{
			mcp.TextResourceContents{
				URI:      "doc://readme",
				MIMEType: "text/plain",
				Text:     "Hello from MCP fake.",
			},
		}, nil
	})

	imageBytes, _ := base64.StdEncoding.DecodeString(tinyPNGBase64)
	s.AddResource(mcp.NewResource("img://pixel", "pixel",
		mcp.WithMIMEType("image/png"),
	), func(ctx context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		return []mcp.ResourceContents{
			mcp.BlobResourceContents{
				URI:      "img://pixel",
				MIMEType: "image/png",
				Blob:     base64.StdEncoding.EncodeToString(imageBytes),
			},
		}, nil
	})

	s.AddResourceTemplate(mcp.NewResourceTemplate("doc://{name}", "doc-template",
		mcp.WithTemplateDescription("Fetch a doc by name."),
	), func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		name := strings.TrimPrefix(req.Params.URI, "doc://")
		return []mcp.ResourceContents{
			mcp.TextResourceContents{
				URI:      req.Params.URI,
				MIMEType: "text/plain",
				Text:     "Doc named " + name,
			},
		}, nil
	})

	s.AddPrompt(mcp.NewPrompt("greet",
		mcp.WithPromptDescription("A no-argument greeting."),
	), func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{
			Description: "Greeting prompt",
			Messages: []mcp.PromptMessage{
				{
					Role:    mcp.RoleUser,
					Content: mcp.TextContent{Type: "text", Text: "Greet the assistant."},
				},
			},
		}, nil
	})

	s.AddPrompt(mcp.NewPrompt("review",
		mcp.WithPromptDescription("Two-message review prompt."),
		mcp.WithArgument("topic", mcp.RequiredArgument(),
			mcp.ArgumentDescription("What to review."),
		),
	), func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		topic := req.Params.Arguments["topic"]
		return &mcp.GetPromptResult{
			Description: "Review " + topic,
			Messages: []mcp.PromptMessage{
				{Role: mcp.RoleUser, Content: mcp.TextContent{Type: "text", Text: "Please review " + topic + "."}},
				{Role: mcp.RoleAssistant, Content: mcp.TextContent{Type: "text", Text: "Sure, will review."}},
				{Role: mcp.RoleUser, Content: mcp.TextContent{Type: "text", Text: "Begin now."}},
			},
		}, nil
	})

	if err := mcpserver.ServeStdio(s); err != nil {
		log.Fatalf("mcp fake stdio serve: %v", err)
	}
}
