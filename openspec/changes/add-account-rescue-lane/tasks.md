# Tasks: add-account-rescue-lane

## Phase 0 — 前置核实（写码前必须完成）

- [ ] 0.1 核实 fork 宿主侧哪条请求路径过插件 Forward 钩子：accounts/:id/test、诊断针
       路径B（triggerDiagnosticProbeNow）、网关转发，三者逐一确认；选定种子流量载体
- [ ] 0.2 核实插件协议能力协商机制，确认新增状态消息对官方 v0.2.11 宿主零影响
- [ ] 0.3 核实判死分类的落点：探针状态机里 pending_replace 迁移的降智类/401类/429类
       判别字段（钩子挂点与入口过滤器共用）
- [ ] 0.4 核实组绑定改写的既有通道（account_groups 触发器/ scheduler_outbox 惯例，
       r17v 教训：绕开触发器撞键）

## Phase 1 — 插件 v0.3.0（~/sub2api-cookie-plugin）

- [ ] 1.1 状态查询消息：每账号 {state, consecutive_passes, consecutive_fails,
       last_probe_at, reroll_count, sign_captured_at, sign_lifetime_stats,
       estimated_remaining}；Authorization/cookie 值绝不进状态（反指纹纪律）
- [ ] 1.2 退避期满复探状态回升语义确认（backoff→active 翻转点）
- [ ] 1.3 testhost 全链路验证 + 官方 sidecar 宿主兼容回归（0.3.0 在官方 v0.2.11 上
       既有功能零回归）
- [ ] 1.4 构建+签名 0.3.0，manifest tested_sub2api_versions 增补
- [ ] 1.5 签寿命计量：捕获时间戳+三类死亡信号（faster-model 实时/探针答错/TTL刷新）
       归档；每账号滚动 p50/p80/min 统计
- [ ] 1.6 卡点排程：next_probe = sign_captured_at + 寿命p80 - 余量，窗口内 jitter
       抖动（分布测试验证与换签时刻无强相关）；退避路径不适用卡点
- [ ] 1.7 寿命统计冷启动：样本<3 时退回保守默认 TTL（现有 240s 兜底逻辑衔接）

## Phase 2 — fork 后端：插件状态桥

- [ ] 2.1 宿主↔插件状态轮询（30s）+ 进程内缓存 + 插件离线判定（>2min 无响应）
- [ ] 2.2 状态暴露到健康快照 API（ListOpenAIProbeHealthSnapshots 扩展救治字段）
- [ ] 2.3 桥接层测试（离线降级/恢复自愈/空状态）

## Phase 3 — fork 后端：救治编排器

- [ ] 3.1 入口转换函数 enterRescue(account)：绑救治组 + 开调度 + Extra 救治标记 +
       种子请求 + 事件 rescue_entered{trigger: auto|manual|reconcile}
- [ ] 3.2 自动钩子：挂判死迁移点（Phase 0.3 核实的落点），入口过滤三类不进
- [ ] 3.3 对账清扫：周期扫描够格未进区 → 补进（trigger: reconcile）
- [ ] 3.4 标签计算：救治中/已复活(连过≥6)/疑似账号级(backoff)；插件离线降级态
- [ ] 3.5 疑似账号级自动撤调度 + 退避回暖恢复调度
- [ ] 3.6 转正后处理：reenable 通过后自动改绑回主池组 + 打复活标记
       （Extra rescued_at 永久 + 复活次数+1；不清除不重置）
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
