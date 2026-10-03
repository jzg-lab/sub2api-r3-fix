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
		name      string
		hostOS    string
		failStage string
		exitCode  int
		signed    bool
	}{
		{name: "five_platforms_and_exported_cgo", hostOS: "darwin"},
		{name: "windows_smoke_executable", hostOS: "windows", signed: true},
		{name: "unit_failure_stops_build", hostOS: "darwin", failStage: "test", exitCode: 41},
		{name: "build_failure_stops_package", hostOS: "darwin", failStage: "build", exitCode: 42},
		{name: "packager_failure_stops_smoke", hostOS: "darwin", failStage: "packager", exitCode: 43},
		{name: "smoke_failure_is_not_hidden", hostOS: "darwin", failStage: "smoke", exitCode: 44},
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
      ./tools/packager) [ "$TEST_FAIL_STAGE" != packager ] || exit 43 ;;
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
			// Test both the unset optional key and a configured signing path.
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("TEST_CALLS", filepath.Join(root, "calls"))
			t.Setenv("TEST_FAIL_STAGE", tc.failStage)
			t.Setenv("TEST_HOST_OS", tc.hostOS)
			t.Setenv("CGO_ENABLED", "1")
			t.Setenv("S2PLUGIN_KEY", "")
			if tc.signed {
				t.Setenv("S2PLUGIN_KEY", "fixture-signing-key")
			}
			cmd := exec.Command("sh", filepath.Join(root, "build.sh"))
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
			if err != nil {
				t.Fatal(err)
			}
			text := string(calls)
			if tc.failStage == "test" || tc.failStage == "build" {
				if strings.Contains(text, "./tools/packager") {
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
			if tc.signed && !strings.Contains(text, "-key fixture-signing-key") {
				t.Fatal("signing key path was not forwarded")
			}
		})
	}
}
