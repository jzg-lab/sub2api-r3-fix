//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	pluginv1 "github.com/liyunlong/sub2api-cookie-plugin/pkg/pluginapi/v1"
)

type hostFixture struct {
	pluginv1.UnimplementedTransportPluginServer
	mode        string
	healthCalls atomic.Int32
}

func TestMain(m *testing.M) {
	if mode := os.Getenv("SUB2API_TESTHOST_FIXTURE"); mode != "" {
		if err := os.WriteFile(os.Getenv("SUB2API_TESTHOST_PID"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			os.Exit(2)
		}
		pluginv1.Serve(&hostFixture{mode: mode})
		return
	}
	os.Exit(m.Run())
}

func (f *hostFixture) GetInfo(context.Context, *pluginv1.GetInfoRequest) (*pluginv1.GetInfoResponse, error) {
	return &pluginv1.GetInfoResponse{
		PluginId: "test.fixture", PluginVersion: "0.0.1",
		ProtocolVersion: pluginv1.ProtocolVersion, TransportApiVersion: pluginv1.TransportAPIVersion,
	}, nil
}

func (f *hostFixture) ValidateConfig(ctx context.Context, _ *pluginv1.ValidateConfigRequest) (*pluginv1.ValidateConfigResponse, error) {
	if f.mode == "timeout" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &pluginv1.ValidateConfigResponse{Valid: f.mode != "reject"}, nil
}

func (f *hostFixture) ApplyConfig(context.Context, *pluginv1.ApplyConfigRequest) (*pluginv1.ApplyConfigResponse, error) {
	return &pluginv1.ApplyConfigResponse{Applied: true}, nil
}

func (f *hostFixture) TestConfig(context.Context, *pluginv1.TestConfigRequest) (*pluginv1.TestConfigResponse, error) {
	return &pluginv1.TestConfigResponse{Success: f.mode != "test-failed"}, nil
}

func (f *hostFixture) Health(context.Context, *pluginv1.HealthRequest) (*pluginv1.HealthResponse, error) {
	call := f.healthCalls.Add(1)
	if f.mode == "final-rpc-error" && call > 1 {
		return nil, errors.New("fixture health failure")
	}
	return &pluginv1.HealthResponse{Healthy: f.mode != "final-unhealthy" || call == 1}, nil
}

func (f *hostFixture) Forward(stream pluginv1.TransportPlugin_ForwardServer) error {
	for {
		frame, err := stream.Recv()
		if err != nil {
			return err
		}
		if frame.GetBodyEnd() {
			break
		}
	}
	status := int32(200)
	if f.mode == "mock-error-status" {
		status = 503
	}
	if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Start{
		Start: &pluginv1.ForwardResponseStart{StatusCode: status},
	}}); err != nil {
		return err
	}
	return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_End{
		End: &pluginv1.ForwardResponseEnd{},
	}})
}

func TestRunReapsPluginOnEveryExit(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		mode string
		want string
		mock bool
	}{
		{mode: "success"},
		{mode: "reject", want: "ValidateConfig"},
		{mode: "test-failed", want: "TestConfig"},
		{mode: "final-unhealthy", want: "最终健康检查"},
		{mode: "final-rpc-error", want: "最终健康检查"},
		{mode: "timeout", want: "ValidateConfig"},
		{mode: "mock-error-status", want: "HTTP", mock: true},
		{mode: "mock-no-upstream", want: "上游命中数", mock: true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			t.Setenv("SUB2API_TESTHOST_FIXTURE", tc.mode)
			t.Setenv("SUB2API_TESTHOST_PID", pidFile)
			args := []string{"-plugin", executable}
			if tc.mode == "timeout" {
				args = append(args, "-timeout", "2s")
			}
			if tc.mock {
				args = append(args, "-mock")
			}
			start := time.Now()
			runErr := run(args)
			// Always clean up a leaked fixture even if a regression fails.
			pidBytes, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(string(pidBytes))
			if err != nil || pid <= 1 {
				t.Fatalf("invalid fixture pid: %v", err)
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				t.Fatalf("plugin process survived run(): pid=%d, check=%v", pid, err)
			}
			if tc.want == "" && runErr != nil {
				t.Fatal(runErr)
			}
			if tc.want != "" && (runErr == nil || !strings.Contains(runErr.Error(), tc.want)) {
				t.Fatalf("error = %v, want %q", runErr, tc.want)
			}
			if time.Since(start) > 10*time.Second {
				t.Fatal("bounded smoke/cleanup exceeded 10 seconds")
			}
		})
	}
}

func TestRunRejectsInvalidModesBeforeStartup(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-timeout", "0s"},
		{"-plugin", executable, "-forward"},
		{"-plugin", executable, "-mock", "-forward"},
	} {
		if err := run(args); err == nil {
			t.Fatalf("accepted invalid args: %v", args)
		}
	}
}
