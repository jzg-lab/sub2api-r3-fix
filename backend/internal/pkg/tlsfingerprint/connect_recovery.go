package tlsfingerprint

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"syscall"
)

// Recovery stays inside tunnel/TLS establishment, before net/http can write an
// application request. Never use this around RoundTrip or a response body read.
func connectBeforeRequest(ctx context.Context, dial func() (net.Conn, error)) (net.Conn, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, err := dial()
		if err == nil {
			if attempt > 0 {
				slog.Info("tls_fingerprint_connect_recovered", "attempt", attempt+1, "application_request_sent", false)
			}
			return conn, nil
		}
		if conn != nil {
			_ = conn.Close()
		}
		if attempt == 1 || ctx.Err() != nil ||
			(!errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, syscall.ECONNRESET)) {
			return nil, err
		}
		slog.Warn("tls_fingerprint_connect_retry", "attempt", attempt+1, "application_request_sent", false)
	}
	panic("unreachable connection recovery")
}
