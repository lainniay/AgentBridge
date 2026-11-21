package plugins

import "embed"

//go:embed antigravity/plugin.json antigravity/hooks.json antigravity/mcp_config.json antigravity/skills/codex/SKILL.md codex/skills/agy/SKILL.md
var Assets embed.FS
