package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type pausingCaptureDownloadClient struct {
	pluginv1.TransportPluginClient
	entered chan struct{}
	resume  chan struct{}
}

func (c *pausingCaptureDownloadClient) DownloadCaptureRecord(ctx context.Context, req *pluginv1.DownloadCaptureRecordRequest, options ...grpc.CallOption) (*pluginv1.DownloadCaptureRecordResponse, error) {
	close(c.entered)
	select {
	case <-c.resume:
		return c.TransportPluginClient.DownloadCaptureRecord(ctx, req, options...)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type captureIntegrationRepo struct {
	PluginRepository
	mu           sync.Mutex
	installation *PluginInstallation
	configErr    error
}

func (r *captureIntegrationRepo) GetByID(context.Context, int64) (*PluginInstallation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	copy := *r.installation
	copy.Bindings = append([]PluginBinding(nil), r.installation.Bindings...)
	return &copy, nil
}
func (r *captureIntegrationRepo) BeginEnable(_ context.Context, _ int64, sha, state string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.installation.BinarySHA256 != sha || r.installation.State != state {
		return ErrPluginStateChanged
	}
	r.installation.State = PluginStateStarting
	return nil
}
func (r *captureIntegrationRepo) UpdateBindingsAndState(_ context.Context, _ int64, bindings []PluginBinding, state, _ string, _ *time.Time, expected, sha string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.installation.BinarySHA256 != sha || (expected != "" && r.installation.State != expected) {
		return ErrPluginStateChanged
	}
	r.installation.State = state
	r.installation.Bindings = append([]PluginBinding(nil), bindings...)
	return nil
}
func (r *captureIntegrationRepo) UpdateState(_ context.Context, _ int64, state, _ string, _ *time.Time, _, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.installation.State = state
	return nil
}
func (r *captureIntegrationRepo) MarkRuntimeHealthy(context.Context, int64, string, string) error {
	return nil
}

func (r *captureIntegrationRepo) UpdateConfig(_ context.Context, _ int64, encrypted, sha string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.configErr != nil {
		return r.configErr
	}
	if r.installation.BinarySHA256 != sha {
		return ErrPluginStateChanged
	}
	r.installation.ConfigEncrypted = encrypted
	return nil
}

// Runs a real signed native plugin process. The database boundary is an isolated fixture.
func TestCaptureSignedPluginUploadEnableSaveDownloadRestart(t *testing.T) {
	packagePath := os.Getenv("SUB2API_TEST_CAPTURE_PACKAGE")
	if packagePath == "" {
		t.Skip("set SUB2API_TEST_CAPTURE_PACKAGE to a signed native package")
	}
	root, err := os.MkdirTemp("/tmp", "capture-plugin-")
	require.NoError(t, err)
	defer func() { require.NoError(t, os.RemoveAll(root)) }()
	cfg := testPluginConfig(root, false)
	manager := NewPluginManager(nil, pluginTokenEncryptor{}, cfg, PluginHostInfo{Version: "0.2.20+r17.6", BuildType: "release"})
	file, err := os.Open(packagePath)
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()
	installation, err := manager.installer.Install(context.Background(), file, nil)
	require.NoError(t, err)
	require.Equal(t, PluginSignatureTrusted, installation.SignatureStatus)
	installation.ID = 7
	installation.Bindings = []PluginBinding{{PluginID: 7, Capability: PluginCapabilityCaptureRecords, Platform: "*", AccountType: "*", RolloutPercent: 100}}
	repo := &captureIntegrationRepo{installation: installation}
	manager.repo = repo
	require.NoError(t, os.MkdirAll(filepath.Join(manager.installer.RootDir(), "runtime"), 0700))
	// An existing outbound slot must survive collector enable/disable/re-enable.
	outbound := &pluginRoute{pluginID: 99, rolloutPercent: 100}
	manager.route.Store(outbound)
	ctx, cancel := context.WithCancel(context.Background())
	manager.captureDone = make(chan struct{})
	manager.reconcileCancel = cancel
	manager.started = true
	go manager.captureLoop(ctx, manager.captureDone)
	t.Cleanup(manager.Stop)
	enabled, err := manager.Enable(context.Background(), 7, true, 100)
	require.NoError(t, err)
	require.True(t, enabled.RuntimeHealthy)
	require.Same(t, outbound, manager.route.Load())
	stateDir, err := pluginEvidenceDir(filepath.Join(manager.installer.RootDir(), "runtime"), installation.PluginKey)
	require.NoError(t, err)
	require.DirExists(t, filepath.Join(stateDir, "archive"))
	date := time.Now().UTC().Format("2006-01-02")
	forward := func(groupID int64, model string) {
		c := captureForwardContext("/v1/responses", nil)
		c.Set("api_key", &APIKey{GroupID: &groupID})
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Model, model))
		captureCtx, finish := manager.beginCapture(context.Background(), c, &Account{ID: 101, Platform: PlatformOpenAI})
		request, err := http.NewRequestWithContext(captureCtx, "POST", "http://local.test/v1/responses", strings.NewReader(`{"model":"upstream-model","input":"hello"}`))
		require.NoError(t, err)
		response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"status":"completed","output":"hello"}`))}
		captureHTTPResponse(request, response, nil)
		_, err = io.ReadAll(response.Body)
		require.NoError(t, err)
		finish(true)
		require.Eventually(t, func() bool { return manager.captureQueuedBytes.Load() == 0 }, 5*time.Second, 10*time.Millisecond)
	}
	// Empty selection must not capture; saving uses the real plugin's validation and apply RPCs.
	forward(42, "client-model")
	records, err := manager.ListCaptureRecords(context.Background(), 7, date)
	require.NoError(t, err)
	require.Empty(t, records)
	config, err := manager.SaveConfig(context.Background(), 7, json.RawMessage(`{"group_ids":[42],"models":["client-model"]}`))
	require.NoError(t, err)
	require.Contains(t, string(config), `"group_ids":[42]`)
	for _, selection := range []struct {
		group int64
		model string
	}{{99, "client-model"}, {42, "upstream-model"}, {0, "client-model"}, {42, ""}} {
		forward(selection.group, selection.model)
	}
	records, err = manager.ListCaptureRecords(context.Background(), 7, date)
	require.NoError(t, err)
	require.Empty(t, records)
	forward(42, "client-model")
	require.Eventually(t, func() bool {
		records, err = manager.ListCaptureRecords(context.Background(), 7, date)
		return err == nil && len(records) == 1
	}, 5*time.Second, 20*time.Millisecond)
	data, err := manager.DownloadCaptureRecord(context.Background(), 7, date, records[0].InstanceId, records[0].RecordId)
	require.NoError(t, err)
	require.Contains(t, string(data), `\"output\":\"hello\"`)
	var record map[string]any
	require.NoError(t, json.Unmarshal(data, &record))
	require.EqualValues(t, 42, record["group_id"])
	require.Equal(t, "client-model", record["requested_model"])
	require.Equal(t, "upstream-model", record["model"])
	_, err = manager.DownloadCaptureRecord(context.Background(), 7, "../../etc", "gateway", "passwd")
	require.Error(t, err)
	_, err = manager.Disable(context.Background(), 7)
	require.NoError(t, err)
	require.Equal(t, PluginStateDisabled, repo.installation.State)
	records, err = manager.ListCaptureRecords(context.Background(), 7, date)
	require.NoError(t, err)
	require.Len(t, records, 1)
	data, err = manager.DownloadCaptureRecord(context.Background(), 7, date, records[0].InstanceId, records[0].RecordId)
	require.NoError(t, err)
	require.Contains(t, string(data), `\"output\":\"hello\"`)
	require.Nil(t, manager.captureRuntime.Load())
	require.NotContains(t, manager.runtimes, int64(7))
	readRuntime := manager.archiveReadRuntime
	require.NotNil(t, readRuntime)
	forward(42, "client-model")
	records, err = manager.ListCaptureRecords(context.Background(), 7, date)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Same(t, readRuntime, manager.archiveReadRuntime)
	installation, err = repo.GetByID(context.Background(), 7)
	require.NoError(t, err)
	require.Equal(t, PluginStateDisabled, installation.State)
	require.False(t, hasEnabledCaptureBinding(installation.Bindings))
	require.Same(t, outbound, manager.route.Load())
	// Hold an actual manager download across enable, then finish through the old native process.
	paused := &pausingCaptureDownloadClient{TransportPluginClient: readRuntime.api, entered: make(chan struct{}), resume: make(chan struct{})}
	readRuntime.api = paused
	resumeDownload := sync.OnceFunc(func() { close(paused.resume) })
	defer resumeDownload()
	downloadCtx, cancelDownload := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelDownload()
	var downloaded []byte
	var downloadErr error
	downloadDone := make(chan struct{})
	go func() {
		downloaded, downloadErr = manager.DownloadCaptureRecord(downloadCtx, 7, date, records[0].InstanceId, records[0].RecordId)
		close(downloadDone)
	}()
	select {
	case <-paused.entered:
	case <-downloadCtx.Done():
		t.Fatal(downloadCtx.Err())
	}
	enableDone := make(chan error, 1)
	go func() {
		_, enableErr := manager.Enable(context.Background(), 7, true, 100)
		enableDone <- enableErr
	}()
	require.Eventually(t, func() bool { return readRuntime.draining.Load() || readRuntime.client.Exited() }, 5*time.Second, 10*time.Millisecond)
	require.False(t, readRuntime.client.Exited(), "enable must not kill an in-flight archive download")
	locked := manager.mu.TryLock()
	if locked {
		manager.mu.Unlock()
	}
	require.True(t, locked, "draining downloads must not hold the manager lock")
	resumeDownload()
	<-downloadDone
	require.NoError(t, downloadErr)
	require.Equal(t, data, downloaded)
	require.NoError(t, <-enableDone)
	require.Nil(t, manager.archiveReadRuntime)
	require.True(t, readRuntime.client.Exited())
	// A failed rollback must stop the native collector and recover only its persisted selection.
	failedRuntime := manager.captureRuntime.Load()
	failedRuntime.api = &captureConfigRollbackClient{TransportPluginClient: failedRuntime.api, rejectRollback: true}
	previousConfig, err := manager.GetConfig(t.Context(), 7)
	require.NoError(t, err)
	repo.mu.Lock()
	repo.configErr = errors.New("database unavailable")
	repo.mu.Unlock()
	_, err = manager.SaveConfig(t.Context(), 7, json.RawMessage(`{"group_ids":[99],"models":["client-model"]}`))
	require.ErrorContains(t, err, "database unavailable")
	require.ErrorContains(t, err, "rollback rejected")
	require.True(t, failedRuntime.client.Exited())
	require.Nil(t, manager.captureRuntime.Load())
	require.Same(t, outbound, manager.route.Load())
	savedConfig, err := manager.GetConfig(t.Context(), 7)
	require.NoError(t, err)
	require.JSONEq(t, string(previousConfig), string(savedConfig))
	repo.mu.Lock()
	repo.configErr = nil
	repo.mu.Unlock()
	installation, err = repo.GetByID(t.Context(), 7)
	require.NoError(t, err)
	require.NoError(t, manager.reconcileCaptureRuntime(t.Context(), installation))
	require.NotNil(t, manager.captureRuntime.Load())
	require.NotSame(t, failedRuntime, manager.captureRuntime.Load())
	forward(99, "client-model")
	records, err = manager.ListCaptureRecords(context.Background(), 7, date)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Same(t, outbound, manager.route.Load())
	_, err = manager.Disable(context.Background(), 7)
	require.NoError(t, err)
	_, err = manager.ListCaptureRecords(context.Background(), 7, date)
	require.NoError(t, err)
	readRuntime = manager.archiveReadRuntime
	manager.Stop()
	require.True(t, readRuntime.client.Exited())
	require.Nil(t, manager.archiveReadRuntime)
}
