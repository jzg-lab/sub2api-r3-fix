# 提示词审计

提示词审计是独立于 OpenAI Moderations 内容审核的模块。管理页面为
`/admin/prompt-audit`，既有内容审核页面为 `/admin/risk-control`。

## 运行模式

| 配置 | 有效模式 |
| --- | --- |
| 风控总开关关闭，或 `enabled=false` | 关闭 |
| `enabled=true`、`blocking_enabled=false` | 异步审计 |
| `enabled=true`、`blocking_enabled=true` | 同步审计并阻止 |

默认关闭。`enabled=false` 与 `blocking_enabled=true` 的组合被拒绝。
异步失败不改变主请求结果；同步模式在 Guard 不可用时拒绝继续请求。

## 节点与数据

- 节点使用 OpenAI 兼容 Chat Completions 协议，解析 Qwen3Guard 的 Safety/Categories。
- 管理员配置节点地址、模型、凭据、超时、输入长度和优先顺序，并可单独测试连接。
- 节点凭据加密保存，管理接口只返回凭据状态；省略新 token 保留旧值，清除需显式指定。
- 审计范围可覆盖全部或指定分组，风险类别可配置；保存携带预期配置版本，防止覆盖并发编辑。
- 异步任务持久化到 PostgreSQL，待扫描原文以短 TTL 放在 Redis；数据库事件使用脱敏快照。
  Worker 具有重试、租约与过期任务回收。审计内容会发往配置的节点，地址由管理员管理。
- 运行态展示有效模式、配置版本、Worker、队列、依赖状态和同步 Guard 指标。

## 管理 API

以下路径以 `/api/v1/admin/prompt-audit` 为前缀，使用现有管理员认证及操作审计：

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET / PUT | `/config` | 读取 / 保存配置 |
| POST | `/endpoints/probe` | 测试节点 |
| GET | `/runtime` | 运行状态 |
| GET | `/events`、`/events/:id` | 事件列表 / 详情 |
| DELETE | `/events/:id` | 删除单条事件 |
| POST | `/events/batch-delete` | 按 ID 批量删除 |
| POST | `/events/delete-preview`、`/events/delete-by-filter` | 筛选删除预览 / 确认执行 |

## 实现与验证入口

- `backend/internal/securityaudit/`：配置、快照、Guard、任务和事件。
- `backend/internal/server/routes/admin.go`：管理路由。
- `backend/internal/server/routes/prompt_audit_route_coverage_test.go`：网关覆盖回归。
- `frontend/src/features/prompt-audit/`：管理页面及测试。

隔离环境下可运行 `cd backend && go test ./internal/securityaudit/...`。
测试通过不等于任意生产节点已启用或满足同步阻断上线条件。
