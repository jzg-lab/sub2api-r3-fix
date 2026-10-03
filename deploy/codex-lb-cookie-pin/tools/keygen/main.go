// keygen 生成 Ed25519 发布密钥对，用于 .s2plugin 的 signature.json。
//
//	go run ./tools/keygen -out ~/.s2plugin-keys/cookiepin.ed25519
//
// 私钥写文件（0600），公钥打印 base64 —— 配到宿主 trusted_publishers。
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	out := flag.String("out", "", "私钥输出路径（必填）")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "keygen: 需要 -out <路径>")
		os.Exit(1)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintln(os.Stderr, "keygen:", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "keygen:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, priv, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "keygen:", err)
		os.Exit(1)
	}
	fmt.Printf("私钥: %s（0600，勿入库勿进包）\n公钥(base64，配到宿主 trusted_publishers): %s\nkey_id(公钥前16hex): %s\n",
		*out, base64.StdEncoding.EncodeToString(pub), hex.EncodeToString(pub)[:16])
}
