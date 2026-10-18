package handler

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Both default and unit-tagged WebSocket tests use the same local upstream.
type drainHTTPUpstream struct{ service.HTTPUpstream }

func (u *drainHTTPUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return http.DefaultClient.Do(req)
}

func (u *drainHTTPUpstream) DoWithTLS(req *http.Request, proxy string, id int64, count int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, count)
}
