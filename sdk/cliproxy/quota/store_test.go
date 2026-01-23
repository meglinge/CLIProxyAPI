package quota

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStore_SetAndGet(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	models := map[string]ModelQuota{
		"claude-sonnet-4-5": {Percent: 75.5, ResetTime: time.Now().UTC()},
		"*":                 {Percent: 50.0},
	}

	changed := store.Set("auth-123", "antigravity", models, time.Now().UTC())
	if !changed {
		t.Error("expected Set to return true for new entry")
	}

	percent, ok := store.GetPercent("auth-123", "claude-sonnet-4-5")
	if !ok {
		t.Error("expected GetPercent to find model")
	}
	if percent != 75.5 {
		t.Errorf("expected 75.5, got %f", percent)
	}

	percent, ok = store.GetPercent("auth-123", "unknown-model")
	if !ok {
		t.Error("expected GetPercent to fall back to wildcard")
	}
	if percent != 50.0 {
		t.Errorf("expected 50.0 (wildcard), got %f", percent)
	}

	_, ok = store.GetPercent("auth-999", "any")
	if ok {
		t.Error("expected GetPercent to return false for unknown auth")
	}
}

func TestStore_SetNoChange(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	models := map[string]ModelQuota{
		"gpt-4": {Percent: 60.0},
	}

	store.Set("auth-1", "openai", models, time.Now().UTC())

	changed := store.Set("auth-1", "openai", models, time.Now().UTC())
	if changed {
		t.Error("expected Set to return false when data unchanged")
	}
}

func TestStore_FlushAndReload(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	models := map[string]ModelQuota{
		"claude-opus-4": {Percent: 80.0},
	}
	store.Set("auth-abc", "antigravity", models, time.Now().UTC())

	if err := store.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	filePath := filepath.Join(dir, defaultQuotaFileName)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		t.Fatal("expected quota file to exist after flush")
	}

	store2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore reload failed: %v", err)
	}

	percent, ok := store2.GetPercent("auth-abc", "claude-opus-4")
	if !ok {
		t.Error("expected reloaded store to have the entry")
	}
	if percent != 80.0 {
		t.Errorf("expected 80.0, got %f", percent)
	}
}

func TestStore_Delete(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	models := map[string]ModelQuota{"*": {Percent: 100.0}}
	store.Set("auth-del", "test", models, time.Now().UTC())

	store.Delete("auth-del")

	_, ok := store.GetPercent("auth-del", "*")
	if ok {
		t.Error("expected entry to be deleted")
	}
}

func TestStore_GetEntry(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	models := map[string]ModelQuota{
		"model-a": {Percent: 30.0},
		"model-b": {Percent: 70.0},
	}
	store.Set("auth-entry", "provider-x", models, time.Now().UTC())

	entry, ok := store.GetEntry("auth-entry")
	if !ok {
		t.Fatal("expected GetEntry to return entry")
	}
	if entry.Provider != "provider-x" {
		t.Errorf("expected provider-x, got %s", entry.Provider)
	}
	if len(entry.Models) != 2 {
		t.Errorf("expected 2 models, got %d", len(entry.Models))
	}
}

func TestStore_NilStore(t *testing.T) {
	var store *Store

	_, ok := store.GetPercent("any", "any")
	if ok {
		t.Error("expected nil store to return false")
	}

	changed := store.Set("id", "p", nil, time.Now())
	if changed {
		t.Error("expected nil store Set to return false")
	}

	store.Delete("id")

	if err := store.Flush(); err != nil {
		t.Error("expected nil store Flush to not error")
	}
}
