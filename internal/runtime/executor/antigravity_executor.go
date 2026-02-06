// Package executor provides runtime execution capabilities for various AI service providers.
// This file implements the Antigravity executor that proxies requests to the antigravity
// upstream using OAuth credentials.
package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	antigravityBaseURLDaily        = "https://daily-cloudcode-pa.googleapis.com"
	antigravitySandboxBaseURLDaily = "https://daily-cloudcode-pa.sandbox.googleapis.com"
	antigravityBaseURLProd         = "https://cloudcode-pa.googleapis.com"
	antigravityCountTokensPath     = "/v1internal:countTokens"
	antigravityStreamPath          = "/v1internal:streamGenerateContent"
	antigravityGeneratePath        = "/v1internal:generateContent"
	antigravityModelsPath          = "/v1internal:fetchAvailableModels"
	antigravityClientID            = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	antigravityClientSecret        = "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"
	defaultAntigravityAgent        = "antigravity/1.104.0 darwin/arm64"
	antigravityAuthType            = "antigravity"
	refreshSkew                    = 3000 * time.Second
	systemInstruction              = "You are Antigravity, a powerful agentic AI coding assistant designed by the Google Deepmind team working on Advanced Agentic Coding.You are pair programming with a USER to solve their coding task. The task may require creating a new codebase, modifying or debugging an existing codebase, or simply answering a question.**Absolute paths only****Proactiveness**"
)

var (
	randSource      = rand.New(rand.NewSource(time.Now().UnixNano()))
	randSourceMutex sync.Mutex
)

// AntigravityExecutor 将请求代理到 antigravity 上游。
type AntigravityExecutor struct {
	cfg *config.Config
}

// NewAntigravityExecutor 创建一个新的 Antigravity 执行器实例。
//
// 参数:
//   - cfg: 应用配置
//
// 返回:
//   - *AntigravityExecutor: 新的 Antigravity 执行器实例
func NewAntigravityExecutor(cfg *config.Config) *AntigravityExecutor {
	return &AntigravityExecutor{cfg: cfg}
}

// Identifier 返回执行器标识符。
func (e *AntigravityExecutor) Identifier() string { return antigravityAuthType }

// PrepareRequest 将 Antigravity 凭证注入到传出的 HTTP 请求中。
func (e *AntigravityExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token, _, errToken := e.ensureAccessToken(req.Context(), auth)
	if errToken != nil {
		return errToken
	}
	if strings.TrimSpace(token) == "" {
		return statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// HttpRequest 将 Antigravity 凭证注入请求并执行。
func (e *AntigravityExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("antigravity executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

// Execute 执行到 Antigravity API 的非流式请求。
func (e *AntigravityExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	isClaude := strings.Contains(strings.ToLower(baseModel), "claude")

	if isClaude || strings.Contains(baseModel, "gemini-3-pro") {
		return e.executeClaudeNonStream(ctx, auth, req, opts)
	}

	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return resp, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("antigravity")

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, false)
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	translated, err = thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	translated = applyPayloadConfigWithRoot(e.cfg, baseModel, "antigravity", "request", translated, originalTranslated, requestedModel)

	baseURLs := antigravityBaseURLFallbackOrder(auth)
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)

	attempts := antigravityRetryAttempts(auth, e.cfg)

attemptLoop:
	for attempt := 0; attempt < attempts; attempt++ {
		var lastStatus int
		var lastBody []byte
		var lastErr error

		for idx, baseURL := range baseURLs {
			httpReq, errReq := e.buildRequest(ctx, auth, token, baseModel, translated, false, opts.Alt, baseURL)
			if errReq != nil {
				err = errReq
				return resp, err
			}

			httpResp, errDo := httpClient.Do(httpReq)
			if errDo != nil {
				recordAPIResponseError(ctx, e.cfg, errDo)
				if errors.Is(errDo, context.Canceled) || errors.Is(errDo, context.DeadlineExceeded) {
					return resp, errDo
				}
				lastStatus = 0
				lastBody = nil
				lastErr = errDo
				if idx+1 < len(baseURLs) {
					log.Debugf("antigravity executor: request error on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
					continue
				}
				err = errDo
				return resp, err
			}

			recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
			bodyBytes, errRead := io.ReadAll(httpResp.Body)
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("antigravity executor: close response body error: %v", errClose)
			}
			if errRead != nil {
				recordAPIResponseError(ctx, e.cfg, errRead)
				err = errRead
				return resp, err
			}
			appendAPIResponseChunk(ctx, e.cfg, bodyBytes)

			if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
				log.Debugf("antigravity executor: upstream error status: %d, body: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), bodyBytes))
				lastStatus = httpResp.StatusCode
				lastBody = append([]byte(nil), bodyBytes...)
				lastErr = nil
				if httpResp.StatusCode == http.StatusTooManyRequests && idx+1 < len(baseURLs) {
					log.Debugf("antigravity executor: rate limited on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
					continue
				}
				if antigravityShouldRetryNoCapacity(httpResp.StatusCode, bodyBytes) {
					if idx+1 < len(baseURLs) {
						log.Debugf("antigravity executor: no capacity on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
						continue
					}
					if attempt+1 < attempts {
						delay := antigravityNoCapacityRetryDelay(attempt)
						log.Debugf("antigravity executor: no capacity for model %s, retrying in %s (attempt %d/%d)", baseModel, delay, attempt+1, attempts)
						if errWait := antigravityWait(ctx, delay); errWait != nil {
							return resp, errWait
						}
						continue attemptLoop
					}
				}
				sErr := statusErr{code: httpResp.StatusCode, msg: string(bodyBytes)}
				if httpResp.StatusCode == http.StatusTooManyRequests {
					if retryAfter, parseErr := parseRetryDelay(bodyBytes); parseErr == nil && retryAfter != nil {
						sErr.retryAfter = retryAfter
					}
				}
				err = sErr
				return resp, err
			}

			reporter.publish(ctx, parseAntigravityUsage(bodyBytes))
			var param any
			converted := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, bodyBytes, &param)
			resp = cliproxyexecutor.Response{Payload: []byte(converted)}
			reporter.ensurePublished(ctx)
			return resp, nil
		}

		switch {
		case lastStatus != 0:
			sErr := statusErr{code: lastStatus, msg: string(lastBody)}
			if lastStatus == http.StatusTooManyRequests {
				if retryAfter, parseErr := parseRetryDelay(lastBody); parseErr == nil && retryAfter != nil {
					sErr.retryAfter = retryAfter
				}
			}
			err = sErr
		case lastErr != nil:
			err = lastErr
		default:
			err = statusErr{code: http.StatusServiceUnavailable, msg: "antigravity executor: no base url available"}
		}
		return resp, err
	}

	return resp, err
}

// executeClaudeNonStream 执行 Claude 到 Antigravity API 的非流式请求。
func (e *AntigravityExecutor) executeClaudeNonStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return resp, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}

	// 检测 web_search 工具 - 如果存在，使用 Gemini 代替 Claude 处理
	if doWebSearchTool(req.Payload) {
		log.Debugf("antigravity executor: web_search tool detected, using Gemini for non-stream: %s", req.Model)
		return e.executeWebSearchOnly(ctx, auth, token, req, opts)
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("antigravity")

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)

	translated, err = thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	translated = applyPayloadConfigWithRoot(e.cfg, baseModel, "antigravity", "request", translated, originalTranslated, requestedModel)

	baseURLs := antigravityBaseURLFallbackOrder(auth)
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)

	attempts := antigravityRetryAttempts(auth, e.cfg)

attemptLoop:
	for attempt := 0; attempt < attempts; attempt++ {
		var lastStatus int
		var lastBody []byte
		var lastErr error

		for idx, baseURL := range baseURLs {
			httpReq, errReq := e.buildRequest(ctx, auth, token, baseModel, translated, true, opts.Alt, baseURL)
			if errReq != nil {
				err = errReq
				return resp, err
			}

			httpResp, errDo := httpClient.Do(httpReq)
			if errDo != nil {
				recordAPIResponseError(ctx, e.cfg, errDo)
				if errors.Is(errDo, context.Canceled) || errors.Is(errDo, context.DeadlineExceeded) {
					return resp, errDo
				}
				lastStatus = 0
				lastBody = nil
				lastErr = errDo
				if idx+1 < len(baseURLs) {
					log.Debugf("antigravity executor: request error on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
					continue
				}
				err = errDo
				return resp, err
			}
			recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
			if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
				bodyBytes, errRead := io.ReadAll(httpResp.Body)
				if errClose := httpResp.Body.Close(); errClose != nil {
					log.Errorf("antigravity executor: close response body error: %v", errClose)
				}
				if errRead != nil {
					recordAPIResponseError(ctx, e.cfg, errRead)
					if errors.Is(errRead, context.Canceled) || errors.Is(errRead, context.DeadlineExceeded) {
						err = errRead
						return resp, err
					}
					if errCtx := ctx.Err(); errCtx != nil {
						err = errCtx
						return resp, err
					}
					lastStatus = 0
					lastBody = nil
					lastErr = errRead
					if idx+1 < len(baseURLs) {
						log.Debugf("antigravity executor: read error on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
						continue
					}
					err = errRead
					return resp, err
				}
				appendAPIResponseChunk(ctx, e.cfg, bodyBytes)
				lastStatus = httpResp.StatusCode
				lastBody = append([]byte(nil), bodyBytes...)
				lastErr = nil
				if httpResp.StatusCode == http.StatusTooManyRequests && idx+1 < len(baseURLs) {
					log.Debugf("antigravity executor: rate limited on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
					continue
				}
				if antigravityShouldRetryNoCapacity(httpResp.StatusCode, bodyBytes) {
					if idx+1 < len(baseURLs) {
						log.Debugf("antigravity executor: no capacity on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
						continue
					}
					if attempt+1 < attempts {
						delay := antigravityNoCapacityRetryDelay(attempt)
						log.Debugf("antigravity executor: no capacity for model %s, retrying in %s (attempt %d/%d)", baseModel, delay, attempt+1, attempts)
						if errWait := antigravityWait(ctx, delay); errWait != nil {
							return resp, errWait
						}
						continue attemptLoop
					}
				}
				sErr := statusErr{code: httpResp.StatusCode, msg: string(bodyBytes)}
				if httpResp.StatusCode == http.StatusTooManyRequests {
					if retryAfter, parseErr := parseRetryDelay(bodyBytes); parseErr == nil && retryAfter != nil {
						sErr.retryAfter = retryAfter
					}
				}
				err = sErr
				return resp, err
			}

			out := make(chan cliproxyexecutor.StreamChunk)
			go func(resp *http.Response) {
				defer close(out)
				defer func() {
					if errClose := resp.Body.Close(); errClose != nil {
						log.Errorf("antigravity executor: close response body error: %v", errClose)
					}
				}()
				scanner := bufio.NewScanner(resp.Body)
				scanner.Buffer(nil, streamScannerBuffer)
				for scanner.Scan() {
					line := scanner.Bytes()
					appendAPIResponseChunk(ctx, e.cfg, line)

					// Filter usage metadata for all models
					// Only retain usage statistics in the terminal chunk
					line = FilterSSEUsageMetadata(line)

					payload := jsonPayload(line)
					if payload == nil {
						continue
					}

					if detail, ok := parseAntigravityStreamUsage(payload); ok {
						reporter.publish(ctx, detail)
					}

					out <- cliproxyexecutor.StreamChunk{Payload: payload}
				}
				if errScan := scanner.Err(); errScan != nil {
					recordAPIResponseError(ctx, e.cfg, errScan)
					reporter.publishFailure(ctx)
					out <- cliproxyexecutor.StreamChunk{Err: errScan}
				} else {
					reporter.ensurePublished(ctx)
				}
			}(httpResp)

			var buffer bytes.Buffer
			for chunk := range out {
				if chunk.Err != nil {
					return resp, chunk.Err
				}
				if len(chunk.Payload) > 0 {
					_, _ = buffer.Write(chunk.Payload)
					_, _ = buffer.Write([]byte("\n"))
				}
			}
			resp = cliproxyexecutor.Response{Payload: e.convertStreamToNonStream(buffer.Bytes())}

			reporter.publish(ctx, parseAntigravityUsage(resp.Payload))
			var param any
			converted := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, resp.Payload, &param)
			resp = cliproxyexecutor.Response{Payload: []byte(converted)}
			reporter.ensurePublished(ctx)

			return resp, nil
		}

		switch {
		case lastStatus != 0:
			sErr := statusErr{code: lastStatus, msg: string(lastBody)}
			if lastStatus == http.StatusTooManyRequests {
				if retryAfter, parseErr := parseRetryDelay(lastBody); parseErr == nil && retryAfter != nil {
					sErr.retryAfter = retryAfter
				}
			}
			err = sErr
		case lastErr != nil:
			err = lastErr
		default:
			err = statusErr{code: http.StatusServiceUnavailable, msg: "antigravity executor: no base url available"}
		}
		return resp, err
	}

	return resp, err
}

func (e *AntigravityExecutor) convertStreamToNonStream(stream []byte) []byte {
	responseTemplate := ""
	var traceID string
	var finishReason string
	var modelVersion string
	var responseID string
	var role string
	var usageRaw string
	parts := make([]map[string]interface{}, 0)
	var pendingKind string
	var pendingText strings.Builder
	var pendingThoughtSig string

	flushPending := func() {
		if pendingKind == "" {
			return
		}
		text := pendingText.String()
		switch pendingKind {
		case "text":
			if strings.TrimSpace(text) == "" {
				pendingKind = ""
				pendingText.Reset()
				pendingThoughtSig = ""
				return
			}
			parts = append(parts, map[string]interface{}{"text": text})
		case "thought":
			if strings.TrimSpace(text) == "" && pendingThoughtSig == "" {
				pendingKind = ""
				pendingText.Reset()
				pendingThoughtSig = ""
				return
			}
			part := map[string]interface{}{"thought": true}
			part["text"] = text
			if pendingThoughtSig != "" {
				part["thoughtSignature"] = pendingThoughtSig
			}
			parts = append(parts, part)
		}
		pendingKind = ""
		pendingText.Reset()
		pendingThoughtSig = ""
	}

	normalizePart := func(partResult gjson.Result) map[string]interface{} {
		var m map[string]interface{}
		_ = json.Unmarshal([]byte(partResult.Raw), &m)
		if m == nil {
			m = map[string]interface{}{}
		}
		sig := partResult.Get("thoughtSignature").String()
		if sig == "" {
			sig = partResult.Get("thought_signature").String()
		}
		if sig != "" {
			m["thoughtSignature"] = sig
			delete(m, "thought_signature")
		}
		if inlineData, ok := m["inline_data"]; ok {
			m["inlineData"] = inlineData
			delete(m, "inline_data")
		}
		return m
	}

	for _, line := range bytes.Split(stream, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || !gjson.ValidBytes(trimmed) {
			continue
		}

		root := gjson.ParseBytes(trimmed)
		responseNode := root.Get("response")
		if !responseNode.Exists() {
			if root.Get("candidates").Exists() {
				responseNode = root
			} else {
				continue
			}
		}
		responseTemplate = responseNode.Raw

		if traceResult := root.Get("traceId"); traceResult.Exists() && traceResult.String() != "" {
			traceID = traceResult.String()
		}

		if roleResult := responseNode.Get("candidates.0.content.role"); roleResult.Exists() {
			role = roleResult.String()
		}

		if finishResult := responseNode.Get("candidates.0.finishReason"); finishResult.Exists() && finishResult.String() != "" {
			finishReason = finishResult.String()
		}

		if modelResult := responseNode.Get("modelVersion"); modelResult.Exists() && modelResult.String() != "" {
			modelVersion = modelResult.String()
		}
		if responseIDResult := responseNode.Get("responseId"); responseIDResult.Exists() && responseIDResult.String() != "" {
			responseID = responseIDResult.String()
		}
		if usageResult := responseNode.Get("usageMetadata"); usageResult.Exists() {
			usageRaw = usageResult.Raw
		} else if usageMetadataResult := root.Get("usageMetadata"); usageMetadataResult.Exists() {
			usageRaw = usageMetadataResult.Raw
		}

		if partsResult := responseNode.Get("candidates.0.content.parts"); partsResult.IsArray() {
			for _, part := range partsResult.Array() {
				hasFunctionCall := part.Get("functionCall").Exists()
				hasInlineData := part.Get("inlineData").Exists() || part.Get("inline_data").Exists()
				sig := part.Get("thoughtSignature").String()
				if sig == "" {
					sig = part.Get("thought_signature").String()
				}
				text := part.Get("text").String()
				thought := part.Get("thought").Bool()

				if hasFunctionCall || hasInlineData {
					flushPending()
					parts = append(parts, normalizePart(part))
					continue
				}

				if thought || part.Get("text").Exists() {
					kind := "text"
					if thought {
						kind = "thought"
					}
					if pendingKind != "" && pendingKind != kind {
						flushPending()
					}
					pendingKind = kind
					pendingText.WriteString(text)
					if kind == "thought" && sig != "" {
						pendingThoughtSig = sig
					}
					continue
				}

				flushPending()
				parts = append(parts, normalizePart(part))
			}
		}
	}
	flushPending()

	if responseTemplate == "" {
		responseTemplate = `{"candidates":[{"content":{"role":"model","parts":[]}}]}`
	}

	partsJSON, _ := json.Marshal(parts)
	responseTemplate, _ = sjson.SetRaw(responseTemplate, "candidates.0.content.parts", string(partsJSON))
	if role != "" {
		responseTemplate, _ = sjson.Set(responseTemplate, "candidates.0.content.role", role)
	}
	if finishReason != "" {
		responseTemplate, _ = sjson.Set(responseTemplate, "candidates.0.finishReason", finishReason)
	}
	if modelVersion != "" {
		responseTemplate, _ = sjson.Set(responseTemplate, "modelVersion", modelVersion)
	}
	if responseID != "" {
		responseTemplate, _ = sjson.Set(responseTemplate, "responseId", responseID)
	}
	if usageRaw != "" {
		responseTemplate, _ = sjson.SetRaw(responseTemplate, "usageMetadata", usageRaw)
	} else if !gjson.Get(responseTemplate, "usageMetadata").Exists() {
		responseTemplate, _ = sjson.Set(responseTemplate, "usageMetadata.promptTokenCount", 0)
		responseTemplate, _ = sjson.Set(responseTemplate, "usageMetadata.candidatesTokenCount", 0)
		responseTemplate, _ = sjson.Set(responseTemplate, "usageMetadata.totalTokenCount", 0)
	}

	output := `{"response":{},"traceId":""}`
	output, _ = sjson.SetRaw(output, "response", responseTemplate)
	if traceID != "" {
		output, _ = sjson.Set(output, "traceId", traceID)
	}
	return []byte(output)
}

// ExecuteStream 执行到 Antigravity API 的流式请求。
func (e *AntigravityExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (stream <-chan cliproxyexecutor.StreamChunk, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	ctx = context.WithValue(ctx, "alt", "")

	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return nil, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	isClaude := strings.Contains(strings.ToLower(req.Model), "claude")

	// Web search 工具 + Claude 模型: 路由到 Gemini 处理 (Claude 原生不支持 web_search)
	if isClaude && doWebSearchTool(req.Payload) {
		log.Debugf("antigravity executor: web_search tool detected, using Gemini for stream: %s", req.Model)
		return e.executeWebSearchOnlyStream(ctx, auth, token, req, opts)
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("antigravity")

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)

	translated, err = thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	translated = applyPayloadConfigWithRoot(e.cfg, baseModel, "antigravity", "request", translated, originalTranslated, requestedModel)

	baseURLs := antigravityBaseURLFallbackOrder(auth)
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)

	attempts := antigravityRetryAttempts(auth, e.cfg)

attemptLoop:
	for attempt := 0; attempt < attempts; attempt++ {
		var lastStatus int
		var lastBody []byte
		var lastErr error

		for idx, baseURL := range baseURLs {
			httpReq, errReq := e.buildRequest(ctx, auth, token, baseModel, translated, true, opts.Alt, baseURL)
			if errReq != nil {
				err = errReq
				return nil, err
			}
			httpResp, errDo := httpClient.Do(httpReq)
			if errDo != nil {
				recordAPIResponseError(ctx, e.cfg, errDo)
				if errors.Is(errDo, context.Canceled) || errors.Is(errDo, context.DeadlineExceeded) {
					return nil, errDo
				}
				lastStatus = 0
				lastBody = nil
				lastErr = errDo
				if idx+1 < len(baseURLs) {
					log.Debugf("antigravity executor: request error on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
					continue
				}
				err = errDo
				return nil, err
			}
			recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
			if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
				bodyBytes, errRead := io.ReadAll(httpResp.Body)
				if errClose := httpResp.Body.Close(); errClose != nil {
					log.Errorf("antigravity executor: close response body error: %v", errClose)
				}
				if errRead != nil {
					recordAPIResponseError(ctx, e.cfg, errRead)
					if errors.Is(errRead, context.Canceled) || errors.Is(errRead, context.DeadlineExceeded) {
						err = errRead
						return nil, err
					}
					if errCtx := ctx.Err(); errCtx != nil {
						err = errCtx
						return nil, err
					}
					lastStatus = 0
					lastBody = nil
					lastErr = errRead
					if idx+1 < len(baseURLs) {
						log.Debugf("antigravity executor: read error on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
						continue
					}
					err = errRead
					return nil, err
				}
				appendAPIResponseChunk(ctx, e.cfg, bodyBytes)
				lastStatus = httpResp.StatusCode
				lastBody = append([]byte(nil), bodyBytes...)
				lastErr = nil
				if httpResp.StatusCode == http.StatusTooManyRequests && idx+1 < len(baseURLs) {
					log.Debugf("antigravity executor: rate limited on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
					continue
				}
				if antigravityShouldRetryNoCapacity(httpResp.StatusCode, bodyBytes) {
					if idx+1 < len(baseURLs) {
						log.Debugf("antigravity executor: no capacity on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
						continue
					}
					if attempt+1 < attempts {
						delay := antigravityNoCapacityRetryDelay(attempt)
						log.Debugf("antigravity executor: no capacity for model %s, retrying in %s (attempt %d/%d)", baseModel, delay, attempt+1, attempts)
						if errWait := antigravityWait(ctx, delay); errWait != nil {
							return nil, errWait
						}
						continue attemptLoop
					}
				}
				sErr := statusErr{code: httpResp.StatusCode, msg: string(bodyBytes)}
				if httpResp.StatusCode == http.StatusTooManyRequests {
					if retryAfter, parseErr := parseRetryDelay(bodyBytes); parseErr == nil && retryAfter != nil {
						sErr.retryAfter = retryAfter
					}
				}
				err = sErr
				return nil, err
			}

		out := make(chan cliproxyexecutor.StreamChunk)
		stream = out
		go func(resp *http.Response) {
			defer close(out)
			defer func() {
				if errClose := resp.Body.Close(); errClose != nil {
					log.Errorf("antigravity executor: close response body error: %v", errClose)
				}
			}()
			scanner := bufio.NewScanner(resp.Body)
			scanner.Buffer(nil, streamScannerBuffer)
			var param any
			for scanner.Scan() {
				line := scanner.Bytes()
				appendAPIResponseChunk(ctx, e.cfg, line)

				// Filter usage metadata for all models
				// Only retain usage statistics in the terminal chunk
				line = FilterSSEUsageMetadata(line)

				payload := jsonPayload(line)
				if payload == nil {
					continue
				}

				if detail, ok := parseAntigravityStreamUsage(payload); ok {
					reporter.publish(ctx, detail)
				}

				chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, bytes.Clone(payload), &param)
					for i := range chunks {
						out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
					}
				}
				tail := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, []byte("[DONE]"), &param)
				for i := range tail {
					out <- cliproxyexecutor.StreamChunk{Payload: []byte(tail[i])}
				}
				if errScan := scanner.Err(); errScan != nil {
					recordAPIResponseError(ctx, e.cfg, errScan)
					reporter.publishFailure(ctx)
					out <- cliproxyexecutor.StreamChunk{Err: errScan}
				} else {
					reporter.ensurePublished(ctx)
				}
			}(httpResp)
			return stream, nil
		}

		switch {
		case lastStatus != 0:
			sErr := statusErr{code: lastStatus, msg: string(lastBody)}
			if lastStatus == http.StatusTooManyRequests {
				if retryAfter, parseErr := parseRetryDelay(lastBody); parseErr == nil && retryAfter != nil {
					sErr.retryAfter = retryAfter
				}
			}
			err = sErr
		case lastErr != nil:
			err = lastErr
		default:
			err = statusErr{code: http.StatusServiceUnavailable, msg: "antigravity executor: no base url available"}
		}
		return nil, err
	}

	return nil, err
}

// Refresh refreshes the authentication credentials using the refresh token.
func (e *AntigravityExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return auth, nil
	}
	updated, errRefresh := e.refreshToken(ctx, auth.Clone())
	if errRefresh != nil {
		return nil, errRefresh
	}
	return updated, nil
}

// CountTokens counts tokens for the given request using the Antigravity API.
func (e *AntigravityExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return cliproxyexecutor.Response{}, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}
	if strings.TrimSpace(token) == "" {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("antigravity")
	respCtx := context.WithValue(ctx, "alt", opts.Alt)

	// Prepare payload once (doesn't depend on baseURL)
	payload := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	payload, err := thinking.ApplyThinking(payload, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	payload = deleteJSONField(payload, "project")
	payload = deleteJSONField(payload, "model")
	payload = deleteJSONField(payload, "request.safetySettings")

	baseURLs := antigravityBaseURLFallbackOrder(auth)
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}

	var lastStatus int
	var lastBody []byte
	var lastErr error

	for idx, baseURL := range baseURLs {
		base := strings.TrimSuffix(baseURL, "/")
		if base == "" {
			base = buildBaseURL(auth)
		}

		var requestURL strings.Builder
		requestURL.WriteString(base)
		requestURL.WriteString(antigravityCountTokensPath)
		if opts.Alt != "" {
			requestURL.WriteString("?$alt=")
			requestURL.WriteString(url.QueryEscape(opts.Alt))
		}

		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), bytes.NewReader(payload))
		if errReq != nil {
			return cliproxyexecutor.Response{}, errReq
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+token)
		httpReq.Header.Set("User-Agent", resolveUserAgent(auth))
		httpReq.Header.Set("Accept", "application/json")
		if host := resolveHost(base); host != "" {
			httpReq.Host = host
		}

		recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
			URL:       requestURL.String(),
			Method:    http.MethodPost,
			Headers:   httpReq.Header.Clone(),
			Body:      payload,
			Provider:  e.Identifier(),
			AuthID:    authID,
			AuthLabel: authLabel,
			AuthType:  authType,
			AuthValue: authValue,
		})

		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			recordAPIResponseError(ctx, e.cfg, errDo)
			if errors.Is(errDo, context.Canceled) || errors.Is(errDo, context.DeadlineExceeded) {
				return cliproxyexecutor.Response{}, errDo
			}
			lastStatus = 0
			lastBody = nil
			lastErr = errDo
			if idx+1 < len(baseURLs) {
				log.Debugf("antigravity executor: request error on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
				continue
			}
			return cliproxyexecutor.Response{}, errDo
		}

		recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
		bodyBytes, errRead := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("antigravity executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			recordAPIResponseError(ctx, e.cfg, errRead)
			return cliproxyexecutor.Response{}, errRead
		}
		appendAPIResponseChunk(ctx, e.cfg, bodyBytes)

		if httpResp.StatusCode >= http.StatusOK && httpResp.StatusCode < http.StatusMultipleChoices {
			count := gjson.GetBytes(bodyBytes, "totalTokens").Int()
			translated := sdktranslator.TranslateTokenCount(respCtx, to, from, count, bodyBytes)
			return cliproxyexecutor.Response{Payload: []byte(translated)}, nil
		}

		lastStatus = httpResp.StatusCode
		lastBody = append([]byte(nil), bodyBytes...)
		lastErr = nil
		if httpResp.StatusCode == http.StatusTooManyRequests && idx+1 < len(baseURLs) {
			log.Debugf("antigravity executor: rate limited on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
			continue
		}
		sErr := statusErr{code: httpResp.StatusCode, msg: string(bodyBytes)}
		if httpResp.StatusCode == http.StatusTooManyRequests {
			if retryAfter, parseErr := parseRetryDelay(bodyBytes); parseErr == nil && retryAfter != nil {
				sErr.retryAfter = retryAfter
			}
		}
		return cliproxyexecutor.Response{}, sErr
	}

	switch {
	case lastStatus != 0:
		sErr := statusErr{code: lastStatus, msg: string(lastBody)}
		if lastStatus == http.StatusTooManyRequests {
			if retryAfter, parseErr := parseRetryDelay(lastBody); parseErr == nil && retryAfter != nil {
				sErr.retryAfter = retryAfter
			}
		}
		return cliproxyexecutor.Response{}, sErr
	case lastErr != nil:
		return cliproxyexecutor.Response{}, lastErr
	default:
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusServiceUnavailable, msg: "antigravity executor: no base url available"}
	}
}

// FetchAntigravityModels retrieves available models using the supplied auth.
func FetchAntigravityModels(ctx context.Context, auth *cliproxyauth.Auth, cfg *config.Config) []*registry.ModelInfo {
	exec := &AntigravityExecutor{cfg: cfg}
	token, updatedAuth, errToken := exec.ensureAccessToken(ctx, auth)
	if errToken != nil || token == "" {
		return nil
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}

	baseURLs := antigravityBaseURLFallbackOrder(auth)
	httpClient := newProxyAwareHTTPClient(ctx, cfg, auth, 0)

	for idx, baseURL := range baseURLs {
		modelsURL := baseURL + antigravityModelsPath
		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, modelsURL, bytes.NewReader([]byte(`{}`)))
		if errReq != nil {
			return nil
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+token)
		httpReq.Header.Set("User-Agent", resolveUserAgent(auth))
		if host := resolveHost(baseURL); host != "" {
			httpReq.Host = host
		}

		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			if errors.Is(errDo, context.Canceled) || errors.Is(errDo, context.DeadlineExceeded) {
				return nil
			}
			if idx+1 < len(baseURLs) {
				log.Debugf("antigravity executor: models request error on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
				continue
			}
			return nil
		}

		bodyBytes, errRead := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("antigravity executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			if idx+1 < len(baseURLs) {
				log.Debugf("antigravity executor: models read error on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
				continue
			}
			return nil
		}
		if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
			if httpResp.StatusCode == http.StatusTooManyRequests && idx+1 < len(baseURLs) {
				log.Debugf("antigravity executor: models request rate limited on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
				continue
			}
			return nil
		}

		result := gjson.GetBytes(bodyBytes, "models")
		if !result.Exists() {
			return nil
		}

		// Update auth quota state based on API response (proactive quota tracking)
		UpdateAntigravityQuotaState(auth, bodyBytes)

		now := time.Now().Unix()
		modelConfig := registry.GetAntigravityModelConfig()
		models := make([]*registry.ModelInfo, 0, len(result.Map()))
		for originalName, modelData := range result.Map() {
			modelID := strings.TrimSpace(originalName)
			if modelID == "" {
				continue
			}
			switch modelID {
			case "chat_20706", "chat_23310", "gemini-2.5-flash-thinking", "gemini-3-pro-low", "gemini-2.5-pro":
				continue
			}
			modelCfg := modelConfig[modelID]

			// Extract displayName from upstream response, fallback to modelID
			displayName := modelData.Get("displayName").String()
			if displayName == "" {
				displayName = modelID
			}

			modelInfo := &registry.ModelInfo{
				ID:          modelID,
				Name:        modelID,
				Description: displayName,
				DisplayName: displayName,
				Version:     modelID,
				Object:      "model",
				Created:     now,
				OwnedBy:     antigravityAuthType,
				Type:        antigravityAuthType,
			}
			// Look up Thinking support from static config using upstream model name.
			if modelCfg != nil {
				if modelCfg.Thinking != nil {
					modelInfo.Thinking = modelCfg.Thinking
				}
				if modelCfg.MaxCompletionTokens > 0 {
					modelInfo.MaxCompletionTokens = modelCfg.MaxCompletionTokens
				}
			}
			models = append(models, modelInfo)
		}
		return models
	}
	return nil
}

func (e *AntigravityExecutor) ensureAccessToken(ctx context.Context, auth *cliproxyauth.Auth) (string, *cliproxyauth.Auth, error) {
	if auth == nil {
		return "", nil, statusErr{code: http.StatusUnauthorized, msg: "missing auth"}
	}
	accessToken := metaStringValue(auth.Metadata, "access_token")
	expiry := tokenExpiry(auth.Metadata)
	if accessToken != "" && expiry.After(time.Now().Add(refreshSkew)) {
		return accessToken, nil, nil
	}
	refreshCtx := context.Background()
	if ctx != nil {
		if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
			refreshCtx = context.WithValue(refreshCtx, "cliproxy.roundtripper", rt)
		}
	}
	updated, errRefresh := e.refreshToken(refreshCtx, auth.Clone())
	if errRefresh != nil {
		return "", nil, errRefresh
	}
	return metaStringValue(updated.Metadata, "access_token"), updated, nil
}

func (e *AntigravityExecutor) refreshToken(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "missing auth"}
	}
	refreshToken := metaStringValue(auth.Metadata, "refresh_token")
	if refreshToken == "" {
		return auth, statusErr{code: http.StatusUnauthorized, msg: "missing refresh token"}
	}

	form := url.Values{}
	form.Set("client_id", antigravityClientID)
	form.Set("client_secret", antigravityClientSecret)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
	if errReq != nil {
		return auth, errReq
	}
	httpReq.Header.Set("Host", "oauth2.googleapis.com")
	httpReq.Header.Set("User-Agent", defaultAntigravityAgent)
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		return auth, errDo
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("antigravity executor: close response body error: %v", errClose)
		}
	}()

	bodyBytes, errRead := io.ReadAll(httpResp.Body)
	if errRead != nil {
		return auth, errRead
	}

	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		sErr := statusErr{code: httpResp.StatusCode, msg: string(bodyBytes)}
		if httpResp.StatusCode == http.StatusTooManyRequests {
			if retryAfter, parseErr := parseRetryDelay(bodyBytes); parseErr == nil && retryAfter != nil {
				sErr.retryAfter = retryAfter
			}
		}
		return auth, sErr
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if errUnmarshal := json.Unmarshal(bodyBytes, &tokenResp); errUnmarshal != nil {
		return auth, errUnmarshal
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = tokenResp.AccessToken
	if tokenResp.RefreshToken != "" {
		auth.Metadata["refresh_token"] = tokenResp.RefreshToken
	}
	auth.Metadata["expires_in"] = tokenResp.ExpiresIn
	now := time.Now()
	auth.Metadata["timestamp"] = now.UnixMilli()
	auth.Metadata["expired"] = now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)
	auth.Metadata["type"] = antigravityAuthType
	if errProject := e.ensureAntigravityProjectID(ctx, auth, tokenResp.AccessToken); errProject != nil {
		log.Warnf("antigravity executor: ensure project id failed: %v", errProject)
	}
	return auth, nil
}

func (e *AntigravityExecutor) ensureAntigravityProjectID(ctx context.Context, auth *cliproxyauth.Auth, accessToken string) error {
	if auth == nil {
		return nil
	}

	if auth.Metadata["project_id"] != nil {
		return nil
	}

	token := strings.TrimSpace(accessToken)
	if token == "" {
		token = metaStringValue(auth.Metadata, "access_token")
	}
	if token == "" {
		return nil
	}

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	projectID, errFetch := sdkAuth.FetchAntigravityProjectID(ctx, token, httpClient)
	if errFetch != nil {
		return errFetch
	}
	if strings.TrimSpace(projectID) == "" {
		return nil
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["project_id"] = strings.TrimSpace(projectID)

	return nil
}

func (e *AntigravityExecutor) buildRequest(ctx context.Context, auth *cliproxyauth.Auth, token, modelName string, payload []byte, stream bool, alt, baseURL string) (*http.Request, error) {
	if token == "" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
	}

	base := strings.TrimSuffix(baseURL, "/")
	if base == "" {
		base = buildBaseURL(auth)
	}
	path := antigravityGeneratePath
	if stream {
		path = antigravityStreamPath
	}
	var requestURL strings.Builder
	requestURL.WriteString(base)
	requestURL.WriteString(path)
	if stream {
		if alt != "" {
			requestURL.WriteString("?$alt=")
			requestURL.WriteString(url.QueryEscape(alt))
		} else {
			requestURL.WriteString("?alt=sse")
		}
	} else if alt != "" {
		requestURL.WriteString("?$alt=")
		requestURL.WriteString(url.QueryEscape(alt))
	}

	// Extract project_id from auth metadata if available
	projectID := ""
	if auth != nil && auth.Metadata != nil {
		if pid, ok := auth.Metadata["project_id"].(string); ok {
			projectID = strings.TrimSpace(pid)
		}
	}
	payload = geminiToAntigravity(modelName, payload, projectID)
	payload, _ = sjson.SetBytes(payload, "model", modelName)

	useAntigravitySchema := strings.Contains(modelName, "claude") || strings.Contains(modelName, "gemini-3-pro-high")
	payloadStr := string(payload)
	paths := make([]string, 0)
	util.Walk(gjson.Parse(payloadStr), "", "parametersJsonSchema", &paths)
	for _, p := range paths {
		payloadStr, _ = util.RenameKey(payloadStr, p, p[:len(p)-len("parametersJsonSchema")]+"parameters")
	}

	if useAntigravitySchema {
		payloadStr = util.CleanJSONSchemaForAntigravity(payloadStr)
	} else {
		payloadStr = util.CleanJSONSchemaForGemini(payloadStr)
	}

	if useAntigravitySchema {
		systemInstructionPartsResult := gjson.Get(payloadStr, "request.systemInstruction.parts")
		payloadStr, _ = sjson.Set(payloadStr, "request.systemInstruction.role", "user")
		payloadStr, _ = sjson.Set(payloadStr, "request.systemInstruction.parts.0.text", systemInstruction)
		payloadStr, _ = sjson.Set(payloadStr, "request.systemInstruction.parts.1.text", fmt.Sprintf("Please ignore following [ignore]%s[/ignore]", systemInstruction))

		if systemInstructionPartsResult.Exists() && systemInstructionPartsResult.IsArray() {
			for _, partResult := range systemInstructionPartsResult.Array() {
				payloadStr, _ = sjson.SetRaw(payloadStr, "request.systemInstruction.parts.-1", partResult.Raw)
			}
		}
	}

	if strings.Contains(modelName, "claude") {
		payloadStr, _ = sjson.Set(payloadStr, "request.toolConfig.functionCallingConfig.mode", "VALIDATED")
	} else {
		payloadStr, _ = sjson.Delete(payloadStr, "request.generationConfig.maxOutputTokens")
	}

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), strings.NewReader(payloadStr))
	if errReq != nil {
		return nil, errReq
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("User-Agent", resolveUserAgent(auth))
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	if host := resolveHost(base); host != "" {
		httpReq.Host = host
	}

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	var payloadLog []byte
	if e.cfg != nil && e.cfg.RequestLog {
		payloadLog = []byte(payloadStr)
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       requestURL.String(),
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      payloadLog,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	return httpReq, nil
}

func tokenExpiry(metadata map[string]any) time.Time {
	if metadata == nil {
		return time.Time{}
	}
	if expStr, ok := metadata["expired"].(string); ok {
		expStr = strings.TrimSpace(expStr)
		if expStr != "" {
			if parsed, errParse := time.Parse(time.RFC3339, expStr); errParse == nil {
				return parsed
			}
		}
	}
	expiresIn, hasExpires := int64Value(metadata["expires_in"])
	tsMs, hasTimestamp := int64Value(metadata["timestamp"])
	if hasExpires && hasTimestamp {
		return time.Unix(0, tsMs*int64(time.Millisecond)).Add(time.Duration(expiresIn) * time.Second)
	}
	return time.Time{}
}

func metaStringValue(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	if v, ok := metadata[key]; ok {
		switch typed := v.(type) {
		case string:
			return strings.TrimSpace(typed)
		case []byte:
			return strings.TrimSpace(string(typed))
		}
	}
	return ""
}

func int64Value(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		return int64(typed), true
	case json.Number:
		if i, errParse := typed.Int64(); errParse == nil {
			return i, true
		}
	case string:
		if strings.TrimSpace(typed) == "" {
			return 0, false
		}
		if i, errParse := strconv.ParseInt(strings.TrimSpace(typed), 10, 64); errParse == nil {
			return i, true
		}
	}
	return 0, false
}

func buildBaseURL(auth *cliproxyauth.Auth) string {
	if baseURLs := antigravityBaseURLFallbackOrder(auth); len(baseURLs) > 0 {
		return baseURLs[0]
	}
	return antigravityBaseURLDaily
}

func resolveHost(base string) string {
	parsed, errParse := url.Parse(base)
	if errParse != nil {
		return ""
	}
	if parsed.Host != "" {
		return parsed.Host
	}
	return strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
}

func resolveUserAgent(auth *cliproxyauth.Auth) string {
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
	return defaultAntigravityAgent
}

func antigravityRetryAttempts(auth *cliproxyauth.Auth, cfg *config.Config) int {
	retry := 0
	if cfg != nil {
		retry = cfg.RequestRetry
	}
	if auth != nil {
		if override, ok := auth.RequestRetryOverride(); ok {
			retry = override
		}
	}
	if retry < 0 {
		retry = 0
	}
	attempts := retry + 1
	if attempts < 1 {
		return 1
	}
	return attempts
}

func antigravityShouldRetryNoCapacity(statusCode int, body []byte) bool {
	if statusCode != http.StatusServiceUnavailable {
		return false
	}
	if len(body) == 0 {
		return false
	}
	msg := strings.ToLower(string(body))
	return strings.Contains(msg, "no capacity available")
}

func antigravityNoCapacityRetryDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := time.Duration(attempt+1) * 250 * time.Millisecond
	if delay > 2*time.Second {
		delay = 2 * time.Second
	}
	return delay
}

func antigravityWait(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func antigravityBaseURLFallbackOrder(auth *cliproxyauth.Auth) []string {
	if base := resolveCustomAntigravityBaseURL(auth); base != "" {
		return []string{base}
	}
	return []string{
		antigravityBaseURLDaily,
		antigravitySandboxBaseURLDaily,
		// antigravityBaseURLProd,
	}
}

func resolveCustomAntigravityBaseURL(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["base_url"]); v != "" {
			return strings.TrimSuffix(v, "/")
		}
	}
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["base_url"].(string); ok {
			v = strings.TrimSpace(v)
			if v != "" {
				return strings.TrimSuffix(v, "/")
			}
		}
	}
	return ""
}

func geminiToAntigravity(modelName string, payload []byte, projectID string) []byte {
	template, _ := sjson.Set(string(payload), "model", modelName)
	template, _ = sjson.Set(template, "userAgent", "antigravity")
	template, _ = sjson.Set(template, "requestType", "agent")

	// Use real project ID from auth if available, otherwise generate random (legacy fallback)
	if projectID != "" {
		template, _ = sjson.Set(template, "project", projectID)
	} else {
		template, _ = sjson.Set(template, "project", generateProjectID())
	}
	template, _ = sjson.Set(template, "requestId", generateRequestID())
	template, _ = sjson.Set(template, "request.sessionId", generateStableSessionID(payload))

	template, _ = sjson.Delete(template, "request.safetySettings")
	if toolConfig := gjson.Get(template, "toolConfig"); toolConfig.Exists() && !gjson.Get(template, "request.toolConfig").Exists() {
		template, _ = sjson.SetRaw(template, "request.toolConfig", toolConfig.Raw)
		template, _ = sjson.Delete(template, "toolConfig")
	}
	return []byte(template)
}

func generateRequestID() string {
	return "agent-" + uuid.NewString()
}

func generateSessionID() string {
	randSourceMutex.Lock()
	n := randSource.Int63n(9_000_000_000_000_000_000)
	randSourceMutex.Unlock()
	return "-" + strconv.FormatInt(n, 10)
}

func generateStableSessionID(payload []byte) string {
	contents := gjson.GetBytes(payload, "request.contents")
	if contents.IsArray() {
		for _, content := range contents.Array() {
			if content.Get("role").String() == "user" {
				text := content.Get("parts.0.text").String()
				if text != "" {
					h := sha256.Sum256([]byte(text))
					n := int64(binary.BigEndian.Uint64(h[:8])) & 0x7FFFFFFFFFFFFFFF
					return "-" + strconv.FormatInt(n, 10)
				}
			}
		}
	}
	return generateSessionID()
}

func generateProjectID() string {
	adjectives := []string{"useful", "bright", "swift", "calm", "bold"}
	nouns := []string{"fuze", "wave", "spark", "flow", "core"}
	randSourceMutex.Lock()
	adj := adjectives[randSource.Intn(len(adjectives))]
	noun := nouns[randSource.Intn(len(nouns))]
	randSourceMutex.Unlock()
	randomPart := strings.ToLower(uuid.NewString())[:5]
	return adj + "-" + noun + "-" + randomPart
}

// (deprecated duplicate web search helpers removed)

// antigravityEffectiveMaxTokens returns the max tokens to cap thinking:
// prefer request-provided maxOutputTokens; otherwise fall back to model default.
// The boolean indicates whether the value came from the model default (and thus should be written back).
func antigravityEffectiveMaxTokens(model string, payload []byte) (max int, fromModel bool) {
	if maxTok := gjson.GetBytes(payload, "request.generationConfig.maxOutputTokens"); maxTok.Exists() && maxTok.Int() > 0 {
		return int(maxTok.Int()), false
	}
	if modelInfo := registry.GetGlobalRegistry().GetModelInfo(model, "antigravity"); modelInfo != nil && modelInfo.MaxCompletionTokens > 0 {
		return modelInfo.MaxCompletionTokens, true
	}
	return 0, false
}

// antigravityMinThinkingBudget returns the minimum thinking budget for a model.
// Falls back to -1 if no model info is found.
func antigravityMinThinkingBudget(model string) int {
	if modelInfo := registry.GetGlobalRegistry().GetModelInfo(model, "antigravity"); modelInfo != nil && modelInfo.Thinking != nil {
		return modelInfo.Thinking.Min
	}
	return -1
}

// 通过 gemini-2.5-flash 的 googleSearch 为 Claude 模型提供 Web 搜索支持

const webSearchGeminiModel = "gemini-2.5-flash"

// googleURLRegex 用于匹配所有 google.com 相关的 URL
var googleURLRegex = regexp.MustCompile(`https?://[a-zA-Z0-9.-]*google\.com[^\s]*`)

// stripGoogleURLs 移除文本中所有 google.com 相关的 URL
func stripGoogleURLs(text string) string {
	return strings.TrimSpace(googleURLRegex.ReplaceAllString(text, ""))
}

// doWebSearchTool 检查原始请求中是否包含 web_search 工具
func doWebSearchTool(payload []byte) bool {
	tools := gjson.GetBytes(payload, "tools")
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		toolType := tool.Get("type").String()
		if strings.HasPrefix(toolType, "web_search") {
			return true
		}
	}
	return false
}

// extractUserQuery 提取最后一条用户消息文本用于 Web 搜索
func extractUserQuery(payload []byte) string {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return ""
	}
	arr := messages.Array()
	// 查找最后一条用户消息
	for i := len(arr) - 1; i >= 0; i-- {
		msg := arr[i]
		if msg.Get("role").String() == "user" {
			content := msg.Get("content")
			if content.Type == gjson.String {
				return content.String()
			}
			// 尝试数组格式
			if content.IsArray() {
				for _, item := range content.Array() {
					if item.Get("type").String() == "text" {
						return item.Get("text").String()
					}
				}
			}
		}
	}
	return ""
}

// executeGeminiWebSearch 使用 Gemini 的 googleSearch 工具执行 Web 搜索
// 返回完整的响应体（包含文本和 groundingMetadata）
func (e *AntigravityExecutor) executeGeminiWebSearch(ctx context.Context, auth *cliproxyauth.Auth, token, query string) ([]byte, error) {
	if query == "" {
		return nil, fmt.Errorf("empty query")
	}

	// 构建带有 googleSearch 工具的 Gemini 请求
	geminiPayload := `{"model":"","request":{"contents":[],"tools":[{"googleSearch":{}}]}}`
	geminiPayload, _ = sjson.Set(geminiPayload, "model", webSearchGeminiModel)
	geminiPayload, _ = sjson.Set(geminiPayload, "request.contents.0.role", "user")
	geminiPayload, _ = sjson.Set(geminiPayload, "request.contents.0.parts.0.text", query)

	// 应用项目 ID
	projectID := ""
	if auth != nil && auth.Metadata != nil {
		if pid, ok := auth.Metadata["project_id"].(string); ok {
			projectID = strings.TrimSpace(pid)
		}
	}
	geminiPayload = string(geminiToAntigravity(webSearchGeminiModel, []byte(geminiPayload), projectID))

	baseURLs := antigravityBaseURLFallbackOrder(auth)
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)

	for _, baseURL := range baseURLs {
		base := strings.TrimSuffix(baseURL, "/")
		requestURL := base + antigravityGeneratePath

		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader([]byte(geminiPayload)))
		if errReq != nil {
			continue
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+token)
		httpReq.Header.Set("User-Agent", resolveUserAgent(auth))
		httpReq.Header.Set("Accept", "application/json")
		if host := resolveHost(base); host != "" {
			httpReq.Host = host
		}

		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			log.Debugf("antigravity web search: request failed: %v", errDo)
			continue
		}

		bodyBytes, errRead := io.ReadAll(httpResp.Body)
		_ = httpResp.Body.Close()
		if errRead != nil {
			continue
		}

		if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
			log.Debugf("antigravity web search: upstream error status: %d", httpResp.StatusCode)
			continue
		}

		log.Debugf("antigravity web search: got response for query: %s", query)
		return bodyBytes, nil
	}

	return nil, fmt.Errorf("web search failed")
}

// executeWebSearchOnly 使用 Gemini 代替 Claude 处理 Web 搜索请求
// 这是一个非流式实现，返回 Claude 格式的响应
func (e *AntigravityExecutor) executeWebSearchOnly(ctx context.Context, auth *cliproxyauth.Auth, token string, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	reporter := newUsageReporter(ctx, e.Identifier(), req.Model, auth)

	query := extractUserQuery(req.Payload)
	if query == "" {
		reporter.publishFailure(ctx)
		return cliproxyexecutor.Response{}, fmt.Errorf("no user query found for web search")
	}

	// 执行 Gemini Web 搜索
	geminiResp, err := e.executeGeminiWebSearch(ctx, auth, token, query)
	if err != nil {
		reporter.publishFailure(ctx)
		return cliproxyexecutor.Response{}, err
	}

	// 发布 Gemini 响应的 usage 统计
	reporter.publish(ctx, parseAntigravityUsage(geminiResp))

	// 将 Gemini 响应转换为 Claude 格式
	claudeResp := convertGeminiToClaudeNonStream(req.Model, geminiResp)
	reporter.ensurePublished(ctx)

	return cliproxyexecutor.Response{Payload: []byte(claudeResp)}, nil
}

// executeWebSearchOnlyStream 使用 Gemini 代替 Claude 处理 Web 搜索请求
// 这是一个流式实现，返回 Claude SSE 格式的响应
func (e *AntigravityExecutor) executeWebSearchOnlyStream(ctx context.Context, auth *cliproxyauth.Auth, token string, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (<-chan cliproxyexecutor.StreamChunk, error) {
	reporter := newUsageReporter(ctx, e.Identifier(), req.Model, auth)

	query := extractUserQuery(req.Payload)
	if query == "" {
		reporter.publishFailure(ctx)
		return nil, fmt.Errorf("no user query found for web search")
	}

	// 执行 Gemini Web 搜索（非流式，然后转换为流式格式）
	geminiResp, err := e.executeGeminiWebSearch(ctx, auth, token, query)
	if err != nil {
		reporter.publishFailure(ctx)
		return nil, err
	}

	// 发布 Gemini 响应的 usage 统计
	reporter.publish(ctx, parseAntigravityUsage(geminiResp))

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)

		// 将 Gemini 响应转换为 Claude SSE 流
		sseEvents := convertGeminiToClaudeSSEStream(req.Model, geminiResp)
		for _, event := range sseEvents {
			out <- cliproxyexecutor.StreamChunk{Payload: []byte(event)}
		}

		reporter.ensurePublished(ctx)
	}()

	return out, nil
}

// convertGeminiToClaudeNonStream 将 Gemini 响应转换为 Claude 非流式格式
func convertGeminiToClaudeNonStream(model string, geminiResp []byte) string {
	// 从 Gemini 响应中提取数据
	// 首先尝试包装格式 (response.candidates...)，然后尝试顶层格式 (candidates...)
	textContent := ""
	if parts := gjson.GetBytes(geminiResp, "response.candidates.0.content.parts"); parts.IsArray() {
		for _, part := range parts.Array() {
			if text := part.Get("text"); text.Exists() {
				textContent += text.String()
			}
		}
	} else if parts := gjson.GetBytes(geminiResp, "candidates.0.content.parts"); parts.IsArray() {
		for _, part := range parts.Array() {
			if text := part.Get("text"); text.Exists() {
				textContent += text.String()
			}
		}
	}

	// 过滤 textContent 中的 google.com 相关 URL
	textContent = stripGoogleURLs(textContent)

	groundingMetadata := gjson.GetBytes(geminiResp, "response.candidates.0.groundingMetadata")
	if !groundingMetadata.Exists() {
		groundingMetadata = gjson.GetBytes(geminiResp, "candidates.0.groundingMetadata")
	}

	// 从 Gemini 响应中获取 usage
	inputTokens := gjson.GetBytes(geminiResp, "response.usageMetadata.promptTokenCount").Int()
	if inputTokens == 0 {
		inputTokens = gjson.GetBytes(geminiResp, "usageMetadata.promptTokenCount").Int()
	}
	outputTokens := gjson.GetBytes(geminiResp, "response.usageMetadata.candidatesTokenCount").Int()
	if outputTokens == 0 {
		outputTokens = gjson.GetBytes(geminiResp, "usageMetadata.candidatesTokenCount").Int()
	}

	// 构建 Claude 响应
	msgID := fmt.Sprintf("msg_%s", uuid.New().String()[:24])
	toolUseID := fmt.Sprintf("srvtoolu_%d", time.Now().UnixNano())

	// 从 webSearchQueries 构建搜索查询
	searchQuery := ""
	if queries := groundingMetadata.Get("webSearchQueries"); queries.IsArray() && len(queries.Array()) > 0 {
		searchQuery = queries.Array()[0].String()
	}

	// 构建 content 数组
	content := []map[string]interface{}{}

	// 1. server_tool_use 块
	serverToolUse := map[string]interface{}{
		"type":  "server_tool_use",
		"id":    toolUseID,
		"name":  "web_search",
		"input": map[string]interface{}{"query": searchQuery},
	}
	content = append(content, serverToolUse)

	// 2. web_search_tool_result 块（过滤 vertexaisearch.cloud.google.com URL）
	webSearchResults := []map[string]interface{}{}
	groundingChunks := groundingMetadata.Get("groundingChunks")
	if groundingChunks.IsArray() {
		for _, chunk := range groundingChunks.Array() {
			web := chunk.Get("web")
			if web.Exists() {
				result := map[string]interface{}{
					"type":     "web_search_result",
					"page_age": nil,
				}
				if title := web.Get("title"); title.Exists() {
					result["title"] = title.String()
				}
				// 只有不包含 vertexaisearch.cloud.google.com 的 URL 才设置
				if uri := web.Get("uri"); uri.Exists() {
					uriStr := uri.String()
					if !strings.Contains(uriStr, "vertexaisearch.cloud.google.com") {
						result["url"] = uriStr
					}
				}
				if domain := web.Get("domain"); domain.Exists() {
					result["encrypted_content"] = domain.String()
				}
				webSearchResults = append(webSearchResults, result)
			}
		}
	}
	if len(webSearchResults) > 0 {
		webSearchToolResult := map[string]interface{}{
			"type":        "web_search_tool_result",
			"tool_use_id": toolUseID,
			"content":     webSearchResults,
		}
		content = append(content, webSearchToolResult)
	}

	// 3. Gemini 响应的 text 块
	if textContent != "" {
		textBlock := map[string]interface{}{
			"type": "text",
			"text": textContent,
		}
		content = append(content, textBlock)
	}

	// 构建最终响应
	response := map[string]interface{}{
		"id":            msgID,
		"type":          "message",
		"role":          "assistant",
		"content":       content,
		"model":         model,
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]interface{}{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
			"server_tool_use": map[string]interface{}{
				"web_search_requests": 1,
			},
		},
	}

	respJSON, _ := json.Marshal(response)
	return string(respJSON)
}

// convertGeminiToClaudeSSEStream 将 Gemini 响应转换为 Claude SSE 流式格式。
func convertGeminiToClaudeSSEStream(model string, geminiResp []byte) []string {
	var events []string

	// 从 Gemini 响应中提取数据
	textContent := ""
	if parts := gjson.GetBytes(geminiResp, "response.candidates.0.content.parts"); parts.IsArray() {
		for _, part := range parts.Array() {
			if text := part.Get("text"); text.Exists() {
				textContent += text.String()
			}
		}
	} else if parts := gjson.GetBytes(geminiResp, "candidates.0.content.parts"); parts.IsArray() {
		for _, part := range parts.Array() {
			if text := part.Get("text"); text.Exists() {
				textContent += text.String()
			}
		}
	}

	// 过滤 textContent 中的 google.com 相关 URL
	textContent = stripGoogleURLs(textContent)

	groundingMetadata := gjson.GetBytes(geminiResp, "response.candidates.0.groundingMetadata")
	if !groundingMetadata.Exists() {
		groundingMetadata = gjson.GetBytes(geminiResp, "candidates.0.groundingMetadata")
	}

	// 从 Gemini 响应中获取 usage
	inputTokens := gjson.GetBytes(geminiResp, "response.usageMetadata.promptTokenCount").Int()
	if inputTokens == 0 {
		inputTokens = gjson.GetBytes(geminiResp, "usageMetadata.promptTokenCount").Int()
	}
	outputTokens := gjson.GetBytes(geminiResp, "response.usageMetadata.candidatesTokenCount").Int()
	if outputTokens == 0 {
		outputTokens = gjson.GetBytes(geminiResp, "usageMetadata.candidatesTokenCount").Int()
	}

	msgID := fmt.Sprintf("msg_%s", uuid.New().String()[:24])
	toolUseID := fmt.Sprintf("srvtoolu_%d", time.Now().UnixNano())

	// 从 webSearchQueries 构建搜索查询
	searchQuery := ""
	if queries := groundingMetadata.Get("webSearchQueries"); queries.IsArray() && len(queries.Array()) > 0 {
		searchQuery = queries.Array()[0].String()
	}

	// 1. message_start
	messageStart := fmt.Sprintf(`{"type":"message_start","message":{"id":"%s","type":"message","role":"assistant","content":[],"model":"%s","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":%d,"output_tokens":0}}}`,
		msgID, model, inputTokens)
	events = append(events, "event: message_start\ndata: "+messageStart+"\n\n")

	contentIndex := 0

	// 2. server_tool_use 块 (index 0)
	serverToolUseStart := fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"server_tool_use","id":"%s","name":"web_search","input":{}}}`,
		contentIndex, toolUseID)
	events = append(events, "event: content_block_start\ndata: "+serverToolUseStart+"\n\n")

	// input_json_delta
	if searchQuery != "" {
		queryJSON, _ := sjson.Set(`{}`, "query", searchQuery)
		inputDelta := fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":""}}`, contentIndex)
		inputDelta, _ = sjson.Set(inputDelta, "delta.partial_json", queryJSON)
		events = append(events, "event: content_block_delta\ndata: "+inputDelta+"\n\n")
	}

	events = append(events, fmt.Sprintf("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", contentIndex))
	contentIndex++

	// 3. web_search_tool_result 块（过滤 vertexaisearch.cloud.google.com URL）
	webSearchResults := "[]"
	hasResults := false
	groundingChunks := groundingMetadata.Get("groundingChunks")
	if groundingChunks.IsArray() {
		for _, chunk := range groundingChunks.Array() {
			web := chunk.Get("web")
			if web.Exists() {
				result := `{"type":"web_search_result"}`
				if title := web.Get("title"); title.Exists() {
					result, _ = sjson.Set(result, "title", title.String())
				}
				// 只有不包含 vertexaisearch.cloud.google.com 的 URL 才设置
				if uri := web.Get("uri"); uri.Exists() {
					uriStr := uri.String()
					if !strings.Contains(uriStr, "vertexaisearch.cloud.google.com") {
						result, _ = sjson.Set(result, "url", uriStr)
					}
				}
				if domain := web.Get("domain"); domain.Exists() {
					result, _ = sjson.Set(result, "encrypted_content", domain.String())
				}
				result, _ = sjson.Set(result, "page_age", nil)
				webSearchResults, _ = sjson.SetRaw(webSearchResults, "-1", result)
				hasResults = true
			}
		}
	}

	if hasResults {
		webSearchToolResultStart := fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"web_search_tool_result","tool_use_id":"%s","content":[]}}`,
			contentIndex, toolUseID)
		webSearchToolResultStart, _ = sjson.SetRaw(webSearchToolResultStart, "content_block.content", webSearchResults)
		events = append(events, "event: content_block_start\ndata: "+webSearchToolResultStart+"\n\n")
		events = append(events, fmt.Sprintf("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", contentIndex))
		contentIndex++
	}

	// 4. Gemini 响应的 text 块
	if textContent != "" {
		textBlockStart := fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, contentIndex)
		events = append(events, "event: content_block_start\ndata: "+textBlockStart+"\n\n")

		// 将文本分割成更小的块以实现更真实的流式效果
		// 使用基于 rune 的分块以避免 UTF-8 多字节字符截断
		runes := []rune(textContent)
		chunkSize := 50
		for i := 0; i < len(runes); i += chunkSize {
			end := i + chunkSize
			if end > len(runes) {
				end = len(runes)
			}
			chunk := string(runes[i:end])
			textDelta := fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":""}}`, contentIndex)
			textDelta, _ = sjson.Set(textDelta, "delta.text", chunk)
			events = append(events, "event: content_block_delta\ndata: "+textDelta+"\n\n")
		}

		events = append(events, fmt.Sprintf("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", contentIndex))
	}

	// 5. 带有 stop_reason 和 usage 的 message_delta
	messageDelta := fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":%d,"output_tokens":%d,"server_tool_use":{"web_search_requests":1}}}`,
		inputTokens, outputTokens)
	events = append(events, "event: message_delta\ndata: "+messageDelta+"\n\n")

	// 6. message_stop
	events = append(events, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")

	return events
}
