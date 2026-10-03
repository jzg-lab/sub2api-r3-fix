package buildcheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the release script without generating cross-platform binaries.
func TestBuildScript(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("POSIX shell is required to run the release script")
	}
	script, err := os.ReadFile("../../build.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		hostOS     string
		failStage  string
		exitCode   int
		signed     bool
		args       []string
		noGo       bool
		missingKey bool
	}{
		{name: "five_platforms_and_exported_cgo", hostOS: "darwin", signed: true},
		{name: "windows_smoke_executable", hostOS: "windows", signed: true},
		{name: "explicit_development_unsigned", hostOS: "darwin", args: []string{"--allow-unsigned"}},
		{name: "default_requires_key", exitCode: 2, noGo: true},
		{name: "release_requires_key", args: []string{"--release"}, exitCode: 2, noGo: true},
		{name: "missing_key_file", signed: true, missingKey: true, exitCode: 2, noGo: true},
		{name: "unsigned_conflicts_with_key", signed: true, args: []string{"--allow-unsigned"}, exitCode: 2, noGo: true},
		{name: "release_cannot_append_unsigned", signed: true, args: []string{"--release", "--allow-unsigned"}, exitCode: 2, noGo: true},
		{name: "unknown_mode", signed: true, args: []string{"--relase"}, exitCode: 2, noGo: true},
		{name: "bad_key_stops_before_build", signed: true, failStage: "keycheck", exitCode: 45},
		{name: "unit_failure_stops_build", hostOS: "darwin", signed: true, failStage: "test", exitCode: 41},
		{name: "build_failure_stops_package", hostOS: "darwin", signed: true, failStage: "build", exitCode: 42},
		{name: "packager_failure_stops_smoke", hostOS: "darwin", signed: true, failStage: "packager", exitCode: 43},
		{name: "smoke_failure_is_not_hidden", hostOS: "darwin", signed: true, failStage: "smoke", exitCode: 44},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			binDir := filepath.Join(root, "bin")
			if err := os.Mkdir(binDir, 0o700); err != nil {
				t.Fatal(err)
			}
			for name, content := range map[string]string{
				"build.sh": string(script),
				"bin/go": `#!/bin/sh
set -eu
[ "${CGO_ENABLED:-}" = 0 ] || exit 90
printf '%s\n' "$*" >> "$TEST_CALLS"
case "$1" in
  test) [ "$TEST_FAIL_STAGE" != test ] || exit 41 ;;
  build)
    [ "$TEST_FAIL_STAGE" != build ] || exit 42
    while [ "$1" != -o ]; do shift; done
    shift
    : > "$1"
    ;;
  env)
    case "$2" in
      GOOS) printf '%s\n' "$TEST_HOST_OS" ;;
      GOARCH) printf '%s\n' amd64 ;;
    esac
    ;;
  run)
    case "$2" in
      ./tools/packager)
        if [ "$3" = -check-key ]; then
          [ "$TEST_FAIL_STAGE" != keycheck ] || exit 45
        else
          [ "$TEST_FAIL_STAGE" != packager ] || exit 43
        fi
        ;;
      ./tools/testhost)
        printf '%s\n' "INFO mock-only diagnostic"
        [ "$TEST_FAIL_STAGE" != smoke ] || exit 44
        ;;
    esac
    ;;
esac
`,
			} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("TEST_CALLS", filepath.Join(root, "calls"))
			t.Setenv("TEST_FAIL_STAGE", tc.failStage)
			t.Setenv("TEST_HOST_OS", tc.hostOS)
			t.Setenv("CGO_ENABLED", "1")
			t.Setenv("S2PLUGIN_KEY", "")
			t.Setenv("S2PLUGIN_KEY_ID", "existing-publisher")
			keyPath := filepath.Join(root, "fixture signing key")
			if tc.signed {
				t.Setenv("S2PLUGIN_KEY", keyPath)
				if !tc.missingKey {
					if err := os.WriteFile(keyPath, []byte("fixture"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			cmd := exec.Command("sh", append([]string{filepath.Join(root, "build.sh")}, tc.args...)...)
			output, runErr := cmd.CombinedOutput()
			gotExit := 0
			if runErr != nil {
				if exitErr, ok := runErr.(*exec.ExitError); ok {
					gotExit = exitErr.ExitCode()
				} else {
					t.Fatal(runErr)
				}
			}
			if gotExit != tc.exitCode {
				t.Fatalf("exit = %d, want %d\n%s", gotExit, tc.exitCode, output)
			}
			calls, err := os.ReadFile(filepath.Join(root, "calls"))
			if tc.noGo {
				if !os.IsNotExist(err) {
					t.Fatal("invalid release options reached the Go toolchain")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			text := string(calls)
			if tc.failStage == "keycheck" {
				if strings.Contains(text, "test ") || strings.Contains(text, "build ") {
					t.Fatal("invalid key reached tests or cross-compilation")
				}
				return
			}
			if tc.failStage == "test" || tc.failStage == "build" {
				if strings.Contains(text, "./tools/packager -runtimes") {
					t.Fatal("packaging ran after a test/build failure")
				}
				return
			}
			for _, platform := range []string{"darwin-arm64", "darwin-amd64", "linux-amd64", "linux-arm64", "windows-amd64"} {
				name := "cookiepin"
				if strings.HasPrefix(platform, "windows-") {
					name += ".exe"
				}
				if _, err := os.Stat(filepath.Join(root, "dist", "runtimes", platform, name)); err != nil {
					t.Fatal(err)
				}
			}
			if tc.failStage == "packager" {
				if strings.Contains(text, "./tools/testhost") {
					t.Fatal("smoke ran after a package failure")
				}
				return
			}
			wantSmoke := "run ./tools/testhost -plugin dist/runtimes/" + tc.hostOS + "-amd64/cookiepin"
			if tc.hostOS == "windows" {
				wantSmoke += ".exe"
			}
			if !strings.Contains(text, wantSmoke+" -mock") {
				t.Fatalf("missing native smoke command: %s", wantSmoke)
			}
			if tc.signed && !strings.Contains(text, "-key "+keyPath+" -key-id existing-publisher") {
				t.Fatal("signing key path or existing publisher ID was not forwarded")
			}
			if !tc.signed && !strings.Contains(text, "-allow-unsigned") {
				t.Fatal("development build omitted the explicit unsigned flag")
			}
		})
	}
}
