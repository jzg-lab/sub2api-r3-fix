# 账号并发与优先级可编辑

日期：2026-10-06。用户要求：并发数、优先级可自由修改，并记入未来合并保留项。
后续明确默认值为并发 5、优先级 2。本记录以该最新确认为准。

## 问题与修复

- 原并发在创建/编辑/批量表单只读；编辑回填强制 50，不读取账号实际值。
- service 的新建、更新、批量、复制、影子路径强制 50；repository 的创建、更新、
  批量 SQL 也再写一次 50。仅改界面或 service 都不能完成修复。
- 新账号并发缺省为 5、优先级缺省为 2，合法明确值原样保存；不运行数据迁移，
  不批量改已有账号。普通/批量创建及 Codex Session/PAT、Grok OAuth/SSO 入口一致。
- 创建并发缺省/0 使用 5；单账号/批量更新省略保留、明确 0/负数拒绝。
  普通创建 priority 使用可空请求字段区分缺省 2 和明确 0，不将 0 强制归默认。
  并发输入为正整数，优先级为非负整数，0 合法，数值越小越优先。
- OpenAI 新账号模板并发只在请求未明确填值时使用，其他模板策略不改。
  CRS/数据恢复携带的显式配置继续遵循导入语义，不批量归默认。
- 复制继承源账号设置；影子新建继承母账号并发或使用显式值。影子新建优先级的
  0 仍按原 API 表示继承母账号，创建后可编辑为 0。既有历史 0 并发不自动迁移。
- 列表最大并发与优先级提供铅笔、数字输入、保存/取消、请求中禁重复、错误保留草稿。
  实时已用并发不可编辑，其他配额徽标保留。快捷保存复用部分字段批量接口，
  单次仅提交当前账号和当前字段，防止并行编辑互相覆盖。
- 编辑期间暂停后台列表自动刷新；回包只修补本次字段，保留其他字段与运行态。
  完成后沿用现有自动刷新静默窗。优先级仍可排序，不重置用户列显隐偏好。
- 不改变账号代理、凭据、分组、人工暂停、救援、调度启停或 Cookie Pin 配置。

## 代码位置

- `backend/internal/service/account.go`、`admin_account.go`：默认、校验、创建/更新/批量/影子。
- `backend/internal/repository/account_repo.go`：真实并发写入和既有原子提交/调度 outbox。
- `backend/internal/handler/admin/{account_handler,account_codex_import,openai_oauth_handler,grok_oauth_handler}.go`：创建入口默认和优先级缺省/0 区分。
- `backend/internal/service/openai_operations_settings.go`：显式并发优先于新账号模板。
- `frontend/src/components/account/{CreateAccountModal,EditAccountModal,BulkEditAccountModal}.vue`。
- `frontend/src/components/account/AccountSchedulingField.vue`：共享快捷编辑控件。
- `frontend/src/components/account/AccountCapacityCell.vue`、`frontend/src/views/admin/AccountsView.vue`。
- `frontend/src/constants/account.ts`、中英文 `admin/accounts.ts`。

## 回归命令

在 `backend/`：

```sh
go test -tags=unit ./internal/service -run 'Test(NormalizeAccountConcurrency|AccountScheduling|DuplicateAccount|CreateShadow|UpdateAccount_|BulkUpdateAccounts_|AdminService.*BulkUpdate|OpenAIOperations|OpenAIOAuthCreationTemplate|AccountServiceCreatePreservesAuthorizationRoute)' -count=1
go test -tags=unit ./internal/handler/admin -count=1
go test ./internal/repository -count=1
```

在 `frontend/`：

```sh
pnpm test:run src/components/account/__tests__/AccountSchedulingField.spec.ts src/components/account/__tests__/BulkEditAccountModal.spec.ts src/components/account/__tests__/CreateAccountModal.spec.ts src/components/account/__tests__/EditAccountModal.spec.ts src/views/admin/__tests__/AccountsView.priorityColumn.spec.ts src/views/admin/__tests__/AccountsView.usageRefresh.spec.ts
pnpm typecheck
```

重点测试：service 明确值/省略/非法值，复制/影子默认与覆盖，repository 创建/更新/
批量真实 mutation/SQL 参数，以及事务失败不发布未提交状态。前端验证自定义值提交、
编辑读取原值、切平台不覆盖、重新打开恢复默认、0 优先级、错误/取消/重复请求及草稿保护。

## 本轮状态

- 前端 191 项回归、类型检查、后端相关服务测试、全部 admin handler 和 repository
  普通测试通过；新增默认 5/2 的表单重置和 JSON 缺省/0 回归。相关调度/复制/影子/
  模板服务 race 检查通过，新控件及其测试 ESLint 检查通过。
- 全量 service 未通过，失败仍为此前文档记录的四组额度测试：
  `TestQueryUsageResetCreditCountPrecedence`、`TestResetCreditTargetedSendsStableCreditAndRedeemIDs`、
  `TestQueryUsageIncludesResetCreditExpirations_EndToEnd`、`TestQueryUsageResetCreditDetails401NonFatal`。
  本轮日志 `/tmp/account-scheduling-service-full.jsonl`；未改额度代码，不声称全量通过。
- 使用 sqlmock 和隔离前端 mock，不发真实上游检测，不改变生产账号。
- 当前只完成本地修改，未提交、推送或部署。本记录不替代后续服务器发布验收。
- Playwright 使用本机 Chrome 和全拦截 mock，在桌面 1440x900 与手机 390x844
  各重复三轮验证并发 75/优先级 0 快捷保存、字段级载荷、停调保留，均无页面 JS 错误。
  每轮在编辑中持续检查三秒，草稿不丢失。此前部分脚本运行中编辑框退出，原因未确定；
  等待首屏加载完成后未再复现，不据此宣称已修复所有重排/加载期间的交互边界。
  截图位于 `/tmp/account-scheduling-{1440,390}-{editing,saved}.png`，已目视核对布局。
- 前端构建通过；验证生成的嵌入 dist 已恢复，避免本地验收造成无关资源哈希变更。
