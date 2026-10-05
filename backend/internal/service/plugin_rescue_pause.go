package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

const rescueCookiePinPluginID = "lyunlong.codex.lb-cookie-pin"

type OpenAIRescueTerminatedAccountLister interface {
	ListOpenAIRescueTerminatedAccountIDs(context.Context) ([]int64, error)
}

// Set before Start. Each instance reads the same durable termination state.
func (m *PluginManager) SetRescueTerminatedAccountSource(source func(context.Context) ([]int64, error)) {
	m.pausedAccountIDsSource = source
}

func (r *pluginRuntime) loadPausedAccountIDs(ctx context.Context) ([]int64, error) {
	if r.pausedAccountIDsSource == nil {
		return nil, nil
	}
	ids, err := r.pausedAccountIDsSource(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取救援终止状态: %w", err)
	}
	ids = slices.Clone(ids)
	for _, id := range ids {
		if id <= 0 {
			return nil, errors.New("invalid terminated rescue account ID")
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

func withPluginProbePauses(raw []byte, ids []int64) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, errors.New("plugin config must be an object")
	}
	delete(fields, "paused_account_ids")
	if len(ids) > 0 {
		encoded, err := json.Marshal(ids)
		if err != nil {
			return nil, err
		}
		fields["paused_account_ids"] = encoded
	}
	return json.Marshal(fields)
}

func withoutPluginDropAction(raw []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	delete(fields, "drop_account_ids")
	return json.Marshal(fields)
}

func (m *PluginManager) syncRuntimeProbePauses(ctx context.Context, route *pluginRoute) error {
	if route == nil || route.runtime == nil || route.runtime.pausedAccountIDsSource == nil {
		return nil
	}
	runtime := route.runtime
	syncCtx, cancel := context.WithTimeout(ctx, pluginHealthTimeout)
	defer cancel()
	ids, err := runtime.loadPausedAccountIDs(syncCtx)
	if err == nil && slices.Equal(ids, runtime.pausedAccountIDs) {
		return nil
	}
	if err == nil {
		err = runtime.validateAndApplyConfig(syncCtx, runtime.probeBaseConfig)
	}
	if err != nil {
		// An unresponsive or older plugin cannot keep probing stopped accounts.
		return errors.Join(err, m.markRuntimeUnavailable(route, "救援探测暂停同步失败: "+err.Error()))
	}
	return nil
}

func (m *PluginManager) SyncRescueProbePauses(ctx context.Context) error {
	if m == nil {
		return nil
	}
	for !m.operationMu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer m.operationMu.Unlock()
	return m.syncRuntimeProbePauses(ctx, m.route.Load())
}

func (l *OpenAIRescueLane) SetPluginProbePauseSync(sync func(context.Context) error) {
	l.pluginProbePauseSync = sync
}

func (l *OpenAIRescueLane) syncPluginProbePauses(ctx context.Context) error {
	if l.pluginProbePauseSync == nil {
		return nil
	}
	syncCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return l.pluginProbePauseSync(syncCtx)
}
