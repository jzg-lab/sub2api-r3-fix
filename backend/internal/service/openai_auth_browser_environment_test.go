package service

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestOpenAIAuthBrowserEnvironment(t *testing.T) {
	allowed := []string{
		"PATH=/usr/bin:/bin", "HOME=/home/browser", "TMPDIR=/tmp",
		"LANG=en_US.UTF-8", "DISPLAY=:0", "SystemRoot=C:\\Windows",
		"SUB2API_AUTH_BROWSER_NODE=/local/bin/node",
		"SUB2API_AUTH_BROWSER_PYTHON=/usr/bin/python3",
		"SUB2API_AUTH_BROWSER_CHROME=/local/browser",
		"SUB2API_AUTH_BROWSER_CURL=/usr/bin/curl",
		"SUB2API_AUTH_BROWSER_PROFILE_ROOT=/local/profiles",
		"SUB2API_AUTH_BROWSER_LOG_FILE=/local/launcher.log",
		"SUB2API_AUTH_BROWSER_PROFILE_MAX_AGE_HOURS=72",
		"SUB2API_AUTH_BROWSER_REQUIRE_STATIC_EXIT_CHECKS=true",
		"SUB2API_AUTH_BROWSER_EXPECTED_EXIT_17931=192.0.2.1",
		"SUB2API_AUTH_BROWSER_EXPECTED_EXIT_65535=192.0.2.2",
	}
	denied := []string{
		"SUB2API_ENV_CANARY=fixture", "DATABASE_URL=fixture", "REDIS_URL=fixture",
		"HTTP_PROXY=fixture", "HTTPS_PROXY=fixture", "ALL_PROXY=fixture", "NO_PROXY=*",
		"http_proxy=fixture", "https_proxy=fixture", "all_proxy=fixture",
		"NODE_OPTIONS=fixture", "NODE_PATH=fixture", "PYTHONPATH=fixture",
		"PYTHONHOME=fixture", "BASH_ENV=fixture", "ENV=fixture", "LD_PRELOAD=fixture",
		"DYLD_INSERT_LIBRARIES=fixture", "CURL_HOME=fixture",
		"SUB2API_AUTH_BROWSER_MODE=automated", "SUB2API_AUTH_BROWSER_UNKNOWN=fixture",
		"SUB2API_AUTH_BROWSER_EXPECTED_EXIT_0=fixture",
		"SUB2API_AUTH_BROWSER_EXPECTED_EXIT_65536=fixture",
		"SUB2API_AUTH_BROWSER_EXPECTED_EXIT_017931=fixture",
		"SUB2API_AUTH_BROWSER_EXPECTED_EXIT_+17931=fixture",
		"SUB2API_AUTH_BROWSER_EXPECTED_EXIT_17931_EXTRA=fixture",
		"SUB2API_AUTH_BROWSER_EXPECTED_EXIT_=fixture", "malformed",
	}
	if got := openAIAuthBrowserEnvironment(append(append([]string{}, denied...), allowed...)); !reflect.DeepEqual(got, allowed) {
		t.Fatal("environment must contain exactly the runtime allowlist and canonical IP pins")
	}
	if got := openAIAuthBrowserEnvironment(nil); got == nil || len(got) != 0 {
		t.Fatal("empty allowlist must be non-nil to prevent implicit host inheritance")
	}
}

func TestOpenAIAuthBrowserDefaultCommandIsolatesEnvironment(t *testing.T) {
	t.Setenv("SUB2API_ENV_CANARY", "fixture")
	t.Setenv("SUB2API_AUTH_BROWSER_MODE", "automated")
	t.Setenv("SUB2API_AUTH_BROWSER_EXPECTED_EXIT_17931", "192.0.2.1")
	cmd := newOpenAIAuthBrowserCommand(context.Background(), "not-executed")
	if !reflect.DeepEqual(cmd.Env, openAIAuthBrowserEnvironment(os.Environ())) {
		t.Fatal("default factory did not sanitize the inherited environment")
	}
}

func TestLauncherDefaultFactoryDoesNotLeakHostEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the bundled launcher uses a POSIX shell")
	}
	t.Setenv("SUB2API_ENV_CANARY", "fixture")
	t.Setenv("SUB2API_AUTH_BROWSER_MODE", "automated")
	t.Setenv("SUB2API_AUTH_BROWSER_EXPECTED_EXIT_17931", "192.0.2.1")
	launcher := launcherTestService()
	launcher.launcherPath = filepath.Join(t.TempDir(), "environment-check")
	script := "#!/bin/sh\n" +
		"test -z \"${SUB2API_ENV_CANARY+x}\" || exit 21\n" +
		"test -z \"${SUB2API_AUTH_BROWSER_MODE+x}\" || exit 22\n" +
		"test \"$SUB2API_AUTH_BROWSER_EXPECTED_EXIT_17931\" = 192.0.2.1 || exit 23\n" +
		"test -n \"$PATH\" || exit 24\n"
	if err := os.WriteFile(launcher.launcherPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := launcher.Launch(context.Background(), "sess-detached")
	if err != nil || result == nil || !result.Launched {
		t.Fatalf("sanitized real launcher failed: %v", err)
	}
}
