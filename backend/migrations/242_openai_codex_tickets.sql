-- r17p: x-codex-turn-state 票据存储（换票打票体系相位B）。
-- 票=上游响应头原值（Fernet token，内嵌签发时间戳）；TTL 判据用内嵌时间戳
-- 而非捕获时刻。按（账号,模型）唯一，新票 upsert 覆盖旧票。
-- 出口指纹绑定（wangyunjeff 工程）：动态采的票绑采集出口指纹，注入侧校验
-- 当前业务出口与采集出口一致才替换，防错配。ticket_value 是敏感凭据：
-- 不进日志、不进遥测、导出必须脱敏。
CREATE TABLE IF NOT EXISTS openai_codex_tickets (
    id BIGSERIAL PRIMARY KEY,
    account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    model VARCHAR(128) NOT NULL,
    ticket_value TEXT NOT NULL,
    ticket_len INT NOT NULL CHECK (ticket_len > 0),
    issued_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    exit_fingerprint VARCHAR(128) NOT NULL,
    harvested_mode VARCHAR(32) NOT NULL DEFAULT 'probe',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_openai_codex_tickets_account_model
    ON openai_codex_tickets(account_id, model);

CREATE INDEX IF NOT EXISTS idx_openai_codex_tickets_expires
    ON openai_codex_tickets(expires_at);
