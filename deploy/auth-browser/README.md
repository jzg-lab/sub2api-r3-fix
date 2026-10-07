# OpenAI 单次自动授权（macOS / Linux）

管理端的自动重新授权使用 Node 标准库通过 Chrome DevTools pipe 驱动独立浏览器。
Linux 自动使用 `--headless=new`，保留 Chrome sandbox；macOS 使用可见窗口。
不依赖 oh-my-sub2api 的外部协议 worker，不提供独立桌面重授权管理工具。

## 运行依赖

Sub2API 进程所在的运行环境需要 Bash、Python 3、Node.js 20+、Chrome/Chromium、curl。
启动器和这些程序必须在同一环境：服务在容器内运行时，不能直接执行宿主机浏览器。
Docker 构建时传 `--build-arg INSTALL_AUTH_BROWSER=true` 安装浏览器依赖，启动器位于
`/app/auth-browser/launch.sh`；默认构建不安装浏览器。部署时设置
`SUB2API_AUTH_BROWSER_LAUNCHER=/app/auth-browser/launch.sh` 和
`SUB2API_AUTH_BROWSER_PROFILE_ROOT=/app/data/auth-browser`，并验证浏览器 sandbox 可用。
用非 root 用户运行 Chrome，不以 `--no-sandbox` 绕过部署问题。

```bash
export SUB2API_AUTH_BROWSER_LAUNCHER="/opt/sub2api/auth-browser/launch.sh"
export SUB2API_AUTH_BROWSER_PROFILE_ROOT="/var/lib/sub2api/auth-browser"
# 可选：Linux 自动寻找 google-chrome / chromium / chromium-browser
export SUB2API_AUTH_BROWSER_CHROME="/usr/bin/chromium"
```

安装整个 `deploy/auth-browser` 目录，保留 `launch.sh` 可执行权限。
`SUB2API_AUTH_BROWSER_NODE`、`SUB2API_AUTH_BROWSER_PYTHON`、`SUB2API_AUTH_BROWSER_CURL`
可指定程序路径。未设置启动器时自动授权关闭，普通 OAuth/令牌导入继续可用。

账号无代理时明确使用 `--no-proxy-server`，不会继承环境代理；显式代理无效时失败，
不回退直连。Chrome 代理入口须免认证；已有老板本机桶映射继续可用。自动授权不要求
历史登录 IP 或固定出口证明。会话仍绑定当前账号、代理配置和凭据版本。
密码和 TOTP 通过子进程 stdin 传递，不放命令行、环境或文件；成功、失败、取消后清理临时浏览器配置。
短信、邮箱验证码、CAPTCHA 等人工挑战不保证完成，可用普通手动授权。

## 管理后台导入 TOTP

仅操作已有的主 OpenAI 浏览器 OAuth 账号。管理员 JWT（`Authorization: Bearer …`）
或管理端 API Key（`x-api-key`）均使用已有管理员鉴权；远程对接使用 HTTPS。
先调用 `GET /api/v1/admin/accounts/:id` 获取当前 `reauthorization_revision`。

```http
PUT /api/v1/admin/openai/accounts/123/totp
Content-Type: application/json

{
  "expected_authorization_revision": "<账号当前 reauthorization_revision>",
  "totp_secret": "<Base32 密钥>"
}
```

兼容别名 `mfa_secret`；两个字段同时提供时，规范化后必须相同。支持 Base32 大小写、空白、
短横线和末尾 padding；不接收六位验证码或 `otpauth://` URI。字段省略保持原值，
空字符串、null、非法格式、未知字段均拒绝。替换用相同 PUT；明确清除：

```json
{"expected_authorization_revision":"<当前版本>","clear":true}
```

`clear` 不能与密钥同时提供。过期版本返回 409，应重新读取账号并核对后提交。
状态读取 `GET /api/v1/admin/openai/accounts/:id/totp`，写入也只返回相同状态：

```json
{"code":0,"data":{"has_totp_secret":true,"encryption_key_configured":true}}
```

实际外层响应格式遵循宿主统一响应约定。普通账号 DTO、列表及导出不包含密钥或密文；
配置状态使用上述独立接口读取。导入不创建账号、不修改令牌/代理/分组/调度、不启动登录。

密钥存放在追加迁移 `251_account_totp_secret.sql` 创建的 `accounts.totp_secret_encrypted`，
由 AccountTOTPService 独立读写，普通 Ent 更新和令牌刷新不碰此列。复用现有 AES-256-GCM，
密文内部绑定账号 ID、邮箱和用户身份。不得绕过 API 直接写入明文。
必须固定配置并备份 `TOTP_ENCRYPTION_KEY`（64 个十六进制字符），或 `totp.encryption_key`。
自动生成的临时密钥不允许保存/使用持久 TOTP；备份数据库时也应单独安全备份该配置。
更换密钥会使已有密文不可读，需要恢复原密钥或重新导入；本次不提供在线密钥轮换。
源码回退可保留此可空列及其密文，不修改或回写历史迁移。

## 使用已保存密钥

重新授权表单明确勾选“使用管理后台已导入的 2FA 密钥”，仍需填写邮箱和本次密码。
服务端解密使用，密钥不回填前端；单次输入的密码/TOTP 不持久化。
底层 `POST /api/v1/admin/openai/launch-auth-browser` 使用已有账号绑定的 `session_id`：

```json
{"session_id":"<绑定会话>","login":{"email":"<账号邮箱>","password":"<本次密码>","use_stored_totp":true}}
```

`use_stored_totp` 与非空 `totp_secret` 互斥。身份/邮箱变化、缺失或不可解密的密钥均失败，
不尝试其他账号的密钥。完成后沿用一次性 code 兑换及绑定凭据的 proof 原子应用流程。
长期保存密码、定时无人值守重登和批量导入 UI 不在本次范围。

## 验证

```bash
node --test deploy/auth-browser/tests/*.test.mjs
python3 -m unittest discover -s deploy/auth-browser/tests -p 'test_*.py'
```

测试使用合成 RFC TOTP、模拟 Chrome 子进程和隔离数据库；真实 Linux 浏览器登录、
目标服务器依赖及真实账号挑战仍需部署验收。模拟测试不代表真实 OAuth 已成功。
