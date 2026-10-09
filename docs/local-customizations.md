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

## 前端主题、渠道概览与探针独立页

2026-10-08 已实现并部署统一蓝色主题、浅深色网格背景及共享控件样式；保持各业务页面原有
功能，用量表缓存率/TPS 徽标与口径不变。生产 V2 沿用原配置，来源与验证见
[前端适配记录](upstream-sync-state.md#前端视觉适配与探针独立页)。

V2 渠道状态默认展示平台/分组卡片、所选区间指标及状态时间线；“详细分析”可进入原有
筛选/矩阵/趋势/明细页面。`monitor_view=details`（兼容 `v2`）和旧分析查询链接进入分析，
`monitor_view=cards` 显式进入概览，往返保留查询。V1 仍使用原页面和开关，详见
[渠道监控](CHANNEL_MONITOR.md)。

管理员 OpenAI 降智探针为账号管理下一行同级菜单，路径 `/admin/openai-downgrade`。
复用原运维统计接口，展示 24 小时、最多 200 个账号及 100 条事件，60 秒自动刷新并支持手动
刷新；沿用管理员认证和监控开关，不改变探针或救治业务。原运维页仅保留跳转入口。

实现入口：`frontend/tailwind.config.js`、`frontend/src/style.css`、
`frontend/src/views/user/ChannelStatusOverview.vue`、
`frontend/src/components/user/monitor/ChannelStatusCard.vue`、
`frontend/src/views/admin/OpenAIDowngradeView.vue`。
回归入口：`ChannelStatusView.mode.spec.ts`、`ChannelStatusOverview.spec.ts`、
`monitorTimeline.spec.ts`、`OpsOpenAIDowngradeCard.spec.ts`、`feature-access.spec.ts`；
内嵌资源与直达路由使用 `backend/internal/web/embed_test.go`（`embed` 标签）。

### 用户页面第二轮对齐

2026-10-08 北京时间 04:06:45 已完成生产切换及应用验收，公网资源和流式调用随后通过。

用户仪表盘改为余额/累计 Token/快捷操作、今日指标/模型摘要、全宽趋势的排布；趋势可切换
总用量和原 Token 明细，原模型图表及完整表格在“模型分布 · 明细”展开查看。累计、今日、
所选日期区间仍分别按原接口计算，平台拆分、其他差额、配额和最近使用保留。
API 密钥与使用记录使用编号标题、紧凑工具栏和细线分区；筛选、列设置、导出及密钥操作不变。
页面专用样式位于 `frontend/src/styles/user-console.css`，不改变管理端表格。

渠道卡片增加标题分隔、记录槽数量和 PAST/NOW 轴，日期仍可通过提示查看。时间线颜色保留
原健康分数分类，高度使用原总体状态；未知槽与失败槽区分，保持缺口和键盘详情。
修复 Vue scoped CSS 中 `:global(.dark)` 后的卡片选择器被丢弃导致的白底白字，改用
`.dark .channel-status-card` 祖先选择器；深色卡片使用炭灰渐变。

回归入口增加 `UserDashboard.spec.ts`、`TokenUsageTrend.spec.ts`，并继续运行
`monitorTimeline.spec.ts`、`KeysView.spec.ts`、`UsageView.spec.ts` 和共享 `UsageTable.spec.ts`。
本轮来源和验证见[第二轮对齐记录](upstream-sync-state.md#用户页面第二轮视觉对齐)，
运行制品和回退证据见[部署状态](deployment-state.md)。

### 密钥厂商分类筛选

2026-10-08 已合并主分支、推送 GitHub 并部署，04:56:49（北京时间）恢复接入。
创建/编辑密钥与列表内切换分组复用
`frontend/src/components/keys/GroupProviderFilter.vue`，提供 Anthropic、OpenAI、国产模型、
其他四张卡片；默认显示全部，再次点击选中分类或“全部分组”恢复完整列表。

`frontend/src/utils/keyGroupProvider.ts` 按原平台分类：Kimi、智谱、DeepSeek 归国产模型；
为兼容截图中的旧分组，OpenAI 协议且名称含“国产”的组也归此类。其余 Anthropic / OpenAI
分别归对应分类，Gemini、Antigravity、Grok、组合及未知平台归其他。名称规则仅为展示兼容，
若以后命名无法表达分类，再增加显式展示分类字段，不更改现有平台和路由。

分类与原文字搜索共同筛选；切换分类不保存密钥。单组表单中若原草稿分组被分类隐藏则清空选择，
继续沿用必选校验；编辑初始分组保留，列表只有点击具体分组才调用原更新接口。切换菜单
限制在视口内，分组列表独立滚动。接口、权限、倍率、额度与其他密钥操作保持原样。
回归入口：`frontend/src/views/user/__tests__/KeysView.spec.ts`；验证见
[密钥筛选记录](upstream-sync-state.md#密钥厂商分类筛选)。

## 密钥智能路由

2026-10-10 智能路由及后续会话粘性/超时修复均已合并主分支、推送并部署；当前运行代码为
`b628e0c328b72507bef8f2b128f64eb071f15906`，实际配置和验收边界见[部署状态](deployment-state.md)。
产品边界见[智能路由规则](upstream-integration-policy.md#密钥智能路由)，
固定来源与验证见[版本记录](upstream-sync-state.md#密钥智能路由与重连避让)。

创建/编辑可启用智能路由并按顺序配置最多 10 个组；跨厂商筛选不会清除已选候选。

2026-10-10 后续界面调整曾按要求只推送；现随上述修复部署：开关移到“分组”标题右侧；
已选路由复用 `GroupBadge`，添加候选复用普通单选的 `GroupOptionItem`，保留平台颜色、
订阅标识、专属/高峰倍率及说明，搜索使用分组专用提示。后台接口和调度计费未改。

用户随后纠正标题位置：厂商卡片仍在上方，下方才是“分组 / 智能路由”标题行和分组选择。
本次顺序修正已于 2026-10-10 06:18 部署（`76d211c72`）；创建和编辑共用同一布局，
彩色徽标与完整下拉信息保持。

列表内智能密钥的快捷切组进入编辑窗口，旧的仅修改主组接口拒绝覆盖智能列表；显式解除绑定
清除列表。`group_ids` 第一项是主组，API 校验正数、去重、数量与全部候选权限。
迁移 `252_api_key_smart_routes.sql` 只新增 `api_keys.route_group_ids` JSONB 列；
单组保留 NULL。主组/列表同一行原子写入，移除或替换分组同步去重、提升主组与失效鉴权缓存。
鉴权快照版本 23 携带列表，按备用组筛选/统计密钥也能命中。

HTTP 三种文本协议复用原转发器：发送错误前显式让出请求，先换同套餐其他合格账号，
再尝试客户配置的下一兼容套餐，保留原计费、调度及协议转换。每个候选重新读取分组并检查余额/订阅；旧主组专属 RPM 覆盖不会带到备用。
全局用户 RPM 和同套餐的组 RPM 成功准入结果在同次请求中共享。模型列表合并合格候选并去重，Codex 清单保留 ETag；
现有自定义模型展示配置仍只影响展示，本轮不提前实施独立的模型白名单迁移。

Redis 按密钥/端点/模型/显式会话/组记录失败 60 秒，只调整候选顺序；缺少会话标识时
沿用密钥/端点/模型/组范围；Redis 不可用时沿用配置顺序。
已输出后的上游失败仅影响下次请求。HTTP 响应 ID 续接查找已有归属并固定组；WebSocket 首轮
尚可安全重试时允许换组，首轮完成后不搬迁会话。图片工具、Gemini 原生和视频维持原路由。
分组或订阅查询异常返回服务不可用，不误报收费资格不足，也不把它记作上游健康故障。
模型清单同样区分资格不符与查询故障：前者跳过该套餐，后者返回 503，不发布
空或不完整模型清单的成功响应与 ETag。

实现入口：

- `frontend/src/views/user/KeysView.vue`、`frontend/src/api/keys.ts`。
- `backend/internal/service/api_key_routes.go`、`api_key_service.go`、`billing_cache_service.go`。
- `backend/internal/handler/api_key_smart_routes.go`、`api_key_route_models.go`、`openai_gateway_handler.go`。
- `backend/internal/server/routes/gateway.go`、`server/middleware/api_key_auth.go`。
- `backend/internal/repository/api_key_repo.go`、`api_key_route_cache.go`。

回归入口：`api_key_smart_routes_test.go`（本地 HTTP/WS 上游及计费）、`api_key_routes_test.go`、
`api_key_route_cache_test.go`、`api_key_repo_integration_test.go`、`api_key_auth_test.go`、
`billing_cache_service_rpm_test.go`、鉴权快照/更新字段测试和 `KeysView.spec.ts`。
实际供应商切换、生产 Redis/超时与客户端五次重连窗口未验证；不将本地模拟通过等同线上保证。

2026-10-10 以下追加要求已随同一提交部署：

- 一个账号就是一条线路；同请求共享失败账号集合，跨组不重试同一失败账号。
  所有账号/套餐共用现有最大切换数加一次的上游尝试预算，不按每个套餐重置预算。
  HTTP 外层同时共享原有容量故障三次上限、OAuth 429 切换限制和首输出超时最多
  切换一次的计数；普通 502 不被容量三次上限截断。容量故障终态复用原协议错误响应。
  一个账号失败不直接判定套餐故障；耗尽账号或尝试预算才记录套餐失败偏好。
- 有智能备用时跳过内层最多 5 分钟的临时可用性恢复等待；单套餐、已绑定响应续接保留
  原有等待。已发出的请求沿用现有超时；管理员内部备用池仍受原有准入控制，账号来源
  不自动改变客户计费套餐。Anthropic 原有故障切换缓存计费保护跨重入保留。
- 显式会话标识（已有 session headers 或 Claude metadata.user_id 中的 session）按
  密钥/端点/模型保存成功套餐 1 小时，每次成功刷新。健康成功套餐优先，失败套餐后置；
  没有显式会话标识时不能可靠识别跨请求对话，只使用配置顺序和近期故障偏好。
- 新增 `service/account_recent_stats.go` 与同名 repository：Redis 原子聚合当前分钟及前
  9 个分钟桶的真实文本转发尝试，空闲 11 分钟过期、多实例共享。不是自然语言答案
  正确性评分，不混入管理员探测、用户取消、请求参数错误或本地额度拒绝。
  WebSocket 统计只使用当前轮次的错误标记；历史错误保留给 Ops，不将后续成功记成失败。
- 前端 `AccountRecentStatsCell.vue` 默认显示近 10 分钟成功率（成功/尝试数）、紫色缓存
  命中率、平均首字延迟；不足 10 次提示样本少，无样本和无延迟显示未知。
  复用账号批量统计接口及 30 秒快照缓存，随列表/手动/自动刷新；Redis 查询失败显示
  统计暂不可用。延迟只包含有首字观测的成功样本。缓存使用转发结果的归一化输入 token；
  强制缓存计费转换路径不计入缓存样本，避免把优惠计费当作上游物理缓存命中。
- 调度可靠度采用 `(成功数+9)/(总尝试数+10)`，避免 1/1 样本压过大量稳定样本；
  最近 60 秒连续失败额外降权。OpenAI 高级调度接入既有 ErrorRate 和 TTFT 权重，
  普通/混合与 OpenAI 旧调度在同优先级内参考可靠度，原能力/价格/配额约束仍有效。
  连续失败的分数上限为 `1 - 0.15 * min(连续失败数, 5)`，与平滑可靠度取小值。
  OpenAI 高级调度默认基础评分为 `1*优先级因子 + 1*空闲因子 + 0.7*排队因子 +
  0.8*可靠度 + 0.5*首字速度因子`；管理员运行时权重可覆盖，保留已有成本/额度等可选因子。
  在合格候选 Top-K 中按 `分数 - 最低分 + 1` 加权选择，并非始终固定选最高分。
  缓存命中率当前只记录与展示，不单独加分；缓存收益主要由健康会话亲和保留。
  无样本回中性值，Redis 异常不阻断请求；不按成功率百分比永久禁用账号。
- 上述部署版本的账号级近期失败会驱散其他健康会话；当前源码已按下节修正。
  账号绑定依据会话标识或内容推导的会话种子，不是按用户 ID 将所有新对话固定到同一账号。
  现有管理员 `scheduler_score` 快照仍是配置分数，不等于实际选择分数。

新增回归入口：`account_recent_stats_test.go`（service/repository/admin）、
`account_recent_scheduler_test.go`、`openai_account_availability_wait_test.go`、
`AccountRecentStatsCell.spec.ts`、`AccountsView.usageRefresh.spec.ts`。

### 会话粘性与 OpenAI 首输出保护

本轮修复 `b628e0c32` 已从 `codex/routing-affinity-timeouts` 快进合入主分支、推送并部署。
本轮不修改前端、数据库迁移或计费公式；实际运行版本以部署状态为准。

- 健康对话先命中原账号，涵盖普通单组、智能路由与 OpenAI 粘性加权模式。
  删除账号级 `recentAccountFailed` 对健康绑定的直接驱散，以及按整体错误率/TTFT 的粘性逃逸。
  近期质量评分仍服务新对话和实际故障切换；权限、模型、冷却、质量/救治与配额门不放宽。
  OAuth 满并发沿用有限等待，API Key 先尝试同套餐其他空闲账号；无空闲候选沿用有限兜底等待。
  新账号成功使用后保留新绑定，已有响应 ID 与会话归属限制保持不变。
  旧 `sticky_escape_*` 配置字段保留读取兼容，但不再按账号总体错误率/延迟迁移健康会话；
  运维快照也不再将这些分数展示为当前粘性逃逸原因。
- 外层失败偏好有显式会话时增加会话哈希，TTL 仍为 60 秒；健康备用套餐亲和仍为 1 小时。
  没有显式会话标识时只能保留原密钥/端点/模型范围，不能保证跨对话隔离。
- OpenAI 文本 HTTP 上游（含透传与 Messages/Chat 转换）采用普通 180 秒、`high/xhigh/max`
  600 秒的首有效输出默认值，无思考等级用普通档。显式配置 `0` 仍可禁用；本次生产无覆盖，已确认采用 180/600 默认值。
  等待包含上游响应头，空格/心跳/空前导事件不解除保护；前导字节暂存后原样交给原解析器，
  不裁剪正文。暂存复用 64 KiB 内存、8 MiB 总上限和安全临时文件，超限安全失败。
  首输出成功后解除该计时器，不打断后续长生成；首字统计同样跳过空白和空前导结构。
- HTTP 流只发过心跳时仍可切换，三种入口共用现有心跳字节扣除；真实内容提交后不能重放。
  已收到完整终态帧后立即收尾，不再等供应商关闭 TCP 连接；裸 `error` 后在适用路径继续读取
  `response.failed` 的用量。上游头部延迟仍只记录头部等待，不混入前导输出等待。
- 客户端断开后补收用量最多 180 秒，到期取消上游、释放连接与并发；缺少完整终态时返回
  用量不完整，不把断线记成供应商故障。已收用量继续计费，超时之后的用量仍可能漏记。
- 原生 Claude/Anthropic 超时不变。既有 WebSocket 首轮保护继续使用同一配置，HTTP 桥复用
  文本保护；原生 WS 已输出帧或已建立的会话仍不可跨账号迁移，不承诺空白帧后透明重放。
- 精确识别 `token_group_required`/Cloudflare 403 和“当前组没有账号支持此模型”的 404，
  避免池模式在同一坏线路上反复重试；模型 404 仅冷却本账号实际映射后的模型，不二次映射。
  普通 endpoint 404、请求回显中的模型文字不会误判，不把一般 403 一律永久封号。
- 身份种子、指纹、会话/线程/缓存 ID、出站头与 Cookie Pin 构造逻辑保持原样；安全切换
  继续调用目标账号既有身份构造流程。没有增加供应商标记、主动收费探测、表或依赖。

回归入口：`account_recent_scheduler_test.go`、`api_key_smart_routes_test.go`、
`openai_text_output_wait_test.go`、`openai_visible_ttft_test.go`、
`ratelimit_service_model_not_found_test.go`，以及已有身份、协议转换、WS、重试预算和计费测试。
本轮最终源码已通过 `go test -tags=unit` 的 `service`、`handler`、`repository`、`config`
四包完整单测；重点粘性、心跳跨组、首输出、断线补收、终态用量、模型冷却和智能路由的
`-race` 回归通过；`go build ./cmd/server` 与 `git diff --check` 通过。
完整包回归包含现有身份/指纹、协议转换、WebSocket、权限、重放预算和计费用例。
本轮未重跑需独立数据库的集成测试；生产验收与制品身份在部署状态和版本记录另记。

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
