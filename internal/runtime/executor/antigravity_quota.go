// Package executor provides quota management for Antigravity models.
// This file implements proactive quota tracking based on the fetchAvailableModels API response.
package executor

import (
	"context"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	// quotaRecoveryBuffer is the extra time to wait after resetTime before refreshing
	quotaRecoveryBuffer = 5 * time.Minute
)

// QuotaRefreshFunc is called when quota needs to be refreshed for an auth.
// The function should call FetchAntigravityModels to update quota state.
type QuotaRefreshFunc func(ctx context.Context, authID string)

// quotaRecoveryScheduler manages delayed quota refresh after reset times.
type quotaRecoveryScheduler struct {
	mu        sync.Mutex
	timers    map[string]*time.Timer // authID -> timer
	refreshFn QuotaRefreshFunc
}

var globalQuotaScheduler = &quotaRecoveryScheduler{
	timers: make(map[string]*time.Timer),
}

// SetQuotaRefreshFunc registers the function to call when quota needs refresh.
// This should be called during initialization by the service layer.
func SetQuotaRefreshFunc(fn QuotaRefreshFunc) {
	globalQuotaScheduler.mu.Lock()
	globalQuotaScheduler.refreshFn = fn
	globalQuotaScheduler.mu.Unlock()
}

// scheduleQuotaRefresh schedules a quota refresh for the given auth after resetTime + buffer.
func scheduleQuotaRefresh(authID string, resetTime time.Time) {
	if authID == "" || resetTime.IsZero() {
		return
	}

	refreshAt := resetTime.Add(quotaRecoveryBuffer)
	delay := time.Until(refreshAt)
	if delay <= 0 {
		// Already past the refresh time, refresh immediately in background
		delay = time.Second
	}

	globalQuotaScheduler.mu.Lock()
	defer globalQuotaScheduler.mu.Unlock()

	// Cancel existing timer if any
	if existing, ok := globalQuotaScheduler.timers[authID]; ok {
		existing.Stop()
	}

	// Schedule new timer
	globalQuotaScheduler.timers[authID] = time.AfterFunc(delay, func() {
		globalQuotaScheduler.mu.Lock()
		delete(globalQuotaScheduler.timers, authID)
		fn := globalQuotaScheduler.refreshFn
		globalQuotaScheduler.mu.Unlock()

		if fn != nil {
			log.Debugf("antigravity quota: triggering scheduled refresh for auth %s", authID)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			fn(ctx, authID)
		}
	})

	log.Debugf("antigravity quota: scheduled refresh for auth %s at %s (in %s)", authID, refreshAt.Format(time.RFC3339), delay.Round(time.Second))
}

// CancelQuotaRefresh cancels any pending quota refresh for the given auth.
func CancelQuotaRefresh(authID string) {
	if authID == "" {
		return
	}

	globalQuotaScheduler.mu.Lock()
	defer globalQuotaScheduler.mu.Unlock()

	if timer, ok := globalQuotaScheduler.timers[authID]; ok {
		timer.Stop()
		delete(globalQuotaScheduler.timers, authID)
	}
}

// quotaInfo holds parsed quota data from the API response.
type quotaInfo struct {
	remainingFraction float64
	resetTime         time.Time
}

// parseAntigravityQuotaFromResponse extracts quota info for each model from the API response.
// Response format: { "models": { "model-name": { "quotaInfo": { "remainingFraction": 0.85, "resetTime": "2024-01-15T10:30:00Z" } } } }
func parseAntigravityQuotaFromResponse(bodyBytes []byte) map[string]quotaInfo {
	quotas := make(map[string]quotaInfo)
	models := gjson.GetBytes(bodyBytes, "models")
	if !models.Exists() || !models.IsObject() {
		return quotas
	}

	models.ForEach(func(key, value gjson.Result) bool {
		modelName := key.String()
		if modelName == "" {
			return true
		}

		qi := quotaInfo{remainingFraction: 1.0} // Default to full quota

		// Try quotaInfo or quota_info
		quotaObj := value.Get("quotaInfo")
		if !quotaObj.Exists() {
			quotaObj = value.Get("quota_info")
		}
		if !quotaObj.Exists() {
			return true
		}

		// Parse remainingFraction (try multiple field names)
		for _, field := range []string{"remainingFraction", "remaining_fraction", "remaining"} {
			if v := quotaObj.Get(field); v.Exists() {
				qi.remainingFraction = v.Float()
				break
			}
		}

		// Parse resetTime (try multiple field names)
		for _, field := range []string{"resetTime", "reset_time"} {
			if v := quotaObj.Get(field); v.Exists() && v.String() != "" {
				if t, err := time.Parse(time.RFC3339, v.String()); err == nil {
					qi.resetTime = t
				}
				break
			}
		}

		quotas[modelName] = qi
		return true
	})

	return quotas
}

// UpdateAntigravityQuotaState updates the auth's ModelStates based on quota info from the API.
// It applies group logic: if any model in a group is exhausted, all models in that group are marked unavailable.
// Uses copy-on-write to reduce race conditions with concurrent readers.
func UpdateAntigravityQuotaState(auth *cliproxyauth.Auth, bodyBytes []byte) {
	if auth == nil || len(bodyBytes) == 0 {
		return
	}

	quotas := parseAntigravityQuotaFromResponse(bodyBytes)
	if len(quotas) == 0 {
		return
	}

	now := time.Now()

	// Track which groups are exhausted and their reset times (using stable group ID as key)
	exhaustedGroups := make(map[string]time.Time)

	// First pass: find exhausted models and their groups
	for modelName, qi := range quotas {
		// Use small threshold to handle floating point precision issues
		if qi.remainingFraction <= 1e-6 {
			groupID := registry.GetAntigravityQuotaGroupID(modelName)

			resetTime := qi.resetTime
			if resetTime.IsZero() || resetTime.Before(now) {
				// Default to 5 minutes if no valid reset time (shorter than before to reduce false blocking)
				resetTime = now.Add(5 * time.Minute)
			}

			// Keep the latest reset time for the group
			if existing, ok := exhaustedGroups[groupID]; !ok || resetTime.After(existing) {
				exhaustedGroups[groupID] = resetTime
			}

			log.Debugf("antigravity quota: model %s (group %s) exhausted, reset at %s", modelName, groupID, resetTime.Format(time.RFC3339))
		}
	}

	// Copy-on-write: create a new map to avoid concurrent read/write issues
	newModelStates := make(map[string]*cliproxyauth.ModelState)
	if auth.ModelStates != nil {
		for k, v := range auth.ModelStates {
			if v != nil {
				newModelStates[k] = v.Clone()
			}
		}
	}

	// Second pass: mark all models in exhausted groups as unavailable
	// and schedule a refresh after the reset time + buffer
	var latestResetTime time.Time
	for groupID, resetTime := range exhaustedGroups {
		if resetTime.After(latestResetTime) {
			latestResetTime = resetTime
		}

		modelsInGroup := registry.GetAntigravityQuotaGroupModels(groupID)
		// If groupID is not a known model, try to get models for it directly
		if len(modelsInGroup) == 0 || (len(modelsInGroup) == 1 && modelsInGroup[0] == groupID) {
			// groupID might be a group name, not a model name
			modelsInGroup = []string{groupID}
		}

		for _, modelName := range modelsInGroup {
			state := newModelStates[modelName]
			if state == nil {
				state = &cliproxyauth.ModelState{Status: cliproxyauth.StatusActive}
				newModelStates[modelName] = state
			}

			// Only update if not already blocked or if new reset time is later
			if !state.Unavailable || resetTime.After(state.NextRetryAfter) {
				state.Unavailable = true
				state.NextRetryAfter = resetTime
				state.Quota = cliproxyauth.QuotaState{
					Exceeded:      true,
					Reason:        "quota_exhausted",
					NextRecoverAt: resetTime,
				}
				state.UpdatedAt = now
				log.Debugf("antigravity quota: marked model %s unavailable until %s", modelName, resetTime.Format(time.RFC3339))
			}
		}
	}

	// Schedule a refresh after the latest reset time + buffer
	if auth.ID != "" && !latestResetTime.IsZero() {
		scheduleQuotaRefresh(auth.ID, latestResetTime)
	}

	// Third pass: clear quota state for models that are not exhausted
	// This runs regardless of whether exhaustedGroups is empty (fixes recovery issue)
	for modelName, qi := range quotas {
		if qi.remainingFraction > 1e-6 {
			groupID := registry.GetAntigravityQuotaGroupID(modelName)
			// Only clear if the group is not exhausted
			if _, exhausted := exhaustedGroups[groupID]; !exhausted {
				if state := newModelStates[modelName]; state != nil && state.Quota.Exceeded {
					state.Unavailable = false
					state.NextRetryAfter = time.Time{}
					state.Quota = cliproxyauth.QuotaState{}
					state.UpdatedAt = now
					log.Debugf("antigravity quota: cleared quota exhausted state for model %s", modelName)
				}
			}
		}
	}

	// Atomic assignment of the new map
	auth.ModelStates = newModelStates
}
