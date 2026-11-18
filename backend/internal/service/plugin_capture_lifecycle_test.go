package service

import (
	"context"
	"errors"
	"testing"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type captureConfigRollbackClient struct {
	pluginv1.TransportPluginClient
	applied        []byte
	applyCount     int
	rejectRollback bool
}

func (c *captureConfigRollbackClient) ValidateConfig(_ context.Context, req *pluginv1.ValidateConfigRequest, _ ...grpc.CallOption) (*pluginv1.ValidateConfigResponse, error) {
	return &pluginv1.ValidateConfigResponse{Valid: true, NormalizedConfigJson: req.ConfigJson}, nil
}

func (c *captureConfigRollbackClient) ApplyConfig(ctx context.Context, req *pluginv1.ApplyConfigRequest, options ...grpc.CallOption) (*pluginv1.ApplyConfigResponse, error) {
	c.applyCount++
	if c.rejectRollback && c.applyCount > 1 {
		return &pluginv1.ApplyConfigResponse{Applied: false, Message: "rollback rejected"}, nil
	}
	if c.TransportPluginClient != nil {
		response, err := c.TransportPluginClient.ApplyConfig(ctx, req, options...)
		if err != nil || !response.GetApplied() {
			return response, err
		}
	}
	c.applied = append([]byte(nil), req.ConfigJson...)
	return &pluginv1.ApplyConfigResponse{Applied: true}, nil
}

type captureFailingEncryptor struct{ pluginTokenEncryptor }

func (captureFailingEncryptor) Encrypt(string) (string, error) {
	return "", errors.New("encryption unavailable")
}

func TestCaptureConfigSaveFailureRollsBackOrStopsCollector(t *testing.T) {
	for _, failure := range []string{"database", "encryption"} {
		for _, rollback := range []string{"restored", "rejected"} {
			t.Run(failure+"/"+rollback, func(t *testing.T) {
				previous := `{"group_ids":[1],"models":["old"]}`
				next := `{"group_ids":[2],"models":["new"]}`
				installation := &PluginInstallation{ID: 7, BinarySHA256: "fixture", ConfigEncrypted: "ENC:" + previous}
				repo := &pluginConfigRepository{installation: installation}
				client := &captureConfigRollbackClient{rejectRollback: rollback == "rejected"}
				runtime := &pluginRuntime{installation: installation, api: client, done: make(chan struct{})}
				manager := NewPluginManager(repo, pluginTokenEncryptor{}, nil, PluginHostInfo{})
				if failure == "database" {
					repo.updateErr = errors.New("database unavailable")
				} else {
					manager.encryptor = captureFailingEncryptor{}
				}
				manager.runtimes[7] = runtime
				manager.captureRuntime.Store(runtime)
				outbound := &pluginRoute{pluginID: 99, runtime: &pluginRuntime{}, rolloutPercent: 100}
				manager.route.Store(outbound)
				manager.runtimes[99] = outbound.runtime

				_, err := manager.SaveConfig(t.Context(), 7, []byte(next))
				require.ErrorContains(t, err, failure+" unavailable")
				require.Equal(t, 2, client.applyCount)
				require.Empty(t, repo.encrypted)
				require.Equal(t, "ENC:"+previous, installation.ConfigEncrypted)
				require.Same(t, outbound, manager.route.Load())
				require.Same(t, outbound.runtime, manager.runtimes[99])
				ctx, finish := manager.beginCapture(t.Context(), nil, &Account{ID: 1, Platform: PlatformOpenAI})
				defer finish(false)
				if rollback == "rejected" {
					require.ErrorContains(t, err, "rollback rejected")
					require.JSONEq(t, next, string(client.applied))
					require.Nil(t, manager.captureRuntime.Load())
					require.NotContains(t, manager.runtimes, int64(7))
					require.True(t, runtime.draining.Load())
					require.False(t, runtime.beginRequest())
					require.Nil(t, ctx.Value(captureContextKey{}))
				} else {
					require.JSONEq(t, previous, string(client.applied))
					require.Same(t, runtime, manager.captureRuntime.Load())
					require.Same(t, runtime, manager.runtimes[7])
					require.False(t, runtime.draining.Load())
					require.NotNil(t, ctx.Value(captureContextKey{}))
				}
			})
		}
	}
}
