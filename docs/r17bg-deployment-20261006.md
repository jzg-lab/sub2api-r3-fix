# 2026-10-06 部署记录

状态：用户已明确授权推送 GitHub 并在服务器拉取部署；执行中，尚不代表部署成功。
本轮对生产配置/数据库的改动仅由本次授权驱动，不沿用已回退的旧部署操作。

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

## 已完成的发布准备

- GitHub main 与工作分支已原子前进到
  `a81f1d70825ad7408854fab41c03113661d6fad6`，没有 force push。
- 服务器新建 `/opt/sub2api-r3-fix` 并从 GitHub main 拉取该提交，旧上游目录不改动。
- 固定镜像 `sub2api:r17bg-a81f1d7` 构建成功；宿主版本为
  `0.3.0+r17bg.a81f1d7`，内嵌 commit 为上述完整 hash。
- 发布工作目录 `/opt/sub2api-backups/releases/r17bg-20261005T184506Z`；
  完整备份在其中 `backup/20261005T184506Z`，custom dump 约 2.6 GiB。
  app-data、数据库、Redis、源码/deploy Git bundles 和容器 inspect 校验全部通过。
  私有 `.env`、compose、config 和旧镜像身份另存 `rollback/`，不上传 GitHub。
- 最终干净提交构建的签名包 SHA256：
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

完整恢复第二轮、升级两次及旧镜像回退演练仍在执行中，生产尚未切换。
隔离 PostgreSQL 为 network=none；测试应用/Redis 仅共享该断网 namespace，
不映射端口，不挂生产 data/env，使用 fixture JWT 和独立数据库。
实际部署、验收及剩余风险在执行后补充，失败不能写作成功。
