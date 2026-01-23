package quota

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/quota"
	log "github.com/sirupsen/logrus"
)

const (
	defaultPollInterval   = 3 * time.Minute
	defaultRequestTimeout = 20 * time.Second
	maxConcurrentRequests = 5
)

const (
	antigravityUserAgent = "antigravity/1.11.5 windows/amd64"
	codexUserAgent       = "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"
)

var (
	antigravityQuotaPaths = []string{
		"https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels",
		"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:fetchAvailableModels",
		"https://cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels",
	}
	geminiCLIQuotaURL = "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota"
	codexUsageURL     = "https://chatgpt.com/backend-api/wham/usage"
)

// Poller periodically fetches quota data for stored auth entries.
type Poller struct {
	manager        *coreauth.Manager
	interval       time.Duration
	requestTimeout time.Duration
	maxConcurrency int
	aliasMap       map[string]string
	mu             sync.RWMutex
}

// NewPoller constructs a quota poller.
func NewPoller(manager *coreauth.Manager) *Poller {
	if manager == nil {
		return nil
	}
	return &Poller{
		manager:        manager,
		interval:       defaultPollInterval,
		requestTimeout: defaultRequestTimeout,
		maxConcurrency: maxConcurrentRequests,
		aliasMap:       defaultAntigravityAliasMap(),
	}
}

// SetConfig updates the alias map used for antigravity model matching.
func (p *Poller) SetConfig(cfg *config.Config) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.aliasMap = aliasMapFromConfig(cfg)
	p.mu.Unlock()
}

// Start launches the polling loop in a background goroutine.
func (p *Poller) Start(ctx context.Context) {
	if p == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	go p.run(ctx)
	log.Infof("quota poller started (interval=%s)", p.interval)
}

func (p *Poller) run(ctx context.Context) {
	for {
		if ctx != nil && ctx.Err() != nil {
			return
		}
		interval := p.poll(ctx)
		if ctx != nil && ctx.Err() != nil {
			return
		}
		if interval <= 0 {
			interval = p.interval
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}

func (p *Poller) poll(ctx context.Context) time.Duration {
	if p == nil || p.manager == nil {
		return 0
	}
	if ctx == nil {
		ctx = context.Background()
	}
	auths := p.manager.List()
	if len(auths) == 0 {
		return p.interval
	}
	sem := make(chan struct{}, p.maxConcurrency)
	var wg sync.WaitGroup
	for _, auth := range auths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		if shouldSkipAuth(auth) {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		switch provider {
		case "antigravity", "codex", "gemini-cli":
		default:
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return p.interval
		}
		wg.Add(1)
		authCopy := auth
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			switch strings.ToLower(strings.TrimSpace(authCopy.Provider)) {
			case "antigravity":
				p.pollAntigravity(ctx, authCopy)
			case "codex":
				p.pollCodex(ctx, authCopy)
			case "gemini-cli":
				p.pollGeminiCLI(ctx, authCopy)
			default:
				return
			}
		}()
	}
	wg.Wait()
	return p.interval
}

func (p *Poller) pollAntigravity(ctx context.Context, auth *coreauth.Auth) {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("User-Agent", resolveUserAgent(auth, antigravityUserAgent))
	body := []byte("{}")

	paths := p.antigravityURLs(auth)
	if len(paths) == 0 {
		return
	}

	for _, url := range paths {
		status, payload, errReq := p.doRequest(ctx, auth, http.MethodPost, url, body, headers)
		if errReq != nil {
			log.WithError(errReq).Warnf("quota poller: antigravity request failed (auth=%s)", auth.ID)
			continue
		}
		if status < http.StatusOK || status >= http.StatusMultipleChoices {
			log.Warnf("quota poller: antigravity status=%d (auth=%s body=%s)", status, auth.ID, summarizePayload(payload))
			continue
		}
		models := extractAntigravityQuota(payload, p.aliasSnapshot())
		if len(models) == 0 {
			return
		}
		p.persistQuota(ctx, auth, "antigravity", models)
		return
	}
}

func (p *Poller) pollCodex(ctx context.Context, auth *coreauth.Auth) {
	metadata := auth.Metadata
	accountID := resolveCodexAccountID(metadata)
	if accountID == "" {
		log.Warnf("quota poller: codex missing account id (auth=%s)", auth.ID)
		return
	}

	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("User-Agent", codexUserAgent)
	headers.Set("Chatgpt-Account-Id", accountID)

	status, payload, errReq := p.doRequest(ctx, auth, http.MethodGet, codexUsageURL, nil, headers)
	if errReq != nil {
		log.WithError(errReq).Warnf("quota poller: codex request failed (auth=%s)", auth.ID)
		return
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		log.Warnf("quota poller: codex status=%d (auth=%s body=%s)", status, auth.ID, summarizePayload(payload))
		return
	}
	models := extractCodexQuota(payload)
	if len(models) == 0 {
		return
	}
	p.persistQuota(ctx, auth, "codex", models)
}

func (p *Poller) pollGeminiCLI(ctx context.Context, auth *coreauth.Auth) {
	metadata := auth.Metadata
	projectID := resolveGeminiProjectID(metadata)
	if projectID == "" {
		log.Warnf("quota poller: gemini-cli missing project id (auth=%s)", auth.ID)
		return
	}

	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	body, errMarshal := json.Marshal(map[string]string{"project": projectID})
	if errMarshal != nil {
		log.WithError(errMarshal).Warnf("quota poller: gemini-cli request body failed (auth=%s)", auth.ID)
		return
	}

	status, payload, errReq := p.doRequest(ctx, auth, http.MethodPost, geminiCLIQuotaURL, body, headers)
	if errReq != nil {
		log.WithError(errReq).Warnf("quota poller: gemini-cli request failed (auth=%s)", auth.ID)
		return
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		log.Warnf("quota poller: gemini-cli status=%d (auth=%s body=%s)", status, auth.ID, summarizePayload(payload))
		return
	}
	models := extractGeminiQuota(payload)
	if len(models) == 0 {
		return
	}
	p.persistQuota(ctx, auth, "gemini-cli", models)
}

func (p *Poller) doRequest(ctx context.Context, auth *coreauth.Auth, method, targetURL string, body []byte, headers http.Header) (int, []byte, error) {
	if p == nil || p.manager == nil {
		return 0, nil, errors.New("quota poller: manager not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	reqCtx, cancel := context.WithTimeout(ctx, p.requestTimeout)
	defer cancel()

	req, errReq := p.manager.NewHttpRequest(reqCtx, auth, method, targetURL, body, headers)
	if errReq != nil {
		return 0, nil, errReq
	}

	resp, errResp := p.manager.HttpRequest(reqCtx, auth, req)
	if errResp != nil {
		return 0, nil, errResp
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("quota poller: close response body error: %v", errClose)
		}
	}()

	payload, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return resp.StatusCode, nil, errRead
	}
	return resp.StatusCode, payload, nil
}

func (p *Poller) antigravityURLs(auth *coreauth.Auth) []string {
	if auth == nil {
		return antigravityQuotaPaths
	}
	if auth.Attributes != nil {
		if base := strings.TrimSpace(auth.Attributes["base_url"]); base != "" {
			return []string{strings.TrimSuffix(base, "/") + "/v1internal:fetchAvailableModels"}
		}
	}
	if auth.Metadata != nil {
		if base, ok := auth.Metadata["base_url"].(string); ok && strings.TrimSpace(base) != "" {
			return []string{strings.TrimSuffix(strings.TrimSpace(base), "/") + "/v1internal:fetchAvailableModels"}
		}
	}
	return antigravityQuotaPaths
}

func (p *Poller) aliasSnapshot() map[string]string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.aliasMap) == 0 {
		return nil
	}
	out := make(map[string]string, len(p.aliasMap))
	for k, v := range p.aliasMap {
		out[k] = v
	}
	return out
}

func (p *Poller) persistQuota(ctx context.Context, auth *coreauth.Auth, provider string, models map[string]quota.ModelQuota) {
	if p == nil || p.manager == nil || auth == nil || len(models) == 0 {
		return
	}
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	if !quota.UpdateMetadata(updated.Metadata, provider, models, time.Now().UTC()) {
		return
	}
	if _, err := p.manager.Update(ctx, updated); err != nil {
		log.WithError(err).Warnf("quota poller: persist quota failed (auth=%s)", auth.ID)
	}
}
