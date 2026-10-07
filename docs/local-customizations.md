# 当前实现与回归入口

本文描述当前源码中已存在的行为及代码入口，包含老板版基础能力和本项目调整。
LOCAL 编号用于索引，不表示对应功能完全由本项目原创。
产品选择及尚未实施的决定统一见 [产品规则与上游差异](upstream-integration-policy.md)，
来源核对进展见 [版本与合并记录](upstream-sync-state.md)，运行环境见 [部署状态](deployment-state.md)。

## 行为与入口

| 编号 | 当前行为 | 依据与边界 |
| --- | --- | --- |
| LOCAL-001 | 无代理 OpenAI OAuth 保留直连；显式代理无效时拒绝，不静默直连 | [授权、代理和导入](upstream-integration-policy.md#授权代理和导入)；保留身份绑定及令牌并发保护 |
| LOCAL-002 | 救治期间保护分组，保留人工暂停、手动终止、原子毕业与旧轮次隔离 | [救治规则](upstream-integration-policy.md#救治)；普通设置不能重新启用停调账号 |
| LOCAL-003 | 质量检测以最终答案判分；模糊答案、正确但 reasoning token 低的答案不单独判失败 | [Cookie Pin](../deploy/codex-lb-cookie-pin/README.md)；中性结果不算恢复通过 |
| LOCAL-004 | Cookie Pin 按账号隔离，合并白名单 Cookie，隔离身份变化及迟到响应 | [Cookie Pin](../deploy/codex-lb-cookie-pin/README.md)；保留暂停及代理保护，不保证模型质量改善 |
| LOCAL-005 | 健康列消费宿主健康信封及插件桥状态 | `backend/internal/service/openai_account_health.go`、`frontend/src/views/admin/AccountsView.vue`；前后端资源须配套 |
| LOCAL-006 | Workspace 显示短 ID、颜色分组及完整 ID 提示 | `frontend/src/views/admin/AccountsView.vue`；颜色来自 ID 哈希，同色不保证同 Workspace，也不是健康等级 |
| LOCAL-007 | 并发数/优先级可编辑，默认 5/2，明确合法值真实保存 | [并发与优先级](upstream-integration-policy.md#账号并发数与优先级)；不重写已有值 |
| LOCAL-008 | 插件配置页遵循 UI Bridge v1，显示具体保存错误，保留未展示参数 | `deploy/codex-lb-cookie-pin/ui/index.html`；保存配置不自动开启质量探针 |
| LOCAL-009 | 救治前置条件失败返回具体原因 | `backend/internal/service/openai_rescue_lane.go`；未启用、未配置和不适用均返回可识别错误 |
| LOCAL-010 | 独立救治区保留客户隔离与原组快照 | 新环境默认关闭；生产显式配置见 [部署状态](deployment-state.md)，不能用生产设置覆盖产品默认值 |
| LOCAL-011 | 质量调度限制按所选分组判断，混合组采用最宽松规则 | 详见下节；不提供按请求隔离混合账号的保证 |

## 用量表缓存命中率与 TPS

用户端和管理端用量表共用 `frontend/src/components/admin/usage/UsageTable.vue`，
在 Token 前默认显示缓存命中率列：紫色缓存比例、青色 TPS，可通过列设置隐藏；不显示公式或公式提示。
缓存比例沿用趋势图口径：缓存读取占普通输入、缓存读取、缓存写入之和；使用计费用量记录，
强制缓存计费路径不代表上游真实缓存命中。TPS 为输出 Token 除以总耗时减首字延迟后的秒数，
是记录口径的平均速度；计时无效、无输出以及图片、音视频和安全策略请求显示 `—`。
未记录或全零的输入用量显示 `—`；有效输入且无缓存读取显示 `0.00%`。
复用现有接口和历史字段，不新增数据库迁移、不改变计费；已于北京时间 2026-10-08 01:43
部署，运行提交及验收证据见 [部署状态](deployment-state.md)。

回归入口：`frontend/src/components/admin/usage/__tests__/UsageTable.spec.ts`。

## 质量调度限制分组

入口：后台 → 插件管理 → Codex LB Cookie Pin → 配置 → 质量调度限制。
选择器由宿主渲染，使用现有 OpenAI 运维设置接口，不扩展插件 iframe 权限。

存储字段为 `openai_operations.quality_protected_group_ids`：

| 配置 | 行为 |
| --- | --- |
| 缺失或 `null` | 全部分组沿用质量限制 |
| `[]` | 不限制任何分组 |
| 非空 ID 数组 | 只有全部绑定都在所选组内的账号受质量限制；有任一未选中绑定或无绑定即放宽 |

- 救治账号按严格校验的原组快照判断，不能把救治组当作普通组。
- 范围外救治账号明确手动开启时，在同一事务内恢复原组、终止救治并开启调度；
  保留终止标记及失败证据，不计质量恢复。原组损坏、目标组被删或显式代理无效时回滚。
- 范围外账号不因纯质量结果停调或自动入区；认证首振暂停、认证错误、限流、人工暂停
  和正常恢复继续适用。
- 保存分组选择不批量开启或释放账号。已在救治区的账号需要明确点击开启。
- 混合账号整体开启后，所选组也可能使用它；没有“所选组只用质量合格账号”的保证。

实现入口：

- `frontend/src/components/plugins/OpenAIQualityScope.vue`
- `backend/internal/repository/openai_quality_scope.go`（共享判定）
- `backend/internal/repository/openai_rescue_termination.go`（手动开启）
- `backend/internal/repository/openai_downgrade_commit.go`（检测提交）
- `backend/internal/repository/openai_oauth_proxy_binding.go`（入区资格）

对应回归位于 `backend/internal/repository/openai_quality_scope_test.go`；
运行方法及通用检查见 [开发指南](../DEV_GUIDE.md)。


## r17bj 适配与 Linux TOTP

本轮使用老板 `a84149c1fcb22b9ce120c84240164185a455fbd3` 的严格冷却/到期恢复，
取消旧每日提前复查和 8 天排期上限；人工暂停、救治终止、质量范围及联合毕业证据继续保留。
账号并发仍默认 5、优先级默认 2，优先级 0 有效；省略并发的后台写入不覆盖显式更新。
插件采用老板 Cookie 续期、过期模板处理和配置加载保护，并保留本地最终答案及中性判分。

账号绑定的重新授权使用凭据身份版本；普通健康状态变化不使其失效，身份/代理变化会拒绝旧结果。
手动 OAuth 和令牌导入继续支持无代理；不要求历史 IP、固定出口或浏览器启动证明。
自动授权复用 Chrome/Node，Linux 使用无头模式；真实账号和目标 Linux 环境未在本次验证。

TOTP 密钥通过管理员 `GET/PUT /api/v1/admin/openai/accounts/:id/totp` 管理，独立加密列
不会随令牌刷新或普通账号编辑丢失，不进入账号 DTO/通用导出；配置状态通过专用 GET 读取。
后台导入后登录可明确选用，单次输入不默认保存，密码不持久化。接口字段、依赖、密钥配置和
回退约束见 [授权辅助程序说明](../deploy/auth-browser/README.md)。

回归入口：`account_totp_service_test.go`、`account_totp_integration_test.go`、
`openai_reauthorization_local_test.go`、`openai_auth_browser_automation_test.go`、
`audit_log_oauth_test.go`、`useAutomaticReauth.spec.ts` 和 `deploy/auth-browser/tests`。
Wire 的探针/救治/浏览器依赖已回归正式 provider，重新生成不会丢失启动/停止连接。
