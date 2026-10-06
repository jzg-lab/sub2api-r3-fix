# 分组质量保护试作及撤回记录

日期：2026-10-07。这是用户自主提出的本地修改，不是上游合并功能。
用户要求回退并重新拟定更小的方案。本轮试作代码已移除，未提交、未推送、未部署。
下文实现、验证与产物均为撤回前的历史，不表示当前工作区仍保留这些功能。
原计划见 [修改计划](group-scoped-quality-protection-plan-20261007.md)。

## 撤回范围

删除本轮新增的 6 个后端设置/预览/测试文件、2 个前端面板/测试文件；
撤回 7 个既有文件中的 CAS 方法、管理路由、前端 API 类型、文案、组件挂载和测试桩。
保留旧方案及本记录作为撤回历史，不再列为已完成的自主修改功能。
不回退并发/优先级、Cookie 插件、健康展示、保存报错及救治前置条件等其他已有修改。
没有执行服务器或数据库回滚，因为本轮从未部署或修改生产配置。

## 撤回验证

- 7 个既有文件与 HEAD 完全一致，本轮新增的策略代码及前端引用已清除。
- 前端 `pnpm typecheck` 通过；既有插件页及调度字段测试共 11 项通过。
- 后端救治设置及管理接口错误契约定向回归通过；仓库包编译通过，但本次筛选没有匹配仓库测试。
- `git diff --check` 通过；本轮启动的前端开发服务已停止。
- 没有推送、部署或改动生产账号。旧方案临时构建产物不得用于发布。

## 曾实现（已撤回）

- `backend/internal/service/openai_quality_policy.go`：独立设置键 `openai_quality_policy`，
  业务字段仅 `enabled` 和 `protected_group_ids`。缺失配置与显式关闭区分；
  坏 JSON、空值、缺字段和数据库错误返回失败，不能被当作显式关闭。
- 设置版本使用数据库 `updated_at`，`setting_repo.go` 的专用 CAS 能力保证同一版本
  仅一个保存成功；首次创建靠唯一键解决竞争；时间戳按微秒保持单调增长。
- 保护组去重、排序、校验正整数；仅允许当前有效 OpenAI 业务组，排除内部救治组。
  业务组来自一次批量读取，不按名称、颜色或显示排序判断。
- 共用纯分类函数区分 legacy/strict/mixed/ordinary/unsupported/unresolved。
  在救治账号使用原组快照，损坏/缺失/null/非法/重复 ID 不推断成普通组。
  分组集合和 `schedulable` 不被分类函数改写。
- 管理 API：GET `/api/v1/admin/openai-operations/quality-policy`，
  POST `/api/v1/admin/openai-operations/quality-policy/preview`。
  沿用现有管理员路由认证。请求体限制 64 KiB、拒绝未知字段和多个对象。
- 预览通过一次平台账号列表读取计算严格/混合/普通/隔离数量，列出隔离账号的原组、
  原优先级及需人工核查的异常；不输出 Cookie、令牌、代理密码或账号邮件地址。
  `ordinary_scope_allowed` 只是拟议质量范围，不能视作基础条件通过或迁移许可。
- `frontend/src/components/plugins/OpenAIQualityPolicyPanel.vue`：插件管理页的宿主区域
  展示草稿总开关、选组和只读影响预览；不经插件 iframe，不扩展插件权限。
  草稿变化使旧报告失效，失败保留草稿，防重复请求，服务端版本漂移有提示。
  返回错误按项目 API 客户端的普通对象格式回显，不只识别 JavaScript Error。

## 撤回前的启用边界（历史）

当前 `OpenAIQualityPolicyEnforcementReady=false`，读取/预览返回
`enforcement_ready=false`，页面没有保存或释放账号按钮。
PUT `/api/v1/admin/openai-operations/quality-policy` 返回 409
`OPENAI_QUALITY_POLICY_NOT_READY`，开启、关闭和空选组都不能绕过。

当前还没有接入实际选号、终检、检测副作用、混合账号独立恢复和原子策略退出。
没有改变生产或本地账号绑定、启停调度位、质量探针频率、插件配置及生产数据。
数据库 CAS 能力已经实现并测试，但策略启用写入仍被拦住。
后续不能只删除上述闸门，也不能将只读预览叫作“新策略已生效”。

## 验证

- 服务、管理 API、仓库定向测试：
  `/tmp/go/bin/go test -tags=unit ./internal/service ./internal/handler/admin ./internal/repository -run 'TestOpenAIQualityPolicy' -count=1`。
- PostgreSQL：独立本地 Unix-socket 临时集群，执行
  `TestOpenAIQualityPolicyPostgresCAS`、`TestOpenAIQualityPolicyPostgresConcurrentCAS`、
  `TestOpenAIQualityPolicyPostgresRevisionIsMonotonic`，三项实际通过，不是跳过。
  验证过期版本拒绝、首次创建竞争、并发更新仅一个获胜、时钟向后时版本仍单调。
- 同一临时集群上的定向 `-race` 测试通过。
- 现有 OpenAI 调度、健康、检测和救治回归通过：运行三个包中
  `Test(OpenAI|GetOpenAI|SetOpenAI|ResolveOpenAI|RunReconcileSweep|LabelOpenAI|ShouldAutoEnterRescueLane)`。
- 前端 `pnpm typecheck`、本轮文件 ESLint 通过；配置面板及既有插件页共 9 项测试通过。
- 宿主 `go build ./cmd/server` 和前端 Vite 生产构建通过。构建仍有现有 Browserslist
  数据较旧及部分大 chunk 提示，本轮没有升级依赖或做无关分包改动。
- Playwright 使用隔离的 Chromium/Chrome 会话和模拟 API，检查真实插件管理页：
  1440x1050 与 390x844，无页面横向溢出、无 JavaScript 异常，长组名可换行。
  这是布局/交互验收，不是生产真实账号效果验证。

本地临时产物：`/tmp/sub2api-quality-policy-host-20261007`（宿主构建校验）、
`/tmp/sub2api-quality-policy-ui-20261007`（前端构建校验）、
`/tmp/sub2api-quality-policy-1440.png` 和 `/tmp/sub2api-quality-policy-390.png`。
这些不构成完整发行包；没有执行 GitHub 推送、服务器拉取或发布。

## 旧方案未完成项（不再执行）

1. P2：共用选号与最终出站准入，严格要求沿粘性、备用组、重试、等待、HTTP/WS、组合路由传递。
2. P3：检测只更新混合/普通账号的质量证据；认证等基础保护保持；恢复不自动开人工关闭账号。
3. P4：现有隔离账号的原生原子策略退出，保留失败证据、原组/优先级及人工控制，独立审计。
4. P5：在运行路径完整后开放预览确认/版本化保存，增加真实健康范围展示。
5. P6：完整矩阵和关键账号事务的真实 PostgreSQL 验收，形成可追溯版本及灰度/回退步骤。

这些旧方案任务不再继续执行。线上继续按旧规则执行。
新方案必须先明确：仅解除手动开启阻断，还是取消全部纯质量强制停调/自动入区；
取消这些保护不等于仍能保证严格组只使用质量合格账号，不能保留相互矛盾的承诺。
