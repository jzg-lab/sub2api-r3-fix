// packager 把构建产物打成官方 .s2plugin 包：
// 读 manifest.source.json → 收集 runtimes/* 与 ui/* → 计算 SHA-256 注入 files →
// 写 manifest.json → （可选）Ed25519 签 manifest 原始字节写 signature.json → ZIP。
//
// 用法：
//
//	go run ./tools/packager -out dist/lyunlong-codex-lb-cookie-pin-0.1.0.s2plugin \
//	    -runtimes dist/runtimes -ui ui [-key ~/.s2plugin-keys/cookiepin.ed25519]
//
// 未签名的本地开发包必须显式传入 -allow-unsigned，发布默认要求签名。
package main

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type manifest struct {
	SchemaVersion int                       `json:"schema_version"`
	ID            string                    `json:"id"`
	Name          string                    `json:"name"`
	Version       string                    `json:"version"`
	Description   string                    `json:"description"`
	Author        string                    `json:"author"`
	Requires      map[string]any            `json:"requires"`
	Capabilities  []map[string]any          `json:"capabilities"`
	Runtimes      map[string]map[string]any `json:"runtimes"`
	UI            map[string]any            `json:"ui"`
	Files         map[string]string         `json:"files"`
}

// fileEntry 同时记包内路径与本地源路径，避免二次反推。
type fileEntry struct {
	name  string
	local string
}

func main() {
	var (
		sourceManifest = flag.String("manifest", "manifest.source.json", "清单源文件")
		runtimesDir    = flag.String("runtimes", "dist/runtimes", "运行时目录（runtimes/<goos>-<goarch>/binary）")
		uiDir          = flag.String("ui", "ui", "UI 目录")
		outPath        = flag.String("out", "", "输出 .s2plugin 路径（默认 dist/<id>-<version>.s2plugin）")
		keyPath        = flag.String("key", "", "现有 Ed25519 私钥文件（raw 64B / seed 32B / base64 文本）")
		keyID          = flag.String("key-id", "", "signature.json 的 key_id，默认公钥前 16 hex")
		allowUnsigned  = flag.Bool("allow-unsigned", false, "显式允许未签名的本地开发包")
		checkKey       = flag.Bool("check-key", false, "仅检查签名配置，不构建或输出插件包")
	)
	flag.Parse()

	priv, err := signingKey(*keyPath, *allowUnsigned)
	fatalIf(err, "检查签名配置")
	if *checkKey {
		if len(priv) == 0 {
			fatal("-check-key 需要现有签名密钥", nil)
		}
		fmt.Println("签名密钥格式检查通过")
		return
	}

	src, err := os.ReadFile(*sourceManifest)
	fatalIf(err, "读清单源")
	var m manifest
	fatalIf(json.Unmarshal(src, &m), "解析清单源")
	if m.SchemaVersion != 1 || m.ID == "" || m.Version == "" {
		fatal("清单源缺少 schema_version/id/version", nil)
	}

	entries, runtimes, err := collectRuntimes(*runtimesDir)
	fatalIf(err, "收集运行时")
	// Only artifacts present in this build may appear in the signed manifest.
	m.Runtimes = runtimes
	platforms := []string{}
	for platform := range runtimes {
		platforms = append(platforms, platform)
	}

	// UI 目录：ui/ 下全部文件。
	err = filepath.WalkDir(*uiDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("UI 包含非普通文件: %s", path)
		}
		rel, err := filepath.Rel(*uiDir, path)
		if err != nil {
			return err
		}
		entries = append(entries, fileEntry{name: "ui/" + filepath.ToSlash(rel), local: path})
		return nil
	})
	fatalIf(err, "收集 UI")
	if !hasEntry(entries, "ui/index.html") {
		fatal("缺少 ui/index.html", nil)
	}

	// files = 路径 → 小写 hex SHA-256；manifest 与 signature 自身不写入。
	m.Files = map[string]string{}
	for _, e := range entries {
		sum, err := hashFile(e.local)
		fatalIf(err, "哈希 "+e.name)
		m.Files[e.name] = sum
	}
	manifestBytes, err := json.Marshal(m)
	fatalIf(err, "序列化 manifest")

	if *outPath == "" {
		*outPath = fmt.Sprintf("dist/%s-%s.s2plugin",
			strings.ReplaceAll(m.ID, ".", "-"), strings.TrimPrefix(m.Version, "v"))
	}
	fatalIf(os.MkdirAll(filepath.Dir(*outPath), 0o755), "建输出目录")

	sort.Strings(platforms)

	// 签名对象 = manifest.json 的精确原始字节。
	extra := ""
	if len(priv) != 0 {
		pub := priv.Public().(ed25519.PublicKey)
		id := *keyID
		if id == "" {
			id = hex.EncodeToString(pub)[:16]
		}
		sigJSON, err := json.Marshal(map[string]string{
			"algorithm": "ed25519",
			"key_id":    id,
			"signature": base64.StdEncoding.EncodeToString(ed25519.Sign(priv, manifestBytes)),
		})
		fatalIf(err, "序列化签名")
		fatalIf(writeZip(*outPath, manifestBytes, sigJSON, entries), "写包")
		extra = fmt.Sprintf("（已签名 key_id=%s）", id)
	} else {
		fatalIf(writeZip(*outPath, manifestBytes, nil, entries), "写包")
		extra = "（未签名：宿主需 allow_unsigned，仅限本地开发）"
	}
	fmt.Printf("打包完成: %s\n  runtimes: %s\n  文件 %d 个 %s\n", *outPath, strings.Join(platforms, ", "), len(entries), extra)
}

func signingKey(path string, allowUnsigned bool) (ed25519.PrivateKey, error) {
	if path == "" {
		if allowUnsigned {
			return nil, nil
		}
		return nil, fmt.Errorf("发布必须提供 -key；未签名本地开发包需显式使用 -allow-unsigned")
	}
	if allowUnsigned {
		return nil, fmt.Errorf("-key 与 -allow-unsigned 不能同时使用")
	}
	return loadKey(path)
}

func collectRuntimes(root string) ([]fileEntry, map[string]map[string]any, error) {
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, err
	}
	var entries []fileEntry
	runtimes := make(map[string]map[string]any)
	for _, platform := range dirs {
		if strings.HasPrefix(platform.Name(), ".") {
			continue
		}
		if !platform.IsDir() || !strings.Contains(platform.Name(), "-") {
			return nil, nil, fmt.Errorf("无效运行时目录: %s", platform.Name())
		}
		dir := filepath.Join(root, platform.Name())
		bins, err := os.ReadDir(dir)
		if err != nil {
			return nil, nil, err
		}
		var binary string
		for _, bin := range bins {
			if strings.HasPrefix(bin.Name(), ".") {
				continue
			}
			if !bin.Type().IsRegular() || binary != "" {
				return nil, nil, fmt.Errorf("%s 必须只有一个普通可执行文件", platform.Name())
			}
			binary = bin.Name()
		}
		if binary == "" {
			return nil, nil, fmt.Errorf("%s 缺少可执行文件", platform.Name())
		}
		if strings.HasPrefix(platform.Name(), "windows-") && !strings.HasSuffix(binary, ".exe") {
			return nil, nil, fmt.Errorf("%s 需要 .exe 文件", platform.Name())
		}
		name := "runtimes/" + platform.Name() + "/" + binary
		entries = append(entries, fileEntry{name: name, local: filepath.Join(dir, binary)})
		runtimes[platform.Name()] = map[string]any{"path": name}
	}
	if len(entries) == 0 {
		return nil, nil, fmt.Errorf("运行时目录为空，先跑 build.sh")
	}
	return entries, runtimes, nil
}

// loadKey 接受 raw 64B 私钥、32B seed 或其 base64 文本。
func loadKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if k := decodeKey(raw); k != nil {
		return k, nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if decoded, err := base64.StdEncoding.DecodeString(trimmed); err == nil {
		if k := decodeKey(decoded); k != nil {
			return k, nil
		}
	}
	return nil, fmt.Errorf("私钥格式不支持: %d 字节且不是 base64 编码的 32/64 字节", len(raw))
}

func decodeKey(raw []byte) ed25519.PrivateKey {
	switch len(raw) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw)
	case ed25519.PrivateKeySize:
		if !bytes.Equal(ed25519.NewKeyFromSeed(raw[:ed25519.SeedSize]), raw) {
			return nil
		}
		return ed25519.PrivateKey(raw)
	default:
		return nil
	}
}

func writeZip(outPath string, manifestBytes, signature []byte, entries []fileEntry) error {
	f, err := os.CreateTemp(filepath.Dir(outPath), ".s2plugin-*.tmp")
	if err != nil {
		return err
	}
	defer f.Close()
	defer os.Remove(f.Name())
	zw := zip.NewWriter(f)
	defer zw.Close()
	write := func(name string, data []byte) error {
		w, err := zw.CreateHeader(&zip.FileHeader{
			Name:     name,
			Method:   zip.Deflate,
			Modified: time.Now(),
		})
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	if err := write("manifest.json", manifestBytes); err != nil {
		return err
	}
	if signature != nil {
		if err := write("signature.json", signature); err != nil {
			return err
		}
	}
	for _, e := range entries {
		info, err := os.Lstat(e.local)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("拒绝打包非普通文件: %s", e.name)
		}
		header := &zip.FileHeader{Name: e.name, Method: zip.Deflate, Modified: time.Now()}
		header.SetMode(0o644)
		if strings.HasPrefix(e.name, "runtimes/") {
			header.SetMode(0o755)
		}
		writer, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		source, err := os.Open(e.local)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(writer, source)
		closeErr := source.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), outPath)
}

func hasEntry(entries []fileEntry, name string) bool {
	for _, e := range entries {
		if e.name == name {
			return true
		}
	}
	return false
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func fatalIf(err error, what string) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "packager: "+what+": "+err.Error())
		os.Exit(1)
	}
}

func fatal(msg string, _ any) {
	fmt.Fprintln(os.Stderr, "packager: "+msg)
	os.Exit(1)
}
