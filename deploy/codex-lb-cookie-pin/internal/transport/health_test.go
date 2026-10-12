package transport

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/evidence"
	pluginv1 "github.com/liyunlong/sub2api-cookie-plugin/pkg/pluginapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

func TestHealthLivenessDoesNotWaitForStatusLocks(t *testing.T) {
	s := &Server{Store: cookiestore.New(), evidence: evidence.NewMemory()}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	pluginv1.RegisterTransportPluginServer(server, s)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///plugin-health",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := pluginv1.NewTransportPluginClient(conn)

	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "sub2api-health-mode", "liveness")
	health, err := client.Health(ctx, &pluginv1.HealthRequest{})
	if err != nil {
		t.Fatalf("liveness was blocked by status collection: %v", err)
	}
	if !health.GetHealthy() || health.GetStatusJson() != "" {
		t.Fatalf("liveness must be healthy without collecting a status snapshot: %+v", health)
	}
}

func TestHealthLegacyStatusStillIncludesEvidence(t *testing.T) {
	s := &Server{Store: cookiestore.New(), evidence: evidence.NewMemory()}
	health, err := s.Health(context.Background(), &pluginv1.HealthRequest{})
	if err != nil || !health.GetHealthy() {
		t.Fatalf("legacy health: %v, %+v", err, health)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(health.GetStatusJson()), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["evidence"] == nil {
		t.Fatalf("full status lost evidence fields: %s", health.GetStatusJson())
	}
}

func TestHealthCanceledRequestDoesNotCollectStatus(t *testing.T) {
	// No initialized store is needed: canceled health must return before any I/O.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	health, err := (&Server{}).Health(ctx, &pluginv1.HealthRequest{})
	if err != context.Canceled || health != nil {
		t.Fatalf("canceled Health: %+v, %v", health, err)
	}
}
