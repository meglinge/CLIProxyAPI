// Package executor 提供 Claude 模型的 token 估算工具。
// 用于补偿 Google countTokens API 对 Claude 模型不正确计算 tools token 的问题。
package executor

import (
	"math"
	"unicode"

	"github.com/tidwall/gjson"
)

// TokenEstimator 提供准确的 Claude 模型 token 估算。
// Google 的 countTokens API 对 tools 返回约 1 token，但 Claude 实际会正确计算。
// 本估算器使用字符单位计算配合分级精度修正。
type TokenEstimator struct{}

// NewTokenEstimator 创建新的 TokenEstimator 实例。
func NewTokenEstimator() *TokenEstimator {
	return &TokenEstimator{}
}

// isWesternChar 判断字符是否为西文字符。
// 西文字符包括 ASCII、拉丁扩展等字符块。
// 非西文字符（中日韩、阿拉伯文等）消耗更多 token。
func isWesternChar(c rune) bool {
	// ASCII (U+0000..U+007F)
	if c <= 0x007F {
		return true
	}
	// 拉丁-1 补充 (U+0080..U+00FF)
	if c >= 0x0080 && c <= 0x00FF {
		return true
	}
	// 拉丁扩展-A (U+0100..U+017F)
	if c >= 0x0100 && c <= 0x017F {
		return true
	}
	// 拉丁扩展-B (U+0180..U+024F)
	if c >= 0x0180 && c <= 0x024F {
		return true
	}
	// 拉丁扩展附加 (U+1E00..U+1EFF)
	if c >= 0x1E00 && c <= 0x1EFF {
		return true
	}
	// 拉丁扩展-C (U+2C60..U+2C7F)
	if c >= 0x2C60 && c <= 0x2C7F {
		return true
	}
	// 拉丁扩展-D (U+A720..U+A7FF)
	if c >= 0xA720 && c <= 0xA7FF {
		return true
	}
	// 拉丁扩展-E (U+AB30..U+AB6F)
	if c >= 0xAB30 && c <= 0xAB6F {
		return true
	}
	return false
}

// countCharUnits 计算字符串的字符单位。
// 西文字符 = 1 单位，非西文（中日韩等）= 4.5 单位。
// 4 个字符单位 = 1 token。
func countCharUnits(s string) float64 {
	var units float64
	for _, c := range s {
		if unicode.IsSpace(c) {
			units += 0.25
		} else if isWesternChar(c) {
			units += 1.0
		} else {
			units += 4.5 // 中日韩及其他非西文字符
		}
	}
	return units
}

// countTokensFromString 估算字符串的 token 数。
// 使用字符单位计算配合分级精度修正。
// 使用上取整确保"宁可多估触发压缩，也不要低估导致超长报错"。
func countTokensFromString(s string) int64 {
	if s == "" {
		return 0
	}

	units := countCharUnits(s)
	tokens := units / 4.0

	// 小文本需要更大的修正系数，因为相对误差更高。
	var corrected float64
	switch {
	case tokens < 100:
		corrected = tokens * 1.5
	case tokens < 200:
		corrected = tokens * 1.3
	case tokens < 300:
		corrected = tokens * 1.25
	case tokens < 800:
		corrected = tokens * 1.2
	default:
		corrected = tokens * 1.0
	}

	// 使用上取整，确保保守估算（宁可多估触发压缩）
	result := int64(math.Ceil(corrected))
	if result < 1 && s != "" {
		return 1
	}
	return result
}

// EstimateToolsTokens 估算请求 payload 中 tools 的 token 数。
// 用于补偿 Google countTokens API 对 Claude 不计算 tools 的问题。
// 计算内容包括：工具名称 + 描述 + input_schema JSON。
// 注意：避免双重计数，OpenAI 风格优先使用 function.* 字段。
func (e *TokenEstimator) EstimateToolsTokens(payload []byte) int64 {
	toolsRaw := gjson.GetBytes(payload, "tools")
	if !toolsRaw.Exists() || !toolsRaw.IsArray() {
		// 尝试 OpenAI 风格的 "functions" 字段
		toolsRaw = gjson.GetBytes(payload, "functions")
		if !toolsRaw.Exists() || !toolsRaw.IsArray() {
			return 0
		}
	}

	var total int64
	toolsRaw.ForEach(func(_, tool gjson.Result) bool {
		// 检测是否为 OpenAI 新版格式 {type:"function", function:{...}}
		isOpenAINewFormat := tool.Get("function").Exists()

		if isOpenAINewFormat {
			// OpenAI 新版格式：只使用 function.* 字段，避免双重计数
			if funcName := tool.Get("function.name").String(); funcName != "" {
				total += countTokensFromString(funcName)
			}
			if funcDesc := tool.Get("function.description").String(); funcDesc != "" {
				total += countTokensFromString(funcDesc)
			}
			if funcParams := tool.Get("function.parameters").Raw; funcParams != "" {
				total += countTokensFromString(funcParams)
			}
		} else {
			// Anthropic 格式或 OpenAI 旧版格式
			if name := tool.Get("name").String(); name != "" {
				total += countTokensFromString(name)
			}
			if desc := tool.Get("description").String(); desc != "" {
				total += countTokensFromString(desc)
			}
			// Input schema（Anthropic 格式）
			if schema := tool.Get("input_schema").Raw; schema != "" {
				total += countTokensFromString(schema)
			}
			// Parameters（OpenAI 旧版格式）
			if params := tool.Get("parameters").Raw; params != "" {
				total += countTokensFromString(params)
			}
		}

		return true
	})

	return total
}

// EstimateMessagesTokens 估算请求 payload 中消息的 token 数。
func (e *TokenEstimator) EstimateMessagesTokens(payload []byte) int64 {
	messagesRaw := gjson.GetBytes(payload, "messages")
	if !messagesRaw.Exists() || !messagesRaw.IsArray() {
		return 0
	}

	var total int64
	messagesRaw.ForEach(func(_, msg gjson.Result) bool {
		// 角色
		if role := msg.Get("role").String(); role != "" {
			total += countTokensFromString(role)
		}

		// 内容 - 可以是字符串或数组
		content := msg.Get("content")
		if content.Type == gjson.String {
			total += countTokensFromString(content.String())
		} else if content.IsArray() {
			content.ForEach(func(_, part gjson.Result) bool {
				if text := part.Get("text").String(); text != "" {
					total += countTokensFromString(text)
				}
				return true
			})
		}

		return true
	})

	return total
}

// EstimateSystemTokens 估算请求 payload 中系统提示的 token 数。
func (e *TokenEstimator) EstimateSystemTokens(payload []byte) int64 {
	systemRaw := gjson.GetBytes(payload, "system")
	if !systemRaw.Exists() {
		return 0
	}

	// System 可以是字符串或对象数组
	if systemRaw.Type == gjson.String {
		return countTokensFromString(systemRaw.String())
	}

	if systemRaw.IsArray() {
		var total int64
		systemRaw.ForEach(func(_, item gjson.Result) bool {
			if text := item.Get("text").String(); text != "" {
				total += countTokensFromString(text)
			}
			return true
		})
		return total
	}

	return 0
}

// EstimateTotalTokens 估算 Claude 请求 payload 的总 token 数。
// 包括系统提示、消息和工具。
func (e *TokenEstimator) EstimateTotalTokens(payload []byte) int64 {
	total := e.EstimateSystemTokens(payload)
	total += e.EstimateMessagesTokens(payload)
	total += e.EstimateToolsTokens(payload)
	return total
}

// 全局估算器实例，方便使用。
var globalTokenEstimator = NewTokenEstimator()

// EstimateToolsTokensForClaude 是估算 tools token 的便捷函数。
// 这是用于补偿 Google countTokens API 的主要函数。
// 返回值已扣除 Google API 可能已计算的 1 token 占位。
func EstimateToolsTokensForClaude(payload []byte) int64 {
	estimated := globalTokenEstimator.EstimateToolsTokens(payload)
	// 占位扣减：Google 已经算了约 1 token，避免过度补偿
	if estimated > 1 {
		return estimated - 1
	}
	return 0
}
