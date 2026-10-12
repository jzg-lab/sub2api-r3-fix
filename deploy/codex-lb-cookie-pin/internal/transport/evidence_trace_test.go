package transport

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"syscall"
	"testing"
)

func TestKnownUnsentRequiresBothConnectionErrorAndNoConnection(t *testing.T) {
	request, err := http.NewRequest(http.MethodPost, "http://fixture.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	request, trace := traceUpstreamRequest(request)
	refused := &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	if !trace.definitelyNotConnected(refused) || trace.definitelyNotConnected(errors.New("unknown")) {
		t.Fatal("unsent classification lacks affirmative connection evidence")
	}
	httptrace.ContextClientTrace(request.Context()).GotConn(httptrace.GotConnInfo{})
	if trace.definitelyNotConnected(refused) {
		t.Fatal("a prior connected attempt was erased by a later dial error")
	}
}
