# 2026-10-06 部署记录

状态：用户授权下已完成 GitHub 推送、服务器拉取、生产部署和短窗验收。
本轮对生产配置/数据库的改动仅由本次授权驱动，不沿用已回退的旧部署操作。

后续状态更新：以下“插件禁用”描述是部署完成时的事实。用户随后打开后台绑定，
并授权全部适用账号参与验证；03:36:24 已通过正式 API 打开插件内部被动总开关，
质量探针仍关闭。实际捕获/注入及范围见
[被动模式启用记录](cookie-pin-passive-activation-20261006.md)。

## 最终版本

- 生产代码提交：`88d8702cb0a0e63660e45186b77e508cf47aefa5`。
- 镜像：`sub2api:r17bg-88d8702`；版本：`0.3.0+r17bg.88d8702`。
- 切换时间：`2026-10-05T19:16:11Z`（北京时间 2026-10-06 03:16:11）。
- 应用 running/healthy、restart_count=0。旧请求正常排空，仅重建应用容器，
  PostgreSQL/Redis 生产容器未重建；旧镜像和上游目录原样保留。
- Cookie Pin 0.3.10 已通过正式管理 API 安装，ID=11，签名 trusted；state=disabled，
  全部绑定关闭、config.enabled=false、quality_probe_enabled=false、无插件运行进程。
  未调用 enable，也未替换 BPS。旧 BPS 安装包继续保留。
- 最终签名包 SHA256：
  `cbafc7776ea2f0ac50d6eda5439037a16488a8a4a971d5701da478fc70c22fb9`。
  最终 macOS（race）及服务器 Linux 安装器/RPC/网关验收均通过，回执为
  `native-acceptance-final-macos.json`、`native-acceptance-final-linux.json`。
  两者绑定最终包、生产版本、上述干净提交与实际信任配置，均拒绝未签名包。

后续仅文档更新的提交不改变该生产代码/镜像身份。

## 顺序和保护

1. 同步最新 GitHub main，将已验收的本地修改提交并正常前进推送 main 和工作分支。
2. 服务器保留旧上游源码 `/opt/sub2api-r3`；本项目单独拉取至 `/opt/sub2api-r3-fix`。
3. 保留旧镜像、配置及 .env；部署使用固定提交镜像，不复用可变 latest 标签。
4. 创建完整 PostgreSQL 备份，在独立 PostgreSQL 18 中恢复并执行迁移两次，验证
   账号、用户、密钥、分组、分组绑定和订阅数据未被改写。生产结构副本不能代替此步骤。
5. 审查 248 的删除及旧镜像兼容性，具备数据/配置一致的回退路径后切换应用。
6. 健康、版本、迁移、HTTP 页面及插件状态验收后记账。保留日志与备份，清理隔离测试进程。

插件真实效果仍未证明，探针预算未确认；默认只安装 0.3.10 并保持禁用，不开启
自动质量探针或默认 100% 客户流量切换。启用范围以用户本轮补充答复为准。

## 初始只读事实

- 应用：`sub2api:r3-v028-3a4f527`，running/healthy。
- 生产库约 47 GB；旧票据表和 harvest 状态行均为 0。
- 当前插件安装表/绑定表均为 0，运行容器没有插件子进程；旧 BPS 0.7.1 包仍在
  `/opt/sub2api-deploy/plugin-packages`，不删除旧包或配置。
- PostgreSQL 曾短暂拒绝新连接，随后只读连接恢复；实际 max_connections=100，
  .env 的应用连接池上限=256，不能把 .env 中 POSTGRES_MAX_CONNECTIONS=1024
  当成服务器已生效设置。本轮先保留配置并记录这一既有容量风险。
- 服务器磁盘可用约 671 GB，足够进行完整备份和隔离恢复。

## 首轮发布准备

- GitHub main 与工作分支已原子前进到
  `a81f1d70825ad7408854fab41c03113661d6fad6`，没有 force push。
- 服务器新建 `/opt/sub2api-r3-fix` 并从 GitHub main 拉取该提交，旧上游目录不改动。
- 固定镜像 `sub2api:r17bg-a81f1d7` 构建成功；宿主版本为
  `0.3.0+r17bg.a81f1d7`，内嵌 commit 为上述完整 hash。
- 发布工作目录 `/opt/sub2api-backups/releases/r17bg-20261005T184506Z`；
  完整备份在其中 `backup/20261005T184506Z`，custom dump 约 2.6 GiB。
  app-data、数据库、Redis、源码/deploy Git bundles 和容器 inspect 校验全部通过。
  私有 `.env`、compose、config 和旧镜像身份另存 `rollback/`，不上传 GitHub。
- 首轮干净提交候选包 SHA256（已被最终版本取代）：
  `e9112b30a1ee3b83f0ea9b0752dc61d1bba4b78035b78a43bfe84b5eea9f7818`。
  该值取代此前 dirty/precommit 候选 hash；不是同一个制品。
- 实际计划配置只追加 `78907dd602a252f6` 公钥，结构化比对确认其他配置完全保留。
  签名包在 macOS（race）和服务器 Linux 的真实宿主安装器、RPC、网关测试均通过。
  干净提交回执位于本机 `/tmp/sub2api-release-20261006/native-acceptance-macos.json`
  及服务器发布目录 `native-acceptance-linux.json`，绑定最终制品、宿主版本和信任配置。
- 生产已有管理员 API Key，step-up 当前为 false。安装将使用正式管理 API，
  不制造 JWT、不改安装数据库记录、不关闭或绕过认证门控。

## 完整恢复首次验收发现

首次完整恢复成功，迁移全部执行两次，但原验收要求 accounts 每个字段完全不变，
忽略了已存在的 `247_openai_oauth_optional_proxy.sql` 及已确认的普通导入规则。
从同一备份独立恢复 accounts 原表后逐字段比对：恰好 405 行 extra 和 updated_at
变化，新增/删除账号为 0；两个旧导入标记之外的 extra、凭据、代理、schedulable、
状态及其他字段均未变。其他五张客户/分组表原始摘要完全一致。

修正验收只许可明确退休的两个导入标记及这些行的更新时间，仍逐字段比较未知 extra
和其他账号信息。可能被自动重新调度的导入账号非零时继续阻断并要求独立审计。
不修改历史 SQL 或账本，不将失败的第一轮验收改写为通过。
随后将重新恢复完整备份，从原始数据执行修正后的迁移验收。

隔离镜像实际启动进一步发现：健康接口可返回 ok，但调度/分组查询报告
`groups.models_list_config` 不存在，143 的迁移记录却已存在。这是物理结构与
历史账本不一致，不能用健康接口通过来证明业务可运行；缺列的历史成因尚未确定。
新增 250 只在列不存在时补上 JSONB NOT NULL DEFAULT '{}'，已有模型展示配置
完全保留，不改 143 或其 checksum。完整数据验收增加所有生成 ORM 表/列的存在性
检查，并按新增空配置列计算 groups 的预期摘要，不许可其他分组数据变化。
原 `a81f1d7` 镜像因此作废为部署候选，必须从修复后的干净提交重建并重新验收。

## 完整数据与回退验收

第二轮从同一完整备份恢复到独立 PostgreSQL 18，恢复日志无错误。
`TestMigrationProductionDataCloneUpgrade` 在原始数据上执行迁移两次通过：
405 个旧导入标记按既有政策退休、自动重新调度账号=0；除已知标记/其更新时间和
250 新增的空模型展示配置之外，客户数据、未知 extra、凭据、代理、状态、调度与
分组数据完整保持。所有生成 ORM 表/列存在；250 的缺列补齐、重复执行和已有非空
模型展示配置保留也通过。全量 repository 普通测试通过。

在同一断网集群中新镜像启动/健康/启动日志通过；停止后用未改动的 242、245、244
SQL 在事务中恢复旧结构，再启动原生产镜像也通过，未重写迁移账本。
第二轮日志 `second/full-data-upgrade.log`、`host-rollback-validation.log` 保留。
第一轮失败日志和 `a81f1d7` 候选包保留，未把失败改写成成功。

生产停机排空后再次确认 tickets/harvest/安装/绑定均为 0，并核对不会自动重新调度
等待导入的账号；比对镜像和三个配置文件原始 hash 后才切换。配置只增加公钥，
.env 只改固定镜像 pin，其他配置和发布者不变。

回退脚本保留在发布目录 `rollback-production.sh`：必须持部署锁、停止应用，先恢复
242/245/244 的旧结构再恢复原 .env/config 和旧镜像。这是本轮空票据/空 harvest
前提下的结构回退，不能外推到有历史票据数据的库，也不是整个产品行为的全面回归。
247/249/250 及其账本保留，旧镜像已验证可启动；不会用全库恢复覆盖上线后的新账务。
再次前进部署时须重新审计旧程序期间是否出现 harvest，显式执行 248 清理恢复的旧
结构（账本不能删改或伪造），再启新版，不能假设应用启动会重跑已经记账的 248。

## 生产验收

- 已应用两个 247，以及 248、249、250。票据表和 harvest_attempts 已不存在，
  249 的成功传输/中性答案约束正确，models_list_config 为 JSONB NOT NULL DEFAULT '{}'。
- 直接从完整备份恢复历史账本并对比生产：原 315 条记录的文件名、checksum 和
  applied_at 全部保留；新增的恰为上述五条。旧名 238–246 不补同义记录。
  其他已存在的 238/239/240 同号不同迁移文件不是这些旧名的别名，不混为重复。
- 健康/精确版本/内嵌 commit、管理 API 账号/分组查询、三个入口 JS/CSS 资源通过。
- 切换后约四分钟观测无 missing-column、checksum mismatch、panic、scheduler outbox
  处理失败；仅有宿主进程，无插件后台进程。短窗检查不是长期稳定性或真实质量收益证明。
- 生产还保留两个既有 NOT VALID usage_logs 图像计费约束，隔离恢复亦为两个；
  本轮不宣称它们已验证或擅自处理既有计费约束。
- PostgreSQL 连接数一次快照=34、上限=100；应用池配置上限仍为 256。
  既有容量风险保留，不将当前健康解释为高峰容量已证明。

具体回执 `production-plugin-receipt.json`、`production-api-assets-acceptance.json`、
`production-history-acceptance.json`、启动/观测日志及完整备份保留于发布工作目录。
真实实验请求仍为 0，账号 #13484 尚未付费实测，预算未确认；插件保持禁用。

隔离 PostgreSQL 为 network=none；测试应用/Redis 仅共享该断网 namespace，
不映射端口，不挂生产 data/env，使用 fixture JWT 和独立数据库。
验收结束清理本轮隔离容器及解压的数据副本，保留完整压缩备份、旧镜像和回执。
详细部署过程记录在本地 docs；本环境没有可用 Obsidian 工具/CLI。
