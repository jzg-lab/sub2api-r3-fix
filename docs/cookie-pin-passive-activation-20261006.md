# Cookie Pin 被动模式启用与观测

日期：2026-10-06；本次操作时间北京时间 03:36:24。

## 授权与范围

用户允许所有账号参与测试，并说明后台启用开关已经打开。本轮采用正常业务流量
观察被动捕获/注入，不批量发送测试题，不开启主动质量探针。

“所有账号”仅表示不再限定 #13484：插件能力为 OpenAI OAuth 出站传输，保留
`inject_scope=codex`，不覆盖 OpenAI API Key、Grok 或其他平台，也不绕过宿主既有
暂停/终止规则。覆盖比例保持用户此前已打开的 100%，不代表每个账号都经过实测。

## 初始事实与改动

- 安装 ID=11，Cookie Pin 0.3.10，签名 trusted，后台 state=enabled、runtime healthy。
- 出站绑定 enabled=true、rollout_percent=100；配置总开关 `enabled=false`。
  初次只读快照已有 23 个观测响应，但捕获/注入均为 0。
- 使用正式管理 API，只将插件配置的 `enabled` 从 false 改为 true；其余字段不变。
  操作持 `/opt/sub2api-deploy/.deploy.lock`，写入前核对安装与配置指纹，避免覆盖
  从初次读取到操作期间的其他修改。该部署锁不等同于前端写入的互斥锁。
- 保持 `quality_probe_enabled=false`、白名单 `__cflb` / `__oailb`、
  `inject_scope=codex`、兜底 TTL=240 秒、提前刷新窗=30 秒、`persist_kv=true`，
  faster-model 信号重摇保留原 true；未执行手动丢 Cookie。
- 未重启应用/数据库/Redis，未改账号凭据、代理、分组、调度或救治标记，
  未修改宿主的其他探针设置。不是重新安装/替换插件。

宿主状态缓存轮询周期为 30 秒，UI 每 5 秒刷新不代表每次都有新快照。
保存配置后立即看到旧的 `enabled=false` 不能直接判为保存失败；须核对
配置 API 以及后续 `checked_at` 更新的状态快照。

## 实际观测

配置写入前与 03:38:12 最新缓存快照比较：

| 指标 | 写入前 | 后续快照 |
| --- | --- | --- |
| 配置/运行状态 enabled | false | true |
| 观测响应 | 29 | 45 |
| 捕获 | 0 | 6 |
| 注入 | 0 | 11 |
| 信号重摇 / 过期 | 0 / 0 | 0 / 0 |
| 插件质量探针 | 0 | 0 |
| 有 Cookie 的账号 | 0 | 2 |

账号 #13486、#13487 均捕获两个白名单 Cookie，状态新鲜。
捕获次数按 Cookie 条目更新累计，不是独立账号数或成功请求数；注入计数表示
Cookie 合并进出站头，不是上游认可某个节点身份的证明。

剩余寿命约 3500 秒不与 240 秒配置冲突：240 秒只是没有显式过期属性的兜底，
上游明确提供 Expires/Max-Age 时按上游属性计算，不是以 240 秒为统一上限。

应用镜像仍为 `sub2api:r17bg-88d8702`、running/healthy、restart_count=0，
插件 running/healthy/offline=false。03:36:24 起约两分钟日志筛选未见 panic、
插件错误、missing-column、checksum mismatch 或 scheduler outbox 错误。

宽泛 upstream/error 正则匹配了 4 行，不能把它们误称为 4 次连接失败：一条为
OpenAI API Key 账号 #6569 的模型能力元数据告警，另外三条为 Grok OAuth 账号
#8552、#8551、#8559 的转发告警。账号平台/类型经只读查询核对，均不在本插件
作用范围内；启用前等长窗口亦有 OpenAI API Key #11697 的上游相关告警。
本轮未计算完整业务错误率，不能宣称整个服务零错误或长期稳定。

03:42:44 最后只读复核：配置未发生其他变化，插件保持健康，捕获=8、注入=20、
观测响应=55，Cookie 账号=3（新增 #13404）；插件质量探针仍为 0。

后台兼容性仍为 compatible=true、tested=false：这是包未声明当前完整宿主版本
的 UI 标记，不是本轮改动。此前绑定最终包和实际宿主的原生验收见部署记录，
本轮不修改包声明、不重新启用接受未测试的开关来消除提示。

## 证据与回退

服务器私有回执目录：
`/opt/sub2api-backups/releases/r17bg-20261005T184506Z/cookie-pin-passive-20261005T193624Z`。
保留 `before.json`、`after.json`、`short-window-acceptance.json`；回执不含管理员
API Key、凭据或 Cookie 值。最后一个回执的快照时间早于上表最终 03:38:12 快照，
其中注入为 7；上表最终值来自后续只读观察，不覆盖原回执时间线。

若要撤回本次功能启用，先读取当前配置，保留其他字段，通过正式配置 API
将 `enabled=false`，等待新 `checked_at` 快照确认运行开关关闭；不要直接覆盖
整份旧配置，以免撤销后续人员的修改。后台插件绑定可以继续维持用户原启用状态。
若插件进程或传输本身出现故障，应通过正式“停用”操作解除绑定，不仅关闭内部总开关。

本轮已证明被动捕获和注入在真实业务路径运行，未证明模型质量提升或实际 LB
节点固定效果。额外主动实验请求为 0；既有客户流量和宿主自身行为不计为本轮
发起的实验，但不能因此宣称整个系统没有任何上游消耗。

## 作者机制说明的核对

用户转述作者方案：好出口探测取 Cookie、按账号/KV/TTL 存储、业务注入、
答错或出 Luna 作废、TTL 内回好出口重取，并称可避免环境变化导致账号降级。
本节是实现与证据边界核对，不修改生产配置，不发起主动实验。

- 存储/注入有实现。Forward 被动捕获所有符合条件的白名单 Cookie，
  捕获发生在读取完整业务答案之前，不会先证明“好节点”再保存。
- 没有独立“好出口”选择、评分、切回或续签路径。业务使用宿主传入的
  `start.GetProxyUrl()`；探针沿用业务模板的 `tmpl.ProxyURL`。
- 探针开启时可以按可明确判分的测试题重摇，并在连续失败时退避；模糊答案、
  正确但低 reasoning token 的答案不直接当作错答。当前生产质量探针关闭，
  不自动判分任意客户业务答案。
- 业务响应重摇条件是配置允许且 `Faster-Model` 头非空；插件并没有直接识别
  “Luna”模型名的分支。缺少响应信号不能推断不存在内部模型替换。
- Cookie 的过期属性/本地兜底 TTL 不是上游路由持续健康的保证，也不是账号
  套餐、额度、风控或实际模型池决策的覆盖权限。

Cloudflare 官方文档明确：`__cflb` 用于 Cloudflare Load Balancer 会话粘性，
Cookie 编码目标 endpoint 信息，在有效期和 endpoint 健康前提下尽量延续路由；
失败转移可下发新 Cookie。因此 Set-Cookie 不证明 Cookie 来自 OAI 内部路由层，
也不证明决策必然通过“服务端会话表”实现。`__oailb` 的内部决策语义未在本轮
取得独立证明，不因名称、Cookie 是否被接收或答案一次正确而外推。

合理假设是 Cookie 可能减少某一层重新分配的波动；不能据此承诺环境变化后
账号不会降级。需作者提供同账号/模型/effort，在出口 A/B 下带/不带 Cookie 的
可复现对照，分别验证跨出口的亲和性和质量收益。Cookie 有效、同一网关与同一
模型服务池是三个不同判断。客户端本地出口与 Sub2API 上游代理出口也需区分。

官方来源（2026-10-06 已直接获取正文）：
- [Cloudflare Cookies](https://developers.cloudflare.com/fundamentals/reference/policies-compliances/cloudflare-cookies/)
- [Session affinity](https://developers.cloudflare.com/load-balancing/understand-basics/session-affinity/)

实现位置：`deploy/codex-lb-cookie-pin/internal/transport/server.go` 的 `sendProbe`、
`Forward`、`clientFor`，以及 `internal/prober/prober.go` 的 `State.Record`。
