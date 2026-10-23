# 当前实现与回归入口

本文描述当前源码中已存在的行为及代码入口，包含老板版基础能力和本项目调整。
LOCAL 编号用于索引，不表示对应功能完全由本项目原创。
产品选择及尚未实施的决定统一见 [产品规则与上游差异](upstream-integration-policy.md)，
来源核对进展见 [版本与合并记录](upstream-sync-state.md)，运行环境见 [部署状态](deployment-state.md)。
本文以本地源码为准；当前线上提交、插件和实际部署证据统一见[部署状态](deployment-state.md)。

## 更新入口缺陷修正（已随 f06a57d 部署）

2026-10-20，本地回归复现更新入口指向官方仓库、沿用无来源缓存、源码构建仍可进入原地替换流程，
以及版本比较丢失构建元数据前的补丁号和本项目序号。`update_service.go` 已修正为本项目仓库，
缓存绑定仓库身份；源码及未知构建类型的更新、回退和回退列表在后端拒绝，未修改管理员鉴权。
`v<官方版号>+r17.<序号>` 按正整数序号比较，部署 SHA 不参与发布排序；旧格式与新格式混用、
非法序号、同序号对应不同官方基线时不猜测升级关系。插件宿主最低版本仍使用原兼容规则。

前端 `VersionBadge.vue` 与 app store 保留检查警告；查询失败或身份无法确认不再显示“已是最新”，
也不保留旧的可更新操作。移除指向官方安装脚本/镜像的手工回退命令，改为本项目部署指南入口。
这些基础修正本身不等于二期部署按钮；本轮追加的受管实现见下节。传统 release 的二进制更新不等于容器快速部署，
不能用版本排序替代迁移、插件或数据回退兼容证明。

回归入口：`backend/internal/service/update_service_test.go`、`system_handler_test.go`、
`plugin_compatibility_test.go`，以及 `frontend/src/components/__tests__/VersionBadge.spec.ts`、
`frontend/src/stores/__tests__/app.spec.ts` 和两种语言的消息编译/键冲突测试。
初次接入时未分配新版本号；2026-10-24 已按用户要求登记首个候选 `v0.2.17+r17.1`，
未创建 GitHub Release。2026-10-23 随 `f06a57d` 首次接入生产，自动准备与受管更新流程见
[二期计划](merge-deploy-guide.md#第二期实施计划)。

## 第二期部署按钮（已首次接入生产）

2026-10-23 `f06a57d` 镜像、双槽及受管控制器已在生产首次接入；当时清单为空、管理员 TOTP 未启用。
2026-10-24 GitHub 清单已登记首个固定源码候选 `v0.2.17+r17.1`，随后已发起真实准备，尚未切换；
真实的“页面准备 → 确认上线/回退”尚未验收。以下保留各开发阶段的本地回归记录，
实际运行身份与首次切换证据以[部署状态](deployment-state.md)为准。

用户纠正了“终端先行”的范围理解：日常普通更新必须在页面完成，不靠 AI 或手工执行 SSH 命令。
复用版本入口新增 `DeploymentDialog.vue` 和 `api/admin/deployment.ts`，支持候选、自动准备、
确认上线、持久结果、断线重连和兼容回退。采用现有弹窗、图标、TOTP 二次验证及中英文消息。
确认绑定原任务，过期或被其他管理员替换则失效；网络错误不自动重发变更。

后端 `managed_deployment_service.go` 只通过 Unix socket 向独立执行器提交固定动作与 ID，
不传凭据、命令、URL 或文件路径。管理员路由沿用鉴权、审计和 step-up；设置
`MANAGED_DEPLOY_SOCKET` 后，服务端阻断旧更新、回退/列表和重启接口。执行器失联不回落到旧按钮。

`deploy/managed_deploy.py` 使用私有文件持久化任务，systemd 独立准备；新配置显式使用 `handoff` 双实例交接，
历史 `single` 记录保持原语义，不自动转换。复用一期构建、备份和封存任务；
从本项目 GitHub 取得已审核清单，固定 SHA 构建、隔离验证、备份后停在 `PREPARED`。
自动阻断已知专项路径、基线/版本不符及资源不足；数据语义审核和受影响业务回归仍由发布者完成。
`verify_fast_release.py` 使用零账号、无发布端口、无生产挂载的 internal 网络夹具，测试原签名插件、
相对/绝对路径、新旧往返、数据库/Redis/插件故障就绪检查，成功后才生成真实回执；
`handoff` 模式追加原签名插件的共库接管往返，未执行不能沿用历史六项顺序重启回执。
Nginx 故障证据仅在配置摘要完全不变时复用；调用方或插件配置漂移阻断准备。

本轮审查修复：`latest.json` 与 `accepted.json` 分开保存最近任务和可回退发布，API 返回独立
`rollback_job`，准备失败不会挤掉成功发布的回退入口；新发布成功取代旧回退，过期/执行中仍阻断。
控制器保留运行目录、持有生命周期锁，仅回收确认已失效的自有 socket；支持 crash 后同路径重启。
`verified.json` 封存最近一次通过的制品、验收文件摘要及完整现场上下文；匹配时只复用镜像/验收，
新任务仍重新备份和预检，绝不复制旧 `prepared.json` 或切换尝试。
受管构建要求本机 Docker 默认驱动/cgroup v2，两个 build 都指向受限的 `sub2api-build.slice`；
缺失实际 CPU/内存限额时阻断准备。Linux 真实容器限额和挂载重连仍待首次专项接入验收。
页面技术身份默认折叠；`frontend/dev/` 提供独立、无生产代理的可点击模拟入口，
通过 `pnpm dev:deployment` 启动，不进入正常生产构建。
2026-10-22 按用户反馈整理部署弹窗为任务状态、四步进度和候选版本列表，复用现有样式及图标，
补齐深浅色和手机布局；回退/异常不显示前进成功进度。预览增加现有 `VersionBadge` 入口及明确的
本地环境标记，正式使用不需要另开独立部署网站。确认、二次验证和任务轮询约束不变。
用户体验后追加上一版本展示，修正原回退区显示本次新版号的歧义：控制器从封存准备记录提取
与 `previous_source` 匹配的 `previous_version`，经现有 API 传递；历史记录缺信息时回落提交标识。
页面仅接受明确的 `rollback` 动作，窗口关闭仍显示旧版信息但不给回退按钮。预览增加受限回退场景。
最终 UI 复用项目的徽标、按钮和 Select，移除预览的独立配色；未修改共享组件或全站样式。

回归入口为 `managed_deployment_service_test.go`、`managed_deployment_handler_test.go`、
`routes/managed_deployment_test.go`、`deploy/tests/test_managed_deploy.py` 及两项组件测试。
早期按钮修复后 Python 共 79 项、前端 5 文件/51 项、Go 定向 race 测试通过；历史制品对真实隔离往返通过，
不等于当前源码已在 Linux 生产容器完整验收。详细证据与未测项见部署状态。
上述为早期开发验证；控制器与 socket 后于 2026-10-23 接入生产，候选后于 2026-10-24 登记。
首个候选本次准备进度见[部署状态](deployment-state.md)，历史验收不代替本次最终镜像验收。
首次页面准备暴露控制器缺失验收清单及动态版本日志导致封存误报，清单已在服务器补齐；
`Host.app_facts()` 现提取稳定版本/完整提交/构建时间，保留镜像及程序摘要核验，相关源码修复未提交。
部署工具三套回归共 130 项通过，实际镜像准备和控制器工具安装证据仍以部署状态为准。

### 共库交接替换（本地，未部署）

2026-10-24 按最新确认追加本体热切换，尚未提交/部署。`Handoff` 协议 2 增加 `release`：
暂停全部插件/自主后台并释放单活锁，但保留本体请求接入；新实例接流量后，旧实例继续完成
已有请求。`pause` 仍用于旧协议及最终计费排空，不删除原保护。插件传输请求可能受暂停影响。
执行器按已验证协议选择流程；热切换路径不关闭入口，原签名插件回执须包含对应往返证据。
成功/回退记录保存待收尾旧实例及 Nginx worker 启动身份，原恢复计时器在 worker、请求、
TCP 和计费续作结束后才关闭旧槽、解冻管理配置。API/页面展示热切换确认和待收尾状态，
收尾中阻断下一次准备/手动回退；旧程序与历史任务保留旧交接语义。
本地 Python 部署测试、Go race 回归、前端组件和正式构建，以及真实 Nginx SSE/WS 和无插件
双应用共库夹具已通过；完整生产签名插件/执行器验收尚未执行，细节见部署指南热切换修订。

用户最后确认两应用共用原数据库/Redis，先暂停插件/任务、切流量、再恢复。
`server/handoff.go` 提供启动 ID 绑定的私有控制、共享所有者锁、请求排空和配置冻结；
`pkg/deployment/gate.go` 对自主后台任务做可恢复门禁，不停止计费续作。
受管启动拒绝初始化/新迁移/随机密钥，价格仅读既有文件。Wire 已接入计费/用量/延迟写入的等待依赖。
`handoff_deploy.py` 接入固定双槽、Nginx 二选一切换、失败反向交接及独立恢复计时器；
原插件包、源码、签名、缓存格式和数据库绑定不变。日常操作仍在原版本按钮，不增加生产网站。
原签名隔离夹具增加共库往返证据要求；旧的顺序重启回执不能放行双实例。
回归入口增加 `handoff_test.go`、`gate_test.go`、`deployment_bootstrap_test.go`、
`test_handoff_deploy.py` 和 `local_application.py`。本次通过/失败/未测项见部署状态。

2026-10-23 修复排空顺序：用量及计费缓存队列纳入共同异步计数，结束后立即刷新 last-used，
失败保留批次。恢复完成记录绑定已验收且解冻的启动 ID；常驻巡检改轻量检查，切流/新启动仍
完整验收。恢复允许同一数据容器重启，但核对原挂载、PostgreSQL 集群与库身份；正常预检不放宽。
增加异步转队列、主动刷新/超时/失败保留、解冻重试、重启身份及轻重检查边界回归；未部署。

同日独立审查后的修复：回退刷新配置与启动身份在接流量前持久化，覆盖关键退出窗口；附属后台
在主应用提交后独立恢复，原活跃名单保存在现有 `handoff-active.json`，失败重试不关健康主站。
按钮复用现有错误码提示待恢复状态并继续轮询，恢复完成前不开放下一次切换。
公开就绪不再对插件暂停返回成功；私有核心检查、实际切流及两套隔离夹具仍按先切后恢复。
恢复服务与主控制器统一脚本/配置路径，安装清单补齐 `handoff_deploy.py` 并增加一致性回归。
不做额外 `sync.Map` 重构，不改插件包、数据库迁移或生产配置。

推送前复查追加修正 `ChannelMonitorRunner.runOne`：先登记 `inFlight` 释放，再判断待命门禁，
防止首次检测在待命期跳过后永久占用运行标记。回归直接通过 `fire` 验证两轮待命/恢复，
不发真实上游请求、不改变检测频率。
内容审核与邮件的已接收队列任务、探针父任务返回后的有限后台子协程也纳入共同排空；
拒绝入队、配置跳过及任务退出均释放登记，不计入常驻 worker，不改审核、封禁或邮件规则。
两个手动消费队列的旧测试同步释放任务登记，并发停止测试补齐消费者依赖，避免污染后续排空测试。
私有锁拆为平台文件，保留 Unix 原保护；Windows 明确不支持交接，不阻断普通服务编译。
正常前端构建已更新仓库内 `backend/internal/web/dist`，直接 embed 构建也包含部署按钮。

### 此前单实例精简（历史）

2026-10-22 较早曾收敛为应用单实例替换，随后被上述共库交接取代。以下保留当时精简事实：

- 删除蓝绿状态机、Docker 双槽适配器、独立监控和重启代次恢复，以及专用测试夹具。
- 删除后台任务单活门禁、备用启动模式、并行管理配置冻结和被动退休协议。
- 撤销本轮未发布的插件共享状态、缓存迁移及退休协议改造；插件源码回到本轮前版本，
  部署复用原签名包、原配置和数据，不需要新签名，不通过管理 API 修改插件启用状态。
- 页面及本地演示改回短维护流程，删除重复四步进度；保留上一版信息、兼容回退、确认身份、二次验证和断线查询。
- 保留分阶段退出、异步计费/写入等待、队列关闭竞争、last-used 只增不减等通用修复。
  删除仅供双实例使用的 runtime 诊断 API；正常就绪检查及已有 WebSocket 排空逻辑保留。

准备期间旧服务继续运行；确认后暂停获批直连后台、维护并排空，排不空就取消本次更新。
停止旧应用时插件子进程随之退出；新应用从原目录加载同一签名插件。失败按一期兼容条件恢复旧镜像。
这不是零停机或任意故障均能自动恢复的承诺。当前验证与未完成接入见[部署状态](deployment-state.md)。

本轮精简后 Python 82 项、前端 11 文件 98 项、类型检查/构建/lint、Go 服务全套单测及
部署相关四包 race 通过；本机真实单实例启动、依赖故障、顺序重启和正常退出验证通过。
原签名插件与最终发布镜像组合仍需首次接入验收；本地模拟不是生产切换。

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

此前误按“终端先行”整理；用户已明确第二期应做按钮。现有切换、查询和条件回退操作仍保留在
[终端操作速查](merge-deploy-guide.md#终端操作速查)，服务器工具/配置路径已只读核实。
本地复现并修正 `status` 对过期材料仍显示 `PREPARED` 的误导：改为准备快照、备份窗口及
UTC 到期时间，明示没有核查实时现场，不改变正式预检或切换逻辑。新增 7 项状态回归，
部署工具合计 47 项通过；服务器旧回执只读兼容验证通过，修正尚未安装到服务器。
通用隔离验收/自动准备的追加实现见上节；不能把此前的手工步骤当作端到端交付证据。

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
左侧选择供应商后，分组选项只保留实际含该供应商模型的分组；混合供应商分组按模型名单判断，
切换供应商或目录刷新使已选分组不再匹配时恢复「全部」，解除供应商筛选后显示全部可见分组。

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

### OpenAI 模糊 400 有限恢复（跨组修订已推送，未部署）

2026-10-23 只读核查确认生产 `f06a57de6` 包含旧版有限恢复，真实请求已记录
`retry_available` / `retry_exhausted`。旧版第二账号失败即终止、客户端重试可能再次进入
原候选的限制，见[会话 502 核查](deployment-state.md#2026-10-23-codex-会话-502-与有限恢复规则核查)。
用户随后明确要求更多尝试备用分组，以下跨组恢复及审查修复已在
`abbca5b0eee5b2728e3eb8b899fbcf976f3443f7` 提交并推送 `main`，尚未部署。

2026-10-22：OpenAI API Key 的 Responses HTTP 原生/透传及 WebSocket HTTP 桥安全首轮，
对原始错误字段精确匹配（忽略大小写及首尾空格）`Upstream request failed` 的 HTTP 400，
允许在原预算内额外换一个账号。仅接受无具体 code/param/detail 等拒绝原因，且 type 为空、
`upstream_error` 或 `server_error`；不把空体、任意未知 400、HTML 或请求回显当成泛化错误。
明确的工具 Schema/参数错误、上下文和策略拒绝仍走已有处理；没有新增计划模式字段改写。

2026-10-23 修订：机会按客户当前套餐分开记录，通过请求共享状态跨同组重选和 handler
重入保留；每组模糊 400 后额外换号一次，API Key 恢复账号仍报可切换错误（包括普通 502）时，
跳过本组剩余账号，继续后续合格备用组。备用组也有独立的一次机会；没有备用或总预算耗尽
才结束此类恢复。总预算与失败账号排除集合跨组共享；同一账号属于多个组也不重复尝试。
不在恢复开始后追加 API Key 的同账号字段兼容重试；明确参数、上下文、策略拒绝及管理员
终止规则仍结束请求。混合套餐切到 OAuth 后，其原有重试、等待及恢复仍生效，
不受此新增限制；切回 API Key 按当前套餐已用机会处理。保留取消和输出保护。
恢复后的 HTTP 413 按账号请求体大小限制继续换线，不因通用 `invalid_request_error`
类型提前截断透传或 WS HTTP 桥；明确上下文超限、策略拒绝及管理员终止规则仍优先，
原生 HTTP 的最终错误处理也不能重新放行这些已阻止的 413。SSE 原有分类保持不变。
原始请求含 `previous_response_id`/`conversation`、图片意图或 compact 时不启用此新增兜底；
WS 后续轮次及原生 WS 帧错误的恢复范围不扩大。OAuth 本身的错误分类保持原行为。
模糊 400 用请求级原因 `openai_ambiguous_400` 标记，不写账号健康惩罚、冷却或组失败偏好，
WS 组内自然耗尽时也按同一规则排除请求级错误的组惩罚；成功后的账号/套餐亲和及实际套餐
计费沿用原链路。多组尝试增加的延迟、上游费用及跨套餐倍率差异仍可能存在，不保证备用成功。

运维尝试记录增加恢复可用、组内机会耗尽、绑定/输出/管理员规则阻止等诊断；本地修订中的
`retry_exhausted` 表示当前组机会耗尽，是否继续备用组仍由外层总预算和准入决定。OpenAI API Key Responses
的普通 400 即使未打开原始错误体日志，也记录限长脱敏的 code/type/param，不新增请求正文采集。
OAuth 的日志行为不变。
入口：`openai_ambiguous_400.go`、`openai_gateway_forward.go`、`openai_gateway_passthrough.go`、
`openai_ws_http_bridge.go`、`openai_gateway_handler.go`、`api_key_smart_routes.go`。
回归：service/handler 的 `openai_ambiguous_400_test.go`，以及已有智能路由、计费、字段兼容
和 WS 首轮/后续轮次测试。此次验证结果及未验证项见版本记录。

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
