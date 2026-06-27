// Fake MCP server used by tests/integration/mcp_client_test.go. Exposes a
// small, deterministic surface so the Nexus mcp.client plugin can be exercised
// end-to-end over stdio:
//
//	tools:     echo (string -> string), add (a+b -> int)
//	resources: readme (text), tiny-image (binary)
//	template:  doc://{name}
//	prompts:   greet (no args), review (one required arg)
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

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 1x1 transparent PNG, base64-decoded at runtime.
const tinyPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII="

type echoArgs struct {
	Text string `json:"text" jsonschema:"Text to echo."`
}

type addArgs struct {
	A float64 `json:"a"`
	B float64 `json:"b"`
}

func main() {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "nexus.mcp.fake",
		Version: "0.1.0",
	}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "echo",
		Description: "Echo back the supplied text.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args echoArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: args.Text}},
		}, nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "add",
		Description: "Sum two integers.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args addArgs) (*mcp.CallToolResult, any, error) {
		sum := int(args.A) + int(args.B)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("%d", sum)}},
		}, nil, nil
	})

	s.AddResource(&mcp.Resource{
		URI:         "doc://readme",
		Name:        "readme",
		MIMEType:    "text/plain",
		Description: "Test readme content.",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      "doc://readme",
				MIMEType: "text/plain",
				Text:     "Hello from MCP fake.",
			}},
		}, nil
	})

	imageBytes, _ := base64.StdEncoding.DecodeString(tinyPNGBase64)
	s.AddResource(&mcp.Resource{
		URI:      "img://pixel",
		Name:     "pixel",
		MIMEType: "image/png",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      "img://pixel",
				MIMEType: "image/png",
				Blob:     imageBytes,
			}},
		}, nil
	})

	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "doc://{name}",
		Name:        "doc-template",
		Description: "Fetch a doc by name.",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		name := strings.TrimPrefix(req.Params.URI, "doc://")
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      req.Params.URI,
				MIMEType: "text/plain",
				Text:     "Doc named " + name,
			}},
		}, nil
	})

	s.AddPrompt(&mcp.Prompt{
		Name:        "greet",
		Description: "A no-argument greeting.",
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{
			Description: "Greeting prompt",
			Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: "Greet the assistant."}},
			},
		}, nil
	})

	s.AddPrompt(&mcp.Prompt{
		Name:        "review",
		Description: "Two-message review prompt.",
		Arguments: []*mcp.PromptArgument{
			{Name: "topic", Required: true, Description: "What to review."},
		},
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		topic := req.Params.Arguments["topic"]
		return &mcp.GetPromptResult{
			Description: "Review " + topic,
			Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: "Please review " + topic + "."}},
				{Role: "assistant", Content: &mcp.TextContent{Text: "Sure, will review."}},
				{Role: "user", Content: &mcp.TextContent{Text: "Begin now."}},
			},
		}, nil
	})

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("mcp fake stdio serve: %v", err)
	}
}
