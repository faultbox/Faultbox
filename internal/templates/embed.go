// Package templates provides embedded file templates for faultbox init.
package templates

import "embed"

//go:embed claude_commands/*.md claude_commands/mcp.json
var ClaudeCommands embed.FS
