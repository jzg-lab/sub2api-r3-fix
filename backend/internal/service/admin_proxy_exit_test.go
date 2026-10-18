package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type exitObservationProber struct {
	info      *ProxyExitInfo
	err       error
	seenRoute string
}

func (p *exitObservationProber) ProbeProxy(_ context.Context, route string) (*ProxyExitInfo, int64, error) {
	p.seenRoute = route
	return p.info, 25, p.err
}

func TestProxyExitObservation_UnknownIsNotSuccess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prober *exitObservationProber
	}{
		{"unavailable", nil},
		{"nil_result", &exitObservationProber{}},
		{"empty_ip", &exitObservationProber{info: &ProxyExitInfo{Country: "unverified"}}},
		{"invalid_ip", &exitObservationProber{info: &ProxyExitInfo{IP: "unknown"}}},
		{"unspecified", &exitObservationProber{info: &ProxyExitInfo{IP: "0.0.0.0"}}},
		{"zoned", &exitObservationProber{info: &ProxyExitInfo{IP: "fe80::1%en0"}}},
		{"failed_with_stale_info", &exitObservationProber{info: &ProxyExitInfo{IP: "192.0.2.1"}, err: errors.New("probe failed")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &adminServiceImpl{}
			if tc.prober != nil {
				svc.proxyProber = tc.prober
			}
			info, _, err := svc.probeProxyExit(context.Background(), "socks5://proxy.example.test:1080")
			require.Error(t, err)
			require.Nil(t, info)
		})
	}
}

func TestProxyExitObservation_PreservesExactRouteAndMeasuredInfo(t *testing.T) {
	prober := &exitObservationProber{info: &ProxyExitInfo{IP: "::ffff:192.0.2.1", Country: "measured"}}
	svc := &adminServiceImpl{proxyProber: prober}
	route := "socks5://proxy.example.test:1080"
	info, latency, err := svc.probeProxyExit(context.Background(), route)
	require.NoError(t, err)
	require.Equal(t, route, prober.seenRoute)
	require.Equal(t, int64(25), latency)
	require.Equal(t, "192.0.2.1", info.IP)
	require.Equal(t, "measured", info.Country)
	require.Equal(t, "::ffff:192.0.2.1", prober.info.IP, "do not mutate shared prober state")
}
