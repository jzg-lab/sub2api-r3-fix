// testhost 是官方插件宿主的独立替身：用 go-plugin 客户端拉起插件二进制，
// 走完整握手 + 生命周期 + Forward 链路。开发期无需任何 sub2api 实例即可
// 端到端验证插件（回应「我们自己也要能装、不然怎么测试呢」）。
//
// 用法：
//
//	go run ./tools/testhost -plugin ./dist/darwin-arm64/cookiepin            # 协议冒烟
//	go run ./tools/testhost -plugin ... -mock                                # 本地假上游全链路
//
// Real upstream verification must use the native host, including its TLS
// profile, assigned proxy and account controls. This tool is loopback-only.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	hcplugin "github.com/hashicorp/go-plugin"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/evidence"
	pluginv1 "github.com/liyunlong/sub2api-cookie-plugin/pkg/pluginapi/v1"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("testhost", flag.ContinueOnError)
	pluginPath := flags.String("plugin", "", "插件二进制路径（必填）")
	configJSON := flags.String("config", `{"enabled":true}`, "ApplyConfig 用的配置 JSON")
	doMock := flags.Bool("mock", false, "本地假上游全链路：验证捕获→注入→信号重摇")
	doForward := flags.Bool("forward", false, "Deprecated: real upstream verification requires the native host")
	flags.String("url", "", "Deprecated: use native host verification")
	flags.String("token", "", "Deprecated: use native host verification")
	flags.String("proxy", "", "Deprecated: use native host verification")
	account := flags.Int64("account", 1, "本地模拟账号 ID")
	timeout := flags.Duration("timeout", time.Minute, "整个验证流程的超时")
	stateDir := flags.String("state-dir", "", "Private test state directory; defaults to an isolated temporary directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *timeout <= 0 {
		return fmt.Errorf("timeout 必须为正数")
	}

	if *pluginPath == "" {
		return fmt.Errorf("用法: testhost -plugin <插件二进制> [-mock]")
	}
	if info, err := os.Stat(*pluginPath); err != nil || info.IsDir() {
		return fmt.Errorf("插件不存在或不是文件: %s", *pluginPath)
	}
	if *doMock && *doForward {
		return fmt.Errorf("-mock 与 -forward 不能同时使用")
	}
	if *doForward {
		return fmt.Errorf("-forward requires native host verification; standalone TLS/account controls are unavailable")
	}
	if *stateDir == "" {
		dir, err := os.MkdirTemp("", "cookiepin-test-state-")
		if err != nil {
			return fmt.Errorf("create isolated test state: %w", err)
		}
		*stateDir = dir
		defer os.RemoveAll(dir)
	}
	if _, err := evidence.Open(*stateDir); err != nil {
		return fmt.Errorf("test state directory must already exist, be private and absolute")
	}
	// mock 假上游是 127.0.0.1，必须放开 inject_scope 才会走注入分支。
	if *doMock && *configJSON == `{"enabled":true}` {
		*configJSON = `{"enabled":true,"inject_scope":"all"}`
	}

	// Ctrl-C 时连子进程一起收掉，避免孤儿插件进程。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, *pluginPath)
	cmd.Env = append(os.Environ(), "SUB2API_PLUGIN_STATE_DIR="+*stateDir)
	client := hcplugin.NewClient(&hcplugin.ClientConfig{
		HandshakeConfig:  pluginv1.HandshakeConfig,
		Plugins:          pluginv1.ClientPluginMap(),
		Cmd:              cmd,
		SkipHostEnv:      true,
		AllowedProtocols: []hcplugin.Protocol{hcplugin.ProtocolGRPC},
		StartTimeout:     min(10*time.Second, *timeout),
	})
	defer client.Kill()

	raw, err := client.Client()
	if err != nil {
		return fmt.Errorf("握手失败: %w", err)
	}
	dispensed, err := raw.Dispense(pluginv1.TransportPluginName)
	if err != nil {
		return fmt.Errorf("Dispense(%s) 失败: %w", pluginv1.TransportPluginName, err)
	}
	transport, ok := dispensed.(*pluginv1.TransportClient)
	if !ok {
		return fmt.Errorf("Dispense 返回类型异常: %T", dispensed)
	}

	// ---- 生命周期：GetInfo → ValidateConfig → ApplyConfig → TestConfig → Health ----
	var mockHost *fixtureHostHTTP
	if err := step("GetInfo", func() error {
		info, err := transport.GetInfo(ctx, &pluginv1.GetInfoRequest{})
		if err != nil {
			return err
		}
		fmt.Printf("   id=%s version=%s protocol=%d transport_api=%d capabilities=%v\n",
			info.GetPluginId(), info.GetPluginVersion(), info.GetProtocolVersion(),
			info.GetTransportApiVersion(), info.GetCapabilities())
		if info.GetPluginId() == "" {
			return fmt.Errorf("plugin_id 为空")
		}
		for _, capability := range info.Capabilities {
			if capability == pluginv1.HostHTTPCapability {
				mockHost, err = startFixtureHostHTTP(ctx, transport)
				if err != nil {
					return err
				}
				break
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if mockHost != nil {
		defer mockHost.server.Stop()
	}
	configBytes := []byte(*configJSON)
	if err := step("ValidateConfig", func() error {
		result, err := transport.ValidateConfig(ctx, &pluginv1.ValidateConfigRequest{ConfigJson: configBytes})
		if err != nil {
			return err
		}
		if !result.GetValid() {
			return fmt.Errorf("配置被拒: %s", result.GetMessage())
		}
		return nil
	}); err != nil {
		return err
	}
	if err := step("ApplyConfig", func() error {
		result, err := transport.ApplyConfig(ctx, &pluginv1.ApplyConfigRequest{ConfigJson: configBytes})
		if err != nil {
			return err
		}
		if !result.GetApplied() {
			return fmt.Errorf("应用被拒: %s", result.GetMessage())
		}
		return nil
	}); err != nil {
		return err
	}
	if err := step("TestConfig", func() error {
		result, err := transport.TestConfig(ctx, &pluginv1.TestConfigRequest{ConfigJson: configBytes})
		if err != nil {
			return err
		}
		if !result.GetSuccess() {
			return fmt.Errorf("配置测试失败: %s", result.GetMessage())
		}
		fmt.Printf("   %s | %s\n", result.GetMessage(), result.GetStatusJson())
		return nil
	}); err != nil {
		return err
	}
	if err := step("Health", func() error {
		health, err := transport.Health(ctx, &pluginv1.HealthRequest{})
		if err != nil {
			return err
		}
		if !health.GetHealthy() {
			return fmt.Errorf("不健康: %s", health.GetMessage())
		}
		fmt.Printf("   %s | %s\n", health.GetMessage(), health.GetStatusJson())
		return nil
	}); err != nil {
		return err
	}

	if *doMock {
		if err := runMock(ctx, transport, *account, mockHost); err != nil {
			return err
		}
	}

	health, err := transport.Health(ctx, &pluginv1.HealthRequest{})
	if err != nil {
		return fmt.Errorf("最终健康检查: %w", err)
	}
	if !health.GetHealthy() {
		return fmt.Errorf("最终健康检查不健康: %s", health.GetMessage())
	}
	fmt.Printf("\n最终状态: %s\n", health.GetStatusJson())
	fmt.Println("全部通过")
	return nil
}

// runMock 用 httptest 假上游验证完整闭环（零真实流量）：
//
//	1笔: 罐空直通 → 上游 Set-Cookie __cflb=mocklb-1 → 插件被动捕获
//	2笔: 注入 mocklb-1 → 上游再发 mocklb-2 → 罐更新
//	3笔: 注入 mocklb-2 → 上游回 Faster-Model 信号 → 插件当场丢罐重摇
//	4笔: 罐空直通（重摇生效的证据）
func runMock(ctx context.Context, transport *pluginv1.TransportClient, accountID int64, host *fixtureHostHTTP) error {
	var hits []http.Header
	var hitsMu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsMu.Lock()
		defer hitsMu.Unlock()
		hits = append(hits, r.Header.Clone())
		w.Header().Add("Set-Cookie", fmt.Sprintf("__cflb=mocklb-%d; Path=/", len(hits)))
		if len(hits) == 3 {
			w.Header().Set("Faster-Model", "gpt-5.2-codex-faster") // 模拟弱后端的模型替换信号
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"response","status":"completed","output":[]}`))
	}))
	defer upstream.Close()
	if host != nil {
		host.mu.Lock()
		host.target, host.account = upstream.URL+"/backend-api/codex/responses", accountID
		host.mu.Unlock()
		ctx = pluginv1.WithHostHTTPBinding(ctx, host.binding)
	}

	round := func(label, wantCookie string) error {
		hitsMu.Lock()
		before := len(hits)
		hitsMu.Unlock()
		start := &pluginv1.ForwardRequest{
			Frame: &pluginv1.ForwardRequest_Start{Start: &pluginv1.ForwardRequestStart{
				RequestId: "req-" + label, Method: http.MethodPost,
				Url:  upstream.URL + "/backend-api/codex/responses",
				Host: strings.TrimPrefix(upstream.URL, "http://"),
				Headers: map[string]*pluginv1.HeaderValues{
					"Authorization": {Values: []string{"Bearer mock-token"}},
					"Content-Type":  {Values: []string{"application/json"}},
				},
				AccountId: accountID, Platform: "codex",
				ContentLength: int64(len(mockBody)), HasBody: true,
			}},
		}
		status, err := forwardOnce(ctx, transport, start, []byte(mockBody))
		if err != nil {
			return fmt.Errorf("mock#%s: Forward 失败: %w", label, err)
		}
		if status != http.StatusOK {
			return fmt.Errorf("mock#%s: HTTP 状态 %d", label, status)
		}
		hitsMu.Lock()
		defer hitsMu.Unlock()
		if len(hits) != before+1 {
			return fmt.Errorf("mock#%s: 上游命中数未按预期增加", label)
		}
		got := hits[len(hits)-1].Get("Cookie")
		switch {
		case wantCookie == "" && got == "":
			fmt.Printf("✓ mock#%s: 无 Cookie 直通（状态 %d）\n", label, status)
		case wantCookie == "":
			return fmt.Errorf("mock#%s: unexpected cookie present", label)
		case strings.Contains(got, wantCookie):
			fmt.Printf("✓ mock#%s: cookie injection verified (status %d)\n", label, status)
		default:
			return fmt.Errorf("mock#%s: cookie injection mismatch", label)
		}
		return nil
	}

	for _, item := range []struct{ label, cookie string }{
		{"1-捕获", ""},
		{"2-注入", "__cflb=mocklb-1"},
		{"3-信号重摇", "__cflb=mocklb-2"},
		{"4-重摇后", ""},
	} {
		if err := round(item.label, item.cookie); err != nil {
			return err
		}
	}
	hitsMu.Lock()
	defer hitsMu.Unlock()
	if len(hits) != 4 {
		return fmt.Errorf("mock 期望 4 笔上游命中，实际 %d", len(hits))
	}
	fmt.Println("✓ mock 全链路：被动捕获 → 粘性注入 → Faster-Model 信号重摇 → 重摇后空罐")
	return nil
}

const mockBody = `{"ping":1}`

// forwardOnce 走一遍 Forward 双向流（同一条流先 Send 后 Recv），返回上游状态码。
func forwardOnce(ctx context.Context, transport *pluginv1.TransportClient, start *pluginv1.ForwardRequest, body []byte) (int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := transport.Forward(ctx)
	if err != nil {
		return 0, err
	}
	if err := stream.Send(start); err != nil {
		return 0, err
	}
	if err := stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyChunk{BodyChunk: body}}); err != nil {
		return 0, err
	}
	if err := stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}}); err != nil {
		return 0, err
	}
	status := 0
	for {
		frame, err := stream.Recv()
		if err != nil {
			return status, err
		}
		if s := frame.GetStart(); s != nil {
			status = int(s.GetStatusCode())
		}
		if e := frame.GetError(); e != nil {
			return status, fmt.Errorf("插件错误帧 [%s] %s (request_sent=%v)", e.GetCode(), e.GetMessage(), e.GetRequestSent())
		}
		if frame.GetEnd() != nil {
			return status, nil
		}
	}
}

func step(name string, fn func() error) error {
	if err := fn(); err != nil {
		return fmt.Errorf("%s 失败: %w", name, err)
	}
	fmt.Printf("✓ %s\n", name)
	return nil
}
