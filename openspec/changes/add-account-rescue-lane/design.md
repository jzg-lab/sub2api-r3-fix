# Phase 0 核实结论（2026-10-02 四路只读调查，全部 file:line 实证）

代码库：`.worktrees/r17ax-rescue-lane/backend`（HEAD 67d990f）。本文件是实现的直接依据。

## 0.1 种子流量载体（已定案）

**载体 = 账号测试路径，service 层直调，零新增协议。**

- 调用链：编排器 → `TestAccountConnection`（account_test_service.go:274，**不校验账号死活**）→
  `testOpenAIAccountConnection`（:647，OAuth 分支 :703 目标=codex responses 端点，与业务转发同 URL）→
  `doOpenAIAccountTestUpstream`（:801）→ `RoundTripOpenAIOAuth`（openai_plugin_transport.go:36）→
  插件 Forward 流（plugin_runtime.go:210，ForwardRequestStart 帧 :216-229，插件捕获模板）。
- 插件路由条件 = 平台OpenAI + OAuth + 灰度桶（plugin_manager.go:957-963），**不看账号状态**；
  生产 rollout=100% 全命中。
- 降智探针/诊断针/转正冷针**不过插件**（openai_downgrade_probe.go:2170 → http_upstream.go:320
  一次性冷传输）——保持无签冷启动语义，判定不被插件 Cookie 污染（设计优点，保留）。
- 网关全部业务出口收口 `doOpenAIUpstream`（20+ 调用点），WS 入口对插件命中号强制 HTTP
  bridge 防绕过（openai_ws_forwarder_ingress.go:123-124）——救治号开调度后流量不会漏出
  插件视野。

## 0.2 状态桥形态（已定案，工程量缩水）

**不新增 gRPC 方法——插件 Health 响应早已携带 status_json，fork proto 缺字段把它丢了。**

- 插件 `Health` 已返回 `status_json`（官方 0.2.11 协议字段 3），内容含 `prober` 区段 =
  每账号 LastVerdict/ConsecFails/QualityRerolls/SuspectAccountLevel（插件 server.go:113-120、
  536-583；prober.State 定义 internal/prober/prober.go:103-116）。
- fork proto `HealthResponse` 只有 {healthy,message}，status_json 落 unknownFields 被丢弃；
  `TestConfigResponse` 同样缺官方的 status_json=4。
- **fork 三处改动**：① plugin.proto 给 HealthResponse 加 `string status_json = 3;`、
  TestConfigResponse 加 `string status_json = 4;`（与官方同字段号，wire 纯加法）+ 重生成；
  ② 新只读端点 GET /admin/plugins/:id/status，内部调 Health 透传 status_json（reconcile
  每秒已在调 Health：plugin_manager.go:309-317，调用模式已验证）；③ PluginsView.vue:611-614
  把 "plugin.status" 加进 expectsResponse + switch 加 case——插件 UI 已每 5s 轮询该消息
  （ui/index.html:259,289），接通后**面板「状态不可用」降级点免费复活**。
- **插件 0.3.0 范围 = status_json JSON 内容扩展**（不碰 proto/manifest）：新增
  consecutive_passes 计数器（现在只有连错）+ 签寿命计量字段（sign_captured_at /
  lifetime stats / estimated_remaining）。status_json 本就是插件自定义 JSON blob。
- **manifest 禁改**：schema 双重锁死（plugin_package.go:214-219 DisallowUnknownFields +
  manifest.schema.json additionalProperties:false），加字段=拒装。未来新 RPC 的能力协商
  位置 = GetInfo.capabilities（repeated string，fork 启动校验不读内容 plugin_runtime.go:82-91）。
- 官方 v0.2.11 零影响的结构性保障：宿主→插件方法只能宿主发起，官方编译产物没有新方法
  的 client stub，物理上发不出来；status_json 字段官方 wire 本就有。

## 0.3 判死落点、分类与钩子（已定案 + 一个必须处理的冲突）

**三个落点，单一 choke point = processStateAtomic 提交块。**

- 落点 A：probe.go:1254-1266（qualification 连败/reenable 一击退出，事件
  `replace_required{reason:"qualification_failed"}`）
- 落点 B：probe.go:1538-1565 finishReplacement（sol 针仍降智，事件 `replace_required{swap_count_7d}`）
- 落点 C：harvest.go:309-341 abandonHarvest（事件 `harvest_abandoned{reason}`；reason=
  `credentials_invalid`=401 类 / `account_level_degraded`=账号级）
- **钩子挂点：commit_svc.go:424-433 `if committed` 块**——三条路径全部经 staged mutation
  在此一次性提交，判死必 committed==true。可用信息：mutation.State / **mutation.Events
  （reason 现成的入口过滤器）** / mutation.Results（可套 ClassifyOpenAIDowngradeProxyOutcome
  五分类复核，注意实际函数名是 ProxyOutcome 不是任务书写的 ProbeResult，probe.go:313-336）/
  mutation.Schedulable / account 快照（RateLimitResetAt）。
- **入口过滤（精确规则）**：Events 含 harvest_abandoned+reason=credentials_invalid → 排除；
  account.RateLimitResetAt 在未来 → 排除（429 永不导致判死，只动 rate_limited_at 两列
  account_repo.go:2341-2368）；其余 pending_replace 提交 → 入区（含 account_level_degraded）。
- auth 两振出局**不落 pending_replace**（落 status=error 线，probe.go:2016 SetError）——
  不需要额外过滤，天然不在视野内。

**⚠ 冲突：maybeAutoHarvestDead（probe.go:832-845）已在抢同一批号。** 每分钟扫
status=active 的 pending_replace 号，静默期（NextProbeAt，2h-24h）满即自动拉进打票线
（改 probe_mode=harvest、state=on_duty、迁动态桶）。**救治区入区转换必须让它让位**
——在 maybeAutoHarvestDead 判定处加救治成员资格闸（救治中的号不进打票线），否则判死
2h-24h 后号被抢走。出区（转正/疑似账号级撤调度/人工了断）后恢复原语义。
- reconcile（schedule.go:103-179）不排 pending_replace 但要求末针=429——边缘竞态由
  入口②对账清扫兜住。
- **调度资格验证成立**：schedulableAccountsQuery（account_repo.go:2048-2067）无任何探针
  状态检查——pending_replace + schedulable=true + status=active 会被排进真实流量；手动
  SetSchedulable 只过 browser-OAuth 资格闸（openai_oauth_proxy_binding.go:268-285）。
  9/28 结论在当前代码成立，救治区机制前提牢靠。

## 0.4 组改写通道（已定案）

- **改绑 = repo 层 `BindGroups`（account_repo.go:1955-2009）**：事务内删光+重插，
  outbox 应用侧（:2004-2007）+ DB 触发器 238 双份自动发。不自写 SQL、不走 HTTP。
  r17v 撞键在此路径结构性不存在。
- **进区前快照原组 id+priority**（GetByID 装配 GroupIDs / loadAccountGroupIDs:3660）——
  account_groups 行会被删光，不存即丢回绑目标。回绑用快照的真实池组 id（不用
  openai-default id）。priority 会被重写为 i+1，需还原则快照存值。
- 救治组命名避开 `openai-default`/`K12`/`Team`/`Plus`（池组语义+BEFORE 改写触发器歧义）。
- r17v 两触发器=生产库本地手工件（sub2api_local_ 前缀），代码库不可见；池组自动绑定
  只挂 accounts INSERT，BindGroups 的 DELETE+INSERT 不会重触发。
- 开/撤调度 = `adminService.SetAccountSchedulable`（admin_account.go:1381-1396）→
  BulkUpdate → account_bulk_changed outbox。
- **CAS 纪律：updated_at 是调度快照版本号**（238 触发器族：组/proxy/组序任何变动都推它）。
  编排器改组/撤调度后**必须重读账号**再构造 reenable mutation，否则必吃
  ErrOpenAIProbeStale（openai_downgrade_commit.go:85-107 CAS）。
- audit：HTTP 审计中间件（audit_log.go:329）不覆盖 service 内部调用——编排器的
  rescue_entered/graduated/suspected 事件自理写 audit_logs。

## 实现顺序修正（依 Phase 0 结论）

Phase 1（插件 0.3.0）与 Phase 2（fork proto+状态端点+桥）解耦可并行；Phase 3 编排器
依赖 0.3 钩子结论与 0.4 通道；**新增前置项：maybeAutoHarvestDead 让位闸必须与入区转换
同一提交落地**（否则救治号 2h 后被抢）。种子调用直接用 TestAccountConnection，无需新载体。
