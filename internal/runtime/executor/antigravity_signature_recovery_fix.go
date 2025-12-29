package executor

// antigravity_signature_recovery_fix.go
// 修复 Antigravity Claude 模型 thinking signature 验证错误
//
// 问题根因:
// 1. Antigravity API 对 thinking blocks 的 signature 进行严格验证
// 2. 当 signature 无效、过期或来自不同 session 时，API 返回错误：
//    {"message": "Invalid `signature` in `thinking` block"}
// 3. 这会导致整个请求失败，中断对话流
//
// 修复方案 (Let it crash and recover):
// 1. 检测到 signature validation 错误
// 2. 将 thinking blocks 转换为普通 text blocks:
//    - type: "thinking" -> type: "text"
//    - thinking: "content" -> text: "content"
//    - 移除 signature 字段
// 3. 使用转换后的 payload 重试请求
// 4. 保留对话上下文，thinking 内容作为普通文本保留

import (
	"bytes"
	"context"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// signatureRecoveryAttemptKey is the context key for tracking recovery attempts.
type signatureRecoveryAttemptKey struct{}

// IsSignatureRecoveryAttemptFix checks if current request is a recovery attempt.
func IsSignatureRecoveryAttemptFix(ctx context.Context) bool {
	v, _ := ctx.Value(signatureRecoveryAttemptKey{}).(bool)
	return v
}

// WithSignatureRecoveryAttemptFix marks the context as a recovery attempt.
func WithSignatureRecoveryAttemptFix(ctx context.Context) context.Context {
	return context.WithValue(ctx, signatureRecoveryAttemptKey{}, true)
}

// DisableThinkingConfigForRecoveryFix removes thinkingConfig from Antigravity payload during recovery.
func DisableThinkingConfigForRecoveryFix(ctx context.Context, payload []byte) []byte {
	if !IsSignatureRecoveryAttemptFix(ctx) {
		return payload
	}
	result, _ := sjson.DeleteBytes(payload, "request.generationConfig.thinkingConfig")
	log.Infof("signature recovery: disabled thinkingConfig for recovery attempt")
	return result
}

// TrySignatureRecoveryFix attempts signature error recovery for Claude models.
// Returns (recoveredPayload, recoveryCtx, shouldRetry).
func TrySignatureRecoveryFix(ctx context.Context, statusCode int, body, payload []byte, model string) ([]byte, context.Context, bool) {
	if !ShouldRetryWithRecoveryFix(statusCode, body, model, IsSignatureRecoveryAttemptFix(ctx)) {
		return nil, ctx, false
	}
	recovered := ConvertThinkingToTextForRecoveryFix(payload)
	if !PayloadChangedAfterRecovery(payload, recovered) {
		return nil, ctx, false
	}
	log.Infof("signature recovery: retrying with thinking blocks converted to text")
	return recovered, WithSignatureRecoveryAttemptFix(ctx), true
}

const (
	// skipThoughtSignatureValidatorFix is the sentinel value used to bypass signature validation
	skipThoughtSignatureValidatorFix = "skip_thought_signature_validator"
)

// IsSignatureValidationErrorFix checks if an error response indicates a thinking signature validation failure.
func IsSignatureValidationErrorFix(statusCode int, body []byte) bool {
	if statusCode < 400 || statusCode >= 500 {
		return false
	}

	bodyStr := strings.ToLower(string(body))

	// Check for known signature validation error patterns
	signatureErrorPatterns := []string{
		"invalid `signature` in `thinking` block",
		"expected `thinking` or `redacted_thinking`",
		"must start with a thinking block",
	}

	for _, pattern := range signatureErrorPatterns {
		if strings.Contains(bodyStr, pattern) {
			return true
		}
	}

	// Also check JSON error structure
	if gjson.ValidBytes(body) {
		errorMsg := gjson.GetBytes(body, "error.message").String()
		errorCode := gjson.GetBytes(body, "error.code").String()
		message := gjson.GetBytes(body, "message").String()

		combined := strings.ToLower(errorMsg + errorCode + message)
		for _, pattern := range signatureErrorPatterns {
			if strings.Contains(combined, pattern) {
				return true
			}
		}
	}

	return false
}

// ConvertThinkingToTextForRecoveryFix converts thinking blocks to text blocks in Claude format request.
// This is the recovery transformation applied to the original Claude API request.
//
// When signature validation fails, this function:
// 1. Converts ALL thinking blocks to text blocks (no preservation)
// 2. Disables Extended Thinking to avoid "must start with thinking block" error
//
// Transforms:
//
//	{type: "thinking", thinking: "content", signature: "xxx"}
//
// To:
//
//	{type: "text", text: "content"}
func ConvertThinkingToTextForRecoveryFix(payload []byte) []byte {
	if !gjson.ValidBytes(payload) {
		return payload
	}

	result := string(payload)
	modified := false

	// Process messages array (Claude API format)
	messages := gjson.Get(result, "messages")
	if !messages.IsArray() {
		return payload
	}

	messagesArray := messages.Array()

	for i, message := range messagesArray {
		contentArray := message.Get("content")
		if !contentArray.IsArray() {
			continue
		}

		for j, content := range contentArray.Array() {
			contentType := content.Get("type").String()

			// Convert ALL thinking blocks to text blocks
			if contentType == "thinking" {
				contentPath := "messages." + strconv.Itoa(i) + ".content." + strconv.Itoa(j)

				// Get the thinking text
				thinkingText := content.Get("thinking").String()

				// Convert: type "thinking" -> "text", thinking -> text, remove signature
				result, _ = sjson.Set(result, contentPath+".type", "text")
				result, _ = sjson.Delete(result, contentPath+".thinking")
				result, _ = sjson.Delete(result, contentPath+".signature")
				result, _ = sjson.Set(result, contentPath+".text", thinkingText)
				modified = true

				log.Debugf("signature recovery: converted thinking block to text at %s", contentPath)
			}
		}
	}

	if modified {
		// Disable Extended Thinking to avoid "must start with thinking block" error
		// This is a graceful degradation - user can start a new conversation if thinking is needed
		if gjson.Get(result, "thinking").Exists() {
			result, _ = sjson.Delete(result, "thinking")
			log.Infof("signature recovery: disabled Extended Thinking for graceful degradation")
		}
		log.Infof("signature recovery: converted all thinking blocks to text")
	}

	return []byte(result)
}

// ConvertThinkingToTextAntigravityFix converts thinking blocks in Antigravity format payload.
// Applied after translation to Antigravity format.
//
// Transforms:
//
//	{thought: true, text: "content", thoughtSignature: "xxx"}
//
// To:
//
//	{text: "content"}
func ConvertThinkingToTextAntigravityFix(payload []byte) []byte {
	if !gjson.ValidBytes(payload) {
		return payload
	}

	result := string(payload)
	modified := false

	// Process request.contents array (Antigravity format)
	contents := gjson.Get(result, "request.contents")
	if !contents.IsArray() {
		return payload
	}

	for i, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}

		for j, part := range parts.Array() {
			// Check if this is a thinking block
			if part.Get("thought").Bool() {
				partPath := "request.contents." + strconv.Itoa(i) + ".parts." + strconv.Itoa(j)

				// Get the thinking text
				thinkingText := part.Get("text").String()

				// Convert: remove thought flag and thoughtSignature, keep text
				result, _ = sjson.Delete(result, partPath+".thought")
				result, _ = sjson.Delete(result, partPath+".thoughtSignature")
				result, _ = sjson.Delete(result, partPath+".thought_signature")

				// Ensure text field exists
				if thinkingText != "" {
					result, _ = sjson.Set(result, partPath+".text", thinkingText)
				}
				modified = true

				log.Debugf("signature recovery: converted thought part to text at %s", partPath)
			}

			// For function calls without valid signature, use skip sentinel
			if part.Get("functionCall").Exists() {
				sig := part.Get("thoughtSignature").String()
				if sig == "" || len(sig) < 50 {
					partPath := "request.contents." + strconv.Itoa(i) + ".parts." + strconv.Itoa(j)
					result, _ = sjson.Set(result, partPath+".thoughtSignature", skipThoughtSignatureValidatorFix)
					modified = true
				}
			}
		}
	}

	if modified {
		log.Infof("signature recovery: converted Antigravity thinking parts for retry")
	}

	return []byte(result)
}

// ShouldRetryWithRecoveryFix determines if a failed request should be retried with signature recovery.
func ShouldRetryWithRecoveryFix(statusCode int, body []byte, model string, alreadyRetried bool) bool {
	if alreadyRetried {
		return false
	}

	if !strings.Contains(strings.ToLower(model), "claude") {
		return false
	}

	return IsSignatureValidationErrorFix(statusCode, body)
}

// PayloadChangedAfterRecovery checks if the recovery transformation actually changed the payload.
// Returns true if the payload was modified, false if it remained the same.
func PayloadChangedAfterRecovery(original, recovered []byte) bool {
	return !bytes.Equal(original, recovered)
}

// HasThinkingBlocksFix checks if a Claude format payload contains any thinking blocks.
func HasThinkingBlocksFix(payload []byte) bool {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return false
	}

	for _, message := range messages.Array() {
		contentArray := message.Get("content")
		if !contentArray.IsArray() {
			continue
		}
		for _, content := range contentArray.Array() {
			if content.Get("type").String() == "thinking" {
				return true
			}
		}
	}

	return false
}

// HasThoughtPartsFix checks if an Antigravity format payload contains any thought parts.
func HasThoughtPartsFix(payload []byte) bool {
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return false
	}

	for _, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		for _, part := range parts.Array() {
			if part.Get("thought").Bool() {
				return true
			}
		}
	}

	return false
}
