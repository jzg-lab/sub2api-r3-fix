# r17bg 本地候选实施与验收

日期：2026-10-06。状态：本地候选已实施；真实效果和生产发布条件尚未全部完成。

## 基线与交付

- 本地 HEAD / 核对时 GitHub main：`d363fc4d78c41d017ad8132ffd26d3bd1ef4a194`。
  远端工作分支仍为 `a46f6c4f599972d752fb7c3427c309c95cf05812`。
- 保留两次 Docker/pnpm 修复及已有暂停、终止、原子毕业、分组保护和旧轮次隔离。
- 没有提交、推送、部署或启用生产候选；这些修改仍在工作区，尚无发布提交身份。
- 功能比较见 `r17bg-plugin-capability-comparison.md`，不将 BPS 和 Cookie Pin 视为等价。

## 迁移修复

执行器只对已核实的 238–246 建立明确文件名别名，并绑定经核对的内容 checksum。
完整名和别名都存在时均检查；任一冲突或查询失败即拒绝。历史记录不改写、不补造，
已有 SQL 不修改；没有历史记录时正常执行。

新增 249：允许成功传输但 `answer_correct=NULL`，用于无法可靠判分的响应；
仍拒绝失败传输携带对错结论。不进行数据重判或回填。

生产只读导出到 `/tmp/sub2api-improve-20261006/` 的是结构与迁移账本，不包含客户数据。
生产数据库约 47 GB，本轮没有做完整业务数据备份或恢复，因此不能宣称数据级升级、
大表性能、备份恢复或旧镜像兼容性已经验证。

结构/账本恢复到本地 PostgreSQL 17 的 `production_schema_clone` 后升级、重复执行成功；
238–246 旧名原记录保留，没有新增同义账本；248 实际删除旧票据表，249 实际放宽约束。
原生 PostgreSQL 另覆盖完整名、旧名、混合、冲突历史及探针结果/触发器不重复写入。

**生产回退限制**：248 删除票据表和字段。回退应用镜像不能恢复删除的数据，
当前旧镜像未经迁移后数据库兼容性验收；没有可恢复的完整备份和一致性方案前不部署。
249 的约束放宽不自动证明旧宿主能正确理解中性答案记录。

## 判定变化

- 正确且只有 reasoning token 偏低：宿主和插件均不单独判质量失败或触发重摇；
  不计入资格/恢复通过。
- 题面追加唯一的 `FINAL_ANSWER: <integer>` 末行约定。兼容整个响应是单一整数、
  “答案：N／答案是N／最少取出 N 个”的短格式。
- 推导中的正确数字、否定、多个冲突结论或格式不明确不构成通过。
  已明确提取出的错误答案仍走原错答路径，模糊答案只观察。
- 宿主将不明确答案的判分持久化和留档为 NULL，同时保留可解析的 usage。
  正确低 token 的答案事实仍为 true，而操作判据中性；不会把低成本当零成本。
- 保留此前明确批准的 turn-state 长度规则及资格/恢复 token 门槛，不声称已重新校准。
  低 token 与不明确答案清掉恢复连胜；原有正确截断指纹保留宿主既有连胜的例外
  不扩大到低 token。插件保留之前已有失败证据，不制造“失忆即恢复”。

## 签名候选

- 插件：`lyunlong.codex.lb-cookie-pin` 0.3.10。
- 包：`deploy/codex-lb-cookie-pin/dist/lyunlong-codex-lb-cookie-pin-0.3.10.s2plugin`。
- SHA256：`2798e58694e06c544ed6aceec1695cc4ce048a1dfd756f24a11196da75cb8a9f`。
- 自有 key_id：`78907dd602a252f6`；复用现有密钥，不上传私钥。
- 五个平台：macOS amd64/arm64、Linux amd64/arm64、Windows amd64。
- 0.3.9 原包 SHA256 保持
  `623667465a615979431d02f2563dd70ac4ed0be68821ad2df622695099b3823c`。
- 宿主测试采用源码 VERSION 的合法 semver `0.3.0+r17bf`；
  本地信任样例位于 `backend/internal/service/testdata/cookiepin-candidate-trust.yaml`，
  `allow_unsigned=false`。它不是生产完整配置；部署前须保留原公钥并追加自有公钥再验收。
- 未设置发布验收 receipt 输出：现有测试要求已提交且干净的源码，不能对 dirty 工作树
  伪造可发布的提交回执。结果仅为候选验收。

## 已运行测试

| 范围 | 结果与边界 |
| --- | --- |
| 迁移别名、冲突 checksum、查询错误、原有兼容规则 | 通过 |
| PostgreSQL 历史迁移和 249 NULL 判分持久化（race） | 通过，使用私有临时 schema |
| 生产结构/历史副本全量升级两次（race） | 通过，不含业务数据 |
| 空白数据库 public schema 全量迁移、重复启动（race） | 通过；不修改硬编码 public 的历史迁移来适配测试 |
| 宿主判分、恢复/救治/暂停与桥状态相关测试（race） | 通过 |
| 插件全模块单测和五平台签名构建 | 通过 |
| 插件 prober / transport（race） | 通过 |
| 最终签名包真实宿主安装器 + RPC + 网关（race） | 通过，没有使用生产账号或真实上游 |
| 服务器 Linux amd64 最终包安装器/RPC 同项验收 | 通过，普通测试；仅在独立临时目录启动假上游，不连接生产数据库、不修改生产插件目录 |
| RPC 专项 | 暂停不注入/捕获且其他账号不受影响；凭据变更、迟到回包、账号隔离、401、重摇、代理拒绝回退、drain；低 token/模糊答案不计失败或连过；明确错答仍失败 |
| 全量 repository 普通测试 | 通过；原生 PG 专项另单独开启 |
| 全量 repository（race + 原生 PG） | 通过，包含本轮迁移/NULL 判分与已有事务回归 |
| 全量 service 普通测试 | 未通过；四组额度测试在未修改 HEAD 基线上同样失败 |

现有失败组：`TestQueryUsageResetCreditCountPrecedence`、
`TestResetCreditTargetedSendsStableCreditAndRedeemIDs`、
`TestQueryUsageIncludesResetCreditExpirations_EndToEnd`、
`TestQueryUsageResetCreditDetails401NonFatal`。
基线抽取到独立临时目录复现，错误包含 `access_token not found in credentials`；
本轮未修改相关额度实现或测试，未把全量测试写作通过。
详细日志在 `/tmp/sub2api-improve-20261006/service-full-test.jsonl`、
`baseline-quota-test.jsonl`。

Linux 验收的宿主测试源码 SHA256：
`87c50779f09e283d53fcd2c0a9534543a3e29521294f3ccee5ee77a256e091b3`；
候选信任样例 SHA256：
`c03059bb7d8dc59a63f6d383ccca6b1aa3f2b9f3b4edf0063c717e8ed2eb0f16`。
两者绑定本轮候选验收，不代替干净提交的发布回执。

## 成本与真实测试待办

保留已有有限重试、失败链上限、退避、单账号互斥、24 小时模板绝对寿命和密集档上限。
探针自身捕获的 Cookie 已由既有 KnownSignAt 机制消费，不触发自激励“新签即探”。
但是密集档上限不是账号全生命周期/跨重启/多实例总预算；失败/出档会重置密集计数，
插件状态主要在进程内，不能宣称它是持久化的全局限额。本轮没有为整个生产服务
凭空设定新的默认额度，也没有启用无限自动探测来代替受控对照。

用户指定实验账号 #13484。只读核对为 OpenAI OAuth、代理 #127 active、未人工暂停；
探针状态 circuit_open/normal，账户 schedulable=true（字段组合不用于推断它已恢复）。
存在 access token、无 refresh token；本轮不刷新凭据、不改账号分组或调度状态。

真实请求数/token 预算尚待明确确认。当前真实实验请求数为 0，未改变生产 Cookie。
单账号小样本不足以估计正常账号误报或一天以上的恢复稳定性。
对照需独立进程串行执行、禁止自动重试超额；未知 usage 单独计数且停止，
401/403/429、无法强制执行约定预算或凭据/出口变化也停止。
完成预算确认和实际路径/输出上限验证后，才能记录有限探索结果；
没有 Cookie 节点身份的可观测证据时不能宣称证明了上游路由机制。

## 发布前剩余条件

完整备份可恢复性及 248 后回退方案、真实旧插件状态与必需能力、
真实账号对照和成本报告、最终提交身份及其干净源码回执、
部署实际信任配置和平台验收仍须完成。
生产目前保留旧镜像和配置，不将“候选测试通过”写作“可以直接上线”。

验收结束已关闭本轮独立 PostgreSQL 集群，删除服务器临时测试目录；
生产应用仍为 `sub2api:r3-v028-3a4f527`、running/healthy。
本地结构副本及非敏感日志保留在上述临时目录，不能替代生产备份。
当前无可用 Obsidian 工具/CLI，本轮详细过程和可保留结论已记录在本地 docs。
