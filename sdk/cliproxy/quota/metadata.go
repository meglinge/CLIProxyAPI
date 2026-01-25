package quota

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	// MetadataKey stores quota snapshots inside auth metadata JSON files.
	MetadataKey = "cliproxy_quota"

	metadataProviderKey  = "provider"
	metadataUpdatedAtKey = "updated_at"
	metadataModelsKey    = "models"
	metadataPercentKey   = "percent"
	metadataResetKey     = "reset_time"
)

const quotaEqualEpsilon = 0.0001

// GetPercentFromMetadata returns the stored quota percentage for a model.
func GetPercentFromMetadata(metadata map[string]any, model string) (float64, bool) {
	if entry, ok := GetModelQuotaFromMetadata(metadata, model); ok {
		return clampPercent(entry.Percent), true
	}
	return 0, false
}

// GetModelQuotaFromMetadata returns the stored quota entry for a model.
func GetModelQuotaFromMetadata(metadata map[string]any, model string) (ModelQuota, bool) {
	if metadata == nil {
		return ModelQuota{}, false
	}
	rawSnapshot, ok := metadata[MetadataKey]
	if !ok {
		return ModelQuota{}, false
	}
	snapshot, ok := rawSnapshot.(map[string]any)
	if !ok {
		return ModelQuota{}, false
	}
	rawModels, ok := snapshot[metadataModelsKey].(map[string]any)
	if !ok {
		return ModelQuota{}, false
	}
	lookup := NormalizeModelKey(model)
	if lookup == "" {
		lookup = "*"
	}
	if entry, ok := rawModels[lookup]; ok {
		if quotaEntry, ok := readModelQuota(entry); ok {
			return quotaEntry, true
		}
	}
	if entry, ok := rawModels["*"]; ok {
		if quotaEntry, ok := readModelQuota(entry); ok {
			return quotaEntry, true
		}
	}
	return ModelQuota{}, false
}

// UpdateMetadata writes quota snapshot into the metadata map.
// Returns true when metadata is changed.
func UpdateMetadata(metadata map[string]any, provider string, models map[string]ModelQuota, updatedAt time.Time) bool {
	if metadata == nil {
		return false
	}
	normalized := normalizeModelQuota(models)
	if len(normalized) == 0 {
		return false
	}

	existingProvider := ""
	existingModels := map[string]ModelQuota{}
	if rawSnapshot, ok := metadata[MetadataKey]; ok {
		if snapshot, ok := rawSnapshot.(map[string]any); ok {
			existingProvider = normalizeString(snapshot[metadataProviderKey])
			existingModels = parseSnapshotModels(snapshot[metadataModelsKey])
		}
	}

	normalizedProvider := strings.ToLower(strings.TrimSpace(provider))
	if normalizedProvider == "" {
		normalizedProvider = existingProvider
	}
	if normalizedProvider == "" {
		normalizedProvider = provider
	}

	if normalizedProvider == strings.ToLower(strings.TrimSpace(existingProvider)) && modelQuotaEqual(existingModels, normalized) {
		return false
	}

	serialized := make(map[string]any, len(normalized))
	for key, entry := range normalized {
		item := map[string]any{
			metadataPercentKey: clampPercent(entry.Percent),
		}
		if !entry.ResetTime.IsZero() {
			item[metadataResetKey] = entry.ResetTime.UTC().Format(time.RFC3339Nano)
		}
		serialized[key] = item
	}

	snapshot := map[string]any{
		metadataProviderKey: strings.TrimSpace(provider),
		metadataModelsKey:   serialized,
	}
	if !updatedAt.IsZero() {
		snapshot[metadataUpdatedAtKey] = updatedAt.UTC().Format(time.RFC3339Nano)
	}
	metadata[MetadataKey] = snapshot
	return true
}

func normalizeModelQuota(models map[string]ModelQuota) map[string]ModelQuota {
	if len(models) == 0 {
		return nil
	}
	out := make(map[string]ModelQuota, len(models))
	for rawKey, entry := range models {
		key := NormalizeModelKey(rawKey)
		if key == "" {
			continue
		}
		entry.Percent = clampPercent(entry.Percent)
		if existing, ok := out[key]; ok {
			if entry.Percent <= existing.Percent {
				continue
			}
		}
		out[key] = entry
	}
	return out
}

func parseSnapshotModels(raw any) map[string]ModelQuota {
	typed, ok := raw.(map[string]any)
	if !ok {
		return map[string]ModelQuota{}
	}
	out := make(map[string]ModelQuota, len(typed))
	for key, value := range typed {
		item, ok := value.(map[string]any)
		if !ok {
			continue
		}
		percent, ok := readFloat(item[metadataPercentKey])
		if !ok {
			continue
		}
		reset := parseTime(item[metadataResetKey])
		modelKey := NormalizeModelKey(key)
		if modelKey == "" {
			continue
		}
		out[modelKey] = ModelQuota{Percent: clampPercent(percent), ResetTime: reset}
	}
	return out
}

func modelQuotaEqual(a, b map[string]ModelQuota) bool {
	if len(a) != len(b) {
		return false
	}
	for key, left := range a {
		right, ok := b[key]
		if !ok {
			return false
		}
		if math.Abs(left.Percent-right.Percent) > quotaEqualEpsilon {
			return false
		}
		if !timeEqual(left.ResetTime, right.ResetTime) {
			return false
		}
	}
	return true
}

func timeEqual(a, b time.Time) bool {
	if a.IsZero() && b.IsZero() {
		return true
	}
	return a.Equal(b)
}

func readPercent(value any) (float64, bool) {
	if value == nil {
		return 0, false
	}
	if m, ok := value.(map[string]any); ok {
		return readFloat(m[metadataPercentKey])
	}
	return readFloat(value)
}

func readModelQuota(value any) (ModelQuota, bool) {
	if value == nil {
		return ModelQuota{}, false
	}
	if m, ok := value.(map[string]any); ok {
		percent, ok := readFloat(m[metadataPercentKey])
		if !ok {
			return ModelQuota{}, false
		}
		reset := parseTime(m[metadataResetKey])
		return ModelQuota{Percent: clampPercent(percent), ResetTime: reset}, true
	}
	if percent, ok := readFloat(value); ok {
		return ModelQuota{Percent: clampPercent(percent)}, true
	}
	return ModelQuota{}, false
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

func parseTime(value any) time.Time {
	switch typed := value.(type) {
	case time.Time:
		return typed.UTC()
	case string:
		ts := strings.TrimSpace(typed)
		if ts == "" {
			return time.Time{}
		}
		if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			return parsed.UTC()
		}
		if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
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
