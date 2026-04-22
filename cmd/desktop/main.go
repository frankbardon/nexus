// Command desktop is the multi-agent Nexus desktop shell. It hosts
// hello-world and staffing-match agents in a single Wails app with
// a left-nav for switching between them.
package main

import (
	"embed"
	"log"

	"github.com/frankbardon/nexus/cmd/desktop/internal/matcher"
	"github.com/frankbardon/nexus/pkg/desktop"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/plugins/apps/helloworld"
	wailsio "github.com/frankbardon/nexus/plugins/io/wails"
	"github.com/frankbardon/nexus/plugins/providers/anthropic"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed config-hello.yaml
var helloConfig []byte

//go:embed config-staffing.yaml
var staffingConfig []byte

func main() {
	if err := desktop.Run(&desktop.Shell{
		Title:  "Nexus Desktop",
		Width:  1024,
		Height: 768,
		Assets: assets,
		Agents: []desktop.Agent{
			{
				ID:          "staffing-match",
				Name:        "Staffing Match",
				Description: "AI-powered candidate ranking",
				Icon:        "fa-solid fa-handshake",
				ConfigYAML:  staffingConfig,
				Factories: map[string]func() engine.Plugin{
					"nexus.io.wails":          wailsio.New,
					"nexus.llm.anthropic":     anthropic.New,
					"nexus.app.staffingmatch": matcher.New,
				},
				Settings: []desktop.SettingsField{
					{
						Key:         "shell.anthropic_api_key",
						Display:     "Anthropic API Key",
						Description: "API key for Claude (shared across all agents that use Anthropic)",
						Type:        desktop.FieldString,
						Secret:      true,
						Required:    true,
					},
					{
						Key:         "input_dir",
						Display:     "Input Folder",
						Description: "Directory containing job descriptions and resumes",
						Type:        desktop.FieldPath,
						Required:    true,
					},
					{
						Key:         "output_dir",
						Display:     "Output Folder",
						Description: "Directory where match results are saved",
						Type:        desktop.FieldPath,
					},
				},
			},
			{
				ID:          "hello-world",
				Name:        "Hello World",
				Description: "Bus bridge proof-of-concept",
				Icon:        "fa-solid fa-hand-wave",
				ConfigYAML:  helloConfig,
				Factories: map[string]func() engine.Plugin{
					"nexus.io.wails":       wailsio.New,
					"nexus.app.helloworld": helloworld.New,
				},
				Settings: []desktop.SettingsField{
					{
						Key:         "greeting",
						Display:     "Greeting",
						Description: "The greeting prefix used by the hello agent",
						Type:        desktop.FieldString,
						Default:     "Hello",
					},
				},
			},
		},
	}); err != nil {
		log.Fatalf("wails run: %v", err)
	}
}
