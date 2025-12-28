package handlers

// model_alias_fix.go
// 修复 Claude Code 模型名到 Antigravity 模型名的映射
//
// 问题：Claude Code 使用 claude-opus-4-5-20251101 等完整版本号，
// 但 antigravity 渠道需要 gemini-claude-opus-4-5-thinking 这样的模型名。
// 如果没有 claude 渠道的 API key，会报 "unknown provider for model" 错误。
//
// 修复方案：
// 在路由层将 Claude Code 模型名映射到 Antigravity 模型名

// antigravityModelAlias 定义 Claude Code 模型名到 Antigravity 模型名的映射
var antigravityModelAlias = map[string]string{
	// Claude Opus 4.5
	"claude-opus-4-5-20251101": "gemini-claude-opus-4-5-thinking",
	// Claude Sonnet 4.5
	"claude-sonnet-4-5-20250929": "gemini-claude-sonnet-4-5-thinking",
}

// mapModelToAntigravity 将 Claude Code 模型名映射到 Antigravity 模型名
// 如果没有映射，返回原模型名
func mapModelToAntigravity(modelName string) string {
	if alias, ok := antigravityModelAlias[modelName]; ok {
		return alias
	}
	return modelName
}
