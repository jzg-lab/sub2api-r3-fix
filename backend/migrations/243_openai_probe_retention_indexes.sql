-- r17x H 项:探针结果/事件表补查询路径索引 + 保留清理支持。
--
-- 动机(性能审查):
-- 1. openai_downgrade_probe_results 只有 (account_id, created_at DESC) 索引,
--    而 RecentProbeOnExitIP 无 exit_ip 分支与 ListDue NOT EXISTS 子查询都按
--    (proxy_id, created_at) 过滤——每次探针到期扫描都是顺序扫描。
-- 2. openai_downgrade_probe_events 只有 created_at 索引,而
--    CountOpenAIDowngradeEvents 按 (account_id, event_type, created_at) 查。
--
-- 两个索引都供每分钟级探针调度路径使用,收益直接;表基数受保留策略封顶,
-- 索引维护成本可忽略。

CREATE INDEX IF NOT EXISTS idx_openai_downgrade_probe_results_proxy_time
    ON openai_downgrade_probe_results(proxy_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_openai_downgrade_probe_events_account_type_time
    ON openai_downgrade_probe_events(account_id, event_type, created_at DESC);

-- 保留清理的批量删除走 ctid 子查询(见 repo 层),需要按 created_at 定位
-- 老行;两个 partial 索引把受害者扫描限定在真正过期的行上。
CREATE INDEX IF NOT EXISTS idx_openai_downgrade_probe_results_created_at
    ON openai_downgrade_probe_results(created_at);

CREATE INDEX IF NOT EXISTS idx_openai_downgrade_probe_events_created_at
    ON openai_downgrade_probe_events(created_at);
