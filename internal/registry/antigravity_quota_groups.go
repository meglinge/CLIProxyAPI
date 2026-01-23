// Package registry provides Antigravity quota group definitions.
// Models in the same group share a common quota pool.
// Source: https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/main/src/utils/quota/constants.ts
package registry

import "strings"

// antigravityQuotaGroups defines models that share quota pools.
// When one model in a group is exhausted, all models in the group are considered unavailable.
var antigravityQuotaGroups = map[string][]string{
	"claude-gpt": {
		"claude-sonnet-4-5-thinking",
		"claude-opus-4-5-thinking",
		"gpt-oss-120b-medium",
	},
	"gemini-3-pro": {
		"gemini-3-pro-high",
		"gemini-3-pro-low",
	},
	"gemini-2-5-flash": {
		"gemini-2.5-flash",
		"gemini-2.5-flash-thinking",
	},
	"gemini-2-5-flash-lite": {
		"gemini-2.5-flash-lite",
	},
	"gemini-2-5-cu": {
		"rev19-uic3-1p",
	},
	"gemini-3-flash": {
		"gemini-3-flash",
	},
	"gemini-image": {
		"gemini-3-pro-image",
	},
}

// antigravityModelToGroup maps each model to its quota group for fast lookup.
var antigravityModelToGroup = buildAntigravityModelToGroupMap()

func buildAntigravityModelToGroupMap() map[string]string {
	m := make(map[string]string)
	for group, models := range antigravityQuotaGroups {
		for _, model := range models {
			m[model] = group
		}
	}
	return m
}

// GetAntigravityQuotaGroupID returns the stable group ID for the given model.
// Returns the model itself if not part of any predefined group.
// Supports prefix matching for models with date suffixes (e.g., claude-sonnet-4-5-20250929).
func GetAntigravityQuotaGroupID(model string) string {
	if model == "" {
		return ""
	}
	
	// Direct match
	if group := antigravityModelToGroup[model]; group != "" {
		return group
	}
	
	// Prefix match for models with date suffixes
	// e.g., "claude-sonnet-4-5-20250929" matches "claude-sonnet-4-5-thinking"
	for groupID, models := range antigravityQuotaGroups {
		for _, baseModel := range models {
			// Remove common suffixes for comparison
			basePrefix := strings.TrimSuffix(baseModel, "-thinking")
			if strings.HasPrefix(model, basePrefix) {
				return groupID
			}
		}
	}
	
	return model
}

// GetAntigravityQuotaGroupModels returns all models that share quota with the given model.
// Returns the input model in a slice if it's not part of any predefined group.
// Supports prefix matching for models with date suffixes.
func GetAntigravityQuotaGroupModels(model string) []string {
	if model == "" {
		return nil
	}
	
	groupID := GetAntigravityQuotaGroupID(model)
	if groupID == model {
		// Not in any predefined group
		return []string{model}
	}
	
	// Return all base models in the group
	return antigravityQuotaGroups[groupID]
}
