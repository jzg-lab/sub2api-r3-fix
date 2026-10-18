package pluginv1

import (
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"
)

func TestHostHTTPMetadataBinding(t *testing.T) {
	binding := HostHTTPBinding{BrokerID: 17, Request: strings.Repeat("a", 64), Probe: strings.Repeat("b", 64), IsProbe: true}
	ctx := WithHostHTTPBroker(WithHostHTTPBinding(t.Context(), binding), 17)
	md, _ := metadata.FromOutgoingContext(ctx)
	incoming := metadata.NewIncomingContext(t.Context(), md)
	got, ok := ReadHostHTTPBinding(incoming)
	if !ok || got != binding || ReadHostHTTPBroker(incoming) != 17 {
		t.Fatal("binding did not survive metadata round trip")
	}
	for _, value := range []string{"", "{}", "{", strings.Repeat("x", 1025),
		`{"broker_id":0,"request":"x","probe":"y"}`} {
		ctx := metadata.NewIncomingContext(t.Context(), metadata.Pairs(hostHTTPMetadataKey, value))
		if _, ok := ReadHostHTTPBinding(ctx); ok {
			t.Fatal("malformed binding accepted")
		}
	}
	md[hostHTTPMetadataKey] = append(md[hostHTTPMetadataKey], md[hostHTTPMetadataKey][0])
	if _, ok := ReadHostHTTPBinding(metadata.NewIncomingContext(t.Context(), md)); ok {
		t.Fatal("ambiguous duplicate binding accepted")
	}
	for _, value := range []string{"", "-1", "4294967296", "1x"} {
		ctx := metadata.NewIncomingContext(t.Context(), metadata.Pairs(hostHTTPBrokerKey, value))
		if ReadHostHTTPBroker(ctx) != 0 {
			t.Fatal("malformed broker id accepted")
		}
	}
}
