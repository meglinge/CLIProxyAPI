# Agent Notes (CN)

## 项目结构（简版）
- cmd/server: 入口与启动
- internal/config: 配置结构与解析
- sdk/cliproxy/auth: 认证与账号选择器
- sdk/cliproxy/service.go / sdk/cliproxy/builder.go: selector 装配与热更新
- sdk/api/handlers: API 路由入口与执行
- internal/runtime/executor: 各 provider 执行逻辑
- sdk/cliproxy/usage + internal/runtime/executor/usage_helpers.go: 用量记录
- internal/quota: 配额轮询（自动刷新）

## 路由/账号选择现状
- routing.strategy: round-robin / fill-first / quota-weighted
- selector 位于 sdk/cliproxy/auth/selector.go（RoundRobinSelector / FillFirstSelector）与 sdk/cliproxy/auth/quota_selector.go
- quota-weighted：按配额百分比平滑加权轮询
- auth.Attributes["priority"] 影响候选排序（高优先级先）
- quota 仅用于冷却/不可用状态（QuotaState + NextRecoverAt）

## 当前问题定位（简述）
- fill-first 会优先打满单一账号，符合“一个号打完再换”的现象
- antigravity / claude 同属 provider 时依赖 selector 决策

## 动态配额路由（已实现）
- 新增 selector：sdk/cliproxy/auth/quota_selector.go（平滑加权轮询）
- 新增配额轮询：internal/quota（自动刷新）
- 配额快照写入各 auth 配置文件的 metadata（键：cliproxy_quota）
- 既有文件仅做最小“挂钩”修改：sdk/cliproxy/builder.go / sdk/cliproxy/service.go / internal/api/handlers/management/config_basic.go / internal/config/config.go / config.example.yaml

## 策略细节（quota-weighted）
- 选候选：仍使用 getAvailableAuths，只在“最高 priority 档”内做权重分配
- 权重算法：Smooth Weighted Round Robin（1-1-1 思路的平滑版）
- 权重来源：quota Percent（0~100），权重 = round(Percent * 100)
- 未知配额：默认低权重（quotaUnknownWeight）
- 全 0 或无权重：退回 RoundRobinSelector

## 配额采集来源（自动刷新）
- antigravity：v1internal:fetchAvailableModels（alias 通过 config.oauth-model-alias/默认映射）
- codex：/backend-api/wham/usage（通过 account_id 或 id_token 解析账号）
- gemini-cli：/v1internal:retrieveUserQuota（metadata.project_id）
- 轮询间隔：3 分钟（内置默认值）
- 存储方式：写入 auth JSON 文件的 metadata，重启后重新采集并覆盖

## 合并冲突最小化规范
- 逻辑尽量新增文件；修改既有文件仅限“接线/配置挂钩”
- 避免在已有文件里大段重构；必要时用小范围 patch
- 新配置字段追加在结构体末尾，避免重排已有字段
