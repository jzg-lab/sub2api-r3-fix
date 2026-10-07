# 部署状态

最近部署：北京时间 **2026-10-08 01:19:10** 开始切换，**01:27:51** 验收通过。
用户明确授权“上传 GitHub，然后服务器拉取并部署”。目标为 `account-manager` Linux amd64，
部署目录 `/opt/sub2api-deploy`；服务器从 GitHub 拉取固定提交并本机构建。

## 版本与配置

| 项目 | 最近核验结果 |
| --- | --- |
| 运行代码 | `040f68b6933ec034d0e3f8045a4197c7c2d6e288` |
| 生产镜像 | `sub2api:r17bj-040f68b69` |
| 镜像身份 | `sha256:a4638d30b0047a4c3844f6a716bdffc3cd863c90eceafd552829feb6452894e8` |
| 宿主版本 | `0.3.0+r17bj.040f68b69`，本次部署标识，未创建正式版本标签/Release |
| Cookie Pin | 0.3.12，原发布者签名受信；启用、健康、绑定灰度 100% |
| 插件包 SHA-256 | `9a715da8ed79251ea5a2aba3ed8f397406a7c14f6dfb777252e1c17f4afde1e8` |
| 数据库迁移 | 追加 251：`accounts.totp_secret_encrypted` 可空列；历史迁移/checksum 不变 |
| TOTP | 沿用固定加密密钥；管理员状态接口可用，未授权请求返回 401；未导入真实账号密钥 |
| 自动授权运行时 | Alpine 3.23、Node 24.18.1、Python 3.12.15、Chromium 149.0.7827.53；启动器已配置 |
| 质量限制范围 | `openai_operations.quality_protected_group_ids=null`，即全部分组 |
| 独立救治区 | 已启用，组 #74 `rescue-lab` 专属且停用；3 次连续通过阈值、5 分钟巡检 |

宿主和前端包含质量调度范围选择、默认并发 5/优先级 2、自由编辑及救治错误回显。
质量范围入口与语义见 [本地定制行为](local-customizations.md)。保存范围不会批量开启账号。
产品在新环境的救治默认值仍为关闭；生产配置不是产品默认值。

管理员设置、原 `config.yaml` 及插件配置的 SHA-256 在切换前后核对一致，数据库和 Redis
容器身份不变。唯一设置差异为后台任务独占更新的 `openai_codex_client_version_synced`
自动同步值（验收时为 `0.161.0`），不是管理员覆写或 Sub2API 版本号。

GitHub `main` 后续已包含 `e3db10524` 的用量表缓存命中率/TPS 界面修改；本次生产固定于上表
提交，**未部署该后续界面提交**。仅文档提交同样不改变运行镜像身份。

## 本次验证与限制

- 服务健康、运行二进制完整提交、管理员账号/分组接口及实际前端资源均通过。
- 0.3.12 签名包在 macOS 和目标 Linux 上均经真实安装器、RPC、网关及模拟上游验收，
  覆盖凭据轮换、迟到结果、暂停、代理拒绝及判分；使用现有生产信任配置。
- Docker 默认 seccomp 阻止 Chrome 创建沙箱。现使用 Moby profiles 固定提交
  `2ceae35d351c156cb5a8efc0fdc4a08cf94569d8` 的默认规则，补充允许 `clone/setns/unshare`；
  文件 `/opt/sub2api-deploy/auth-browser-seccomp.json`，SHA-256
  `2a376b268a44fe1ac638d7aeccf45bb1220c385f72d4b9842aecda2c6488d58b`。
  保持非 root 浏览器和 Chrome sandbox，没有增加 `SYS_ADMIN`、关闭 seccomp 或使用 `--no-sandbox`。
  隔离镜像和生产容器内 UID 1000 的原生无头浏览器启动均通过；共享内存 256 MiB。
- 首次插件启用因 manifest 未声明当前宿主版本为已测试而返回 400，回退包也被相同条件阻止。
  确认完整版本组合原生验收已通过后，使用原生 `accept_untested` 参数完成启用；签名/兼容检查仍保留。
  最终插件配置哈希不变，启用和运行均通过；失败及重试证据均保留。
- **未测真实账号 OAuth/2FA 登录、上游人工挑战及长期稳定性。** 浏览器启动和模拟验收不等于
  真实登录成功。后台 TOTP 导入契约见 [授权说明](../deploy/auth-browser/README.md)。

## 回退与证据

- 本次目录：`/opt/sub2api-backups/releases/r17bj-20261008/`。包含完整数据库备份约 2.4 GiB、
  设置/插件表备份、原环境/Compose/config、插件目录及前后快照、构建日志、原生验证收据、
  浏览器验证、`acceptance.json` 和切换脚本。私有备份及密钥不进入 Git。
- 前一可用组合为 `sub2api:r17bh-70326f6` + Cookie Pin 0.3.11。镜像和 `previous.s2plugin`
  保留，旧包 SHA-256 为 `d6443e3da6430b1c7c2d6b1d70c5b7807757ffc8c9189f59c2917ded2605b106`。
- 回退持有 `/opt/sub2api-deploy/.deploy.lock`，通过原生接口停用插件、安装旧签名包、保持配置并重新启用，
  再恢复已备份的环境/Compose 和前一应用镜像；重新核对健康、插件、前端及数据库/Redis 身份。
  原生请求结果不明确时先核查操作是否结束，不能并发重试安装或回退。
- 251 仅追加可空列，旧应用可保留该列和新密文；回退应用不删除列、不恢复整库覆盖新业务数据，
  不轮换 TOTP 加密密钥。本次未执行生产回退。
- 之前的部署证据继续保留于 `r17bh-20261007-70326f6/` 和 `r17bg-20261005T184506Z/`。
  旧结构回退脚本具有特定前置条件，不作为本次通用回退命令。

私有配置和备份不进入 Git。后续仅文档提交不改变运行代码身份；下次部署在本页替换快照。
