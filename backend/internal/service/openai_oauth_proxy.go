package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	errOpenAIOAuthProxyUnavailable = errors.New("OpenAI OAuth assigned proxy is unavailable")
	errOpenAIOAuthProxyInvalid     = errors.New("OpenAI OAuth assigned proxy is invalid")
)

type openAIOAuthProxyLookup interface {
	GetByID(context.Context, int64) (*Proxy, error)
}

// Persist only a fingerprint of the route, never its authentication material.
func openAIOAuthProxyRouteHash(route string) string {
	sum := sha256.Sum256([]byte(route))
	return hex.EncodeToString(sum[:])
}

// An absent assignment means direct traffic. A configured but invalid proxy
// remains an error so callers cannot silently bypass the selected route.
func resolveOpenAIOAuthProxyURL(ctx context.Context, repo openAIOAuthProxyLookup, proxyID *int64) (string, error) {
	if proxyID == nil || *proxyID == 0 {
		return "", nil
	}
	if *proxyID < 0 {
		return "", errOpenAIOAuthProxyInvalid
	}
	if repo == nil {
		return "", errOpenAIOAuthProxyUnavailable
	}
	p, err := repo.GetByID(ctx, *proxyID)
	if err != nil {
		// Repository errors may contain proxy credentials; do not propagate them.
		return "", errOpenAIOAuthProxyUnavailable
	}
	return openAIOAuthProxySnapshotURL(p, proxyID)
}

func openAIOAuthProxySnapshotURL(p *Proxy, proxyID *int64) (string, error) {
	if proxyID == nil || *proxyID == 0 {
		return "", nil
	}
	if *proxyID < 0 {
		return "", errOpenAIOAuthProxyInvalid
	}
	if p == nil || p.ID != *proxyID || !p.IsActive() || p.IsExpired(time.Now()) {
		return "", errOpenAIOAuthProxyUnavailable
	}
	switch p.Protocol {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", errOpenAIOAuthProxyInvalid
	}
	if p.Host == "" || strings.ContainsAny(p.Host, "/?#@[]\\ \t\r\n") ||
		(strings.Contains(p.Host, ":") && net.ParseIP(p.Host) == nil) ||
		p.Port < 1 || p.Port > 65535 ||
		((p.Username == "") != (p.Password == "")) {
		return "", errOpenAIOAuthProxyInvalid
	}
	return p.URL(), nil
}

// isOpenAIDynamicProxyBucket 桶名约定判动态桶（换票主线：novproxy 轮换
// 桶名 novproxy-dynamic-residential）。Proxy 实体无显式动态标志，桶名
// 是面板可见、运营可控的唯一数据驱动判据；改名即失去动态票语义。
func isOpenAIDynamicProxyBucket(p *Proxy) bool {
	return p != nil && strings.Contains(strings.ToLower(p.Name), "dynamic")
}

// Guard the final network boundary, including plugin and pooled WS paths.
// Snapshot validity does not attest the proxy's physical public IP.
func validateOpenAIAccountProxyRoute(account *Account, route string) error {
	if !IsOpenAIBrowserOAuthAccount(account) {
		return nil
	}
	expected, err := openAIOAuthProxySnapshotURL(account.Proxy, account.ProxyID)
	if err != nil {
		return err
	}
	if route != expected {
		return errOpenAIOAuthProxyInvalid
	}
	return nil
}

// Empty routes are direct; nonempty routes must be valid proxy URLs.
func validateOpenAIOAuthProxyURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Opaque != "" || u.RawQuery != "" ||
		u.ForceQuery || u.Fragment != "" || u.Path != "" ||
		strings.ContainsAny(raw, " \t\r\n") {
		return errOpenAIOAuthProxyInvalid
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return errOpenAIOAuthProxyInvalid
	}
	host := u.Hostname()
	port, err := strconv.Atoi(u.Port())
	if host == "" || strings.ContainsAny(host, "/?#@[]\\ \t\r\n") ||
		(strings.Contains(host, ":") && net.ParseIP(host) == nil) ||
		err != nil || port < 1 || port > 65535 {
		return errOpenAIOAuthProxyInvalid
	}
	if u.User != nil {
		password, present := u.User.Password()
		if u.User.Username() == "" || !present || password == "" {
			return errOpenAIOAuthProxyInvalid
		}
	}
	return nil
}
