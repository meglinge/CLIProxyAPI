package auth

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

// resolveOAuthUpstreamModelWithFallback resolves the upstream model for OAuth auths using:
// 1) configured oauth-model-alias
// 2) built-in defaults (antigravity)
// 3) registry-based auto matching (date suffix + thinking fallback)
func (m *Manager) resolveOAuthUpstreamModelWithFallback(auth *Auth, requestedModel string, reg *registry.ModelRegistry) string {
	if m == nil || auth == nil {
		return ""
	}
	requestedModel = strings.TrimSpace(rewriteModelForAuth(requestedModel, auth))
	if requestedModel == "" {
		return ""
	}
	channel := modelAliasChannel(auth)
	if channel == "" {
		return ""
	}

	if upstream := m.resolveOAuthUpstreamModel(auth, requestedModel); upstream != "" {
		return upstream
	}
	if upstream := resolveDefaultOAuthAlias(channel, requestedModel); upstream != "" {
		return upstream
	}
	return ""
}

func resolveDefaultOAuthAlias(channel, requestedModel string) string {
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel == "" || requestedModel == "" {
		return ""
	}
	aliases := defaultOAuthAliasMap(channel)
	if len(aliases) == 0 {
		return ""
	}
	key := strings.ToLower(strings.TrimSpace(requestedModel))
	if key == "" {
		return ""
	}
	if upstream := strings.TrimSpace(aliases[key]); upstream != "" && !strings.EqualFold(upstream, requestedModel) {
		return upstream
	}
	return ""
}

func defaultOAuthAliasMap(channel string) map[string]string {
	if channel != "antigravity" {
		return nil
	}
	return map[string]string{
		"gemini-2.5-computer-use-preview-10-2025": "rev19-uic3-1p",
		"gemini-3-pro-image-preview":             "gemini-3-pro-image",
		"gemini-3-pro-preview":                   "gemini-3-pro-high",
		"gemini-3-flash-preview":                 "gemini-3-flash",
	}
}
