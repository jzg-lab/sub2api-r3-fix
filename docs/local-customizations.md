# 当前实现与回归入口

本文描述当前源码中已存在的行为及代码入口，包含老板版基础能力和本项目调整。
LOCAL 编号用于索引，不表示对应功能完全由本项目原创。
产品选择及尚未实施的决定统一见 [产品规则与上游差异](upstream-integration-policy.md)，
来源核对进展见 [版本与合并记录](upstream-sync-state.md)，运行环境见 [部署状态](deployment-state.md)。
本文以本地源码为准；当前线上提交、插件和实际部署证据统一见[部署状态](deployment-state.md)。

## 第一期部署工具（已接入并部署）

按用户批准的同机短维护方案新增 `deploy/build_fast_release.py`、`deploy/fast_deploy.py` 与配置示例。
快速通道只处理同插件、无迁移、兼容运行环境的应用替换；固定提交构建，准备与切换分离，
独立 systemd 任务排空、验收及条件恢复。用法、接入前提、回执和未测项统一维护在
[合并与部署更新指南](merge-deploy-guide.md#第一期工具与操作流程)，不在这里复制操作清单。

后端新增 `internal/server/readiness.go` 与 `internal/service/plugin_readiness.go`，
`/readyz` 联合初始化、SQL、Redis 及权威插件绑定/运行时健康判断，原 `/health` 不变。
`pluginEvidenceDir` 将实际状态路径转为绝对路径，兼容默认相对部署目录，不改变目录位置或保护。
回归入口为对应 Go 测试、`plugin_evidence_path_test.go` 和 `deploy/tests/test_fast_deploy.py`。

代码已提交并推送至 `5c28184c0`；最终镜像通过真实隔离故障、成功切换、回退和中断恢复演练。
2026-10-20 首次生产任务因请求未排空返回 `DEFERRED`，随后按用户指令新建准备目录重试，
06:17:00 完成部署，维护采样约 4 秒。上述 `/readyz`、源码路径修复及模型漏列修复现已进入生产应用。
第一次延后的证据保留；实测时间不代表每次更新都能固定四秒完成。

## 老板 e3cad515 本地适配

本轮从老板 `1f7d36c60f361c93c828cec2a1516f11e852608f` 到
`e3cad515d5171ffc2e16eb725fa07f98352adec1` 的真实增量进行适配，保留已集成官方及本地行为。
状态、结果提交和本次验证见[版本记录](upstream-sync-state.md#老板-e3cad515-整合2026-10-19)。

- OAuth 与下游窗口/租户身份隔离、重登录同账号校验、WS continuation 归属、备用池权限检查配套接入。
  Linux 和无代理直连保留，不依赖历史登录 IP、地理信息或出口时区。
- Cookie Pin 源码更新至 0.3.20，宿主提供受账号/路由/灰度限制的 HTTP 通道与短期探针许可。
  新批量准入保留手动终止保护；保留旧暂停同步及 HostService broker 兼容。流结束与持久结算配套，
  已有用量不重放。判分按用户后续选择完整对齐老板：全文数字匹配，宿主正确低 token 先快速复检，
  再次失败按两次证据处理；插件低 token 判失败，保留新版截断指纹中性例外。
- Codex 模型清单 OAuth 直接向上游请求（包括条件请求），API Key 保留账号级缓存；
  两类账号保留权限、白名单与固定排序、500k context 和实时元数据，
  新增 simple mode 目录范围对齐、null service_tiers 规范化及规范化后 ETag。
- 保留无数据终态和缺失终态的协议错误处理改进、连接池/上传超时与启动队列隔离、环境诊断界面，
  健康限流标签以 reset 截止时间为准。原首输出期限、空白流判断与计费模型回写继续保留。
- Mihomo 下层排序和启动策略脚本只提供工具及测试，没有修改或执行生产配置。
  本轮没有新增依赖、迁移或覆盖历史 SQL/checksum；内嵌前端由本地源码构建。

新增回归主要在 `openai_codex_models_service_tiers_test.go`、`openai_oauth_relogin_test.go`、
`plugin_host_http_test.go`、`plugin_probe_policy_test.go`、`plugin_bridge_settlement_test.go`、
`openai_ws_turn_billing_test.go`、Cookie Pin transport 测试及环境审计前端测试。追加对齐回归包含 `openai_downgrade_answer_veto_test.go`、
`openai_probe_grading_test.go`、`openai_probe_verdict_postgres_test.go`、OAuth 直接请求/ETag 与 API Key 缓存测试。

## 官方 v0.2.17 本地适配

本地分支 `codex/integrate-official-v0.2.17` 按用户指定的官方 release 固定提交
`060d0160417782cd962a0c7f460f6cd4565d4916` 进行选择性移植。该源码与本项目没有共同祖先，
因此按行为适配而非整树合并。已采用 GPT-5.5 OAuth / SetupToken 退役、待定退款继续冻结额度、
账号筛选平铺布局、Gemini 3.6 Flash 限时优惠、GPT-6.1 Sol OAuth / SetupToken 长上下文免附加费、
Vertex 共享容量不足单请求退避和 Antigravity Hub UA；已确认的本地账号路由、调度、冷却、身份绑定及 Cookie Pin
规则继续保留。Gemini 3.6 Flash 优惠在配置时区的 2027-01-01 起恢复标准价，涵盖目录价、回退价和显式缓存存储价；
非官方自定义 token 单价及自定义缓存存储单价保持不变。新增迁移 261–264；历史迁移文件未改。提交与验证状态见[版本记录](upstream-sync-state.md)。

## 官方 v0.2.15 本地适配

2026-10-12，`codex/integrate-official-v0.2.15` 按固定官方提交
`f2669c8cf62555cd92389b3f55920e9e6e7c6ff2` 完成已确认范围适配。
2026-10-13 已提交为 `64bda45c3e53d18810b0f300aff4e7ec48dd1d0c` 并快进合入本地 `main`。
2026-10-13 已连同运维 TPS 首 token 口径修订推送至 GitHub `main`，并已按最小变更方式部署；
以下描述主分支源码，不能据此推定生产已升级。
完整来源、取舍、迁移和验收见[版本记录](upstream-sync-state.md)。

- 分组模型配置改为调用白名单；HTTP 智能路由按候选分别检查客户端公开模型，WS 每轮检查，
  Composite 别名随后按候选解析，既有响应归属和会话边界保持。鉴权快照 v24；模型目录按账号/URL
  隔离，固定排序仍受暂停、冷却、影子母账号、代理隔离保护。迁移 253 保留并双向同步旧展示列。
- TypeSafe、Cline、Command Code 及 MiniMax/OpenCode 前置能力已串联平台清单、表单、导入、协议、
  配额及计费。TypeSafe 内容审核与独立 prompt audit 均接入；风控白名单不绕过后者。
- Key 默认未删除上限 200、固定小时创建上限 60，各自为 0 时关闭对应限制。在途预占默认启用、
  无法估价默认放行；失败尝试释放、成功交异步结算，客户换套餐按实际套餐重新估价。删除 Key 不跳过其他结算。
- 充值支持活动赠金/折扣，赠金不参与推广返利；不重算历史。平台日/周/月限额全部清空后删除额度行，
  重设从零开始，数值 0 仍代表明确限额配置。缓存 schema v2 与刷库快照携带 quota_id，旧行快照不能
  覆盖新建额度；可选 flusher 默认关闭，同一行强制重置的既有并发覆盖限制仍保留。
- WS 后续轮刷新已绑定客户套餐价格及图片输入用量；加密内容恢复保留本地会话谱系和完整工具历史。
  心跳字节在通用/等待/Antigravity 路径统一计数，只有心跳时可安全换线，实际内容输出后不重放。
- 批量生图账号按数字较小的优先级先选，同级按 ID；已保存优先级及资格过滤保持，
  不改变文本 OAuth 订阅池优先。
- 运维输出速率现按用户 2026-10-13 的追加要求统一扣除首字等待，标为“输出速率（首字后）”，
  展示逐请求速率的 P5/P10/P50、平均值和有效样本数。首字时间缺失、负数、超过或等于总耗时，
  或无输出的记录均排除；图片、音视频及安全策略请求不参与。原始/预聚合概览共用同一明细查询，
  历史数据按新公式查询，不回写历史用量或计费。下方用量表 TPS 本来就扣首字等待。
  支持配置上游请求 ID、推理计费倍率、订阅批量操作及账号统计，保留本地布局。
- 插件 HostService 账号目录/身份权限限制在能力、当前运行时绑定与灰度范围内；暂停、终止、
  冷却和代理保护继续生效，旧运行时句柄失效。目录无凭据/代理密码，出站身份延续本地 UA/指纹；
  KV 按插件隔离，旧 Cookie Pin 协议兼容。详见[插件协议](../backend/pkg/pluginapi/README.md)。

主要新增回归入口：`group_model_allowlist_test.go`、`api_key_smart_routes_test.go`、
`billing_inflight_reservation_test.go`、`user_platform_quota_generation_postgres_test.go`、
`group_model_allowlist_migration_test.go`、`openai_gateway_ws_composite_test.go`、
`openai_plugin_account_directory_test.go`、`plugin_host_services_test.go` 及各新平台表单测试。
全量结果见版本记录；真实供应商/支付、生产性能与本轮浏览器人工验收未执行。

## 行为与入口

| 编号 | 当前行为 | 依据与边界 |
| --- | --- | --- |
| LOCAL-001 | 无代理 OpenAI OAuth 保留直连；显式代理无效时拒绝，不静默直连 | [授权、代理和导入](upstream-integration-policy.md#授权代理和导入)；保留身份绑定及令牌并发保护 |
| LOCAL-002 | 救治期间保护分组，保留人工暂停、手动终止、原子毕业与旧轮次隔离 | [救治规则](upstream-integration-policy.md#救治)；普通设置不能重新启用停调账号 |
| LOCAL-003 | 质量判分采用老板 e3cad515 全文匹配；宿主正确低 token 先复检，插件低 token 判失败 | [Cookie Pin](../deploy/codex-lb-cookie-pin/README.md)；中性结果不算恢复通过 |
| LOCAL-004 | Cookie Pin 按账号隔离，合并白名单 Cookie，隔离身份变化及迟到响应 | [Cookie Pin](../deploy/codex-lb-cookie-pin/README.md)；保留暂停及代理保护，不保证模型质量改善 |
| LOCAL-005 | 健康列消费宿主健康信封及插件桥状态 | `backend/internal/service/openai_account_health.go`、`frontend/src/views/admin/AccountsView.vue`；前后端资源须配套 |
| LOCAL-006 | Workspace 显示短 ID、颜色分组及完整 ID 提示 | `frontend/src/views/admin/AccountsView.vue`；颜色来自 ID 哈希，同色不保证同 Workspace，也不是健康等级 |
| LOCAL-007 | 并发数/优先级可编辑，默认 5/2，明确合法值真实保存 | [并发与优先级](upstream-integration-policy.md#账号并发数与优先级)；不重写已有值 |
| LOCAL-008 | 插件配置页遵循 UI Bridge v1，显示具体保存错误，保留未展示参数 | `deploy/codex-lb-cookie-pin/ui/index.html`；保存配置不自动开启质量探针 |
| LOCAL-009 | 救治前置条件失败返回具体原因 | `backend/internal/service/openai_rescue_lane.go`；未启用、未配置和不适用均返回可识别错误 |
| LOCAL-010 | 独立救治区保留客户隔离与原组快照 | 新环境默认关闭；生产显式配置见 [部署状态](deployment-state.md)，不能用生产设置覆盖产品默认值 |
| LOCAL-011 | 质量调度限制按所选分组判断，混合组采用最宽松规则 | 详见下节；不提供按请求隔离混合账号的保证 |

## 模型广场

当前源码的右上角入口直接进入 `/model-plaza` 独立页面，旧 `embedded=1` 链接也显示独立页面。
使用已选定的灰底/蓝色卡片样式，提供分组、平台、计费特性与协议筛选、搜索、卡片/列表、
人民币/真实美元切换、官方原价替换开关及价格/状态详情；Token 价格固定按 1M 展示。
保留站点名称与 Logo。
供应商平台筛选位于分组上方，复用现有品牌图标，以图标、名称和数量徽标展示，未知品牌沿用首字母占位。

2026-10-20 漏列修复已随 `5c28184c0` 部署；随后用户指出默认目录包含未上架模型，确认改为
按分组取得明确供给名单。以下过滤已在 `ea55196deec3996005ff201a69fd7c0507e4e05f`
合入 `main`，随后按固定提交 `083053ab2` 于 2026-10-20 06:58:50 部署；公网实际返回
16 个分组、160 个条目、80 个不同名称。补充发现的 Claude 别名阶梯价格异常已在本地修复，尚未部署，详见
部署状态及版本记录，不能将目录过滤上线或健康通过当成全部价格验收通过。

- 候选来自该组账号原始 `model_mapping` 的具体请求名、已启用分组白名单的具体名称及
  启用渠道的精确映射别名；账号必须启用且允许调度，并支持渠道映射后的名称。
- 不用 `GetModelMapping()` 的自动补充结果枚举模型；内置目录、价格目录、自动别名、
  同步能力元数据、通配模式不再扩充展示名单。OpenAI 透传忽略残留账号映射；仍可使用
  明确的分组白名单/渠道别名，经账号支持检查后展示。
- 渠道及分组价格卡只为已有供给的模型补价，不因配置了价格就显示。保留账号退役规则、
  分组白名单、平台隔离、渠道计费基准及账号上游映射限制；同平台按名称不区分大小写去重。
- 无模型的分组隐藏；没有关联渠道但存在明确账号模型的分组仍展示。一次查询批量读取一次
  启用账号，下一次轮询读取配置变化；临时限流/过载不移除价格卡。账号查询失败或缺少账号
  仓储返回错误，不返回只有价格的目录。价格继续走原计费解析链，通配图片/按次价卡仍可命中。

回归入口为 `model_plaza_catalog_test.go` 与 `model_plaza_service_test.go`。本轮没有修改前端、
实际调用准入、数据库迁移或生产配置，也未增加上游同步或收费探测。

后续精简已在 `b3c33601e23e93890354b3af00f17b17a6ec5b05` 合入 `main`，尚未部署：
同一分组、平台、模型找到首个合格供应账号后，不再对其他
重复供应账号重新解析映射；失败检查仍继续尝试后续账号。合并分组初始化遍历，删除额外顺序数组，
移除账号名单的预排序，合并渠道/分组候选时统一归一化和排序，不增加跨请求缓存。
`BenchmarkModelPlazaSharedAccountModels` 对比单账号与 200 账号共享 40 模型、关联 2 分组；
基准不含数据库、网络与完整计费服务，具体测量及验证边界见版本记录。

Claude 别名定价稳定性修复已在 `ed7ddbdd4` 提交并推送，尚未部署：`claude-...-thinking` 在无精确别名价时匹配
对应基础型号，兼容 `models/`、供应商前缀和日期后缀；已有精确别名价格仍优先。日期等价项
采用固定键顺序；系列回退保留原有系列优先级，先取规范键，再按固定键顺序取候选，避免 Go map
遍历顺序影响价格。同次阶梯测算复用预解析的目录价格，刷新目录也不会混用两套基准；缺价报错，
不将异常负数钳成零。分组/渠道自定义价与长上下文开关不变。回归入口
`pricing_lookup_stability_test.go` 同时覆盖广场阶梯与费用计算；测试结果与未测边界见版本记录。

价格仍使用 `ModelPricingResolver` 和 `ResolveContextPricingSchedule` 的实际扣费解析链。
广场从本次查询的渠道和分组行构建请求内渠道快照，避免进程长缓存导致刚保存的价格仍展示旧值；
不改变网关缓存或扣费规则。`current_rate_multiplier` 复用峰值、图片、视频倍率函数并包含当前
渠道时段倍率；用户倍率查询失败直接报错，不降级成另一个价格。阶梯表增加 1h 缓存价探针，
因此展示包含计费自己的回退策略（例如某些模型显式 1h 零价仍回退缓存价），不直接把配置误当扣费。
视频分辨率价保留分组模型卡 > 分组分辨率 > 渠道的优先级。卡片显示常规服务档、首档价格；
详情列出各档与已有思考倍率，Fast/Flex、搜索/存储等请求选项明确另按实际配置结算。

`balance_recharge_multiplier` 通过支付服务读取当前配置，使用与充值相同的归一化规则。
本站 CNY 售价为站内扣费额除以充值倍率，USD 售价再除以用户指定固定汇率 6.70。
「显示原价」替换卡片、阶梯详情和排序的价格来源：USD 用官方美元原价，CNY 用其乘以 6.70；
不套用用户/分组倍率和充值比例，不将官方 Token 单价用作按次/图片/视频原价，缺失显示 `—`。
模型列表每 3 秒条件请求一次，
请求预算 1.8 秒；响应 ETag 覆盖可见分组、用户倍率、当前时段与充值倍率，未变化返回 304。
返回 `private, no-cache` 并按授权和语言区分，不共享用户定价响应。ETag 节省传输，服务端仍会
重算目录；未进行生产大并发压测。隐藏页面暂停轮询，恢复/联网立即检查；错误保留旧价并标记过期，
权限撤回则清空价格；账户/语言变化重置条件标记，卸载取消请求和计时器。5 秒目标以浏览器前台
正常调度和网络请求能在预算内成功为前提；2026-10-20 已部署，生产 API 返回充值倍率 10。
尚未执行真实充值交易核验或网关多实例缓存收敛验收。

监控独立使用现有 V2 的 24 小时分组模型矩阵及其 60/300 秒周期，沿用登录、分组可见性和
吞吐隐藏规则。可选 `include_output_tps=true` 只在分组模型维度、≤24 小时范围读取有效文本请求
的首字后平均 TPS；查询限时 2 秒，超时或无样本为空，未来高流量需改为预聚合。
监控不通时显示未知，不影响价格同步；指标统计不是向上游发起主动探测。
模型卡片和详情的成功率时间线复用 `monitorBucketStarts`，按后端 `bucket_seconds` 与请求区间
生成格子；当前 24 小时对应 24 个小时格，不再把 24 个桶投到固定 30 格而制造灰色空档。
颜色和成功率是否显示统一使用 `monitorAvailabilityState`：有请求即按可用率着色，无请求才灰色，
不受最小样本数、综合健康分、缓存率或首字延迟是否缺失影响；普通用户隐藏请求数时保留有效状态。
此修订尚未部署；本地 7 文件/74 项定向回归、类型检查、定向 lint、前端构建和内嵌资源测试通过，
配套内嵌资源已同步；未执行生产浏览器验收。

回归入口：`model_plaza_service_test.go`、`model_plaza_handler_test.go`、
`billing_context_schedule_test.go`、`payment_config_service_test.go`、`channel_monitor_v2_test.go`、
`channel_monitor_v2_repo_test.go`，以及前端 `PlazaCatalog.spec.ts`、`PlazaStatus.spec.ts`、`ModelPlazaView.spec.ts`。
本次本地类型检查、全仓 lint、前端生产构建、后端编译与定向测试通过；前端全量 335 文件/2693 项通过，
随后补充的接口条件刷新测试在 7 文件/49 项定向回归中通过。币种与原价替换修订后，
目录交互 9 项、页面刷新 3 项及类型检查、定向 lint、构建通过。提交主分支前已同步配套内嵌前端，
重新生成 Wire，并通过内嵌资源测试和带 embed 的后端编译。用户已查看并确认本地示例预览；
2026-10-20 已随固定提交 `aa88b32a8` 部署，生产只读 API、ETag、资源摘要及公网检查通过；
自动浏览器实屏、登录后不同用户定价和真实付款/收费调用的端到端验收仍未完成。

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
[渠道监控](CHANNEL_MONITOR.md)。详细分析错误列表隐藏管理员配置为忽略的类别；管理员仍可在运维错误日志中查看原始记录。

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

渠道卡片增加标题分隔、记录槽数量和 PAST/NOW 轴，日期仍可通过提示查看。只要时间段有请求，
时间线颜色就按可用率健康阈值显示绿、黄或红色；只有无请求的时间段显示灰色。柱高按缓存命中率
70% 与首 Token 延迟表现 30% 加权，越高越好；缺少一项时使用另一项有效指标，缺口仍显示灰色。
两项都缺失时显示中位空心占位，只有无请求的时间段显示灰色。图例直接标明颜色和高度的含义，保持键盘详情。
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

2026-10-10 智能路由及后续会话粘性/超时修复已部署；下文按 2026-10-11 本地适配后的源码描述。
本轮未访问目标环境，运行代码、实际配置和验收边界见[部署状态](deployment-state.md)。
产品边界见[智能路由规则](upstream-integration-policy.md#密钥智能路由)，
固定来源与验证见[版本记录](upstream-sync-state.md#密钥智能路由与重连避让)。

创建/编辑可启用智能路由并按顺序配置最多 10 个组；跨厂商筛选不会清除已选候选。
2026-10-13 新建表单默认勾选智能路由，关闭后再次新建也恢复勾选，允许手动取消；
编辑已有密钥仍按已保存分组回填。入口为 `frontend/src/views/user/KeysView.vue`，
回归入口为同目录 `__tests__/KeysView.spec.ts`；部署证据见[部署状态](deployment-state.md)。

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
最初鉴权快照版本 23 携带列表（本轮白名单适配已升为 v24），按备用组筛选/统计密钥也能命中。

HTTP 三种文本协议复用原转发器：发送错误前显式让出请求，先换同套餐其他合格账号，
再尝试客户配置的下一兼容套餐，保留原计费、调度及协议转换。每个候选重新读取分组并检查余额/订阅；旧主组专属 RPM 覆盖不会带到备用。
全局用户 RPM 和同套餐的组 RPM 成功准入结果在同次请求中共享。模型列表合并合格候选并去重，Codex 清单保留 ETag；
原单独智能路由交付中展示配置仅影响展示；本轮官方适配已追加逐候选调用白名单及迁移 253，
当前源码以本节开头说明为准；生产迁移历史见部署状态，2026-10-20 本次部署未新增迁移。

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

### 套餐恢复顺序与共享预算

- 一个账号就是一条线路；同套餐先恢复、再尝试其他合格账号，无可用账号才进入已配置的
  备用套餐。OAuth 先执行作者允许的同账号重试、短 429 等待及内层恢复；API Key 故障可更早
  让外层重选同套餐账号。通用协议入口复用 `FailoverState.HandleFailoverError`，不复制重试循环。
- 同请求共享失败账号集合与最大切换数加一次的总尝试预算；同账号重试也消耗总预算，
  换组不清空已失败账号。HTTP 另共享容量故障三次上限、OAuth 429 账号切换限制和首输出
  超时最多一次切换；同账号重试不冒充 429 账号切换。WS 在安全首轮共享总预算，
  已建立会话、已输出内容及响应归属仍由既有边界限制。
- 智能备用可用时，API Key 不参与内层最长 5 分钟的临时不可用等待；OAuth 继续按作者等待。
  单套餐、固定响应归属与既有冷却/质量/救治准入保持。管理员内部账号池不自动改变客户套餐。
- 显式 session headers、OpenAI `prompt_cache_key`（包括 WS envelope）或 Claude metadata
  session 按密钥/端点/模型保存成功套餐 1 小时，成功刷新。故障偏好仍按会话隔离、保留 60 秒。
  套餐内恢复成功不惩罚整个套餐；健康备用保持亲和，新会话按配置顺序进入原账号调度。
- 同套餐换账号不改变价格；跨套餐按实际套餐倍率及余额/订阅归属计费。原权限、RPM、额度、
  取消、缓存计费保护和安全重放检查贯穿重入。

### 账号近期表现与分型调度

`account_recent_stats.go` 与 repository 继续用 Redis 分钟桶汇总近 10 分钟真实文本尝试，
空闲 11 分钟过期、多实例共享。取消、参数错误、本地额度拒绝、管理员探测不计失败。
明确的插件公共故障及对应 SSE 终态不计账号失败；WS 只采用当前 turn 的错误，历史错误保留给 Ops。
成功样本有首字观测才参与延迟，强制缓存计费转换不当作物理缓存命中。

前端 `AccountRecentStatsCell.vue` 显示成功率、样本量、缓存命中率及平均首字延迟；不足 10 次
提示样本少，无样本显示未知，查询失败显示暂不可用。复用批量统计与 30 秒快照缓存，界面未改。
展示仍包括 OAuth，但 **只有 API Key 使用本地近期统计参与调度**；缓存率只展示、不加分。

- 可靠度为 `(成功数+9)/(总尝试数+10)`；最近 60 秒连续失败再按
  `1 - 0.15 * min(连续失败数, 5)` 限制上限。此为实现选择，不是用户指定公式。
- OpenAI 高级调度只用 API Key 近期统计替换其 ErrorRate/TTFT 输入，OAuth 保持作者运行时 EWMA。
  默认基础权重仍为优先级 1、空闲 1、排队 0.7、可靠度 0.8、首字速度 0.5；管理员覆盖及原成本、
  额度因子继续有效。在合格 Top-K 内按 `分数 - 最低分 + 1` 加权，获取槽位失败可扩展候选；
  共享 64 次槽位获取/数据库复核预算，不能解释成 64 次业务请求重试。
- 普通/混合与 OpenAI 旧调度保留能力、价格、优先级及负载/排队/LRU 基础次序；同优先级内
  复用已有加权选择，以原排名 `(候选数-位置)/候选数` 为基础，只给 API Key 增加有限调整：
  `0.8*(可靠度-0.9)`，有延迟样本时再加 `0.5*(1/(1+平均首字毫秒/1000)-0.5)`。
  不先按微小可靠度差硬筛候选，同优先级组内没有 API Key 样本则保留该组原次序。
- 混合池不增加 OAuth/API Key 强制批次；API Key 分数会改变两类账号相对概率，
  因此不宣称整个混合池结果与作者完全一致，也不承诺均匀分流或始终选择最高分。

2026-10-12 补充源码核查：上段“不增加强制批次”不能解读为作者原有的**订阅优先**开关
已经删除或关闭。`openai_advanced_scheduler_subscription_priority_enabled` 与高级调度
同时开启时，在进入负载均衡选择后，先尝试 ChatGPT 订阅池，拿不到槽位再尝试普通池。
订阅池按 `IsOpenAIChatGPTSubscription` 判断：OpenAI OAuth 且 `plan_type` 非空、
非 `free`、非 `abnormal`；API Key、免费/未知套餐 OAuth 等属于普通池。
分池发生在池内评分前，**不只在相同数字优先级时生效**；它不等于另增“全部 OAuth
耗尽才用 API Key”的规则。已有响应归属、适用的会话硬绑定、模型能力与其他准入仍先行。
主分支的 API Key 健康硬绑定仍先于负载均衡，不能因订阅账号恢复就迁移健康 API Key 会话。
开关关闭时两类候选才在同一合格池参与评分；高级调度中的 Priority 本身也是综合评分因子，
不能一概解释为严格先耗尽某个数字优先级。

源码：`openai_account_scheduler.go` 的 `Select`、`selectByLoadBalance`、
`partitionOpenAIChatGPTSubscriptionAccounts` 和 `account.go` 的 `IsOpenAIChatGPTSubscription`。
已有测试覆盖订阅池先选、订阅满并发后普通池、关闭开关后按评分；本次仅阅读，未重跑。
2026-10-12 已只读核验线上三个开关（高级调度、粘性加权、订阅优先）均为 true；
当时线上仍为 r17bj 系列部署镜像；不能据此推定当前运行版本，最新身份见[部署状态](deployment-state.md)。

OpenAI 代理临时隔离沿用 r17bm：候选耗尽不提前放行隔离代理，保留账号池回退与适用的
有限等待，以及 CONNECT 故障短隔离/迟到成功保护。`openai_proxy_stream_circuit.go` 与
本次指定 r17bm 源码树相同；API Key 灵活选路同样受该隔离约束。用户已确认保留，具体来源
与核验边界见[版本记录](upstream-sync-state.md#追加调度核查与潜在冲突2026-10-12)。

### 会话粘性与 OpenAI 首输出保护

此前全类型强粘性、统一 180/600 秒保护已按最新分型要求修订：

- API Key 健康会话先用原账号，包括 OpenAI 粘性加权模式；总体近期错误率、速度变化不驱散
  健康会话。满并发可选同套餐空闲账号，成功后保留新绑定，无空闲候选沿用有限等待。
- OAuth 恢复作者的错误率/拥堵逃逸及加权模式；慢但健康的 OpenAI OAuth 保留作者已有的
  缓存亲和例外。逃逸不搬身份、不直接清除旧绑定。运维快照仅对非 API Key 展示作者逃逸原因。
- API Key 的 OpenAI 文本 HTTP 三入口、透传和 WS 沿用普通 180 秒、`high/xhigh/max` 600 秒
  首有效输出保护，配置仍为 `gateway.openai_first_output_timeout_seconds` 与
  `gateway.openai_high_effort_first_output_timeout_seconds`。首输出包含响应头等待；空白、
  心跳和空前导事件不解除保护。正文、思考或工具输出开始后解除该计时器。
- OAuth 使用独立配置 `gateway.openai_oauth_first_output_timeout_seconds` 与
  `gateway.openai_oauth_high_effort_first_output_timeout_seconds`，默认均为 0，遵循作者原生
  Responses/WS 的首输出计时路径；协议转换/透传不附加本地前导暂存和 180 秒断线补收层。
  普通档允许 0 或 30–600 秒，高思考档允许 0 或 30–1800 秒；高档 0 回退同类型普通档。
  旧字段不再同时控制 OAuth；部署时须核对实际配置，源码默认值不代表线上值。
- API Key 前导暂存沿用 64 KiB 内存、8 MiB 总上限和安全临时文件。HTTP 仅发心跳仍可安全
  切换，真实输出不能重放；完整终态收到后及时结束，适用路径读取 `response.failed` 用量。
  HTTP 断开后最多补收 180 秒，随后释放资源并标记用量不完整，已收用量继续原计费。
  OAuth 恢复作者 detach 行为，不承诺相同的 180 秒释放上限。
- 原生 Claude/Anthropic、Gemini、图片及视频的超时范围不扩大。原有模型 404、
  `token_group_required`/Cloudflare 403 精确分类及模型冷却保留。
- 身份种子、设备指纹、会话/线程/缓存 ID、头部及 Cookie Pin 构造复用原机制与现有数据；
  换账号调用目标账号原身份流程，不重建种子或搬移已有上游会话。

回归入口：`api_key_smart_routes_test.go`、`account_recent_scheduler_test.go`、
`openai_account_scheduler_cache_affinity_test.go`、`openai_account_scheduler_capacity_test.go`、
`openai_account_availability_wait_test.go`、`openai_first_output_timeout_test.go`、
`openai_text_output_wait_test.go`、既有身份/计费/重放回归及 `config_test.go`。
本次完整验证与未测项统一见[版本记录](upstream-sync-state.md#r17bm-本地适配结果)。

## r17bm 刷新、插件与流保护

固定来源 `1f7d36c60f361c93c828cec2a1516f11e852608f`，宿主与 Cookie Pin 0.3.14 配套适配，
已于 2026-10-12 合入主分支并推送，当时尚未部署；没有新增迁移或依赖，没有恢复独立桌面重授权工具。
2026-10-20 当前主分支已部署，实际配套插件为后续 0.3.20，具体身份及验证边界见部署状态，
不以本节历史适配版本号代替线上插件版本。

- 管理员单个/批量、OpenAI 专用及 CRS 存量刷新共用租约，覆盖上游交换至落库；新鲜 CRS
  凭据复用。按凭据/路由快照 CAS 隔离迟到结果，落库失败不发布缓存、不重复消耗旧 refresh token。
  保留无代理直连、显式无效代理拒绝、TOTP 与 Linux 授权工具；Wire 从本地 provider 重建。
- 新探针模板等待完整业务 2xx 转发后才准入，旧模板/同代失败隔离迟到成功。完整传输、
  Responses 语义成功、救治毕业仍是不同条件；人工暂停、终止、质量范围及毕业联合证据有效。
- 插件在稳定 `state/<sha256(pluginKey)>` 目录同步记录退避与操作状态，每账号两个约 8 MiB
  日志段轮转、不预分配。已派发而结果不明的探针保存为 `unknown`，跨重启阻挡后续探针；
  按精确 operation ID 人工对账后只解除此栅栏，不重放、不变成成功、不清冷却或人工暂停。
  普通业务不单因 `unknown` 被禁。具体管理命令与磁盘成本见[插件说明](../deploy/codex-lb-cookie-pin/README.md)。
- 宿主轻量健康检查异常时拒绝新插件请求，在途流最多两分钟恢复/排空。HTTP 智能路由仅对
  **明确在插件派发前拒绝**的内部错误尝试同套餐独立路径，再无候选才下一套餐；按实际插件绑定
  排除同依赖，不能按 OAuth 类型猜测。此类故障不记坏账号/坏套餐，模糊派发、已输出和取消均不重放。
  WS 插件公共故障保留作者终止行为，本轮未增加独立路径兜底。
- 不同代理/直连的 HTTP/2 池隔离；没有变更 Cookie、出口、指纹身份规则。
- SSE 增加有限失败元数据；明确的前输出服务端错误可重试，未知裸错误仍按原安全边界处理。
  待定裸错误可由后续权威完成澄清，明确失败不被覆盖；HTTP 200 不等于成功。
- WS HTTP 桥补齐有序消息、工具调用与可用加密推理历史，去重保留后续完整数据，
  不扩大首轮之外的跨组迁移或响应归属范围。插件页面分别显示探测通过、业务派发及业务完整成功。

回归入口：`openai_stored_refresh_test.go`（service/admin）、`openai_stream_failure_observation_test.go`、
`openai_ws_replay_output_test.go`、`plugin_health_runtime_integration_test.go`、`plugin_evidence_test.go`、
插件 `internal/evidence`、`internal/transport` 和 `tools/ui/bridge.test.mjs`。

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
插件采用老板 Cookie 续期、过期模板处理和配置加载保护；判分已按用户后续要求对齐 e3cad515，
不再保留本地最终答案解析及低 token 中性规则。

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
