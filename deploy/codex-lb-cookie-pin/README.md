# Codex LB Cookie Pin（sub2api 插件）

`lyunlong.codex.lb-cookie-pin` v0.3.7 是本地维护的 sub2api 配套救号插件，
实现 OpenAI 负载均衡粘性 Cookie 的**被动捕获 → 按账号注入 → 信号自动重摇**闭环；
v0.2 增加可选的**质量探针自愈**：定期判别题检测静默降智，答错自动重摇。

本插件使用 Sub2API 插件协议，但不是 Sub2API 官方发布的账号重授权插件。
它不登录账号、不更新 OAuth 凭据，也不代替重授权流程。

## 0.3.7 修复

- 凭据、账号身份或代理变更后隔离旧探针及 Cookie，不继承旧资格。
- 配置、模型、手动重摇和模板代次变更后拒绝旧回包写回。
- 保留同账号单轮互斥与失败退避，限制模板续期和错误重试。
- 打包覆盖 macOS/Linux 双架构及 Windows amd64，拒绝不完整产物。
- 发布验收须使用 Sub 宿主的真实插件进程路径；模拟宿主不代表生产已采用。

## 它救什么（边界）

- ✅ **路由级降智**：被 LB 分到弱后端导致的模型替换（faster-model 头）、推理截断、
  516 截断。把账号钉在满血网关上。
- ✅ **静默降智**（v0.2）：模型字段永远显示所请求模型、faster-model 头不出现——
  唯一判别法是质量。探针答错 → 自动丢 Cookie 重摇；连错达阈值判疑似账号级
 （重摇无解）退避停探。
- ❌ **执法级死号**：workspace 连坐、token 吊销、配额耗尽——那是静置/改密/重授权的领域。

机制：`__cflb`（Cloudflare LB）/`__oailb`（OpenAI LB）是基础设施会话粘性 Cookie，
与账号票据无关（「票管资格，Cookie 管路由，两者不绑定」）。

## 设计原则（反指纹三律 + 一个显式权衡）

1. **零探针**：不从插件发起任何额外请求，只从真实业务响应的 Set-Cookie 被动吸收。
   （对照：kumu-ze 的 "Reply with exactly: pong" 探针正是被 OpenAI 聚类识别的固定指纹。）
2. **最小注入**：只注入白名单 LB Cookie（默认 `__cflb,__oailb`），codex CLI 正常不带
   Cookie 头，注入面最小。
3. **自然重摇**：Cookie 过期或命中 faster-model 信号即丢弃，下一笔真实业务请求自然
   摇出新 Cookie，无主动收割流量。
4. **显式权衡（v0.2）**：质量探针打破「零探针」——但 (a) 默认关闭，须显式开启；
   (b) 探针请求复用该账号最近一笔业务请求的 URL/headers/代理，除题面外与业务流量
   同形；(c) 题库 3 题确定性轮换、间隔下限 300s、连错即退避——频率与形态都收敛在
   「像一笔普通业务请求」的量级内。

## 宿主兼容矩阵

| 宿主 | 能装？ | 说明 |
|---|---|---|
| 清单声明的版本 | 候选 | 见 manifest.source.json；历史版本记录不等于 0.3.7 已逐一回归 |
| 本地 r17bd 联合修复候选 | 待联合验收 | 使用合法 semver；以当前源码、安装包、真实宿主集成测试和部署回执为准 |
| KV 持久化 | 取决于宿主能力 | 宿主不调用 InitHostServices 时不持久化；探针启动不依赖该回调 |

## 目录

```
manifest.source.json   # 清单源（packager 注入 files/runtimes 后写入包内）
ui/index.html          # UI Bridge v1 配置页 + 状态面板（零外链，CSP 合规）
internal/pluginconfig  # 严格配置解析（*bool 区分省略与显式 false）
internal/cookiestore   # 按账号隔离的 Cookie 罐（TTL/合并/信号/快照）
internal/transport     # 官方 openai.oauth.outbound_transport.v1 实现
pkg/pluginapi/v1       # 官方 v0.2.11 插件 API（原样 vendor）
cmd/cookiepin          # 插件进程入口
tools/testhost         # 模拟宿主：无需 sub2api 即可全链路验证
tools/packager         # 打 .s2plugin（SHA-256 files + 可选 Ed25519 签名）
tools/keygen           # Ed25519 发布密钥对
build.sh               # 单测 → 5 个系统/架构构建 → 打包 → 冒烟
```

## 构建

```sh
S2PLUGIN_KEY=/existing/publisher.key ./build.sh --release # 默认要求现有发布者签名
./build.sh --allow-unsigned                     # 仅限隔离的本地开发
```

两个 GoReleaser 配置均强制使用 `--release`。缺失、不可读或格式错误的签名密钥
会在构建前终止，不覆盖已有包。`S2PLUGIN_KEY_ID` 可指定宿主已经信任的发布者 ID；
省略时沿用公钥前 16 位十六进制 ID。不要为发布生成新信任根或开启生产
`allow_unsigned`。部署前必须用目标宿主的原有信任配置验证签名、文件哈希和版本兼容性。

产物：`dist/lyunlong-codex-lb-cookie-pin-0.3.7.s2plugin`。
包含 macOS amd64/arm64、Linux amd64/arm64、Windows amd64 的 runtimes 和 UI。
不要通过删除平台绕过交付矩阵；宿主上传限制须在安装前核验。

发布验收必须使用目标宿主的现有信任配置运行
`TestRescuePluginRuntimeReauthorizationIsolation`。传入包路径
`SUB2API_TEST_RESCUE_PLUGIN_PACKAGE`、配置路径
`SUB2API_TEST_RESCUE_TRUST_CONFIG`、准确宿主版本
`SUB2API_TEST_RESCUE_HOST_VERSION`，可用
`SUB2API_TEST_RESCUE_PLUGIN_RECEIPT` 保存非敏感验收摘要。
验收在临时目录使用真实安装器、RPC 和本地模拟上游，不安装到生产。

升级前完成包与回滚包校验，再停用插件。上传被明确拒绝且旧包未改变时，
直接重新启用原插件，不重复上传旧包。网络超时代表结果未知，必须先确认
原宿主操作终止并核对安装记录，禁止与在途安装并发回滚。
状态恢复逻辑的回归命令为
`node --test tools/release/replace-plugin.test.mjs`。

## 测试（不装任何 sub2api）

```sh
# 本地假上游全链路：捕获→注入→Faster-Model 信号重摇→重摇后空罐
go run ./tools/testhost -plugin dist/runtimes/darwin-arm64/cookiepin -mock

# 真实上游单发（拿一个号手动验证粘性是否成立）
go run ./tools/testhost -plugin dist/runtimes/darwin-arm64/cookiepin -forward \
    -url https://chatgpt.com/backend-api/codex/responses \
    -token "$CODEX_TOKEN" -proxy "http://u:p@127.0.0.1:17922" -account 1210
```

## 配置

| 字段 | 默认 | 说明 |
|---|---|---|
| `enabled` | true | 总开关，关=完全直通 |
| `cookie_names` | `["__cflb","__oailb"]` | 白名单，1–8 个 |
| `default_ttl_seconds` | 240 | 会话 Cookie 兜底寿命（LB Cookie 无 Expires 属性） |
| `refresh_before_seconds` | 30 | 剩余寿命低于此值视为陈旧重摇 |
| `inject_scope` | `codex` | `codex`=仅 chatgpt.com/backend-api/codex*；`all`=全部出站 |
| `reroll_on_faster_model` | true | 响应头出现 faster-model（模型被替换）当场丢 Cookie |
| `persist_kv` | true | 宿主 KV 持久化（旧宿主无 KV 时自动跳过） |
| `drop_account_ids` | — | 一次性手动重摇入口（UI 里填账号保存即执行，不驻留配置） |
| `quality_probe_enabled` | false | 质量探针自愈（v0.2）。主动流量，须显式开启 |
| `probe_interval_seconds` | 900 | 稳态档探针间隔（已验证账号），300–7200 |
| `probe_model` | ""（跟随业务模型） | 探针模型；空 = 用该账号最近一笔业务请求的模型 |
| `max_consecutive_probe_failures` | 3 | 连续答错阈值：达此值判疑似账号级、停探退避 |
| `probe_backoff_seconds` | 3600 | 账号级疑似冷却期，期满自动复探 |
| `probe_burst_interval_seconds` | 120 | 密集档间隔（v0.3.2，未验证账号），60–300 |
| `probe_burst_until_passes` | 3 | 密集档退出线：连过达此值视为已验证，回稳态档 |
| `probe_burst_max_probes` | 30 | 单轮密集档封顶针数（防 error 空转烧额度；答错/达标清零） |

## 质量探针工作方式（v0.2 + v0.3.2）

1. 开启后，每笔业务出站请求顺带刷新该账号的**探针模板**（URL/headers/代理/模型，
   含 Authorization——只存内存，绝不进日志与状态面板）。
2. 探针回路每 10s 扫描台账，到期账号发一道判别题（题库 3 题轮换：字母计数 /
   算术 / 星期推算——均为无歧义判别器，歧义题会把好号误判成降智号）。
3. **双档排程（v0.3.2）**：连过未达退出线（默认 3）= 未验证态，走密集档
  （默认 120s）快速攒证据；达标回稳态档（`probe_interval_seconds`）。救治号
   停调度后零业务流量，密集档是它唯一的快速复活通道——签捕获 → 3×120s 连过
   → 宿主毕业针，上岗 ≈ 8 分钟。error 空转有封顶（默认 30 针后退稳态档），
   答错重开回合恢复预算。
4. **新签即探（v0.3.2）**：捕获到更晚的签（10 分钟内新鲜）→ 下一针拉近到
   +5s——新签好坏立验：坏签立刻进重摇搜索链，好签立刻开攒连过。只拉近
   不推远；KV 恢复的旧签只对齐不拉近（防启动风暴）。
5. **探针续命（v0.3.2）**：每轮探针回写模板 SeenAt——零业务流量的救治号
   不再因 horizon 过期失探（2026-10-02 生产实测：种子 5/5 封顶后 10 分钟
   四号全部失探的根因）。模板绝对上限 24h，与罐 KV TTL 对齐。
6. 答错 → 丢该账号 Cookie 重摇 → 立即复探（重摇可救池级降智）；探针响应的
   Set-Cookie 走同一被动捕获路径——探针自己就能把新签钉回罐。
7. 连续答错达阈值 → 判疑似账号级（重摇无解，继续摇纯属烧额度）→ 退避停探，
   期满复探给新机会；下一次答对自动摘嫌疑帽。
8. 传输错误 / 5xx / 429 重试至多 3 次后记 error，**不计质量失败**（冷会话首发
   503 是常态，不是降智信号）。

## 安全边界

- Cookie 值只进宿主 KV（加密存储），**绝不进日志**；Status/TestConfig 输出全部脱敏。
- 探针模板含 Authorization，只存进程内存，绝不进日志/状态/KV。
- 默认零探针、无主动收割；开启质量探针后也仅限「同形业务请求 + 判别题面」。
- 插件进程权限 = sub2api 服务用户权限（官方插件模型如此），只装自己构建的包或
  签名可信的包。
