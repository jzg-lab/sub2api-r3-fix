package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

type openAIOAuthProxyLookupFunc func(context.Context, int64) (*Proxy, error)

func (f openAIOAuthProxyLookupFunc) GetByID(ctx context.Context, id int64) (*Proxy, error) {
	return f(ctx, id)
}

func TestResolveOpenAIOAuthProxyURLRejectsUnusableAssignment(t *testing.T) {
	id, zero, negative := int64(7), int64(0), int64(-1)
	now := time.Now()
	tests := []struct {
		name    string
		id      *int64
		missing bool
		lookup  error
		mutate  func(*Proxy)
		want    error
	}{
		{name: "unassigned", want: errOpenAIOAuthProxyRequired},
		{name: "zero", id: &zero, want: errOpenAIOAuthProxyRequired},
		{name: "negative", id: &negative, want: errOpenAIOAuthProxyRequired},
		{name: "deleted", id: &id, missing: true, want: errOpenAIOAuthProxyUnavailable},
		{name: "lookup failure", id: &id, lookup: errors.New("private repository detail"), want: errOpenAIOAuthProxyUnavailable},
		{name: "wrong row", id: &id, mutate: func(p *Proxy) { p.ID++ }, want: errOpenAIOAuthProxyUnavailable},
		{name: "disabled", id: &id, mutate: func(p *Proxy) { p.Status = StatusDisabled }, want: errOpenAIOAuthProxyUnavailable},
		{name: "unknown status", id: &id, mutate: func(p *Proxy) { p.Status = "" }, want: errOpenAIOAuthProxyUnavailable},
		{name: "expired direct fallback", id: &id, mutate: func(p *Proxy) {
			p.ExpiresAt, p.FallbackMode = &now, FallbackModeDirect
		}, want: errOpenAIOAuthProxyUnavailable},
		{name: "expired backup fallback", id: &id, mutate: func(p *Proxy) {
			p.ExpiresAt, p.FallbackMode, p.BackupProxyID = &now, FallbackModeProxy, &negative
		}, want: errOpenAIOAuthProxyUnavailable},
		{name: "unknown protocol", id: &id, mutate: func(p *Proxy) { p.Protocol = "direct" }, want: errOpenAIOAuthProxyInvalid},
		{name: "empty host", id: &id, mutate: func(p *Proxy) { p.Host = "" }, want: errOpenAIOAuthProxyInvalid},
		{name: "host whitespace", id: &id, mutate: func(p *Proxy) { p.Host = " 127.0.0.1" }, want: errOpenAIOAuthProxyInvalid},
		{name: "host path", id: &id, mutate: func(p *Proxy) { p.Host = "localhost/path" }, want: errOpenAIOAuthProxyInvalid},
		{name: "host URL", id: &id, mutate: func(p *Proxy) { p.Host = "http://localhost" }, want: errOpenAIOAuthProxyInvalid},
		{name: "host port", id: &id, mutate: func(p *Proxy) { p.Host = "localhost:8080" }, want: errOpenAIOAuthProxyInvalid},
		{name: "zero port", id: &id, mutate: func(p *Proxy) { p.Port = 0 }, want: errOpenAIOAuthProxyInvalid},
		{name: "negative port", id: &id, mutate: func(p *Proxy) { p.Port = -1 }, want: errOpenAIOAuthProxyInvalid},
		{name: "overflow port", id: &id, mutate: func(p *Proxy) { p.Port = 65536 }, want: errOpenAIOAuthProxyInvalid},
		{name: "incomplete auth", id: &id, mutate: func(p *Proxy) { p.Username = "fixture" }, want: errOpenAIOAuthProxyInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{ID: id, Status: StatusActive, Protocol: "http", Host: "127.0.0.1", Port: 8080}
			if tc.mutate != nil {
				tc.mutate(p)
			}
			repo := openAIOAuthProxyLookupFunc(func(context.Context, int64) (*Proxy, error) {
				if tc.missing {
					return nil, nil
				}
				return p, tc.lookup
			})
			got, err := resolveOpenAIOAuthProxyURL(context.Background(), repo, tc.id)
			if got != "" || !errors.Is(err, tc.want) {
				t.Fatalf("must reject with the sanitized assignment error; got URL=%t, error=%v", got != "", err)
			}
		})
	}
	t.Run("repository unavailable", func(t *testing.T) {
		got, err := resolveOpenAIOAuthProxyURL(context.Background(), nil, &id)
		if got != "" || !errors.Is(err, errOpenAIOAuthProxyUnavailable) {
			t.Fatal("nil repository must not result in a direct request")
		}
	})
}

func TestResolveOpenAIOAuthProxyURLPreservesAssignedRoute(t *testing.T) {
	for _, protocol := range []string{"http", "https", "socks5", "socks5h"} {
		for _, host := range []string{"127.0.0.1", "::1", "proxy.localhost"} {
			t.Run(protocol+"/"+host, func(t *testing.T) {
				id := int64(7)
				future := time.Now().Add(time.Hour)
				p := &Proxy{ID: id, Status: StatusActive, Protocol: protocol, Host: host, Port: 8080, ExpiresAt: &future}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := 0
				repo := openAIOAuthProxyLookupFunc(func(gotCtx context.Context, gotID int64) (*Proxy, error) {
					calls++
					if gotID != id || gotCtx != ctx {
						t.Fatal("lookup changed the assigned proxy or request context")
					}
					return p, nil
				})
				got, err := resolveOpenAIOAuthProxyURL(ctx, repo, &id)
				if err != nil || got != p.URL() || calls != 1 {
					t.Fatal("valid route was not preserved")
				}
			})
		}
	}
}

func TestValidateOpenAIOAuthProxyURLRejectsUnsafeRoutes(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want error
	}{
		{"empty", "", errOpenAIOAuthProxyRequired},
		{"blank", " \t", errOpenAIOAuthProxyRequired},
		{"relative", "localhost:8080", errOpenAIOAuthProxyInvalid},
		{"unsupported", "direct://localhost:8080", errOpenAIOAuthProxyInvalid},
		{"missing host", "http://:8080", errOpenAIOAuthProxyInvalid},
		{"missing port", "http://localhost", errOpenAIOAuthProxyInvalid},
		{"zero port", "http://localhost:0", errOpenAIOAuthProxyInvalid},
		{"negative port", "http://localhost:-1", errOpenAIOAuthProxyInvalid},
		{"overflow port", "http://localhost:65536", errOpenAIOAuthProxyInvalid},
		{"nonnumeric port", "http://localhost:invalid", errOpenAIOAuthProxyInvalid},
		{"space", " http://localhost:8080", errOpenAIOAuthProxyInvalid},
		{"newline", "http://localhost:8080\n", errOpenAIOAuthProxyInvalid},
		{"path", "http://localhost:8080/other", errOpenAIOAuthProxyInvalid},
		{"query", "http://localhost:8080?route=other", errOpenAIOAuthProxyInvalid},
		{"empty query", "http://localhost:8080?", errOpenAIOAuthProxyInvalid},
		{"fragment", "http://localhost:8080#other", errOpenAIOAuthProxyInvalid},
		{"opaque", "http:localhost:8080", errOpenAIOAuthProxyInvalid},
		{"incomplete auth", "http://fixture@localhost:8080", errOpenAIOAuthProxyInvalid},
		{"empty auth", "http://fixture:@localhost:8080", errOpenAIOAuthProxyInvalid},
		{"missing username", "http://:fixture@localhost:8080", errOpenAIOAuthProxyInvalid},
		{"invalid ipv6", "http://[wrong]:8080", errOpenAIOAuthProxyInvalid},
		{"unbracketed ipv6", "http://::1:8080", errOpenAIOAuthProxyInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateOpenAIOAuthProxyURL(tc.raw); !errors.Is(err, tc.want) {
				t.Fatalf("expected sanitized route error, got %v", err)
			}
		})
	}
}

func TestValidateOpenAIOAuthProxyURLAcceptsSerializedProxy(t *testing.T) {
	for _, protocol := range []string{"http", "https", "socks5", "socks5h"} {
		for _, host := range []string{"127.0.0.1", "::1", "proxy.localhost"} {
			t.Run(protocol+"/"+host, func(t *testing.T) {
				for _, port := range []int{1, 8080, 65535} {
					p := &Proxy{Protocol: protocol, Host: host, Port: port}
					if err := validateOpenAIOAuthProxyURL(p.URL()); err != nil {
						t.Fatalf("valid route rejected: %v", err)
					}
					p.Username, p.Password = "fixture@user", "fixture:/?#"
					if err := validateOpenAIOAuthProxyURL(p.URL()); err != nil {
						t.Fatalf("encoded route rejected: %v", err)
					}
				}
			})
		}
	}
}
