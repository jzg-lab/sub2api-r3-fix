//go:build unit

package service

import (
	"fmt"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

func mappingReadAccount() *Account {
	return &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping":              map[string]any{"gpt-6-astra": "gpt-6-astra"},
			credKeyHeaderOverrideEnabled: true,
			credKeyHeaderOverrides:       map[string]any{"X-Test": "original"},
		},
	}
}

func TestAccountSnapshotConcurrentReadersAndCopies(t *testing.T) {
	account := mappingReadAccount()
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 100 {
				require.Equal(t, "gpt-6-astra", account.GetMappedModel("gpt-6-astra"))
				require.Equal(t, "original", account.GetHeaderOverrides()["x-test"])
				copy := *account
				require.Equal(t, "gpt-6-astra", copy.GetMappedModel("gpt-6-astra"))
				require.Equal(t, "original", copy.GetHeaderOverrides()["x-test"])
			}
		})
	}
	wg.Wait()
}

func TestAccountSnapshotResultsDoNotPoisonLaterReads(t *testing.T) {
	account := mappingReadAccount()
	account.GetModelMapping()["gpt-6-astra"] = "wrong-model"
	account.GetHeaderOverrides()["x-test"] = "wrong-header"
	require.Equal(t, "gpt-6-astra", account.GetMappedModel("gpt-6-astra"))
	require.Equal(t, "original", account.GetHeaderOverrides()["x-test"])
}

func TestAccountSnapshotDefaultMappingIsolation(t *testing.T) {
	for _, credentials := range []map[string]any{
		nil,
		{},
		{"model_mapping": map[string]any{"invalid": false}},
	} {
		account := &Account{Platform: domain.PlatformAntigravity, Credentials: credentials}
		got := account.GetModelMapping()
		require.NotEmpty(t, got)
		for key, expected := range got {
			got[key] = "wrong-model"
			require.Equal(t, expected, account.GetModelMapping()[key])
			break
		}
	}
}

func TestAccountSnapshotSequentialUpdatesAndCopyIsolation(t *testing.T) {
	account := mappingReadAccount()
	require.Equal(t, "original", account.GetHeaderOverrides()["x-test"])
	copy := *account
	copy.Credentials = map[string]any{
		"model_mapping":              map[string]any{"other": "other"},
		credKeyHeaderOverrideEnabled: true,
		credKeyHeaderOverrides:       map[string]any{"X-Test": "copy"},
	}
	require.False(t, copy.IsModelSupported("gpt-6-astra"))
	require.Equal(t, "copy", copy.GetHeaderOverrides()["x-test"])
	require.True(t, account.IsModelSupported("gpt-6-astra"))
	require.Equal(t, "original", account.GetHeaderOverrides()["x-test"])

	account.Credentials[credKeyHeaderOverrides].(map[string]any)["X-Test"] = "updated"
	require.Equal(t, "updated", account.GetHeaderOverrides()["x-test"])
	account.Credentials[credKeyHeaderOverrideEnabled] = false
	require.Nil(t, account.GetHeaderOverrides())
	account.Credentials = nil
	require.Nil(t, account.GetModelMapping())
	require.Nil(t, account.GetHeaderOverrides())
}

func BenchmarkAccountSnapshotModelMapping(b *testing.B) {
	for _, size := range []int{0, 8, 64} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			raw := make(map[string]any, size)
			for i := range size {
				model := fmt.Sprintf("model-%d", i)
				raw[model] = model
			}
			account := &Account{Credentials: map[string]any{"model_mapping": raw}}
			b.ReportAllocs()
			for b.Loop() {
				account.GetModelMapping()
			}
		})
	}
}
