// Package registry provides Antigravity quota group definitions.
// Models in the same group share a common quota pool.
// Source: https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/main/src/utils/quota/constants.ts
package registry

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
func GetAntigravityQuotaGroupID(model string) string {
	if model == "" {
		return ""
	}
	if group := antigravityModelToGroup[model]; group != "" {
		return group
	}
	return model
}

// GetAntigravityQuotaGroupModels returns all models that share quota with the given model.
// Returns the input model in a slice if it's not part of any predefined group.
func GetAntigravityQuotaGroupModels(model string) []string {
	if model == "" {
		return nil
	}
	group := antigravityModelToGroup[model]
	if group == "" {
		return []string{model}
	}
	return antigravityQuotaGroups[group]
}
