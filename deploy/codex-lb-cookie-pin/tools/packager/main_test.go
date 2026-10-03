package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCollectRuntimes(t *testing.T) {
	root := t.TempDir()
	for _, platform := range []string{"darwin-arm64", "darwin-amd64", "linux-arm64", "linux-amd64", "windows-amd64"} {
		dir := filepath.Join(root, platform)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		name := "cookiepin"
		if platform == "windows-amd64" {
			name += ".exe"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	entries, runtimes, err := collectRuntimes(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 || len(runtimes) != 5 {
		t.Fatalf("incomplete platform matrix: %d entries, %d runtimes", len(entries), len(runtimes))
	}
	if runtimes["windows-amd64"]["path"] != "runtimes/windows-amd64/cookiepin.exe" {
		t.Fatal("Windows executable path is not preserved")
	}
}

func TestCollectRuntimesRejectsAmbiguousOrUnsafeInputs(t *testing.T) {
	for _, mode := range []string{"empty", "multiple", "symlink", "directory", "windows-extension"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "symlink" && runtime.GOOS == "windows" {
				t.Skip("Windows symlink creation requires a separate privilege")
			}
			root := t.TempDir()
			dir := filepath.Join(root, "windows-amd64")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			executable := filepath.Join(dir, "cookiepin.exe")
			switch mode {
			case "multiple":
				for _, name := range []string{"cookiepin.exe", "stale.exe"} {
					if err := os.WriteFile(filepath.Join(dir, name), nil, 0o700); err != nil {
						t.Fatal(err)
					}
				}
			case "symlink":
				if err := os.Symlink(filepath.Join(root, "outside"), executable); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(executable, 0o700); err != nil {
					t.Fatal(err)
				}
			case "windows-extension":
				if err := os.WriteFile(filepath.Join(dir, "cookiepin"), nil, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := collectRuntimes(root); err == nil {
				t.Fatal("invalid runtime input was accepted")
			}
		})
	}
}

func TestWriteZipPreservesPreviousPackageOnFailure(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "release.s2plugin")
	previous := "previous validated package"
	if err := os.WriteFile(out, []byte(previous), 0o600); err != nil {
		t.Fatal(err)
	}
	err := writeZip(out, []byte(`{}`), nil, []fileEntry{{name: "runtimes/linux-amd64/cookiepin", local: filepath.Join(root, "missing")}})
	if err == nil {
		t.Fatal("missing runtime must fail packaging")
	}
	got, err := os.ReadFile(out)
	if err != nil || string(got) != previous {
		t.Fatalf("previous package was damaged: %v", err)
	}
	leftovers, err := filepath.Glob(filepath.Join(root, ".s2plugin-*.tmp"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary package was not removed: %v, %v", leftovers, err)
	}
}

func TestWriteZipAndHashFile(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "cookiepin.exe")
	content := []byte("small executable fixture")
	if err := os.WriteFile(binary, content, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	digest, err := hashFile(binary)
	if err != nil || digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("file digest mismatch: %v", err)
	}
	out := filepath.Join(root, "release.s2plugin")
	if err := writeZip(out, []byte(`{}`), []byte(`{}`), []fileEntry{{name: "runtimes/windows-amd64/cookiepin.exe", local: binary}}); err != nil {
		t.Fatal(err)
	}
	archive, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if len(archive.File) != 3 {
		t.Fatalf("unexpected archive entries: %d", len(archive.File))
	}
	file := archive.File[2]
	if file.Name != "runtimes/windows-amd64/cookiepin.exe" || file.Mode().Perm() != 0o755 {
		t.Fatal("runtime archive path or permissions are incorrect")
	}
	reader, err := file.Open()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(got) != string(content) {
		t.Fatalf("archive content differs: %v", err)
	}
}
