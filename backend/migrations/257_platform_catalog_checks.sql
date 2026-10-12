-- 平台白名单改由应用层的平台清单（internal/domain/platforms.go）统一校验，
-- 新增平台不再需要修改数据库约束的迁移。
--
-- 1. user_platform_quotas.platform：admin 接口 / 设置校验 service.AllowedQuotaPlatforms，
--    repository 写入前校验 domain.IsConcretePlatform，ent schema Validate 同源。
-- 2. composite_model_routes.target_platform：admin 接口 binding、service
--    compositeRouteFromInput 与 ent schema Validate 均校验为已登记的具体平台。
--
-- channel_monitors / channel_monitor_request_templates 的 provider CHECK 表示
-- 渠道监控已实现的探测能力；仅追加本轮实现的 MiniMax/OpenCode Go。
--
-- DROP ... IF EXISTS 保证可重入。

ALTER TABLE user_platform_quotas
    DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;

ALTER TABLE composite_model_routes
    DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;

ALTER TABLE channel_monitors DROP CONSTRAINT IF EXISTS channel_monitors_provider_check;
ALTER TABLE channel_monitors ADD CONSTRAINT channel_monitors_provider_check
 CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok', 'antigravity', 'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go'));
ALTER TABLE channel_monitor_request_templates DROP CONSTRAINT IF EXISTS channel_monitor_request_templates_provider_check;
ALTER TABLE channel_monitor_request_templates ADD CONSTRAINT channel_monitor_request_templates_provider_check
 CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok', 'antigravity', 'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go'));
