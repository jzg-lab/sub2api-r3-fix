package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAITelemetryProxyRejectsStaleQueuedRoute(t *testing.T) {
	route := openAITransportTestProxy().URL()
	for _, mode := range []string{"current", "unresolved", "direct", "changed", "missing_lookup", "missing_account"} {
		t.Run(mode, func(t *testing.T) {
			account := &Account{ID: 17, ProxyID: openAITransportTestProxyID()}
			manager := &openAICodexTelemetryManager{}
			calls := 0
			if mode != "missing_lookup" {
				manager.bindProxyLookup(func(ctx context.Context, got *Account) (string, error) {
					calls++
					require.Same(t, account, got)
					switch mode {
					case "unresolved":
						return "", errOpenAIOAuthProxyUnavailable
					case "direct":
						return "", nil
					case "changed":
						return "http://127.0.0.1:18081", nil
					default:
						return route, nil
					}
				})
			}
			job := openAICodexTelemetryJob{client: openAICodexTelemetryIdentity{account: account, proxyURL: route}}
			if mode == "missing_account" {
				job.client.account = nil
			}
			got, err := manager.resolveProxyURL(context.Background(), job)
			if mode == "current" {
				require.NoError(t, err)
				require.Equal(t, route, got)
			} else {
				require.Error(t, err)
				require.Empty(t, got)
			}
			if mode != "missing_account" && mode != "missing_lookup" {
				require.Equal(t, 1, calls, "queued URL cannot bypass current lookup")
			}
		})
	}
}

func TestOpenAITelemetryProxyReloadsAccountAndHonorsCancellation(t *testing.T) {
	for _, mode := range []string{"current", "deleted", "reassigned", "inactive_account", "inactive_proxy", "expired_proxy", "lookup_error", "cancelled", "edited_proxy"} {
		t.Run(mode, func(t *testing.T) {
			proxy := openAITransportTestProxy()
			queued := &Account{ID: 17, Status: StatusActive, ProxyID: &proxy.ID, Proxy: proxy}
			current := *queued
			repo := &downgradeProbeAccountRepoStub{account: &current}
			lookupCalls := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			currentProxy := *proxy
			switch mode {
			case "deleted":
				repo.account = nil
			case "reassigned":
				id := int64(99)
				current.ProxyID = &id
			case "inactive_account":
				current.Status = StatusDisabled
			case "inactive_proxy":
				currentProxy.Status = StatusDisabled
			case "expired_proxy":
				expired := time.Now().Add(-time.Second)
				currentProxy.ExpiresAt = &expired
			case "edited_proxy":
				currentProxy.Port++
			case "cancelled":
				cancel()
			}
			proxies := &mockProxyRepoForOAuth{getByIDFunc: func(got context.Context, id int64) (*Proxy, error) {
				require.Equal(t, ctx, got)
				require.Equal(t, proxy.ID, id)
				lookupCalls++
				if mode == "lookup_error" {
					return nil, errors.New("injected lookup failure")
				}
				return &currentProxy, nil
			}}
			runner := &OpenAIDowngradeProbeRunner{accountRepo: repo, proxyRepo: proxies}
			got, err := runner.resolveTelemetryProxyURL(ctx, queued)
			if mode == "current" {
				require.NoError(t, err)
				require.Equal(t, proxy.URL(), got)
				require.Equal(t, 1, lookupCalls)
			} else {
				require.Error(t, err)
				require.Empty(t, got)
			}
			if mode == "cancelled" || mode == "deleted" || mode == "reassigned" || mode == "inactive_account" {
				require.Zero(t, lookupCalls)
			}
		})
	}
}

func TestOpenAITelemetryUsesCurrentDirectRoute(t *testing.T) {
	account := &Account{ID: 17, Status: StatusActive}
	runner := &OpenAIDowngradeProbeRunner{accountRepo: &downgradeProbeAccountRepoStub{account: account}}
	manager := &openAICodexTelemetryManager{}
	manager.bindProxyLookup(runner.resolveTelemetryProxyURL)
	job := openAICodexTelemetryJob{client: openAICodexTelemetryIdentity{account: account}}
	route, err := manager.resolveProxyURL(context.Background(), job)
	require.NoError(t, err)
	require.Empty(t, route)
}
