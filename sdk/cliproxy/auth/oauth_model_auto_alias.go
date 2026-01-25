package auth

import (
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
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
	if reg == nil {
		return ""
	}
	if upstream := resolveModelFromRegistry(reg, auth.ID, requestedModel); upstream != "" {
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
		"gemini-claude-sonnet-4-5":               "claude-sonnet-4-5",
		"gemini-claude-sonnet-4-5-thinking":      "claude-sonnet-4-5-thinking",
		"gemini-claude-opus-4-5-thinking":        "claude-opus-4-5-thinking",
	}
}

func resolveModelFromRegistry(reg *registry.ModelRegistry, authID, requestedModel string) string {
	if reg == nil || authID == "" || requestedModel == "" {
		return ""
	}
	infos := reg.GetModelsForClient(authID)
	if len(infos) == 0 {
		return ""
	}
	models := collectModelIDs(infos)
	if len(models) == 0 {
		return ""
	}
	return resolveModelFromCandidates(requestedModel, models)
}

func collectModelIDs(infos []*registry.ModelInfo) []string {
	if len(infos) == 0 {
		return nil
	}
	out := make([]string, 0, len(infos))
	seen := make(map[string]struct{}, len(infos))
	for _, info := range infos {
		if info == nil {
			continue
		}
		id := strings.TrimSpace(info.ID)
		if id == "" {
			id = strings.TrimSpace(info.Name)
		}
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, id)
	}
	return out
}

func resolveModelFromCandidates(requestedModel string, models []string) string {
	if requestedModel == "" || len(models) == 0 {
		return ""
	}
	normalized := normalizeRequestedModel(requestedModel)
	if normalized == "" {
		normalized = strings.TrimSpace(requestedModel)
	}

	if matched := matchExactModel(models, normalized); matched != "" {
		return matched
	}
	if normalized != requestedModel {
		if matched := matchExactModel(models, requestedModel); matched != "" {
			return matched
		}
	}

	base := stripDateSuffix(normalized)
	if base != normalized {
		if matched := matchExactModel(models, base); matched != "" {
			return matched
		}
	}
	// Try latest dated variant for base (works for both date and non-date requests).
	if matched := matchLatestDatedModel(models, base); matched != "" {
		return matched
	}

	if strings.HasSuffix(base, "-thinking") {
		alt := strings.TrimSuffix(base, "-thinking")
		if matched := matchExactModel(models, alt); matched != "" {
			return matched
		}
		if matched := matchLatestDatedModel(models, alt); matched != "" {
			return matched
		}
	} else {
		alt := base + "-thinking"
		if matched := matchExactModel(models, alt); matched != "" {
			return matched
		}
		if matched := matchLatestDatedModel(models, alt); matched != "" {
			return matched
		}
	}

	return ""
}

func normalizeRequestedModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	parsed := thinking.ParseSuffix(model)
	if parsed.ModelName != "" {
		return strings.TrimSpace(parsed.ModelName)
	}
	return model
}

func matchExactModel(models []string, needle string) string {
	needle = strings.ToLower(strings.TrimSpace(needle))
	if needle == "" {
		return ""
	}
	for _, model := range models {
		if strings.EqualFold(strings.TrimSpace(model), needle) {
			return model
		}
	}
	return ""
}

func matchLatestDatedModel(models []string, base string) string {
	base = strings.ToLower(strings.TrimSpace(base))
	if base == "" {
		return ""
	}
	type dated struct {
		date  string
		model string
	}
	candidates := make([]dated, 0)
	prefix := base + "-"
	for _, model := range models {
		lower := strings.ToLower(strings.TrimSpace(model))
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(lower, prefix)
		if len(suffix) != 8 || !isAllDigits(suffix) {
			continue
		}
		candidates = append(candidates, dated{date: suffix, model: model})
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].date > candidates[j].date
	})
	return candidates[0].model
}
