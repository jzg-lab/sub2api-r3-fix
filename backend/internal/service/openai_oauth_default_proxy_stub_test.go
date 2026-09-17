//go:build !unit

package service

import (
	"context"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

// The OAuth-focused tests are part of the default test build, while the
// broader OAuth test helper is intentionally unit-tagged.
type mockProxyRepoForOAuth struct {
	getByIDFunc func(ctx context.Context, id int64) (*Proxy, error)
}

func (m *mockProxyRepoForOAuth) Create(context.Context, *Proxy) error {
	panic("Create not implemented")
}

func (m *mockProxyRepoForOAuth) GetByID(ctx context.Context, id int64) (*Proxy, error) {
	if m.getByIDFunc != nil {
		return m.getByIDFunc(ctx, id)
	}
	return nil, fmt.Errorf("proxy not found")
}

func (m *mockProxyRepoForOAuth) ListByIDs(context.Context, []int64) ([]Proxy, error) {
	panic("ListByIDs not implemented")
}

func (m *mockProxyRepoForOAuth) Update(context.Context, *Proxy) error {
	panic("Update not implemented")
}

func (m *mockProxyRepoForOAuth) Delete(context.Context, int64) error {
	panic("Delete not implemented")
}

func (m *mockProxyRepoForOAuth) List(context.Context, pagination.PaginationParams) ([]Proxy, *pagination.PaginationResult, error) {
	panic("List not implemented")
}

func (m *mockProxyRepoForOAuth) ListWithFilters(context.Context, pagination.PaginationParams, string, string, string) ([]Proxy, *pagination.PaginationResult, error) {
	panic("ListWithFilters not implemented")
}

func (m *mockProxyRepoForOAuth) ListWithFiltersAndAccountCount(context.Context, pagination.PaginationParams, string, string, string) ([]ProxyWithAccountCount, *pagination.PaginationResult, error) {
	panic("ListWithFiltersAndAccountCount not implemented")
}

func (m *mockProxyRepoForOAuth) ListActive(context.Context) ([]Proxy, error) {
	panic("ListActive not implemented")
}

func (m *mockProxyRepoForOAuth) ListActiveWithAccountCount(context.Context) ([]ProxyWithAccountCount, error) {
	panic("ListActiveWithAccountCount not implemented")
}

func (m *mockProxyRepoForOAuth) ExistsByHostPortAuth(context.Context, string, int, string, string) (bool, error) {
	panic("ExistsByHostPortAuth not implemented")
}

func (m *mockProxyRepoForOAuth) CountAccountsByProxyID(context.Context, int64) (int64, error) {
	panic("CountAccountsByProxyID not implemented")
}

func (m *mockProxyRepoForOAuth) ListAccountSummariesByProxyID(context.Context, int64) ([]ProxyAccountSummary, error) {
	panic("ListAccountSummariesByProxyID not implemented")
}

func (m *mockProxyRepoForOAuth) SweepExpiredProxies(context.Context, time.Time) (int64, error) {
	panic("SweepExpiredProxies not implemented")
}

func (m *mockProxyRepoForOAuth) ListAllForFallback(context.Context) ([]Proxy, error) {
	panic("ListAllForFallback not implemented")
}

func (m *mockProxyRepoForOAuth) CountExpired(context.Context) (int64, error) {
	panic("CountExpired not implemented")
}

func (m *mockProxyRepoForOAuth) CountExpiringSoon(context.Context, time.Time) (int64, error) {
	panic("CountExpiringSoon not implemented")
}
