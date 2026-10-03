// cookiepin 是 Codex LB Cookie Pin 插件进程入口。
// 实际逻辑在 internal/transport 与 internal/cookiestore，入口只做组装。
package main

import (
	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/transport"
	pluginv1 "github.com/liyunlong/sub2api-cookie-plugin/pkg/pluginapi/v1"
)

func main() {
	pluginv1.Serve(transport.New(cookiestore.New()))
}
