package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/quota"
)

func TestFillFirstSelectorPick_Deterministic(t *testing.T) {
	t.Parallel()

	selector := &FillFirstSelector{}
	auths := []*Auth{
		{ID: "b"},
		{ID: "a"},
		{ID: "c"},
	}

	got, err := selector.Pick(context.Background(), "gemini", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil {
		t.Fatalf("Pick() auth = nil")
	}
	if got.ID != "a" {
		t.Fatalf("Pick() auth.ID = %q, want %q", got.ID, "a")
	}
}

func TestRoundRobinSelectorPick_CyclesDeterministic(t *testing.T) {
	t.Parallel()

	selector := &RoundRobinSelector{}
	auths := []*Auth{
		{ID: "b"},
		{ID: "a"},
		{ID: "c"},
	}

	want := []string{"a", "b", "c", "a", "b"}
	for i, id := range want {
		got, err := selector.Pick(context.Background(), "gemini", "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		if got == nil {
			t.Fatalf("Pick() #%d auth = nil", i)
		}
		if got.ID != id {
			t.Fatalf("Pick() #%d auth.ID = %q, want %q", i, got.ID, id)
		}
	}
}

func TestRoundRobinSelectorPick_PriorityBuckets(t *testing.T) {
	t.Parallel()

	selector := &RoundRobinSelector{}
	auths := []*Auth{
		{ID: "c", Attributes: map[string]string{"priority": "0"}},
		{ID: "a", Attributes: map[string]string{"priority": "10"}},
		{ID: "b", Attributes: map[string]string{"priority": "10"}},
	}

	want := []string{"a", "b", "a", "b"}
	for i, id := range want {
		got, err := selector.Pick(context.Background(), "mixed", "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		if got == nil {
			t.Fatalf("Pick() #%d auth = nil", i)
		}
		if got.ID != id {
			t.Fatalf("Pick() #%d auth.ID = %q, want %q", i, got.ID, id)
		}
		if got.ID == "c" {
			t.Fatalf("Pick() #%d unexpectedly selected lower priority auth", i)
		}
	}
}

func TestFillFirstSelectorPick_PriorityFallbackCooldown(t *testing.T) {
	t.Parallel()

	selector := &FillFirstSelector{}
	now := time.Now()
	model := "test-model"

	high := &Auth{
		ID:         "high",
		Attributes: map[string]string{"priority": "10"},
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusActive,
				Unavailable:    true,
				NextRetryAfter: now.Add(30 * time.Minute),
				Quota: QuotaState{
					Exceeded: true,
				},
			},
		},
	}
	low := &Auth{ID: "low", Attributes: map[string]string{"priority": "0"}}

	got, err := selector.Pick(context.Background(), "mixed", model, cliproxyexecutor.Options{}, []*Auth{high, low})
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil {
		t.Fatalf("Pick() auth = nil")
	}
	if got.ID != "low" {
		t.Fatalf("Pick() auth.ID = %q, want %q", got.ID, "low")
	}
}

func TestRoundRobinSelectorPick_Concurrent(t *testing.T) {
	selector := &RoundRobinSelector{}
	auths := []*Auth{
		{ID: "b"},
		{ID: "a"},
		{ID: "c"},
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, 1)

	goroutines := 32
	iterations := 100
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				got, err := selector.Pick(context.Background(), "gemini", "", cliproxyexecutor.Options{}, auths)
				if err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				if got == nil {
					select {
					case errCh <- errors.New("Pick() returned nil auth"):
					default:
					}
					return
				}
				if got.ID == "" {
					select {
					case errCh <- errors.New("Pick() returned auth with empty ID"):
					default:
					}
					return
				}
			}
		}()
	}

	close(start)
	wg.Wait()

	select {
	case err := <-errCh:
		t.Fatalf("concurrent Pick() error = %v", err)
	default:
	}
}

func TestQuotaWeightedSelectorWeight_ResetTimeBoost(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 24, 12, 0, 0, 0, time.UTC)
	model := "claude-opus-4-5-20251101"
	soon := &Auth{ID: "soon", Provider: "antigravity", Metadata: map[string]any{}}
	later := &Auth{ID: "later", Provider: "antigravity", Metadata: map[string]any{}}

	quota.UpdateMetadata(soon.Metadata, "antigravity", map[string]quota.ModelQuota{
		model: {Percent: 60, ResetTime: now.Add(2 * time.Hour)},
	}, now)
	quota.UpdateMetadata(later.Metadata, "antigravity", map[string]quota.ModelQuota{
		model: {Percent: 60, ResetTime: now.Add(5 * 24 * time.Hour)},
	}, now)

	selector := &QuotaWeightedSelector{}
	weightSoon, okSoon := selector.weightFor(soon, model, now)
	weightLater, okLater := selector.weightFor(later, model, now)
	if !okSoon || !okLater {
		t.Fatalf("expected quota entries to be found")
	}
	if weightSoon <= weightLater {
		t.Fatalf("expected nearer reset to have higher weight: %d <= %d", weightSoon, weightLater)
	}
}

func TestQuotaWeightedSelectorPick_SkipZeroPercent(t *testing.T) {
	t.Parallel()

	model := "claude-sonnet-4-5"
	zeroA := &Auth{ID: "zero-a", Provider: "antigravity", Metadata: map[string]any{}}
	zeroB := &Auth{ID: "zero-b", Provider: "antigravity", Metadata: map[string]any{}}

	quota.UpdateMetadata(zeroA.Metadata, "antigravity", map[string]quota.ModelQuota{
		model: {Percent: 0},
	}, time.Now())
	quota.UpdateMetadata(zeroB.Metadata, "antigravity", map[string]quota.ModelQuota{
		model: {Percent: 0},
	}, time.Now())

	selector := &QuotaWeightedSelector{}
	_, err := selector.Pick(context.Background(), "antigravity", model, cliproxyexecutor.Options{}, []*Auth{zeroA, zeroB})
	if err == nil {
		t.Fatalf("expected error when all quotas are zero")
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.Code != "auth_not_found" {
		t.Fatalf("unexpected error: %v", err)
	}
}
