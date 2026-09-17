# r17 稀疏复查（限流持有号每日随机复查）— Codex 评审清单

> 2026-09-16。基线：Codex 22:04 批次 + 22:11/22:13 增量（openai_gateway_usage.go 拆分、
> openai_quota_reset_signal_test.go）之上的增量改动。**未构建发布包、未部署**——等 Codex
> 剩余改动落地后合成 r17 一起出。

## 0. 用户裁定（设计依据，原文）

- 「重置不是固定的啊 有时候可以手动重置 有时候是官方重置啊」→ 死等重置点会错过
  提前重置，长持有号必须每天有复查机会。
- auto-reset 纪律不变：**检测到恢复才放行，不猜时间自动重置**。释放唯一依据 =
  上游真实接受（TransportOK + 2xx）+ CAS 清除观察到的持有对。

## 1. 行为变化（一屏版）

| 场景 | r15h/r16 行为 | r17 行为 |
|---|---|---|
| 429 带远重置点（>27h） | 下一针=重置点+错峰（可 7 天零探针） | 下一针=min(重置点+错峰, now+spread(22h))→[22h,27.5h] 每日随机复查 |
| 429 带近重置点（5h 窗等） | 重置点+错峰 | 不变（min 取重置点一侧） |
| 持有中再吃无信息 429 | 进 1h 风暴闸 + 计数 | 稀疏复查节奏，不计数，事件 class=recheck_streak_suppressed |
| 未持有吃无信息 429 | 短周期+计数+风暴闸 | 不变（对照组测试钉死） |
| 复查针被上游接受（2xx） | 持有纹丝不动，等时间到 | CAS 清除观察到的 (rate_limited_at, rate_limit_reset_at) 对 + 事件 rate_limit_recheck_recovered |
| 复查针 401/5xx/CAS 未命中 | — | 不清除、不落恢复事件（slog.Warn 最多） |

## 2. 文件与行号锚点（backend/ 相对）

### 2.1 核心实现 `internal/service/openai_downgrade_probe.go`
- `:40` 事件常量 `OpenAIDowngradeEventRateLimitRecheckRecovered = "rate_limit_recheck_recovered"`
- `:70` `openAIDowngradeRateLimitRecheckInterval = 22 * time.Hour`（spread 后包络 [22h, 27.5h]）
- `:329` 新接口 `OpenAIDowngradeRateLimitReleaser`（可选能力，窄接口+类型断言，仿 Grok 先例）
- `:1199` `applyRateLimitDeferral` **签名加了 `account *Account`**（5 个调用点同步：
  processState / reprobe / halfOpen / startSolFallback / processSolFallback）
  - 非 429 分支入口先调 `releaseObservedRateLimitOnAcceptedProbe`（见下），再 `return false, nil`
  - 有重置点分支排期改为 min 规则：`resetAt+stagger` vs `now+spread(recheck)` 取早者，
    取到复查侧时 details 落 `recheck: true`
  - 无信息 429 分支、风暴闸**之前**插入持有守卫：账号持有未到期 → 稀疏复查节奏 +
    Consecutive429s 不动 + class=`recheck_streak_suppressed`
- `:1302` `releaseObservedRateLimitOnAcceptedProbe`：守卫链（account 非空、双时间戳在场、
  持有未到期、`TransportOK && 2xx`）→ 类型断言 Releaser → CAS 清除 → 成功才追加
  `rate_limit_recheck_recovered` 事件（details 带 reset_at/http）。**所有失败路径只
  slog.Warn，绝不破坏当针成功状态机；返回 void。**

### 2.2 仓库层 `internal/repository/account_repo.go`
- `:2234` `ClearOpenAIRateLimitIfObserved`（OpenAI 版新方法）
- `:2238` `clearPlatformRateLimitIfObserved`（原 Grok CAS 重构为平台参数化私有方法，
  Grok 公开方法委托之；**去掉了 TypeEQ(OAuth)**——id+platform 已钉行，时间戳对才是真 CAS）

### 2.3 测试 `internal/service/openai_downgrade_probe_test.go`
新增 5 个（`:1203`-`:1347`）：
- `:1203` TestProbe429SparseRecheckLongHoldGetsDailyCadence — 7d 重置 → 22-27.5h 包络 +
  recheck 标记 + SetRateLimitedIfLater 照常延长持有（持有语义与探针节奏解耦）
- `:1225` TestProbe429ShortWindowCadenceUnchangedByRecheck — 1h 重置 → 重置点+错峰不变
- `:1246` TestProbe429RecheckConvergesTowardResetPoint — 23h 重置 → 无空档无双重等待
- `:1264` TestProbe429WhileHeldSkipsStormGate — 持有+无信息 429：稀疏节奏+计数为 0；
  **未持有对照组**：短周期+计数=1（r15g 语义钉死）
- `:1302` TestProbeAcceptedResultReleasesHeldRateLimit — 200 → CAS 调用带精确观察对 +
  恢复事件 + handled=false；401 → 不清；CAS false → 尝试了但不落事件

更新 4 个旧测试到 r17 语义：
- TestProbe429ResetTimeFloorsAndCaps cap 用例：90d 重置 → 断言复查包络（cap 只钳持有，
  不再钉死探针节奏）
- TestProbe429WindowClassificationRequiresExplicitEvidence：远重置（>27h）断言复查包络，
  近重置保持 After(resetAt)
- TestProbe429CooldownFailureIsNotSuccess / TestProbe429PersistenceErrorsPropagate：
  applyRateLimitDeferral 直调补 `nil` account 参数
- 桩：downgradeProbeAccountRepoStub 加 `ClearOpenAIRateLimitIfObserved` 记录调用；
  store 桩加 `eventTypes` 记录

### 2.4 跟随 Codex 批次语义更新的测试（非我的设计）
- `admin_service_duplicate_account_test.go`：复制账号并发断言 6 → `LocalAccountConcurrency`
  （Codex 并发统一 50 漏改的测试）
- `openai_account_runtime_block_fastpath_test.go` TestOpenAIHTTP429StillUsesQuotaResetHeaders：
  `x-codex-primary-used-percent` 37 → 100（r16「只认耗尽窗口」语义；37% 正是 1043/1045 病根）

## 3. 与 Codex G4 原子提交栅栏的集成（已验证，重点评审区）

**结论：生产路径下我的释放自动走你们的原子栅栏，非旁路。** 链路：

1. `processStateAtomic` → `stagedRunner(stage)`，staged runner 的 `accountRepo` **就是**
   `*openAIProbeStaging`（openai_downgrade_commit.go:246 `accountRepo: stage`）
2. 我在 `releaseObservedRateLimitOnAcceptedProbe` 里类型断言
   `r.accountRepo.(OpenAIDowngradeRateLimitReleaser)` → 命中 staging 的同名方法
   （openai_downgrade_commit.go:159）→ 暂存 `mutation.RateLimitClear`，不落活写
3. 提交侧 `CommitOpenAIDowngradeMutation`（repository/openai_downgrade_commit.go:24）校验：
   - RateLimitClear 要求 mutation.Results 里有 TransportOK+2xx（我的 401/5xx 守卫与之互为
     印证；RecordOpenAIDowngradeProbe 暂存结果，正常路径必然在场）
   - `RateLimitResetAt != nil` 则拒绝——与我的分支互斥（延长持有只发生在 429 分支，
     释放只发生在接受分支），一次 run 不可能同时暂存两者
4. SQL 真身：UPDATE 的 WHERE 带
   `($7 IS NULL OR (rate_limited_at = $7 AND rate_limit_reset_at = $8))` —— DB 级 CAS，
   外加 updated_at/status/schedulable/proxy 行版本 FOR UPDATE 复核 + 事件与状态同事务落库

评审建议关注两点：
- staging 方法（你们写的）在内存里对 `s.account` 快照做了一次时间戳预检，我的 helper
  在断言前也做了一轮等价预检——双预检冗余但无害（DB CAS 才是安全边界），可留可合并
- 非 JSON 事件 details：我的 `rate_limit_recheck_recovered` details 含 time.Time 与 int，
  走你们事件写入路径（jsonb），与其他事件同构

## 4. 验证证据

- `go test -tags=unit ./internal/service/...` → `ok 177.914s`（全绿；含 Codex 22:04 批次
  及 22:13 新测试文件）
- `go test -tags=unit ./internal/repository/...` → `ok 8.591s`
- probe/429/QuotaResetSignal 子集 → `ok 0.102s`
- `go build ./...` exit 0；gofmt 全部触碰文件干净
- 注意：**必须 `-tags=unit`**（不带标签会报 `undefined: longContextBillingRepoStub`）

## 4b. 9/17 01:44 全量回归发现的失败（Codex 22:04/22:2x 批次路径，非稀疏复查改动）

`go test -tags=unit ./internal/...` 首次全包跑，48 包 ok，3 包 FAIL。定责依据：
`.codex-validation/unit-route-final.jsonl`（9/16 03:53，即 22:04 批次**之前**）里
CapacityBudget 与 AliyunCaptcha 均 pass；两失败路径的文件全部在 Codex 昨晚批次
mtime 名单内；我的改动（探针排期/仓库 CAS/2 个测试文件）不触及这些路径。

| 失败 | 包 | 症状 | 大概率源头 |
|---|---|---|---|
| TestAPIContracts | server | usage items 多出 `reasoning_tokens:0`，契约期望没这个键 | account_usage_service.go（22:07，额度显示真实性）测试滞后 |
| TestAliyunCaptchaVerifier_TransportError | repository | transport 错误被归一化成 API 错误（期望 false 实为 true） | http_upstream.go（22:2x 批次碰过） |
| TestCapacityBudgetThroughRealHTTPHandlers（3 子测） | handler | 容量请求 0 次上游调用（期望 3 次失败后停） | ratelimit_service.go / scheduler_cache.go / account.go 选择与熔断语义变化 |
| TestResponsesFailover_CodexMachineToOff_NoResidualIDs | handler | 期望 200 实得 502 upstream_error | 同上（failover 链路） |
| TestOpenAIGatewayHandlerImages_ServerError...、Responses_FailoverAborts/Continues | handler | failover 行为变化 | 同上 |

均为确定性失败（非 flaky）。要么是行为回归需要修，要么是语义有意变更需同步测试期望
（如 TestAPIContracts 补 `reasoning_tokens` 键）——请 Codex 裁定。裁定前 r17 不宜打包。

## 4c. Codex 22:2x-22:5x 补充批次与稀疏复查的关系（已核，咬合正常）

- `openai_downgrade_schedule.go`（repository 22:51）：`ReconcileOpenAIRateLimitProbeSchedules`
  每次 RunOnce 扫描前把**存量**「上一针 429 且 next_probe_at 超出每日包络」的排期拉回
  `max(last_probe+22-27.5h, now+15-45min)`——正是我 min() 规则覆盖不到的存量长持有号
  （如 1035/1038 钉到 9/22 的）的 retrofit。注释明言不动账号持有，释放仍走 CAS。
- `Consecutive429s` 已进 `OpenAIDowngradeProbeState` 持久化（:184），风爆计数重启不再清零
  （TestProbe429StormSurvivesRunnerRestart）。与我的持有守卫兼容：持有号不进风爆闸、
  不计数，两边测试同轮全绿。
- `openai_downgrade_recovery_commit_*test.go`：给恢复提交栅栏（我释放路径的路由终点）
  补了 CAS 参数/证据校验/回滚/重放测试。
- 交互验证：probe/recovery/storm 子集 `ok`；我的 5 个新测试 + 4 个更新测试原样通过；
  我的锚点（probe.go :40/:70/:329/:1199/:1302、account_repo.go :2234/:2238）未被动过。

## 5. 明确没做的事

- 未打包、未部署（生产仍 r15h，pid 50047 / 127.0.0.1:18420 未动）
- 未改 candy 锚点（7/7、9/6、8/4→21）、阈值 800/1400、指纹 {516,1034,1552}±8
- 未动 5h 短窗行为、风暴闸对未持有号的语义、认证 1 针上岗
- 未自动重置任何账号（纪律：恢复动作=检测到的上游接受 + CAS，非时间猜测）

## 6. 9/17 r17 合成批次（本机实现，待 Codex 抽查）

### 6.1 4b 失败表的三项裁决与处置（全部落测试侧，零生产代码改动）

| 失败 | 根因裁定 | 处置 |
|---|---|---|
| TestAPIContracts (server) | Codex「额度显示真实性」批次有意给 usage items 加 `reasoning_tokens` 键，契约快照滞后 | （若已由 Codex 后续批次修齐则以当轮全量回归为准） |
| TestAliyunCaptcha_TransportError (repository) | http_upstream 错误归一化语义变更的测试滞后 | 同上 |
| handler 5 个 failover 测试 | **夹具缺陷，非行为回归**：r15 起 OAuth 出网硬闸（validateOpenAIAccountProxyRoute → openAIOAuthProxySnapshotURL）要求非 PAT openai oauth 账号带 ProxyID+活跃快照；测试账号两者皆无 → 选号期即拒，0 上游命中，502 upstream_error。设计裁定为正确（有意硬闸），修夹具不修闸 | 三处 harness 补 `ProxyID: &proxyID` + `&service.Proxy{ID, Protocol:"http", Host:"127.0.0.1", Port:18080, Status:Active}` 快照：openai_gateway_codex_machine_failover_test.go（811/812）、openai_responses_failover_cancel_test.go（1/2）、openai_images_failover_test.go（1/2） |

### 6.2 僵尸 state 饿死修复（9/17 发现，本机实现）

根因：面板软删账号的 probe state 不删，ListDue 每 IP 桶 rank-1 被僵尸钉死，
同桶活号零探针（实证：1054 钉死 p5 → 1055 降智 5.5h 未检出；15 僵尸钉死全部
5 个在用桶）。openai_downgrade_probe.go processState：
- GetByID 返回 ErrAccountNotFound 哨兵 → dropGoneAccountState 就地 DeleteOpenAIDowngradeState
- 删除失败（暂态）→ 退化为让位重排（30-37.5min spread），绝不留在队首
- 瞬时 DB 错误（非哨兵）→ 不删不重排原样上抛
- 顺带修复资格不符/不可调度 status 两条路径的静默 skip → 同样让位重排
回归：TestProbeZombieStateDeletedWhenAccountGone / TestProbeIneligibleLiveAccountYieldsQueueHead /
TestProbeDisabledAccountCannotEnterRescue（后半）/ TestOpenAIDowngradeProbePausedAccountYieldsQueueHead

### 6.3 二开 fork PreserveAccountProtection 移植（Extra 系统管理键防剥）

- repository/account_repo.go lockAndMergeAccountProbeExtra：FOR NO KEY UPDATE 行锁下
  读 sol_fallback/qualification 行内现值，陈旧表单快照一律让位；行内没有则丢弃表单值
  （+专项测试 TestLockAndMergeAccountProbeExtraPreservesDowngradeManagedKeys）
- admin_account.go / account_service.go：model_rate_limits（网关 429 运行态）加入
  陈旧表单保留清单（+TestUpdateAccount_PlainEditPreservesModelRateLimits /
  TestAccountServiceUpdatePreservesModelRateLimits）
- 附带 sqlmock 列扩容：FOR NO KEY UPDATE 行 mock 补 sol_fallback/qualification 两列

### 6.4 OAuth 来源闸（Codex 测试意图的实现）

admin CreateAccount 拒绝无真实凭证材料的 openai+oauth（缺 refresh_token/access_token/
tokens 嵌套/PAT auth_mode 一律 OPENAI_OAUTH_PROVENANCE_REQUIRED）。codex import
（account_codex_import.go:331）与数据恢复（account_data.go:432）天然携带材料不受影响。

### 6.5 验证状态

- service `ok 176s` / repository `ok 8.5s` / handler `ok 38.9s`（-tags=unit -count=1）
- 全量 ./internal/... 回归在打包前执行（结果见交付报告）
- candy 锚点 7/7、9/6、8/4→21，阈值 800/1400，指纹 {516,1034,1552}±8：未动

### 6.6 冒烟发现的 DSN 硬化项（交 Codex 裁定，非部署阻断）

setup.go buildPostgresDSN 在密码为空时产出 `password= dbname=...`（空值未加引号）。
libpq 关键字/值解析会把紧随的 token 吞为 password 的值 → dbname 丢失 → 回退
「用户名当库名」连接（psql 与应用行为一致，已实证二分定位）。生产均用真实密码
（pwfile）不受影响。建议：空值输出 `password=''` 或省略该项。未改生产代码
（改动会使已构建五平台二进制失效）；冒烟以 trust 集群 + 哑密码绕过。

## 7. r17 生产部署记录（2026-09-17，用户授权「检查没问题就部署」）

- 验证链：全量 ./internal/... 单测绿（server 契约补 reasoning_tokens 键后）→ 五平台构建
  （版本自证嵌入）→ ~/Downloads 五包 + SHA256SUMS-r17.txt → 隔离冒烟绿（health 200 /
  迁移 238-240 / 触发器在场 / 嵌入 UI 200）
- 部署：二进制落位 + start 脚本 line6 重指向（含 .before-r17 备份）+ launchd kickstart。
  旧 r15h 09:59:12 干净退出。
- 事故：SSD 卷第三次闪断（10:03-约19:00，近 9 小时）：PG 新连接全数
  `could not open file "global/pg_filenode.map": Interrupted system call`，postmaster 存活、
  checkpoint 正常、已开 fd 可用、新 open 全挂。用户裁定等待自愈；~19:00 卷自愈，
  r17 进程（21241）自行连库完成迁移并启动（19:00:01 Server started on 18420）。
- 部署后验证：health ok / schema_migrations 顶=240 / openai_downgrade_probe_controls
  13 行全 manual_paused（与池全暂停一致）/ 双触发器在场。
- 备份：部署前 pg_dump 因脚本 PGPASSWORD 未 export 而失败（0 字节残留
  sub2api-pre-r17-20260917-095911.dump 留档）；愈合后补
  sub2api-post-r17-migrations-20260917-190321.dump（79.5MB，迁移后锚点）。
  迁移集为增量式（建表/回填/触发器），回滚=脚本重指向旧二进制即可，兼容 237 库。
- 遗留观察：池 13 号全 manual_paused → 探针全走暂停分支让位重排（30-37.5min），
  零实际探针，直至用户重新启用；1052 的 redis:nil 缓存 miss 为中断后良性现象。

## 8. 裁定请求：无桶 OAuth 新号的自动绑桶已被 authorization_proxy_missing 前置拦截（2026-09-17 20:5x 生产实证）

**现象**：20:44 新传 1070/1071（openai/oauth，凭证齐全，无 controls 行）→ state 正常 armed（qualification/on_duty/sched=f）→ 每轮吃 `qualification_blocked{reason:authorization_proxy_missing}`，`FindOpenAIDowngradeMainProxy` 绑桶分支不可达（桶有余量：6×1、8×2、9×2、10×2，非"无候选"）。

**代码**：openai_downgrade_probe.go :718-736（r16 拦截，含注释 "A missing authorization route is not permission to assign a new IP... never restore it by guessing"）先于 :745-760（r15b 自动绑桶）返回。**r15b 的 OAuth 新号自动绑桶在 r17 上事实上失效**（9/17 凌晨 1053-1055 的自动分桶流程在 r15h 可用、r17 不再可用）。

**语义张力**：拦截分支的意图是"丢失路由的号不得乱猜 IP 恢复"（证据保全），但写法命中了**从未绑过桶的新上传号**——这类号没有"旧绑定"可保全，拦它们只产生停机坪效果。

**候选裁定**：(a) 维持现状=新号一律面板人工绑桶（运维多一步，语义最保守）；(b) 放行"从未有过 proxy 绑定且 qualification 模式"的新号进 FindOpenAIDowngradeMainProxy（恢复 r15b 行为，丢失绑定的号仍拦）——需要区分"从未绑定"与"绑定丢失"（accounts.proxy_id IS NULL 且 state.original_proxy_id IS NULL 可判）。

**当前生产处置**：不改代码；1070/1071 由用户面板人工绑桶后认证针自动恢复。已告知用户两选项。

### 8.1 裁定与实施（2026-09-17 21:1x-21:3x，r17b）

**用户裁定：「肯定要自动啊」→ 方案 (b)**。新上传号必须自动分桶（恢复 r15b 行为），
丢失绑定保护收窄为只拦「绑过又丢」的号。

**实现**（openai_downgrade_probe.go 拦截条件加一个析取否定）：

```go
if state.ProbeMode == "qualification" && account.ProxyID == nil &&
    state.OriginalProxyID != nil &&            // ← 新增判据
    !account.IsOpenAIPersonalAccessToken() && !account.IsOpenAIAgentIdentity() {
```

判据 = state 里从未记录过 original 绑定（`OriginalProxyID == nil`）。绑过桶又丢绑定的号
（OriginalProxyID 非 nil）照旧吃 `authorization_proxy_missing`（证据保全语义不动）；
从未绑过的新上传号落到 :745-760 的 `FindOpenAIDowngradeMainProxy` 自动绑桶。

**原子路径**（processStateAtomic→armQualificationAtomic 同判据）：staging 的
`SetOpenAIAccountProxy` 置 `mutation.ProxyChanged=true; mutation.ProxyID=new`；
`ExpectedProxyID` 保持 CAS 旧值语义（nil = 只允许从未绑定态提交绑定，并发下面板
先手绑桶会 stale 失败回滚，不覆盖）。

**测试**（-tags=unit，service 全绿 176.9s）：
- 翻转：`TestOpenAIProbeMissingAuthorizationRouteCannotAssignNewProxy` 删 new_account
  用例（该场景现 intentional 放行）；`TestOpenAIProbeQualificationMissingAuthorizationRoute`
  删 unbound 用例（同上）。保留用例全部 oldProxy 非 nil（拦截语义回归锁）。
- 新增：`TestOpenAIProbeFreshUploadAutoBindsBucket`（plain 路径：proxyChanges 落桶、
  探针在绑后桶上发、Schedulable 不被 lost-route 守卫误停）、
  `TestOpenAIProbeFreshUploadAtomicAutoBind`（原子路径：ExpectedProxyID=nil 的 CAS 前置、
  绑桶与探针结果同事务提交、Results=1）。

**生产预期**：1070/1071（9/17 20:44 上传，原始触发例）部署 r17b 后下一轮 ~1min 扫描
自动绑主桶（6/8/9/10 有余量）→ 资格认证针 → 1 针通过即上岗（r15h 语义）。

## 8. 资格探针 200-无-usage 谜题：三轮裁决与真根因（2026-09-17/18，r17d→r17e→r17f）

**现象**：r17b 之后自动绑桶与资格流程已通（1070-1077 全部进入 qualification
节奏、稳定吃到 200），但 `transport_ok=f` 恒败于同一形态：200 全长 SSE 流、
`probe response missing valid completion or reasoning usage`。生产 0/N。

### 8.1 裁决路径（含两次证伪，保留证据链）

- **r17d「连接复用」假说：证伪**。探针改一次性专用传输（DoProbeWithTLS，
  02:16 重启 / 03:48 部署）后 1076/1077 仍 0/2 败。该修复本身保留（与真实
  codex exec 每进程新连接形态一致，防御正确），但不是本案根因。
- **进程外复现的陷阱**：冻结/live body × uTLS/原生 × h1/h2 共 12/12「通过」——
  但复现用的是 **gjson 宽松提取 usage 字段**，从未跑生产严格解析器。这批
  「通过」只能证明服务端返回了 usage，不能证明流可判分。
- **r17e 取证日志**（仅 ！TransportOK 落一条 Warn，纯诊断零行为变更）：
  04:14:44 首针取证——bytes=249602、data_records=436、has_terminal=t、
  has_usage=t、has_reasoning=t、tail 落在 usage 对象上。04:27:44 第二针同形。
  **服务端没剥 usage、流没截断——解析器拒收合法流。**

### 8.2 真根因（离线冻结流裁决，零网络）

拿 02:06 生产冻结捕获流（1077/proxy6，gjson 验过 rt=1552/2070）喂生产解析器
（TestFrozenProbeStreamParse）+ 逐门 python 忠实复刻状态机：

- SSE 框架/ID 一致性/delta 通道全过（无 reject、单 lane、trailing 干净）
- 终态 `response.completed` 嵌套 `response`：status=completed、error=null、
  usage 齐全——**唯独 `output=[]`（键在场、数组空）**
- 死点：文本提取级。终态 output 空走查 → text 空；delta 累积通道被
  `allowLegacyDeltas = !nested` 对嵌套终态关死 → 拒收。

**服务端 9/17 05:25 后改流形态**：终态不再回显 output 条目，答案全文改经
`response.output_item.done` 的 message 条目（+delta 流）交付。DB 时间线互证：
全库最后一次探针通过 = 1060 @ 9/17 05:25:13；200-无-usage 形态 21:49 起全部
属于其后新探测的批次。与账号类无关（1056-1069 若今天被探同样会败）。

**附带闭环**：1076/1077 schedulable=t 却从未资格通过——200-无-usage 是
「无结论」（IsDegraded 首行 ！TransportOK→false），不惩罚也不拦调度，非 bug。

### 8.3 修复（r17f，openai_downgrade_probe.go）

1. SSE 走查新增 `response.output_item.done` 分支：与终态 output 数组走查
   同规则（共用新 helper `appendOpenAIProbeItemOutputText`：跳过非 message/
   非 assistant、status 非 completed 硬拒、output_text 逐段追加）累积条目终文。
2. 文本提取级改三段式：终态回显优先 → **空回显（键缺席或空数组皆算）回退
   条目终文** → legacy delta 兜底。旧形态行为逐字节不变。
3. 回归：TestParseOpenAIDowngradeProbeResponseAcceptsItemDoneDelivery
   （新形态判分认条目终文不认中途 delta / 旧形态终态回显优先 / 无终态仍拒 /
   incomplete 条目硬拒）+ TestFrozenProbeStreamParse（env 门控，生产冻结流
   必须过——注意判分正则不能传 nil，nil 死在解析器第一行，勿重蹈覆辙）。

### 8.4 部署与验证

- r17e（取证版）04:12:42 pid 84803；r17f（修复版）随后。备份锚点
  sub2api-pre-r17{e,f}-*.dump。验证看点：1076/1077 首次 `transport_ok=t`
  带 reasoning_tokens，资格 1 针上岗（r15h 语义）。
- r17d 一次性传输与「探针遥测默认关」（OPENAI_PROBE_TELEMETRY 双开关）保留；
  plist 注入的 OPENAI_CODEX_TELEMETRY_ENABLED=0 在验证后移除恢复真实遥测。
