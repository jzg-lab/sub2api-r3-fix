package service

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/stretchr/testify/require"
)

type captureIntegrationRepo struct {
	PluginRepository
	mu           sync.Mutex
	installation *PluginInstallation
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
	captureCtx, finish := manager.beginCapture(context.Background(), nil, &Account{ID: 101, Platform: PlatformOpenAI})
	request, _ := http.NewRequestWithContext(captureCtx, "POST", "http://local.test/v1/responses", strings.NewReader(`{"model":"local-fixture","input":"hello"}`))
	response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"status":"completed","output":"hello"}`))}
	captureHTTPResponse(request, response, nil)
	_, err = io.ReadAll(response.Body)
	require.NoError(t, err)
	finish(true)
	date := time.Now().UTC().Format("2006-01-02")
	var records []*pluginv1.CaptureRecordInfo
	require.Eventually(t, func() bool {
		records, err = manager.ListCaptureRecords(context.Background(), 7, date)
		return err == nil && len(records) == 1
	}, 5*time.Second, 20*time.Millisecond)
	data, err := manager.DownloadCaptureRecord(context.Background(), 7, date, records[0].InstanceId, records[0].RecordId)
	require.NoError(t, err)
	require.Contains(t, string(data), `\"output\":\"hello\"`)
	_, err = manager.DownloadCaptureRecord(context.Background(), 7, "../../etc", "gateway", "passwd")
	require.Error(t, err)
	_, err = manager.Disable(context.Background(), 7)
	require.NoError(t, err)
	require.Same(t, outbound, manager.route.Load())
	_, err = manager.Enable(context.Background(), 7, true, 100)
	require.NoError(t, err)
	records, err = manager.ListCaptureRecords(context.Background(), 7, date)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Same(t, outbound, manager.route.Load())
}
