-- 自动打票线（相位B 全链，2026-09-21 用户批准）：问题号自动进打票循环。
--
-- 流程（对齐社区 wangyunjeff/sub2api-state-kit 的 schedule/collect 循环，
-- 但采集载体沿用我们的探针顺带采票——零新增流量形态，不抄 pong 针）：
--   问题号 → probe_mode='harvest' + 迁动态桶（每针换 IP）
--   → 探针照常打（同头同体同判定），响应头顺带采票
--   → 采到白名单长度(292/332) → 回原静态桶复检
--   → 复检通过（答对+健康长度）→ 回 normal 上岗
--   → 动态上连续降级长度 → 账号级 → 回 pending_replace（社区实证：账号级
--     312 永续=换票无解，别硬打）
--
-- 新列 harvest_attempts：动态桶上的采票尝试计数（停打判据）。
-- probe_mode CHECK 加 'harvest'。

ALTER TABLE openai_downgrade_probe_states
    ADD COLUMN IF NOT EXISTS harvest_attempts integer NOT NULL DEFAULT 0;

ALTER TABLE openai_downgrade_probe_states
    DROP CONSTRAINT IF EXISTS openai_downgrade_probe_states_mode_check;

ALTER TABLE openai_downgrade_probe_states
    ADD CONSTRAINT openai_downgrade_probe_states_mode_check
    CHECK (probe_mode::text = ANY (ARRAY[
        'normal'::character varying,
        'qualification'::character varying,
        'half_open'::character varying,
        'accelerated'::character varying,
        'sol_fallback'::character varying,
        'harvest'::character varying
    ]::text[]));
