# r17bf 修复实施与验收记录

日期：2026-10-04；最终复核实施与验收更新：2026-10-05

## 后续 r17bg 集成

2026-10-05 已在本工作区继续集成 r17bg `3ebcc8d` 和本地 main 配额修复 `e521d14`，完整保留本文四项修复及手动终止等定制。最新结果见 `r17bg-integration.md`，长期授权/代理约束见 `upstream-integration-policy.md`。

当前本地联合插件版本为 0.3.9。毕业新增当前轮次、15 分钟窗口内的插件与宿主资格联合证据；救援资格通过不先开调度，毕业事务一起恢复分组与调度。关闭救援仍可按有效联合证据收尾，允许读取桥状态，不能凭离线缓存毕业。

上游此次修正了两项图片失败的入口夹具和两项影子配额夹具，当前专项已通过；下文失败记录保留其历史时间范围。最新五包专项 race 5,493 项通过，插件 188 项通过，前端 151 项通过。用户随后已授权将 r17bg 和本文修复一起提交并推送，最新同步记录见 `r17bg-integration.md`；未部署。下文未提交状态均为当时的历史记录。

## 2026-10-05 最终复核补充实施与验收

依据 `r17bf-final-review-fix-plan.md` 实施四项追加修复。新增长期回归先在原实现失败，再在修复后通过；旧编辑同时覆盖停调被开启和启用被关闭两个方向，快照覆盖缺失、null、错误类型、混合非法值、小数、非法和重复 ID，暂停覆盖终止前两种状态，旧清扫同时覆盖已退出和重入。

普通设置编辑复用已有 `UpdateWithAccountBillingSettings` / `UpdateWithAccountGroups`，不再写入旧对象的调度值；内部 CRS 的明确全量同步保留 `Update`，并增加救援/终止启用限制。种子和自动资格记账保留原始快照与未知字段，只写检测变更；退出与仓库毕业事务严格拒绝 null 等损坏原组快照。终止/重入不改 `manual_paused`，到期查询、预检、资格排队和结果提交分别检查终止标记；自动资格针使用 `unpause=false`。健康列表独立识别终止状态。清扫退出携带历史 `EnteredAt`，新轮次不产生旧退出事件。

四项修复及本地联合验收已完成。没有新增表、迁移或依赖。保留实施前已有的未提交改动和未跟踪文件；尚未提交、发布、部署或修改生产数据。

| 修复 | 代码位置 | 长期回归与结果 |
| --- | --- | --- |
| 普通编辑保留当前调度 | `backend/internal/repository/account_repo.go` 的 `updateLockedAccount`；`backend/internal/service/account_service.go` 的 `Update`；`backend/internal/repository/openai_oauth_proxy_binding.go` 的启用守卫 | `TestIndependentReviewStaleEditAfterEntry` 覆盖带/不带分组、两个调度方向；启用矩阵覆盖单个、批量、手动批量、内部 Update 和旧救援入口；真实行锁等待及 outbox 失败整体回滚通过 |
| 记账不改写原组快照 | `backend/internal/service/openai_rescue_lane.go` 的 `updateMarkerFieldsForEpisode`；`openai_rescue_termination.go` 的 `OpenAIRescueOriginalGroups`；仓库 `openai_rescue_graduation.go` | `TestIndependentReviewCorruptSnapshotPostgres`、`TestOpenAIRescueAccountingPreservesRawSnapshot` 通过；缺失/null/非法快照不被修成空组，未知字段和轮次原文保留，正常及明确空组可恢复 |
| 终止阻断独立于人工暂停 | 仓库 `openai_rescue_termination.go`、`openai_downgrade_probe_repo.go`、`openai_downgrade_commit.go`；服务 `openai_downgrade_commit.go`、`openai_account_health.go`；`backend/cmd/server/wire_gen.go` | `TestIndependentReviewPauseBeforeTermination` 和自动闸门回归通过；终止前两种暂停状态跨重入保持，自动针不能解除暂停，管理员明确解除仍有效；终止健康标签不冒充人工暂停，插件暂停同步和真实本地 RPC 通过 |
| 旧清扫不能退出新轮次 | `backend/internal/service/openai_rescue_lane.go` 的 `RunReconcileSweep` 携带 `EnteredAt` | `TestIndependentReviewStaleSweepExitPostgres`、`TestOpenAIRescueSweepExitKeepsNewEpisode` 通过；旧快照遇已退出或新轮次不改变分组/标记，不新增退出事件 |

本轮最终验证（下列范围有重叠，测试及子用例数量不能累加）：

| 检查 | 2026-10-05 结果 |
| --- | --- |
| 四项新增数据库失败回归 | 修复前复现失败，修复后通过；最终 race 再次覆盖 |
| 服务、仓库、管理员接口专项 `-tags=unit -race` | 1,905 个测试及子用例通过、3 项外部条件跳过；三个包均通过。仅用 `-skip` 排除已在 HEAD 基线复现数据竞争的 `TestGrokOAuthHandlerQueryQuotaProbesUpstream`，原始失败记录保留 |
| 同一专项、不排除 Grok | 1,905 个通过、上述 Grok 用例失败、3 项跳过；不能称为全通过 |
| 三个受影响包的默认完整回归 | 9,044 个通过、22 个失败记录（6 个顶层用例及其 16 个子用例）、7 项跳过；6 个顶层失败全部在未修改的 HEAD 基线复现 |
| 已知图片问题（需要 `-tags=unit`） | 两项分别在当前工作区及 HEAD 基线复现失败 |
| 插件全部测试 `-race -count=1` | 183 个测试及子用例通过 |
| 宿主连接本地插件进程 | `TestPluginRescuePauseLocalRuntime` 通过，使用本轮编译插件和模拟上游，包含实际 RPC；不验证发布签名 |
| 前端相关回归 | 8 个文件、140 项通过 |
| 前端类型检查及生产构建 | 通过；保留已有大分块警告，输出 `/tmp/r17bf-final-frontend-dist` |
| 宿主及插件编译 | 通过，输出 `/tmp/r17bf-final-server`、`/tmp/r17bf-final-cookie-plugin` |
| 发布替换 | 46/46 通过 |
| 格式及 `git diff --check` | 通过 |

专项选择覆盖 `IndependentReview`、`RunReconcile`、救援/重启、健康、探针/原子提交、上游计费、账号锁与编辑、OAuth/CRS、影子代理及插件；使用隔离 PostgreSQL 17、Go 1.27.0 和本地模拟上游。专项的 3 个跳过分别是 `TestOpenAIProbeLiveBodyRepro`、`TestPluginRuntimeIntegration`、`TestRescuePluginRuntimeReauthorizationIsolation`。默认完整回归另外跳过 TLS/流解析/请求体复现及真实 token 比对用例，共 7 个；未提供对应外部输入。

扩大范围暴露的已有失败（未改动相关实现）：

- 配额测试夹具缺少 `access_token`，报 `OPENAI_QUOTA_TOKEN_UNAVAILABLE`：`TestQueryUsageResetCreditCountPrecedence`、`TestResetCreditTargetedSendsStableCreditAndRedeemIDs`、`TestPrepareUpstreamCallShadowResolve`、`TestQueryUsageIncludesResetCreditExpirations_EndToEnd`、`TestQueryUsageResetCreditDetails401NonFatal`、`TestQueryUsageShadowResolve_EndToEnd`。6 个顶层用例及其失败子用例全部在 HEAD 的隔离副本复现。
- Grok 测试数据竞争：`grok_oauth_handler_test.go:42` 的异步观测模型同步写测试 map，与同文件 125/162 行的断言并发读取；开启 race 后在 HEAD 隔离副本同样失败。
- 两项图片失败仍存在，名称及现象见下文；本轮当前和 HEAD 分别验证。默认完整回归没有 `unit` 标签，不能据此声称覆盖图片用例。

失败隔离采用当前 HEAD `c832c118ff7dd88a3f0d77f80563f36ba035f389` 的临时副本，未回退工作区文件。上述失败不属于四项修复，但扩大范围测试整体确实失败，不能宣称全部后端测试通过。发布前仍需目标签名插件包与信任配置联合验收、生产历史数据只读审计和真实账号环境验证。

本轮证据日志：`/tmp/r17bf-final-before*.jsonl`、`/tmp/r17bf-final-after.jsonl`、`/tmp/r17bf-final-race-complete.jsonl`（包含 Grok 失败）、`/tmp/r17bf-final-race-acceptance.jsonl`（排除该用例后通过）、`/tmp/r17bf-final-broad.jsonl`、`/tmp/r17bf-final-baseline-failures.jsonl`、`/tmp/r17bf-final-baseline-grok-race.jsonl`、`/tmp/r17bf-final-images-current.jsonl`、`/tmp/r17bf-final-images-baseline.jsonl`、`/tmp/r17bf-final-plugin.jsonl`、`/tmp/r17bf-final-frontend.log`、`/tmp/r17bf-final-typecheck.log`、`/tmp/r17bf-final-frontend-build.log`、`/tmp/r17bf-final-backend-build.log`、`/tmp/r17bf-final-release.log`。隔离数据库在验收结束后关闭。

下文保留此前实施及验收记录；涉及暂停的行为描述已按本次修复校正，旧测试数量不能替代本轮结果。

## 当前结论

已实施此前修复，以及用户确认的“救援期间拒绝修改分组，毕业或手动终止后再编辑”。手动终止入口、事务保护和插件停止机制已补齐。插件本地修复版为 `0.3.8`，需要与修复后的宿主共同发布。生产历史账号检查、目标签名包联合验收和部署尚未执行。

基线：`c832c118ff7dd88a3f0d77f80563f36ba035f389`。
工作分支：`codex/merge-r17bf-20261003`。修复保留在工作区，尚未提交、推送或部署。

## 已实施的修复

| 链路 | 实施结果 | 主要证据 |
| --- | --- | --- |
| 检测传输 | 临时执行器传入插件传输能力，仍隔离正式救援写入 | 实际检测调用覆盖插件处理、未处理、错误、普通直连和救援代理路径 |
| 检测提交后转正 | 仅资格通过且正式提交成功后调用转正；缓存刷新失败仍执行，原错误保留 | 提交失败、陈旧结果、成功和缓存失败回归 |
| 请求取消 | 提交后转正使用独立且有 10 秒上限的上下文 | 提交成功后取消请求，转正仍完成 |
| 转正原子性 | 生产仓库同一事务锁账号和探针态，核对当前标记和健康证据，恢复原组、清标记和写调度通知 | PostgreSQL 分组写、Extra 写和 outbox 写失败均整体回滚 |
| 并发和重复转正 | 当前标记已变化或资格状态已变化时放弃；同一轮并发只提交一次 | 新标记、在途资格、再次判死、禁用和两个并发调用回归 |
| 开关关闭后的收尾 | 关闭救援或清除组配置时仍补偿已合格账号；不访问插件、入区、种子、资格检测或重新绑定救援组 | 禁用和缺少组配置回归；真实事务失败后关闭状态清扫恢复 |
| 旧表单保护 | 普通更新在数据库行锁下保留当前 `openai_rescue_*`，既不删新状态也不复活旧状态 | 旧快照、空 Extra、JSON null 和真实行锁等待回归 |
| 内部标记回写 | 种子/自动检测的标记更新核对当前标记；陈旧回写不覆盖新一轮或复活已清标记 | 服务入口守卫及真实 PostgreSQL 陈旧种子回写回归 |
| 其他写入口 | 服务单独 Extra、服务/仓库批量更新剥离救援字段；新建入口清理旧救援状态 | 单独更新、批量、未来前缀和两个创建入口回归 |
| 毕业时的历史异常记录 | 原分组字段缺失、类型损坏或含非法 ID 时保留标记并报错；合法空分组可恢复 | 损坏快照回归；真实 PostgreSQL 空分组恢复 |
| 手动暂停 | 转正不主动修改调度开关；后续手动暂停不会被转正重新启用 | 真实 PostgreSQL 暂停账号转正回归 |
| 分组编辑 | 拒绝救援期间的变更或清空；提交相同组集合仍可编辑其他字段；单个/批量编辑整次回滚 | 服务前置校验、真实 PostgreSQL 并发入区、批量和写入失败回归 |
| 手动终止 | 原组恢复、停调、独立终止阻断、自动入区抑制、清成员标记和终止事件同一事务提交；保留人工暂停及恢复计数/徽标 | 分组、账号、探针态、事件、outbox 故障注入；重复终止、并发毕业和旧结果回写 |
| 再次救援 | 手动送入才清终止抑制，不清人工暂停；记录当前组为新一轮原组，仍保持停调 | 终止后编辑与重入回归；终止前两种人工暂停状态都保持 |
| 插件停止 | 数据库终止名单在插件启动、每秒对账和终止/重启时同步；取消在途探测，删除该账号模板和证据，拒绝后续重建 | 真实本地插件进程暂停/重启；在途取消、迟到探测/业务回包、暂停期间请求跨越重启回包回归 |
| 插件配置保护 | 终止名单由数据库派生，覆盖普通配置输入，保存前剥离；仅名单变化时保留其他账号证据，不重放一次性重摇 | 多实例/重启名单恢复、配置保存/回滚、无变化不重应用、一次性动作不重放回归 |

重新授权已有的救援字段保护继续保留。救援服务专用 `UpdateExtra` 写入仍可修改救援字段。

创建账号共用路径包含管理端创建和复制构建。OAuth 类型原本不允许复制，本轮未改变该限制。

## 我们的定制行为

- 无代理 OpenAI OAuth 的授权、重新授权、检测和调用规则保留。直连重新授权可以启用账号。
- 已指定代理仍受代理有效性检查；无效代理不能自动改成直连。
- 普通账号导入后立即启用；资格检测和救援流程继续遵守自己的状态规则。
- 手动暂停和影子账号代理继承的现有规则保留。
- 不恢复旧 harvest/ticket 链路；上游 rescue lane、插件签名、升级失败保留及版本隔离仍保留。

## 本地验收

| 检查 | 结果 |
| --- | --- |
| 救援/Extra 保护/转正/插件配置相关 `go test -race` | 最近一轮 175 个测试及子用例通过，包含 47 个真实 PostgreSQL 测试及子用例；2 个签名包测试因缺少外部输入跳过 |
| 插件及宿主连接相关扩展 `go test -race` | 630 个测试及子用例通过，2 个签名包测试跳过；此轮与上面的选择范围重叠，不能累加 |
| 后端较大范围相关测试 | 最近一轮 4,765 个测试及子用例通过、2 个已有图片失败、2 个外部输入跳过；此前更大选择范围为 4,844 通过、同样 2 个失败、4 个跳过 |
| 前端重新授权、浏览器入口、OAuth 生命周期、分组锁定及终止救援 | 最近一轮 8 个文件、162 项通过 |
| 前端类型检查和类型编译 | 通过 |
| 前端生产构建 | 通过；已有大分块警告，输出到 `/tmp/r17bf-repair-frontend-dist` |
| 后端编译 | 通过，输出到 `/tmp/r17bf-repair-server` |
| 新插件全部 Go 测试（不使用缓存，含 race） | 通过 |
| `0.3.8` 五平台构建、开发包打包及模拟宿主冒烟 | 通过；开发包未签名，不能作为生产发布包 |
| 发布替换测试 | 46/46 通过 |
| 只读数据审计 SQL | 隔离样例验证通过，事务只读且最终回滚 |
| 格式与差异检查 | 通过 |

真实数据库使用隔离 PostgreSQL 17，仅监听临时本地 socket，未连接生产数据库。执行过迁移升级/重放、资格提交、人工暂停、并发重新授权、代理过期和调度快照相关数据库测试。

已更新的旧测试夹具包括代理删除字段顺序、缓存令牌与持久化账号核对、影子母账号令牌。生产校验规则没有因测试失败而放宽。

## 已有失败及未覆盖范围

以下两项在我们原版本和上游版本的复核日志中都失败，本轮未改变图片转发实现：

- `TestOpenAIGatewayServiceForwardImages_TextFallbackDoesNotCoolImageCapability`：错误链未包含预期的 `UpstreamFailoverError`。
- `TestOpenAIGatewayServiceForwardImages_StructuredUnavailableCoolsImageCapability`：预期的能力冷却记录未产生。

较大范围测试因此整体返回失败，不能宣称全部后端测试通过。

四项需要外部条件的测试跳过：真实 OpenAI 请求体复现、真实 API token 估算比对、通用签名插件进程集成、救援签名插件重新授权进程集成。未提供目标签名包和信任配置，因此没有为候选宿主生成生产插件验收凭据；插件单元、构建/打包和发布替换测试通过不能替代该凭据。

新增的 `TestPluginRescuePauseLocalRuntime` 使用本地编译的插件二进制、真实 RPC 和本地模拟上游，验证暂停后不生成探测态、重启后重新生成、再次暂停后清除，并验证读取终止状态失败时实际插件进程被停止。它不验证发布者签名，也不访问真实上游账号。隔离 PostgreSQL 已在测试完成后关闭。

未执行全仓库所有测试，未使用真实上游账号发请求，未做生产历史账号审计。

## 已确认的救援操作规则

救援期间拒绝实际修改或清空分组；表单提交当前相同的组集合可以保存其他设置。生产仓库在账号锁下复核，并把分组与其他编辑字段一并提交，防止旧表单或批量编辑只保存一半。

救不回来时可从账号操作菜单选择“终止救援”。恢复入区前的原组，保持停调，使用独立终止标记阻止资格检测并停留在待处理状态，清除救援成员标记；不改变原有人工暂停。该动作不增加恢复次数，不新增恢复徽标，并且持续阻止自动入区。终止后可编辑分组，也可由管理员手动重新送入实验台；新一轮使用编辑后的当前组作为恢复目标，清终止标记但保留人工暂停。人工暂停只能由管理员明确解除。

重复终止已退出的账号不会重新绑定旧组，也不会改变调度。毕业与终止竞争时先提交者完成出口，另一方不重复退出或累计恢复。已发出的网络请求可能完成，但陈旧种子、清扫或资格结果不能重新绑定或开启已终止的账号。

插件另有独立探测循环，因此只恢复分组和暂停宿主资格检测不足以停止它。本轮新增宿主管理的 `paused_account_ids`：终止后取消该账号的插件探测、删除模板和证据，暂停期间的请求不捕获/注入 Cookie，也不重建模板。手动重启先同步解除暂停再发种子；失败则保留入区状态，由后续清扫重试。其他账号的模板与证据不因名单变化而失效。

终止的账号事务先提交，然后尝试同步本实例插件；每个实例还会独立读取数据库并补偿同步。该流程不是跨实例数据库/RPC 原子事务，其他实例可能短暂保有已发出的请求。同步失败或旧插件拒绝配置时停止本实例该插件进程，记录不可用原因；账号仍保持停调和终止标记。插件恢复前，该实例绑定到插件的 OAuth 出站会报插件不可用。这一故障路径不能称为完全无影响。

原分组记录缺失、损坏、重复、包含非法 ID 或目标组已删除时，终止/毕业报错并保留救援状态，供管理员按可靠记录修复；不会猜测恢复目标。合法的原空分组可以恢复。关闭全局救援开关不妨碍手动终止。

## 历史数据及发布步骤

1. 在目标环境记录宿主版本、插件版本/签名/信任配置、救援开关、救援组 ID 和数据库备份。
2. 用 `docs/r17bf-rescue-data-audit.sql` 只读检查，传入已经核实的救援组 ID；配置已清除时使用历史记录里的组 ID。脚本要求 PostgreSQL 16 或以上。
3. 原分组记录完整、账号资格状态有效的残留标记可由修复后的清扫收尾。标记缺失、快照损坏、原组删除或正在凭据拒绝退出的账号先人工确认；不猜测原分组。
4. 分组和终止规则已实现并回归；两项图片已有问题的影响单独评估。
5. 对实际候选宿主和已包含暂停能力的 `0.3.8` 目标签名插件包补做联合验收，再用少量专用直连/代理账号做部署环境验证。宿主与插件必须共同更新；原版 `0.3.7` 不支持暂停字段，终止后会拒绝该配置。两个发布清单已同步为 `0.3.8` 包名。
6. 经确认后提交、发布；观察转正失败日志、残留标记和调度通知消费情况。关闭救援后，已合格账号仍可被补偿转正，这是此次修复的明确语义。

本轮没有新增数据库表或迁移。回滚需采用与现有 rescue 数据兼容的宿主和插件；已删除 ticket 表的旧 harvest 程序不能直接作为回滚方案。修复不会自动重建丢失的历史标记或原分组记录。

最后补充的标记写入沿用现有单调账号版本规则；未来时间戳也不会因回写而倒退，专门的 PostgreSQL 竞态回归通过。

本地原始日志：`/tmp/r17bf-repair-backend-tests-final.jsonl`、`/tmp/r17bf-repair-race.log`、`/tmp/r17bf-repair-marker-race.log`、`/tmp/r17bf-repair-plugin-tests.log`、`/tmp/r17bf-repair-release-tests.log`、`/tmp/r17bf-repair-frontend-tests.log`、`/tmp/r17bf-repair-frontend-build.log`。

终止救援的最新日志：`/tmp/r17bf-termination-race-plugin-final.jsonl`、`/tmp/r17bf-termination-plugin-host-regression.jsonl`、`/tmp/r17bf-termination-regression-current.jsonl`、`/tmp/r17bf-termination-frontend-final.log`、`/tmp/r17bf-termination-typebuild.log`、`/tmp/r17bf-termination-frontend-build.log`、`/tmp/r17bf-termination-backend-build.log`、`/tmp/r17bf-termination-plugin-final.log`、`/tmp/r17bf-termination-plugin-build-final.log`、`/tmp/r17bf-termination-release-final.log`。

计划、结果说明和审计 SQL 均位于被现有 `.gitignore` 忽略的 docs 目录，目前作为本地交付文件提供；尚未强制加入版本控制。
