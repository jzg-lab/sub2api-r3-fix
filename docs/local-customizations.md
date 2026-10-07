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
