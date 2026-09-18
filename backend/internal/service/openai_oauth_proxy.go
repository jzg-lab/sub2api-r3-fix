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
	errOpenAIOAuthProxyRequired    = errors.New("OpenAI OAuth requires an assigned proxy")
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

// OAuth auxiliary requests must not silently bypass the account's proxy.
// This validates the assignment, not the proxy's observed public egress IP.
func resolveOpenAIOAuthProxyURL(ctx context.Context, repo openAIOAuthProxyLookup, proxyID *int64) (string, error) {
	if proxyID == nil || *proxyID <= 0 {
		return "", errOpenAIOAuthProxyRequired
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
	if proxyID == nil || *proxyID <= 0 {
		return "", errOpenAIOAuthProxyRequired
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

// Raw refresh callers must not turn an omitted or malformed route into direct
// traffic. Assignment-based callers also validate the current proxy row above.
func validateOpenAIOAuthProxyURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return errOpenAIOAuthProxyRequired
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
