package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type cyberRuntimeSettingRepo struct {
	fakeSettingRepo
	getValue    func(context.Context, string) (string, error)
	getAll      func(context.Context) (map[string]string, error)
	setMultiple func(context.Context, map[string]string) error
}

func (r *cyberRuntimeSettingRepo) GetValue(ctx context.Context, key string) (string, error) {
	// These fixtures isolate session protection; local allowlist behavior has separate regressions.
	if key == SettingKeyCyberPolicyUserAllowlist {
		return "", ErrSettingNotFound
	}
	return r.getValue(ctx, key)
}

func (r *cyberRuntimeSettingRepo) GetAll(ctx context.Context) (map[string]string, error) {
	return r.getAll(ctx)
}

func (r *cyberRuntimeSettingRepo) SetMultiple(ctx context.Context, values map[string]string) error {
	return r.setMultiple(ctx, values)
}

func TestCyberSessionBlockRuntimeSettings(t *testing.T) {
	for _, tt := range []struct {
		name      string
		values    map[string]string
		failedKey string
		wantOn    bool
		wantTTL   time.Duration
		wantErr   bool
		wantReads int32
	}{
		{name: "absent policy defaults off", wantTTL: time.Hour, wantReads: 1},
		{name: "explicit off skips TTL", values: map[string]string{SettingKeyCyberSessionBlockEnabled: "false"}, failedKey: SettingKeyCyberSessionBlockTTLSeconds, wantTTL: time.Hour, wantReads: 1},
		{name: "enabled default TTL", values: map[string]string{SettingKeyCyberSessionBlockEnabled: "true"}, wantOn: true, wantTTL: time.Hour, wantReads: 2},
		{name: "enabled configured TTL", values: map[string]string{SettingKeyCyberSessionBlockEnabled: " true ", SettingKeyCyberSessionBlockTTLSeconds: "60"}, wantOn: true, wantTTL: time.Minute, wantReads: 2},
		{name: "policy unavailable", failedKey: SettingKeyCyberSessionBlockEnabled, wantErr: true, wantReads: 1},
		{name: "invalid policy", values: map[string]string{SettingKeyCyberSessionBlockEnabled: "invalid"}, wantErr: true, wantReads: 1},
		{name: "empty policy", values: map[string]string{SettingKeyCyberSessionBlockEnabled: ""}, wantErr: true, wantReads: 1},
		{name: "TTL unavailable", values: map[string]string{SettingKeyCyberSessionBlockEnabled: "true"}, failedKey: SettingKeyCyberSessionBlockTTLSeconds, wantErr: true, wantReads: 2},
		{name: "invalid TTL", values: map[string]string{SettingKeyCyberSessionBlockEnabled: "true", SettingKeyCyberSessionBlockTTLSeconds: "bad"}, wantErr: true, wantReads: 2},
		{name: "zero TTL", values: map[string]string{SettingKeyCyberSessionBlockEnabled: "true", SettingKeyCyberSessionBlockTTLSeconds: "0"}, wantErr: true, wantReads: 2},
		{name: "negative TTL", values: map[string]string{SettingKeyCyberSessionBlockEnabled: "true", SettingKeyCyberSessionBlockTTLSeconds: "-1"}, wantErr: true, wantReads: 2},
		{name: "overflow TTL", values: map[string]string{SettingKeyCyberSessionBlockEnabled: "true", SettingKeyCyberSessionBlockTTLSeconds: "9223372036854775807"}, wantErr: true, wantReads: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var reads atomic.Int32
			svc := &SettingService{settingRepo: &cyberRuntimeSettingRepo{
				getValue: func(_ context.Context, key string) (string, error) {
					reads.Add(1)
					if key == tt.failedKey {
						return "", errors.New("private database diagnostic")
					}
					value, ok := tt.values[key]
					if !ok {
						return "", ErrSettingNotFound
					}
					return value, nil
				},
			}}
			for i := 0; i < 2; i++ {
				on, ttl, err := svc.GetCyberSessionBlockRuntime(context.Background())
				if tt.wantErr {
					require.ErrorIs(t, err, ErrCyberSessionProtectionUnavailable)
					require.NotContains(t, err.Error(), "private")
				} else {
					require.NoError(t, err)
					require.Equal(t, tt.wantOn, on)
					require.Equal(t, tt.wantTTL, ttl)
				}
			}
			require.Equal(t, tt.wantReads, reads.Load(), "both successes and failures must be cached")
		})
	}
}

func TestCyberSessionBlockRuntimeRefreshDoesNotDisableProtection(t *testing.T) {
	var fail atomic.Bool
	var reads atomic.Int32
	svc := &SettingService{settingRepo: &cyberRuntimeSettingRepo{
		getValue: func(_ context.Context, key string) (string, error) {
			reads.Add(1)
			if fail.Load() {
				return "", errors.New("settings offline")
			}
			if key == SettingKeyCyberSessionBlockEnabled {
				return "true", nil
			}
			return "60", nil
		},
	}}
	on, ttl, err := svc.GetCyberSessionBlockRuntime(context.Background())
	require.NoError(t, err)
	require.True(t, on)
	require.Equal(t, time.Minute, ttl)

	fail.Store(true)
	svc.cyberSessionBlockRuntimeCache.Store(&cachedCyberSessionBlockRuntime{enabled: true, ttl: time.Minute})
	_, _, err = svc.GetCyberSessionBlockRuntime(context.Background())
	require.ErrorIs(t, err, ErrCyberSessionProtectionUnavailable)
	cached := svc.cyberSessionBlockRuntimeCache.Load().(*cachedCyberSessionBlockRuntime)
	require.ErrorIs(t, cached.err, ErrCyberSessionProtectionUnavailable)
	require.LessOrEqual(t, time.Until(time.Unix(0, cached.expiresAt)), cyberSessionBlockRuntimeErrorTTL)

	fail.Store(false)
	svc.cyberSessionBlockRuntimeCache.Store(&cachedCyberSessionBlockRuntime{})
	on, ttl, err = svc.GetCyberSessionBlockRuntime(context.Background())
	require.NoError(t, err)
	require.True(t, on)
	require.Equal(t, time.Minute, ttl)
	require.EqualValues(t, 5, reads.Load())
}

func TestCyberSessionBlockRuntimeCallerCancellationDoesNotCancelSharedRefresh(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var reads atomic.Int32
	svc := &SettingService{settingRepo: &cyberRuntimeSettingRepo{
		getValue: func(ctx context.Context, key string) (string, error) {
			reads.Add(1)
			if key != SettingKeyCyberSessionBlockEnabled {
				return "60", nil
			}
			close(started)
			select {
			case <-release:
				return "true", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan error, 1)
	go func() {
		_, _, err := svc.GetCyberSessionBlockRuntime(ctx)
		first <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("shared settings refresh did not start")
	}
	cancel()
	select {
	case err := <-first:
		require.ErrorIs(t, err, ErrCyberSessionProtectionUnavailable)
	case <-time.After(time.Second):
		t.Fatal("caller waited for the shared database timeout")
	}
	close(release)
	nextCtx, nextCancel := context.WithTimeout(context.Background(), time.Second)
	defer nextCancel()
	on, ttl, err := svc.GetCyberSessionBlockRuntime(nextCtx)
	require.NoError(t, err)
	require.True(t, on)
	require.Equal(t, time.Minute, ttl)
	require.EqualValues(t, 2, reads.Load())
}

func TestCyberSessionBlockRuntimeMissingRepository(t *testing.T) {
	for _, svc := range []*SettingService{nil, {}} {
		_, _, err := svc.GetCyberSessionBlockRuntime(context.Background())
		require.ErrorIs(t, err, ErrCyberSessionProtectionUnavailable)
	}
}

func TestCyberSessionBlockRuntimeSettingsSaveInvalidates(t *testing.T) {
	for _, tt := range []struct {
		name      string
		partial   bool
		writeFail bool
	}{
		{name: "full save"},
		{name: "partial save even when settings reload fails", partial: true},
		{name: "failed save preserves cached policy", writeFail: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var enabled atomic.Bool
			var reads atomic.Int32
			repo := &cyberRuntimeSettingRepo{
				getValue: func(_ context.Context, key string) (string, error) {
					reads.Add(1)
					if key == SettingKeyCyberSessionBlockEnabled && !enabled.Load() {
						return "false", nil
					}
					if key == SettingKeyCyberSessionBlockEnabled {
						return "true", nil
					}
					return "60", nil
				},
				setMultiple: func(_ context.Context, values map[string]string) error {
					if tt.writeFail {
						return errors.New("write unavailable")
					}
					enabled.Store(values[SettingKeyCyberSessionBlockEnabled] == "true")
					return nil
				},
				getAll: func(context.Context) (map[string]string, error) {
					return nil, errors.New("reload unavailable")
				},
			}
			svc := NewSettingService(repo, &config.Config{})
			on, _, err := svc.GetCyberSessionBlockRuntime(context.Background())
			require.NoError(t, err)
			require.False(t, on)
			var omitted OmittedSettingKeys
			if tt.partial {
				omitted = OmittedSettingKeys{SettingKeyCompactHomeEnabled: {}}
			}
			err = svc.UpdateSettingsOmitting(context.Background(), &SystemSettings{
				CyberSessionBlockEnabled: true, CyberSessionBlockTTLSeconds: 60,
			}, omitted)
			if tt.writeFail {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			on, ttl, err := svc.GetCyberSessionBlockRuntime(context.Background())
			require.NoError(t, err)
			require.Equal(t, !tt.writeFail, on)
			if tt.writeFail {
				require.EqualValues(t, 1, reads.Load())
			} else {
				require.Equal(t, time.Minute, ttl)
				require.EqualValues(t, 3, reads.Load())
			}
		})
	}
}

func TestCyberSessionBlockRuntimeLateRefreshCannotUndoInvalidation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var reads atomic.Int32
	svc := &SettingService{settingRepo: &cyberRuntimeSettingRepo{
		getValue: func(ctx context.Context, key string) (string, error) {
			if reads.Add(1) == 1 {
				close(started)
				select {
				case <-release:
					return "false", nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			if key == SettingKeyCyberSessionBlockEnabled {
				return "true", nil
			}
			return "60", nil
		},
	}}
	first := make(chan error, 1)
	go func() {
		_, _, err := svc.GetCyberSessionBlockRuntime(context.Background())
		first <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("old policy read did not start")
	}
	svc.invalidateCyberSessionBlockRuntime()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	on, ttl, err := svc.GetCyberSessionBlockRuntime(ctx)
	require.NoError(t, err, "new generation must not join the old refresh")
	require.True(t, on)
	require.Equal(t, time.Minute, ttl)
	close(release)
	select {
	case err := <-first:
		require.ErrorIs(t, err, ErrCyberSessionProtectionUnavailable)
	case <-time.After(time.Second):
		t.Fatal("old refresh did not terminate")
	}
	on, _, err = svc.GetCyberSessionBlockRuntime(context.Background())
	require.NoError(t, err)
	require.True(t, on, "late refresh must not publish disabled policy")
	require.EqualValues(t, 3, reads.Load(), "late refresh must not evict the new cached policy")
}

func TestCyberSessionBlockRuntimeSettingsSaveRejectsTTLOverflow(t *testing.T) {
	seconds := int64((1<<63-1)/time.Second) + 1
	if int64(int(seconds)) != seconds {
		t.Skip("integer cannot represent an overflowing duration on this platform")
	}
	svc := NewSettingService(&cyberRuntimeSettingRepo{}, &config.Config{})
	err := svc.UpdateSettings(context.Background(), &SystemSettings{
		CyberSessionBlockEnabled: true, CyberSessionBlockTTLSeconds: int(seconds),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "TTL")
}

func TestFindCyberSessionBlockedForRequestSettingsFailureRejects(t *testing.T) {
	combo := &comboCacheAndStore{}
	svc := &OpenAIGatewayService{
		cache: combo,
		settingService: &SettingService{settingRepo: &cyberRuntimeSettingRepo{
			getValue: func(context.Context, string) (string, error) {
				return "", errors.New("settings offline")
			},
		}},
	}
	c, body := newCyberBlockTestCtx(map[string]string{"session_id": "session"}, `{}`)
	key, err := svc.FindCyberSessionBlockedForRequest(context.Background(), 1, c, body, "", "")
	require.Empty(t, key)
	require.ErrorIs(t, err, ErrCyberSessionProtectionUnavailable)
	require.Zero(t, combo.store.findCalls)
	require.Zero(t, combo.store.scopeCalls)
	require.ErrorIs(t, svc.MarkCyberSessionBlocked(context.Background(), "", []string{"key"}), ErrCyberSessionProtectionUnavailable)
	require.Empty(t, combo.store.blocked)
}

func TestFindCyberSessionBlockedForRequestSettingsDeadline(t *testing.T) {
	drained := make(chan struct{})
	svc := &OpenAIGatewayService{settingService: &SettingService{settingRepo: &cyberRuntimeSettingRepo{
		getValue: func(ctx context.Context, _ string) (string, error) {
			defer close(drained)
			<-ctx.Done()
			return "", ctx.Err()
		},
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := svc.FindCyberSessionBlockedForRequest(ctx, 1, nil, nil, "", "")
	require.ErrorIs(t, err, ErrCyberSessionProtectionUnavailable)
	require.Less(t, time.Since(start), time.Second)
	select {
	case <-drained:
	case <-time.After(cyberSessionBlockRuntimeDBTimeout + time.Second):
		t.Fatal("shared refresh exceeded its independent deadline")
	}
}
