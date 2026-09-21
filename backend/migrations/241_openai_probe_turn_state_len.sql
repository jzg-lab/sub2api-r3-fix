-- r17p: 探针结果落 x-codex-turn-state 头长度。
-- 此前长度只在降智事件 details 与 slog 观察哨里，健康针的 332 不落库，
-- 账号健康证据行（相位A）无数据源。0=无头（401/异常路径）。
ALTER TABLE openai_downgrade_probe_results
    ADD COLUMN IF NOT EXISTS turn_state_len INT NOT NULL DEFAULT 0;
