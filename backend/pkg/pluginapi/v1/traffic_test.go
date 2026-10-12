package pluginv1

import (
	"bytes"
	"os"
	"testing"
)

func TestProbeTrafficPluginCopyMatches(t *testing.T) {
	host, err := os.ReadFile("traffic.go")
	if err != nil {
		t.Fatal(err)
	}
	plugin, err := os.ReadFile("../../../../deploy/codex-lb-cookie-pin/pkg/pluginapi/v1/traffic.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(host, plugin) {
		t.Fatal("host and independently built plugin traffic contracts differ")
	}
}
