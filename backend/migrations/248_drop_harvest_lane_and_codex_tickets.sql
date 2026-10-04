-- 删除打票线与票据基础设施（2026-10-02 用户裁定：打票线当下已无用了，
-- 救号只保留救治区一条线，其它救号路径统统清理）。
--
-- 覆盖面（r17ax-rescue-lane 分支同批代码删除）：
--   - probe_mode='harvest' 状态机（openai_harvest_pipeline.go 整删）
--   - x-codex-turn-state 票表 openai_codex_tickets（采票观察哨+票仓储删除；
--     网关注入 r17ai 起已在装配层停用，本迁移落锤删表——表内 27 行敏感
--     ticket_value 只写不读无清理，属纯负债数据）
--   - turn-state 跨账号剥离安全守卫（guardOpenAICodexTurnStateEcho）保留，
--     业务流量仍在使用。
--
-- 生产实况（2026-10-02 只读验证）：openai_downgrade_probe_states 中
-- probe_mode='harvest' 行数 = 0。以下 UPDATE 均为防御性兜底，正常环境
-- 影响 0 行。

-- 1) 兜底归位：残留 harvest 态号回原静态桶（先归位 accounts 侧，此时
--    harvest 行仍可识别；original_proxy_id 为空的裸行保持现桶）。
UPDATE accounts a
SET proxy_id = s.original_proxy_id
FROM openai_downgrade_probe_states s
WHERE s.account_id = a.id
  AND s.probe_mode = 'harvest'
  AND s.original_proxy_id IS NOT NULL
  AND a.proxy_id IS DISTINCT FROM s.original_proxy_id;

-- 2) 兜底归位：harvest 态号若仍 schedulable（熔断入口进线未落停打），
--    判死语义下统一停打。
UPDATE accounts a
SET schedulable = FALSE
FROM openai_downgrade_probe_states s
WHERE s.account_id = a.id
  AND s.probe_mode = 'harvest'
  AND a.schedulable IS TRUE;

-- 3) 兜底归位：harvest 态行收口为判死终态（pending_replace + normal）。
--    判死号的唯一救援入口 = 手动启用/救治区（r17x 选项A + r17ax 救治区）。
UPDATE openai_downgrade_probe_states
SET probe_mode = 'normal',
    state = 'pending_replace',
    current_proxy_id = COALESCE(original_proxy_id, current_proxy_id),
    updated_at = now()
WHERE probe_mode = 'harvest';

-- 4) 收紧 CHECK：probe_mode 枚举移除 'harvest'。
ALTER TABLE openai_downgrade_probe_states
    DROP CONSTRAINT IF EXISTS openai_downgrade_probe_states_mode_check;

ALTER TABLE openai_downgrade_probe_states
    ADD CONSTRAINT openai_downgrade_probe_states_mode_check
    CHECK (probe_mode::text = ANY (ARRAY[
        'normal'::character varying,
        'qualification'::character varying,
        'half_open'::character varying,
        'accelerated'::character varying,
        'sol_fallback'::character varying
    ]::text[]));

-- 5) 删除采票尝试计数百（唯一写者=已删的打票线）。
ALTER TABLE openai_downgrade_probe_states
    DROP COLUMN IF EXISTS harvest_attempts;

-- 6) 删票表：敏感 ticket_value 落库即负债，注入已停用、读者已删。
DROP TABLE IF EXISTS openai_codex_tickets;
