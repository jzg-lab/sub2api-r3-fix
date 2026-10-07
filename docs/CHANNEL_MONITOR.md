# 渠道监控

渠道监控支持 V1 主动探测和 V2 被动聚合。`channel_monitor_mode` 缺失或非法时按 `v1`
处理；V2 需要显式启用，已有显式 `v2` 设置不会被默认值覆盖。

- 总开关启用且模式为 `v1` 时，允许主动探测。
- 总开关启用且模式为 `v2` 时，允许被动聚合；V1 的定时和手动检测入口不发起主动探测。
- `channel_monitor_default_interval_seconds` 的范围为 15–3600 秒，缺省回退为 60 秒。
- 面向用户的吞吐展示默认隐藏；配额展示只在配置明确为 `true` 时开启。

设置与判定入口：`backend/internal/service/setting_public.go`。
主动探测边界回归：`backend/internal/service/channel_monitor_probe_retirement_test.go`。
监控模式和前端展示回归包含在根目录 `make test-frontend-critical` 中。
