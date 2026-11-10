# UI Bridge v1

## 加载方式

宿主为每次打开配置页创建短时 UI 会话：

```text
/api/v1/plugin-ui/<asset-token>/index.html#bridge_token=<bridge-token>
```

资源 Token 用于读取包内 `ui/` 文件，Bridge Token 只存在于 URL fragment，不会发送到服务器。iframe 使用 `sandbox="allow-scripts"`，不授予 `allow-same-origin`。

UI 只能加载包内、已在清单声明的资源。CSP 禁止外部网络连接、表单提交和外部 frame。

## 消息信封

UI 到宿主：

```json
{
  "source": "sub2api-plugin-ui",
  "bridge_token": "TOKEN",
  "type": "config.load",
  "request_id": "UNIQUE_ID"
}
```

宿主到 UI：

```json
{
  "source": "sub2api-plugin-host",
  "bridge_token": "TOKEN",
  "request_id": "UNIQUE_ID",
  "ok": true
}
```

## 方法

| `type` | UI 参数 | 成功响应 |
|---|---|---|
| `sub2api.plugin.ready` | 无 | 无响应 |
| `config.load` | 无 | `config` |
| `config.save` | `config` 对象 | 规范化后的 `config` |
| `config.test` | 无 | `result` |
| `archive.list` | `date`：UTC `YYYY-MM-DD` | `records` 数组 |
| `archive.download` | `record`：含 `date`、`instance_id`、`record_id` 的对象 | 宿主触发 JSON 下载，返回 `ok: true` |
| `ui.resize` | `height` | 无响应 |
| `ui.notify` | `level`、`message` | 无响应 |

`config.test` 在 v1 中测试已保存配置。UI 若要测试当前表单，应先调用 `config.save`。

`archive.*` 仅对声明 `data.capture.v1` 的当前插件开放。列表元素包含 `date`、`instance_id`、
`record_id`、`size`；文件只能经管理员 API 从当前启用的采集插件读取，不接受服务器路径。
下载内容由父页面获取为 Blob 并下载，不向 iframe 返回管理员凭据或档案正文。
这些是 v1 的可选扩展，旧宿主不会因版本号相同而自动支持。

## 必须执行的校验

UI 接收消息时必须验证 `event.source === parent`、消息来源标识、Bridge Token 和等待中的 `request_id`。每个请求必须有超时和卸载清理。

iframe 的不透明 origin 为 `null`，父页面的 origin 仍是宿主真实地址；插件 UI 不得要求父页面消息的
`event.origin` 为 `null`。宿主校验 iframe 窗口、origin、当前插件和会话 Token；异步返回时再次校验会话，
防止关闭或切换插件后错误投递结果或触发下载。

宿主不会向 iframe 提供管理员 Token。插件 UI 不得尝试访问管理 API、Cookie、父页面 DOM 或浏览器存储中的宿主数据。
