# 开发指南

项目使用 Go（Gin、Ent）、Vue 3、TypeScript、PostgreSQL 和 Redis。
功能文档从 [文档索引](docs/README.md) 进入；合并前核对
[本地定制行为](docs/local-customizations.md) 和 [长期规则](docs/upstream-integration-policy.md)。

## 环境与版本

- Go 版本以 `backend/go.mod` 为准；CI 和 Docker 构建配置须同步。
- 前端使用 `frontend/package.json` 的 `packageManager` 固定版本及 `pnpm-lock.yaml`。
- 数据库、Redis 和服务配置见 `deploy/config.example.yaml`、`deploy/.env.example`
  及 [部署指南](deploy/README.md)。本地地址和凭据由实际环境提供。

## 常用命令

在仓库根目录执行：

```sh
pnpm --dir frontend install --frozen-lockfile
pnpm --dir frontend dev
make build
```

`make build` 先构建前端，再构建带内嵌资源的后端；输出 `backend/bin/server`。
仅开发后端可使用 `cd backend && go run ./cmd/server/`。

按修改范围运行检查：

```sh
make -C backend test-unit
make -C backend test-integration
pnpm --dir frontend lint:check
pnpm --dir frontend typecheck
pnpm --dir frontend test:run
```

集成测试使用隔离数据库/Redis 或测试容器，不能指向生产。根目录 `make test` 运行
后端测试与 lint、前端 lint/类型检查和关键前端回归；它不等于完整前端测试集。

## 修改约定

- Ent schema 或 Wire 装配变更后执行 `make -C backend generate` 并检查生成差异。
- 前端依赖变更同步锁文件；发布构建不执行 `go mod tidy`。
- 接口变更同步所有实现与测试替身；定位代码时，有 `.codegraph/` 则优先使用 CodeGraph。
- 数据库迁移遵循 [迁移指南](backend/migrations/README.md)，不修改已执行 SQL/checksum。
- 配置与业务变更直接更新对应现行文档；完成后的计划、进度和交接不留在文档入口中。
- 发布遵循 [发布要求](RELEASE-POLICY.md)；部署验收遵循 [生产可靠性](docs/PRODUCTION_RELIABILITY.md)。

## 代码入口

| 路径 | 用途 |
| --- | --- |
| `backend/cmd/server/` | 服务启动与装配 |
| `backend/internal/handler/`、`service/`、`repository/` | HTTP、业务和数据访问 |
| `backend/internal/securityaudit/` | 提示词审计 |
| `backend/ent/`、`backend/migrations/` | 数据模型及迁移 |
| `frontend/src/` | 页面、组件、API、状态与国际化 |
| `deploy/` | 部署配置、脚本与 Cookie Pin 插件 |
