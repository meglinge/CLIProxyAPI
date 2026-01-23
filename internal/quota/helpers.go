package quota

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/quota"
)

var defaultAntigravityAliases = map[string]string{
	"rev19-uic3-1p":               "gemini-2.5-computer-use-preview-10-2025",
	"gemini-3-pro-image":          "gemini-3-pro-image-preview",
	"gemini-3-pro-high":           "gemini-3-pro-preview",
	"gemini-3-flash":              "gemini-3-flash-preview",
	"claude-sonnet-4-5":           "gemini-claude-sonnet-4-5",
	"claude-sonnet-4-5-thinking":  "gemini-claude-sonnet-4-5-thinking",
	"claude-opus-4-5-thinking":    "gemini-claude-opus-4-5-thinking",
}

func defaultAntigravityAliasMap() map[string]string {
	out := make(map[string]string, len(defaultAntigravityAliases))
	for k, v := range defaultAntigravityAliases {
		out[strings.ToLower(k)] = strings.TrimSpace(v)
	}
	return out
}

func aliasMapFromConfig(cfg *config.Config) map[string]string {
	if cfg == nil || cfg.OAuthModelAlias == nil {
		return defaultAntigravityAliasMap()
	}
	entries := cfg.OAuthModelAlias["antigravity"]
	if len(entries) == 0 {
		return defaultAntigravityAliasMap()
	}
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		name := strings.TrimSpace(entry.Name)
		alias := strings.TrimSpace(entry.Alias)
		if name == "" || alias == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, exists := out[key]; exists {
			continue
		}
		out[key] = alias
	}
	if len(out) == 0 {
		return defaultAntigravityAliasMap()
	}
	return out
}

func shouldSkipAuth(auth *coreauth.Auth) bool {
	if auth == nil {
		return true
	}
	if auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return true
	}
	if auth.Attributes == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["runtime_only"]), "true")
}

func resolveUserAgent(auth *coreauth.Auth, fallback string) string {
	if auth != nil {
		if auth.Attributes != nil {
			if ua := strings.TrimSpace(auth.Attributes["user_agent"]); ua != "" {
				return ua
			}
		}
		if auth.Metadata != nil {
			if ua, ok := auth.Metadata["user_agent"].(string); ok && strings.TrimSpace(ua) != "" {
				return strings.TrimSpace(ua)
			}
		}
	}
	return fallback
}

func extractAntigravityQuota(payload []byte, aliasMap map[string]string) map[string]quota.ModelQuota {
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil
	}
	models, ok := root["models"].(map[string]any)
	if !ok {
		return nil
	}
	result := make(map[string]quota.ModelQuota)
	for key, raw := range models {
		record, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, okProvider := record["modelProvider"]; !okProvider {
			continue
		}
		info, ok := record["quotaInfo"].(map[string]any)
		if !ok {
			continue
		}
		remaining, okRemain := readFloat(info["remainingFraction"])
		if !okRemain {
			continue
		}
		percent := clampPercent(remaining * 100)
		resetTime := parseResetTime(info["resetTime"])
		name := normalizeString(record["model"])
		if name == "" {
			name = strings.TrimSpace(key)
		}
		if name == "" {
			continue
		}
		entry := quota.ModelQuota{Percent: percent, ResetTime: resetTime}
		addModelQuota(result, name, entry)
		if aliasMap != nil {
			if alias := strings.TrimSpace(aliasMap[strings.ToLower(name)]); alias != "" {
				addModelQuota(result, alias, entry)
			}
		}
	}
	return result
}

func extractGeminiQuota(payload []byte) map[string]quota.ModelQuota {
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil
	}
	rawBuckets, ok := root["buckets"].([]any)
	if !ok {
		return nil
	}
	result := make(map[string]quota.ModelQuota)
	for _, raw := range rawBuckets {
		bucket, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := normalizeString(bucket["modelId"])
		if name == "" {
			continue
		}
		remaining, okRemain := readFloat(bucket["remainingFraction"])
		if !okRemain {
			continue
		}
		percent := clampPercent(remaining * 100)
		resetTime := parseResetTime(bucket["resetTime"])
		entry := quota.ModelQuota{Percent: percent, ResetTime: resetTime}
		addModelQuota(result, name, entry)
	}
	return result
}

func extractCodexQuota(payload []byte) map[string]quota.ModelQuota {
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil
	}
	percent := resolveCodexPercent(root)
	if percent < 0 {
		return nil
	}
	entry := quota.ModelQuota{Percent: percent}
	return map[string]quota.ModelQuota{"*": entry}
}

func resolveCodexPercent(root map[string]any) float64 {
	if root == nil {
		return -1
	}
	rateLimit := toRecord(root["rate_limit"])
	reviewLimit := toRecord(root["code_review_rate_limit"])
	candidates := []float64{}
	if p, ok := codexLimitPercent(rateLimit); ok {
		candidates = append(candidates, p)
	}
	if p, ok := codexLimitPercent(reviewLimit); ok {
		candidates = append(candidates, p)
	}
	if len(candidates) == 0 {
		return -1
	}
	best := candidates[0]
	for _, p := range candidates[1:] {
		if p < best {
			best = p
		}
	}
	return best
}

func codexLimitPercent(limit map[string]any) (float64, bool) {
	if limit == nil {
		return 0, false
	}
	allowed := normalizeBoolean(limit["allowed"])
	limitReached := normalizeBoolean(limit["limit_reached"])
	primary := toRecord(limit["primary_window"])
	secondary := toRecord(limit["secondary_window"])
	candidates := []float64{}
	if p, ok := codexWindowPercent(primary, allowed, limitReached); ok {
		candidates = append(candidates, p)
	}
	if p, ok := codexWindowPercent(secondary, allowed, limitReached); ok {
		candidates = append(candidates, p)
	}
	if len(candidates) == 0 {
		return 0, false
	}
	best := candidates[0]
	for _, p := range candidates[1:] {
		if p < best {
			best = p
		}
	}
	return best, true
}

func codexWindowPercent(window map[string]any, allowed, limitReached bool) (float64, bool) {
	if window == nil {
		return 0, false
	}
	if limitReached || !allowed {
		return 0, true
	}
	used, ok := readFloat(window["used_percent"])
	if !ok {
		return 0, false
	}
	return clampPercent(100 - used), true
}

func addModelQuota(dst map[string]quota.ModelQuota, model string, entry quota.ModelQuota) {
	if dst == nil {
		return
	}
	key := quota.NormalizeModelKey(model)
	if key == "" {
		return
	}
	if existing, ok := dst[key]; ok {
		if entry.Percent <= existing.Percent {
			return
		}
	}
	dst[key] = entry
}

func normalizeString(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return strings.TrimSpace(typed.String())
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return ""
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case float32:
		val := float64(typed)
		if math.IsNaN(val) || math.IsInf(val, 0) {
			return ""
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case uint64:
		return strconv.FormatUint(typed, 10)
	default:
		return ""
	}
}

func readFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return 0, false
		}
		return typed, true
	case float32:
		val := float64(typed)
		if math.IsNaN(val) || math.IsInf(val, 0) {
			return 0, false
		}
		return val, true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case json.Number:
		if f, err := typed.Float64(); err == nil {
			return f, true
		}
	case string:
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

func parseResetTime(value any) time.Time {
	if value == nil {
		return time.Time{}
	}
	if ts := normalizeString(value); ts != "" {
		if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
			return parsed.UTC()
		}
		if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func clampPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func normalizeBoolean(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		trimmed := strings.ToLower(strings.TrimSpace(typed))
		if trimmed == "true" || trimmed == "1" {
			return true
		}
		if trimmed == "false" || trimmed == "0" {
			return false
		}
	}
	return false
}

func toRecord(value any) map[string]any {
	if value == nil {
		return nil
	}
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	return nil
}

func resolveCodexAccountID(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	if accountID := normalizeString(metadata["account_id"]); accountID != "" {
		return accountID
	}
	if accountID := normalizeString(metadata["accountId"]); accountID != "" {
		return accountID
	}

	meta := toRecord(metadata["metadata"])
	attrs := toRecord(metadata["attributes"])
	candidates := []any{
		metadata["id_token"],
		meta["id_token"],
		attrs["id_token"],
	}
	for _, candidate := range candidates {
		if accountID := extractCodexAccountID(candidate); accountID != "" {
			return accountID
		}
	}
	return ""
}

func extractCodexAccountID(value any) string {
	payload := parseIDTokenPayload(value)
	if payload == nil {
		return ""
	}
	accountID := normalizeString(payload["chatgpt_account_id"])
	if accountID != "" {
		return accountID
	}
	return normalizeString(payload["chatgptAccountId"])
}

func resolveGeminiProjectID(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	return normalizeString(metadata["project_id"])
}

func parseIDTokenPayload(value any) map[string]any {
	if value == nil {
		return nil
	}
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	raw := normalizeString(value)
	if raw == "" {
		return nil
	}

	var parsed map[string]any
	if errUnmarshal := json.Unmarshal([]byte(raw), &parsed); errUnmarshal == nil {
		return parsed
	}

	segments := strings.Split(raw, ".")
	if len(segments) < 2 {
		return nil
	}
	payload, errDecode := decodeBase64URL(segments[1])
	if errDecode != nil || payload == "" {
		return nil
	}
	if errUnmarshal := json.Unmarshal([]byte(payload), &parsed); errUnmarshal != nil {
		return nil
	}
	return parsed
}

func decodeBase64URL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	decoded, errDecode := base64.RawURLEncoding.DecodeString(value)
	if errDecode == nil {
		return string(decoded), nil
	}

	padded := value
	if rem := len(value) % 4; rem != 0 {
		padded = value + strings.Repeat("=", 4-rem)
	}
	decoded, errDecode = base64.URLEncoding.DecodeString(padded)
	if errDecode != nil {
		return "", errDecode
	}
	return string(decoded), nil
}

func summarizePayload(payload []byte) string {
	const max = 512
	trimmed := bytesTrimSpace(payload)
	if len(trimmed) == 0 {
		return ""
	}
	if len(trimmed) > max {
		return string(trimmed[:max]) + "...(truncated)"
	}
	return string(trimmed)
}

func bytesTrimSpace(input []byte) []byte {
	if len(input) == 0 {
		return nil
	}
	start := 0
	end := len(input)
	for start < end {
		if input[start] > ' ' {
			break
		}
		start++
	}
	for end > start {
		if input[end-1] > ' ' {
			break
		}
		end--
	}
	return input[start:end]
}
