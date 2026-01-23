package auth

import (
	"context"
	"math"
	"strings"
	"sync"
	"time"

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
}

// NewQuotaWeightedSelector constructs a selector that reads quota from auth metadata.
func NewQuotaWeightedSelector() *QuotaWeightedSelector {
	return &QuotaWeightedSelector{}
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
	percent, ok := quota.GetPercentFromMetadata(auth.Metadata, lookupModel)
	if !ok {
		return quotaUnknownWeight
	}
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
