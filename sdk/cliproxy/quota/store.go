package quota

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	defaultQuotaFileName = "quota.json"
	schemaVersion        = 1
)

type StoreEntry struct {
	Provider  string                `json:"provider"`
	UpdatedAt time.Time             `json:"updated_at"`
	Models    map[string]ModelQuota `json:"models"`
}

type storeData struct {
	SchemaVersion int                    `json:"schema_version"`
	WrittenAt     time.Time              `json:"written_at"`
	AuthQuotas    map[string]*StoreEntry `json:"auth_quotas"`
}

type Store struct {
	mu       sync.RWMutex
	filePath string
	data     *storeData
	dirty    bool
}

func NewStore(dir string) (*Store, error) {
	if dir == "" {
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			cacheDir = os.TempDir()
		}
		dir = filepath.Join(cacheDir, "cliproxy")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("quota store: create dir failed: %w", err)
	}
	s := &Store{
		filePath: filepath.Join(dir, defaultQuotaFileName),
		data: &storeData{
			SchemaVersion: schemaVersion,
			AuthQuotas:    make(map[string]*StoreEntry),
		},
	}
	if err := s.load(); err != nil {
		return s, nil
	}
	return s, nil
}

func (s *Store) SetPath(path string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.filePath = path
	s.mu.Unlock()
}

func (s *Store) GetPercent(authID, model string) (float64, bool) {
	if s == nil {
		return 0, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.data == nil || s.data.AuthQuotas == nil {
		return 0, false
	}
	entry, ok := s.data.AuthQuotas[authID]
	if !ok || entry == nil || entry.Models == nil {
		return 0, false
	}
	lookup := NormalizeModelKey(model)
	if lookup == "" {
		lookup = "*"
	}
	if mq, ok := entry.Models[lookup]; ok {
		return clampPercent(mq.Percent), true
	}
	if mq, ok := entry.Models["*"]; ok {
		return clampPercent(mq.Percent), true
	}
	return 0, false
}

func (s *Store) GetEntry(authID string) (*StoreEntry, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.data == nil || s.data.AuthQuotas == nil {
		return nil, false
	}
	entry, ok := s.data.AuthQuotas[authID]
	if !ok || entry == nil {
		return nil, false
	}
	copied := &StoreEntry{
		Provider:  entry.Provider,
		UpdatedAt: entry.UpdatedAt,
		Models:    make(map[string]ModelQuota, len(entry.Models)),
	}
	for k, v := range entry.Models {
		copied.Models[k] = v
	}
	return copied, true
}

func (s *Store) Set(authID, provider string, models map[string]ModelQuota, updatedAt time.Time) bool {
	if s == nil || authID == "" || len(models) == 0 {
		return false
	}
	normalized := normalizeModelQuotaMap(models)
	if len(normalized) == 0 {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.data == nil {
		s.data = &storeData{
			SchemaVersion: schemaVersion,
			AuthQuotas:    make(map[string]*StoreEntry),
		}
	}
	if s.data.AuthQuotas == nil {
		s.data.AuthQuotas = make(map[string]*StoreEntry)
	}

	existing := s.data.AuthQuotas[authID]
	if existing != nil && existing.Provider == provider && modelQuotaMapEqual(existing.Models, normalized) {
		return false
	}

	s.data.AuthQuotas[authID] = &StoreEntry{
		Provider:  provider,
		UpdatedAt: updatedAt.UTC(),
		Models:    normalized,
	}
	s.dirty = true
	return true
}

func (s *Store) Delete(authID string) {
	if s == nil || authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil || s.data.AuthQuotas == nil {
		return
	}
	if _, ok := s.data.AuthQuotas[authID]; ok {
		delete(s.data.AuthQuotas, authID)
		s.dirty = true
	}
}

func (s *Store) Flush() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	return s.saveLocked()
}

func (s *Store) load() error {
	if s.filePath == "" {
		return nil
	}
	raw, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("quota store: read failed: %w", err)
	}
	if len(raw) == 0 {
		return nil
	}
	var loaded storeData
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return fmt.Errorf("quota store: unmarshal failed: %w", err)
	}
	if loaded.AuthQuotas == nil {
		loaded.AuthQuotas = make(map[string]*StoreEntry)
	}
	s.data = &loaded
	return nil
}

func (s *Store) saveLocked() error {
	if s.filePath == "" {
		return nil
	}
	s.data.WrittenAt = time.Now().UTC()
	s.data.SchemaVersion = schemaVersion

	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("quota store: marshal failed: %w", err)
	}

	dir := filepath.Dir(s.filePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("quota store: create dir failed: %w", err)
	}

	tmpFile := s.filePath + ".tmp"
	if err := os.WriteFile(tmpFile, raw, 0o600); err != nil {
		return fmt.Errorf("quota store: write tmp failed: %w", err)
	}

	if err := os.Rename(tmpFile, s.filePath); err != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("quota store: rename failed: %w", err)
	}

	s.dirty = false
	return nil
}

func normalizeModelQuotaMap(models map[string]ModelQuota) map[string]ModelQuota {
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

func modelQuotaMapEqual(a, b map[string]ModelQuota) bool {
	if len(a) != len(b) {
		return false
	}
	for key, left := range a {
		right, ok := b[key]
		if !ok {
			return false
		}
		if left.Percent != right.Percent {
			return false
		}
		if !left.ResetTime.Equal(right.ResetTime) {
			return false
		}
	}
	return true
}
