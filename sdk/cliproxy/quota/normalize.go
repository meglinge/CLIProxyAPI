package quota

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
)

// NormalizeModelKey standardizes model identifiers for quota lookups.
func NormalizeModelKey(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	parsed := thinking.ParseSuffix(model)
	if parsed.ModelName != "" {
		model = parsed.ModelName
	}
	if idx := strings.LastIndex(model, "/"); idx >= 0 {
		model = model[idx+1:]
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	return strings.ToLower(model)
}
