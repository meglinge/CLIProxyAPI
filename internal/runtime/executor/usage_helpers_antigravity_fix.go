package executor

// usage_helpers_antigravity_fix.go
// 修复 Antigravity 渠道流式计费丢失问题
//
// 问题根因:
// 1. FilterSSEUsageMetadata 在非终止块中将 usageMetadata 重命名为 cpaUsageMetadata
// 2. parseAntigravityStreamUsage 只查找 usageMetadata，找不到 cpaUsageMetadata
// 3. 导致非终止块的计费数据丢失
//
// 修复方案:
// 在 antigravity_executor.go 中将以下函数替换为 Fix 版本:
// - parseAntigravityStreamUsage -> parseAntigravityStreamUsageFix
// - parseAntigravityUsage -> parseAntigravityUsageFix

import (
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

// usageMetadata 的所有可能路径（按优先级排序）
var antigravityUsagePaths = []string{
	"response.usageMetadata",    // 原始路径（终止块）
	"usageMetadata",             // 原始路径
	"usage_metadata",            // 下划线格式
	"response.cpaUsageMetadata", // 重命名后（非终止块）
	"cpaUsageMetadata",          // 重命名后
}

// parseAntigravityStreamUsageFix 修复版流式解析
func parseAntigravityStreamUsageFix(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}

	for _, path := range antigravityUsagePaths {
		if node := gjson.GetBytes(payload, path); node.Exists() {
			return parseGeminiFamilyUsageDetail(node), true
		}
	}

	return usage.Detail{}, false
}

// parseAntigravityUsageFix 修复版非流式解析
func parseAntigravityUsageFix(data []byte) usage.Detail {
	root := gjson.ParseBytes(data)

	for _, path := range antigravityUsagePaths {
		if node := root.Get(path); node.Exists() {
			return parseGeminiFamilyUsageDetail(node)
		}
	}

	return usage.Detail{}
}
