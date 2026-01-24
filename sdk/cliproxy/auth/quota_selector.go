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
	quotaWeightScale   = 100
	quotaUnknownWeight = 100
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

	weights := make([]int, len(available))
	totalWeight := 0
	for i, candidate := range available {
		weight := s.weightFor(candidate, model)
		weights[i] = weight
		totalWeight += weight
	}
	if totalWeight <= 0 {
		return s.fallback.Pick(ctx, provider, model, opts, auths)
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
	for i, candidate := range available {
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
	state[available[bestIdx].ID].current -= totalWeight
	if len(state) > len(available) {
		live := make(map[string]struct{}, len(available))
		for _, candidate := range available {
			live[candidate.ID] = struct{}{}
		}
		for id := range state {
			if _, ok := live[id]; !ok {
				delete(state, id)
			}
		}
	}
	s.mu.Unlock()
	return available[bestIdx], nil
}

func (s *QuotaWeightedSelector) weightFor(auth *Auth, model string) int {
	if auth == nil {
		return quotaUnknownWeight
	}
	lookupModel := model
	if strings.TrimSpace(lookupModel) == "" {
		lookupModel = "*"
	}
	if percent, ok := s.lookupPercent(auth, lookupModel); ok {
		return percentToWeight(percent)
	}

	base := stripDateSuffix(lookupModel)
	if base != lookupModel {
		if percent, ok := s.lookupPercent(auth, base); ok {
			return percentToWeight(percent)
		}
		if !strings.Contains(base, "thinking") {
			if percent, ok := s.lookupPercent(auth, base+"-thinking"); ok {
				return percentToWeight(percent)
			}
		}
	}

	if strings.EqualFold(auth.Provider, "antigravity") && lookupModel != "*" {
		groupModels := registry.GetAntigravityQuotaGroupModels(lookupModel)
		if percent, ok := s.lookupGroupPercent(auth, groupModels); ok {
			return percentToWeight(percent)
		}
	}

	return quotaUnknownWeight
}

func percentToWeight(percent float64) int {
	if percent <= 0 {
		return 0
	}
	if percent > 100 {
		percent = 100
	}
	weight := int(math.Round(percent * quotaWeightScale))
	if weight < 0 {
		return 0
	}
	return weight
}

func (s *QuotaWeightedSelector) lookupPercent(auth *Auth, model string) (float64, bool) {
	if auth == nil {
		return 0, false
	}
	if s != nil && s.store != nil {
		if percent, ok := s.store.GetPercent(auth.ID, model); ok {
			return percent, true
		}
	}
	return quota.GetPercentFromMetadata(auth.Metadata, model)
}

func (s *QuotaWeightedSelector) lookupGroupPercent(auth *Auth, models []string) (float64, bool) {
	if auth == nil || len(models) == 0 {
		return 0, false
	}
	found := false
	min := 0.0
	for _, model := range models {
		if model == "" {
			continue
		}
		if percent, ok := s.lookupPercent(auth, model); ok {
			if !found || percent < min {
				min = percent
				found = true
			}
			continue
		}
		if strings.HasSuffix(model, "-thinking") {
			base := strings.TrimSuffix(model, "-thinking")
			if base != "" {
				if percent, ok := s.lookupPercent(auth, base); ok {
					if !found || percent < min {
						min = percent
						found = true
					}
				}
			}
		}
	}
	if !found {
		return 0, false
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
