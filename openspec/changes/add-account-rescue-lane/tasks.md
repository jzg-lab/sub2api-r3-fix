# Tasks: add-account-rescue-lane

## Phase 0 — 前置核实（写码前必须完成）

- [x] 0.1 种子载体=TestAccountConnection（service 层直调，不校验死活，过插件 Forward，
       插件路由只看平台/OAuth/灰度桶；降智探针/冷针不过插件=判定不被插件污染）→ design.md 0.1
- [x] 0.2 状态桥=插件 Health 早已带 status_json（官方字段3）fork proto 缺字段丢弃；
       补字段+透传端点+PluginsView 桥 case 即可（不新增 RPC）；manifest 双重锁死禁改；
       官方零影响=结构性保障（官方产物无新方法 stub）→ design.md 0.2
- [x] 0.3 钩子=commit_svc.go:424-433 committed 块（三判死路径单一 choke point）；
       过滤=Events reason + RateLimitResetAt；调度资格验证成立（schedulableAccountsQuery
       无探针状态检查）；**原冲突=maybeAutoHarvestDead 抢号——已由打票线整体删除解决（2026-10-02）** → design.md 0.3
- [x] 0.4 改绑=BindGroups（事务删光+重插，outbox 双份，不撞 r17v 触发器）；进区前快照
       原组 id+priority；CAS 纪律=改组后必须重读再构造 mutation；audit 编排器自理
       → design.md 0.4

## Phase 1 — 插件 v0.3.0（~/sub2api-cookie-plugin）

- [x] 1.1 状态扩展（status_json 内容，不碰 proto/manifest）：consecutive_passes/
       in_backoff/sign_captured_at/sign_lifetime_stats{p50,p80,min,samples}/
       estimated_remaining_seconds/estimate_basis{measured|fallback|none}；
       契约测试锁定；Cookie 值零泄漏（脱敏边界测试）
- [x] 1.2 退避期满复探回升语义确认（BackoffUntil 过即 Due→pass 摘帽；既有
       TestStateFailRerollPath 覆盖，v0.3 未改动该路径）
- [x] 1.3 testhost 全链路 ✓（build.sh 冒烟绿）；官方 sidecar 宿主兼容回归 ✓
       （10/2 官方 v0.2.11@18200：上传 trusted+compatible+tested、启用健康
       "lb cookie pin ready"、prober/adaptive_scheduling 区段经 /status 完整
       透传、新配置键 adaptive_probe_scheduling/probe_schedule_margin_seconds
       SaveConfig 往返生效。**官方宿主升级须先 disable 再 upload**——生产
       0.2.1→0.3.0 会有一个插件停用窗口，上产清单单列）
- [x] 1.4 构建+签名 0.3.0（dist/lyunlong-codex-lb-cookie-pin-0.3.0.s2plugin，
       key_id=843c85d4b99d8a25）；tested_versions 暂不变，r17ax 落地后补
- [x] 1.5 签寿命计量：三死亡信号全归档（捕获覆写=换签/TTL 到期清理/重摇——
       faster-model+探针答错+手动共用 Drop/Reroll 通道）；32 样本滚动窗口
       最近秩百分位；零值/未来时刻守卫防时钟跳变
- [x] 1.6 卡点排程 AdaptiveNextProbe：目标=捕获+p80−margin，年轻签少探省额度
       （上限 7200s）、临死密集盯防（下限 300s 指纹纪律）、jitter=min(距目标1/4,
       margin) 均匀前抖；分布测试 2000 抽样验证散布+均值（TestAdaptiveJitterSpread）
- [x] 1.7 冷启动：样本<3 或无签 → 固定间隔（MinAdaptiveSamples=3）；估计基准
       estimate_basis=fallback 兜底 TTL 口径诚实标注

## Phase 2 — fork 后端：插件状态桥

- [x] 2.1 fork proto 补 status_json 字段（HealthResponse=3 / TestConfigResponse=4，与官方
       同字段号 wire 纯加法）+ 重生成
- [x] 2.2 新只读端点 GET /admin/plugins/:id/status（内部调 Health 透传 status_json）
- [x] 2.3 PluginsView 桥补 plugin.status case（插件 UI 已 5s 轮询，面板「状态不可用」复活）
- [x] 2.4 宿主状态轮询缓存（30s）+ 插件离线判定（>2min），暴露到健康快照 API
       （信封化：OpenAIAccountHealthListResult{accounts, plugin_bridge}；离线=距最近
       成功 Health RPC>2min 读取时刻现算，RPC 失败轮保全旧锚点；桥故障绝不拖垮账号列表）
- [x] 2.5 桥接层测试（离线降级/恢复自愈/空状态/旧插件无字段兼容）
       （plugin_manager_status_test.go：15 用例——离线矩阵 5 态/自愈/锚点保全/
       旧插件/非法 JSON/RPC 静默/缓存不重探/BridgeStatus nil×2/信封透传×3）

## Phase 3 — fork 后端：救治编排器

- [x] 3.1 入口转换函数 enterRescue(account)：快照原组 id+priority → BindGroups 绑救治组
       → 开调度 → Extra 救治标记 → 种子=TestAccountConnection service 直调 → 事件
       rescue_entered{trigger: auto|manual|reconcile}（改组后重读账号，CAS 纪律）
       ✓（10/2）openai_rescue_lane.go EnterRescue：标记-first（崩溃可对账补绑）→BindGroups
       →重读后 SetSchedulable→种子（RunTestBackground 内存直调，失败只记 seed_ok 不回滚）
       →事件；幂等（标记在场直接返回）；ShouldAutoEnterRescueLane 纯函数过滤；
       19 用例全绿（openai_rescue_lane_test.go）；wire 装配=DefaultOpenAIRescueLaneConfig
       （enabled=false 上线默认关）+种子适配器（3.8 落地时换 settings 闭包）
- [x] 3.2 自动钩子：挂 committed 块（三判死路径单一 choke point），
       入口过滤=mutation.Events reason + RateLimitResetAt（三类不进）
       ✓（10/2）openai_downgrade_commit.go committed 块：pending_replace 提交生效→
       异步 goroutine（3min 上界，mutation 值拷贝防竞争）→MaybeAutoEnterRescue
       （开关关零成本跳过）；过滤改按 mutation.Results 复核 Classify…AuthError
       （7ce95d8 后 Events reason 收敛，401 类判据改 Results——打票线删除时已定）；
       账号快照提交后重读取（限流复核用），读失败按无证据继续（对账兜底）
- [x] 3.2b ~~maybeAutoHarvestDead 让位闸~~——打票线已整体删除（2026-10-02 用户裁定
       「把打票这条线清除，救号只保留救治区这条线」）：openai_harvest_pipeline.go /
       openai_codex_ticket*.go / 票表 repo 删除；migration 248 防御性清场（harvest 行归
       normal+pending_replace、DROP harvest_attempts、DROP openai_codex_tickets、CHECK 收窄）；
       网关注入+采票观察哨+前端入口+i18n 全清；让位闸不再需要（无打票状态机可抢号）
- [x] 3.3 对账清扫：周期扫描够格未进区 → 补进（trigger: reconcile）
      （r17ax：RunReconcileSweep 双职责——①补进：OpenAI+资格+status=active（auth 两振
      SetError 天然排除凭据死）+限流未持有+探针态 pending_replace → EnterRescue{reconcile}，
      探针状态经 OpenAIProbeHealthLister 窄接口批量取（wire 启动断言，失败降级=只自愈）；
      ②标记-first 崩溃窗自愈：标记在场但绑定丢失→补绑、调度未开→补开，疑似账号级撤调
      标记（openai_rescue_suspected，3.5 写入）在场时不复活调度。Start/Stop 进 wire
      生命周期，清扫循环每轮重读配置（3.8 settings 热更即生效）+正向散布 1.0-1.25x，
      单轮 5min 上界。6 用例：自愈补绑补开/疑似不复活/补进/凭据死与限流持有排除/
      无状态源降级/关闸零名单拉取）
- [x] 3.4 标签计算：救治中/已复活(连过≥6)/疑似账号级(backoff)；插件离线降级态
      （r17ax：LabelOpenAIRescueAccount 纯函数裁决——in_backoff→疑似账号级灰红不可点/
      连过≥阈值→已复活绿可点（转正必须考证,冷针拦截过期数据,离线缓存仍可点）/
      其余→救治中蓝紫；退避期满 in_backoff 翻回 false 自然回升救治中。数据链：仓库
      快照加 extra->'openai_rescue_lane' 原文列→ParseOpenAIRescueLaneMarkerJSON 容错解析
      （与 Extra 读共享 parseOpenAIRescueLaneMarkerFields 核心）；桥 status_json 经
      ParseOpenAIPluginBridgeProber 容错解析 prober.accounts[]（3.5 撤调同一数据源）；
      ListOpenAIAccountHealth 组装时覆盖标签+挂 Rescue 注记（悬停：救治时长/连过/连错/
      退避到期/毕业阈值/plugin_offline）；paused 与 rate_limited 优先于救治标签；
      无桥/桥离线→救治中+plugin_offline 注记（调度语义不变）。12 用例）
- [ ] 3.5 疑似账号级自动撤调度 + 退避回暖恢复调度
- [ ] 3.6 转正后处理：reenable 通过后用快照 BindGroups 改绑回原池组（真实组 id，不用
       openai-default）+ 打复活标记（Extra rescued_at 永久 + 复活次数+1；不清除不重置）
- [ ] 3.7 手动入区端点 POST /admin/openai/accounts/:id/rescue（幂等+audit）
- [ ] 3.8 配置项 rescue_lane.*（enabled/group/consecutive_clean_passes/
       reconcile_interval）+ 缺省安全值（enabled=false 上线默认关）
- [ ] 3.9 编排器状态机测试（入口过滤/对账/标签流转/撤调恢复/转正改绑/删除清位）

## Phase 4 — fork 前端

- [ ] 4.1 三标签 + 插件离线角标（健康格渲染规则，沿用 7 标签既有模式）
- [ ] 4.2 「已复活」点击 → 现有 reenable 确认弹窗（probeDeadConfirm 复用）
- [ ] 4.3 「送入实验台」按钮 + 三条可见性规则 + 幂等提示
- [ ] 4.4 复活号永久徽标（转正后行内常驻；悬停复活时间/次数；与正常号视觉强区分）
- [ ] 4.5 签余量读秒芯片（徽标旁：签龄/预计余 mm:ss 逐秒走钟；桥 30s 校准；
       faster-model 归零事件联动；估计值标注）
- [ ] 4.6 vue-tsc + vitest 全绿

## Phase 5 — 验证与部署（铁律全流程）

- [ ] 5.1 sidecar E2E 全流程（验收标准 1-4 逐条）
- [ ] 5.2 全量回归 + 新增测试全绿；前端构建后 commit 再 make build（版本戳教训）
- [ ] 5.3 铁律三段式：同配方重建 r17aw 对照 → 候选 vs 对照 rodata 去版本串逐字符比对
- [ ] 5.4 生产部署清单（start 脚本/配置/回滚件/插件 0.3.0 上传）→ 用户逐项点头执行
- [ ] 5.5 生产验证：组隔离、首号入区观测、主池流量零影响

## Phase 6 — 收尾

- [ ] 6.1 手动通道处置存量 6 号（1214-1219）的决定：等正式版 or 手工先试
- [ ] 6.2 记忆更新（部署形态 + 插件项目）
