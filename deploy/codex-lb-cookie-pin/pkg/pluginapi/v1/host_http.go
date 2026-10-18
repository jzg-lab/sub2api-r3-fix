package pluginv1

import (
	"context"
	"encoding/json"
	"strconv"

	"google.golang.org/grpc/metadata"
)

// HostHTTP uses the existing reverse broker and Forward frames. It is separate
// from HostService (KV/accounts), and carries no credentials in metadata.
const HostHTTPCapability = "sub2api.host_http.v1"

const hostHTTPMetadataKey = "sub2api-host-http"
const hostHTTPBrokerKey = "sub2api-host-http-broker"

func WithHostHTTPBroker(ctx context.Context, id uint32) context.Context {
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set(hostHTTPBrokerKey, strconv.FormatUint(uint64(id), 10))
	return metadata.NewOutgoingContext(ctx, md)
}

func ReadHostHTTPBroker(ctx context.Context) uint32 {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get(hostHTTPBrokerKey)
	if len(values) != 1 {
		return 0
	}
	id, err := strconv.ParseUint(values[0], 10, 32)
	if err != nil {
		return 0
	}
	return uint32(id)
}

type HostHTTPBinding struct {
	BrokerID uint32 `json:"broker_id"`
	Request  string `json:"request"`
	Probe    string `json:"probe"`
	IsProbe  bool   `json:"is_probe,omitempty"`
}

func WithHostHTTPBinding(ctx context.Context, binding HostHTTPBinding) context.Context {
	raw, _ := json.Marshal(binding)
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set(hostHTTPMetadataKey, string(raw))
	return metadata.NewOutgoingContext(ctx, md)
}

func ReadHostHTTPBinding(ctx context.Context) (HostHTTPBinding, bool) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get(hostHTTPMetadataKey)
	var binding HostHTTPBinding
	if len(values) != 1 || len(values[0]) > 1024 ||
		json.Unmarshal([]byte(values[0]), &binding) != nil || binding.BrokerID == 0 ||
		len(binding.Request) != 64 || len(binding.Probe) != 64 {
		return HostHTTPBinding{}, false
	}
	return binding, true
}
