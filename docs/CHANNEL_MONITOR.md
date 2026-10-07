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

## V2 用户界面

`/monitor` 默认展示平台/分组卡片及彩色时间线；这是现有 V2 被动数据的新展示，未新增 V3
监控模式。缓存率、可用率和首 Token P50 使用所选区间汇总；时间线按窗口时间戳对齐，
缺口留灰色，缺少有效证据的指标显示 `—`。用户接口脱敏的计数零值不代表没有请求，
前端同时使用服务端健康状态判断指标是否可用，继续保持用户与管理员数据范围。

“详细分析”保留原筛选、矩阵/趋势和角色允许的明细。`monitor_view=details` 或 `v2`、
以及带分析筛选的旧链接进入分析；`monitor_view=cards` 明确选择卡片。两页往返保留查询参数。
本轮不接入参考页面的质量检测历史、降智记录或作品预览，不改监控模式、主动探测和统计接口。
