# 项目文档

这里维护当前功能、使用方法和长期规则。阅读本项目时，从本页进入；判断实现以当前
源码及对应测试为准，判断运行状态以目标环境的实际配置和运行版本为准。
合并更新时，用户明确的个人要求优先于两个上游；新版内容先由用户确认并指定处理方式，
再实施。具体规则见[决策优先级与升级授权](upstream-integration-policy.md#决策优先级与升级授权)。

## 项目规则与运维

| 文档 | 用途 |
| --- | --- |
| [Agent 指令](../AGENTS.md) | 项目必读入口、个人要求优先级及合并操作规则 |
| [当前实现与回归入口](local-customizations.md) | 已存在的行为、质量调度范围及代码入口 |
| [产品规则与上游差异](upstream-integration-policy.md) | 两个来源的选择、长期规则及待决问题 |
| [版本与合并记录](upstream-sync-state.md) | 三个项目的版本对应、历次合并事实、当前基线和待核对差异 |
| [部署状态](deployment-state.md) | 最近一次已记录的生产版本、配置和回退资源，含核验时间 |
| [开发指南](../DEV_GUIDE.md) | 工具版本来源、构建和测试入口 |
| [部署指南](../deploy/README.md) | 安装、配置和运维方法 |
| [生产可靠性](PRODUCTION_RELIABILITY.md) | 容量、计费、连接排空和验收边界 |
| [发布要求](../RELEASE-POLICY.md) | 多平台交付与发布验收要求 |

## 功能与接口

| 文档 | 用途 |
| --- | --- |
| [支付配置](PAYMENT_CN.md) / [English](PAYMENT.md) | 内置支付配置 |
| [外部支付集成 API](ADMIN_PAYMENT_INTEGRATION_API.md) | 外部充值系统对接 |
| [异步图片任务](ASYNC_IMAGE_TASKS.md) | 异步提交和查询 |
| [批量图片](BATCH_IMAGE_MVP.md) | Gemini / Vertex 批量图片接口 |
| [组合分组](COMPOSITE_GROUPS.md) | 跨平台模型路由 |
| [提示词审计](PROMPT_AUDIT.md) | 独立审计、同步阻断和管理接口 |
| [反绕过边界](ANTI_BYPASS_PROMPT_BOUNDARIES.md) | 请求身份、提示词检查和 WebSocket 限制 |
| [渠道监控](CHANNEL_MONITOR.md) | 主动探测与被动聚合的启用规则 |
| [插件开发](PLUGIN_DEVELOPMENT.md) / [公开协议](../backend/pkg/pluginapi/README.md) | 插件开发、打包和 UI Bridge |
| [Cookie Pin](../deploy/codex-lb-cookie-pin/README.md) | 插件机制、配置、探针与救治边界 |
| [数据库迁移](../backend/migrations/README.md) | 迁移命名、校验和与兼容要求 |
| [管理员合规说明](legal/admin-compliance.zh.md) / [English](legal/admin-compliance.en.md) | 管理员使用与合规边界 |

## 维护约定

- 功能变更直接更新对应文档，替换已失效描述；一个主题保留一个现行说明入口。
- 已完成、已撤回的计划、进度、评审和交接不留在现行文档中。已提交历史通过 Git 查询。
- 版本与合并记录保留长期事实：来源提交、实际取舍、本项目结果及发布身份，不随旧计划删除。
- 未执行方案不能写成已实现功能；测试通过不能写成已部署或效果已证明。
- 环境快照必须写明核验时间；源码默认值、管理员配置和运行中的实际值分别核对。
- 长期规则保留在规则文档中，不通过过期实施记录间接引用。
