//go:build deprecated

package config

import "strings"

// defaultAntigravityModelMappings provides built-in mappings from Claude Code model names
// to Antigravity model names for seamless routing when no claude API key is available.
var defaultAntigravityModelMappings = []ModelNameMapping{
	{Name: "gemini-claude-sonnet-4-5-thinking", Alias: "claude-sonnet-4-5-20250929"},
	{Name: "gemini-claude-opus-4-5-thinking", Alias: "claude-opus-4-5-20251101"},
}

// ApplyDefaultModelMappings adds default model mappings if not already configured by user.
func (cfg *Config) ApplyDefaultModelMappings() {
	if cfg == nil {
		return
	}
	if cfg.OAuthModelMappings == nil {
		cfg.OAuthModelMappings = make(map[string][]ModelNameMapping)
	}

	existing := cfg.OAuthModelMappings["antigravity"]
	seenAlias := make(map[string]struct{}, len(existing))
	for _, m := range existing {
		seenAlias[strings.ToLower(m.Alias)] = struct{}{}
	}

	for _, dm := range defaultAntigravityModelMappings {
		if _, exists := seenAlias[strings.ToLower(dm.Alias)]; !exists {
			existing = append(existing, dm)
		}
	}

	if len(existing) > 0 {
		cfg.OAuthModelMappings["antigravity"] = existing
	}
}
