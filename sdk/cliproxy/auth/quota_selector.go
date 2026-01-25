package auth

import (
	"context"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/quota"
)

const (
	quotaUnknownWeight = 0
	quotaWeightPower   = 3
	quotaResetBoost    = 0.25
	quotaResetTau      = 48 * time.Hour
)

type quotaCursor struct {
	current int
}

// QuotaWeightedSelector chooses auths based on remaining quota percentage.
// It uses smooth weighted round-robin within the highest priority tier.
type QuotaWeightedSelector struct {
	mu       sync.Mutex
	cursors  map[string]map[string]*quotaCursor
	fallback RoundRobinSelector
	store    *quota.Store
}

// NewQuotaWeightedSelector constructs a selector that reads quota from auth metadata.
func NewQuotaWeightedSelector() *QuotaWeightedSelector {
	return &QuotaWeightedSelector{}
}

// NewQuotaWeightedSelectorWithStore constructs a selector with a quota store.
func NewQuotaWeightedSelectorWithStore(store *quota.Store) *QuotaWeightedSelector {
	return &QuotaWeightedSelector{store: store}
}

// SetStore sets the quota store for this selector.
func (s *QuotaWeightedSelector) SetStore(store *quota.Store) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.store = store
	s.mu.Unlock()
}

// Pick selects the next auth using quota-aware weighting.
func (s *QuotaWeightedSelector) Pick(ctx context.Context, provider, model string, opts executor.Options, auths []*Auth) (*Auth, error) {
	_ = ctx
	_ = opts
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	if len(available) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if s == nil {
		rr := &RoundRobinSelector{}
		return rr.Pick(ctx, provider, model, opts, auths)
	}

	candidates := make([]*Auth, 0, len(available))
	weights := make([]int, 0, len(available))
	totalWeight := 0
	unknownCount := 0
	for _, candidate := range available {
		weight, known := s.weightFor(candidate, model, now)
		if known && weight <= 0 {
			continue
		}
		if !known {
			unknownCount++
		}
		candidates = append(candidates, candidate)
		weights = append(weights, weight)
		totalWeight += weight
	}
	if len(candidates) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if totalWeight <= 0 {
		if unknownCount > 0 {
			return s.fallback.Pick(ctx, provider, model, opts, candidates)
		}
		return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}

	key := provider + ":" + quota.NormalizeModelKey(model)
	s.mu.Lock()
	if s.cursors == nil {
		s.cursors = make(map[string]map[string]*quotaCursor)
	}
	state := s.cursors[key]
	if state == nil {
		state = make(map[string]*quotaCursor)
		s.cursors[key] = state
	}
	bestIdx := 0
	bestScore := 0
	bestScoreSet := false
	for i, candidate := range candidates {
		cursor := state[candidate.ID]
		if cursor == nil {
			cursor = &quotaCursor{}
			state[candidate.ID] = cursor
		}
		cursor.current += weights[i]
		if !bestScoreSet || cursor.current > bestScore {
			bestScore = cursor.current
			bestIdx = i
			bestScoreSet = true
		}
	}
	state[candidates[bestIdx].ID].current -= totalWeight
	if len(state) > len(candidates) {
		live := make(map[string]struct{}, len(candidates))
		for _, candidate := range candidates {
			live[candidate.ID] = struct{}{}
		}
		for id := range state {
			if _, ok := live[id]; !ok {
				delete(state, id)
			}
		}
	}
	s.mu.Unlock()
	return candidates[bestIdx], nil
}

func (s *QuotaWeightedSelector) weightFor(auth *Auth, model string, now time.Time) (int, bool) {
	if auth == nil {
		return quotaUnknownWeight, false
	}
	lookupModel := model
	if strings.TrimSpace(lookupModel) == "" {
		lookupModel = "*"
	}
	if entry, ok := s.lookupQuota(auth, lookupModel); ok {
		return quotaToWeight(entry, now), true
	}
	return quotaUnknownWeight, false
}

func quotaToWeight(entry quota.ModelQuota, now time.Time) int {
	percent := entry.Percent
	if percent <= 0 {
		return 0
	}
	if percent > 100 {
		percent = 100
	}
	base := math.Pow(percent, quotaWeightPower)
	if base <= 0 {
		return 0
	}
	factor := 1.0
	if !entry.ResetTime.IsZero() {
		remaining := entry.ResetTime.Sub(now)
		if remaining < 0 {
			remaining = 0
		}
		timeScore := math.Exp(-remaining.Seconds() / quotaResetTau.Seconds())
		factor += quotaResetBoost * timeScore
	}
	weight := int(math.Round(base * factor))
	if weight < 0 {
		return 0
	}
	return weight
}

func (s *QuotaWeightedSelector) lookupQuota(auth *Auth, model string) (quota.ModelQuota, bool) {
	if auth == nil {
		return quota.ModelQuota{}, false
	}
	if entry, ok := s.lookupModelQuota(auth, model); ok {
		return entry, true
	}

	base := stripDateSuffix(model)
	if base != model {
		if entry, ok := s.lookupModelQuota(auth, base); ok {
			return entry, true
		}
		if !strings.Contains(base, "thinking") {
			if entry, ok := s.lookupModelQuota(auth, base+"-thinking"); ok {
				return entry, true
			}
		}
	}

	if strings.EqualFold(auth.Provider, "antigravity") && model != "*" {
		groupModels := registry.GetAntigravityQuotaGroupModels(model)
		if entry, ok := s.lookupGroupQuota(auth, groupModels); ok {
			return entry, true
		}
	}

	return quota.ModelQuota{}, false
}

func (s *QuotaWeightedSelector) lookupModelQuota(auth *Auth, model string) (quota.ModelQuota, bool) {
	if auth == nil {
		return quota.ModelQuota{}, false
	}
	if s != nil && s.store != nil {
		if entry, ok := s.store.GetModelQuota(auth.ID, model); ok {
			return entry, true
		}
	}
	return quota.GetModelQuotaFromMetadata(auth.Metadata, model)
}

func (s *QuotaWeightedSelector) lookupGroupQuota(auth *Auth, models []string) (quota.ModelQuota, bool) {
	if auth == nil || len(models) == 0 {
		return quota.ModelQuota{}, false
	}
	found := false
	var min quota.ModelQuota
	for _, model := range models {
		if model == "" {
			continue
		}
		entry, ok := s.lookupModelQuota(auth, model)
		if !ok && strings.HasSuffix(model, "-thinking") {
			base := strings.TrimSuffix(model, "-thinking")
			if base != "" {
				entry, ok = s.lookupModelQuota(auth, base)
			}
		}
		if !ok {
			continue
		}
		if !found || entry.Percent < min.Percent {
			min = entry
			found = true
			continue
		}
		if entry.Percent == min.Percent && !entry.ResetTime.IsZero() {
			if min.ResetTime.IsZero() || entry.ResetTime.Before(min.ResetTime) {
				min = entry
			}
		}
	}
	if !found {
		return quota.ModelQuota{}, false
	}
	return min, true
}

func stripDateSuffix(model string) string {
	if model == "" {
		return model
	}
	parts := strings.Split(model, "-")
	if len(parts) < 2 {
		return model
	}
	last := parts[len(parts)-1]
	if len(last) != 8 || !isAllDigits(last) {
		return model
	}
	return strings.Join(parts[:len(parts)-1], "-")
}

func isAllDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}
