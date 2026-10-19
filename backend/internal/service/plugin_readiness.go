package service

import (
	"context"
	"errors"
)

// Ready checks the authoritative binding and the actual serving runtime, not
// merely whether the host's HTTP listener or plugin process is alive.
func (m *PluginManager) Ready(ctx context.Context) error {
	if m == nil {
		return errors.New("plugin manager unavailable")
	}
	// Configuration writes and reconciliation own runtime.installation.
	if !m.operationMu.TryLock() {
		return errors.New("plugin operation in progress")
	}
	defer m.operationMu.Unlock()
	m.mu.Lock()
	started := m.started
	m.mu.Unlock()
	if !started {
		return errors.New("plugin manager not started")
	}
	installations, err := m.repo.List(ctx)
	if err != nil {
		return err
	}
	var enabled *PluginInstallation
	for _, installation := range installations {
		if hasEnabledOpenAIBinding(installation.Bindings) {
			if enabled != nil {
				return errors.New("ambiguous plugin binding")
			}
			enabled = installation
		}
	}
	route := m.route.Load()
	if enabled == nil {
		if route != nil {
			return errors.New("plugin binding not reconciled")
		}
		return nil
	}
	if enabled.State != PluginStateEnabled || route == nil || route.pluginID != enabled.ID || route.runtime == nil {
		return errors.New("enabled plugin unavailable")
	}
	runtime := route.runtime
	if runtime.installation == nil || runtime.installation.BinarySHA256 != enabled.BinarySHA256 ||
		runtime.installation.ConfigEncrypted != enabled.ConfigEncrypted ||
		route.rolloutPercent != bindingRollout(enabled.Bindings) || runtime.draining.Load() || runtime.healthUnavailable.Load() {
		return errors.New("plugin runtime not ready")
	}
	if err := runtime.checkHealth(ctx); err != nil {
		return err
	}
	if m.route.Load() != route {
		return errors.New("plugin runtime changed during check")
	}
	return nil
}
