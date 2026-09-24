-- r17ae: 票+Cookie 对成套采集注入（2026-09-22 用户批准方案A，堵实例粘性缺口）。
--
-- 社区实证（fenjue.py）：铸票响应 Set-Cookie 摘 __cflb+__oailb 对（两者必须齐），
-- 复用票时原样带回——Fernet 票钉在铸票的那台后端实例上，缺 Cookie 对会被
-- 负载均衡路由到别家实例导致票失效。采票时一并入库，注入票时同步注入 Cookie 头。
--
-- cookie_pair 是敏感凭据：不进日志、不进遥测、导出必须脱敏（同 ticket_value 纪律）。
-- 可空：旧票/未摘齐对的票无值，注入侧自动降级为只注票不注 Cookie（FailOpen）。

ALTER TABLE openai_codex_tickets
    ADD COLUMN IF NOT EXISTS cookie_pair TEXT;
