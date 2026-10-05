package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/require"
)

func TestOpenAIDowngradeProbeArchivesExactlyOneFinalOutcome(t *testing.T) {
	for _, name := range []string{
		"transport_failure", "nil_response", "http_failure", "success",
		"retry_transport_failure", "retry_body_failure", "retry_success",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("SUB2API_PROBE_ARCHIVE_DIR", dir)
			retry := strings.HasPrefix(name, "retry_")
			firstBody := &downgradeProbeResponseBody{Reader: strings.NewReader("stream unsupported")}
			lastBody := &downgradeProbeResponseBody{Reader: strings.NewReader(
				"event: response.completed\ndata: {\"output\":[{\"content\":[{\"text\":\"21\"}]}],\"usage\":{\"reasoning_tokens\":1992}}\n\n",
			)}
			status := http.StatusOK
			switch name {
			case "http_failure":
				status = http.StatusServiceUnavailable
			case "retry_body_failure":
				status = http.StatusTooManyRequests
				lastBody.Reader = iotest.ErrReader(io.ErrUnexpectedEOF)
			}
			calls := 0
			runner, account := newDowngradeProbeHTTPTestRunner(&downgradeProbeHTTPUpstream{
				doProbe: func() (*http.Response, error) {
					calls++
					if retry && calls == 1 {
						return &http.Response{StatusCode: http.StatusBadRequest, Body: firstBody}, nil
					}
					if name == "nil_response" {
						return nil, nil
					}
					response := &http.Response{StatusCode: status, Body: lastBody}
					if name == "transport_failure" || name == "retry_transport_failure" {
						return response, fmt.Errorf("private-network-marker: %w", context.DeadlineExceeded)
					}
					return response, nil
				},
			})

			result := runner.probe(context.Background(), account, "normal")

			wantCalls := 1
			if retry {
				wantCalls = 2
				require.Equal(t, 1, firstBody.closeCalls)
			}
			require.Equal(t, wantCalls, calls)
			if name != "nil_response" {
				require.Equal(t, 1, lastBody.closeCalls)
			}
			files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
			require.NoError(t, err)
			require.Len(t, files, 1)
			data, err := os.ReadFile(files[0])
			require.NoError(t, err)
			require.Len(t, bytes.Split(bytes.TrimSpace(data), []byte("\n")), 1)
			require.NotContains(t, string(data), "private-network-marker")
			require.NotContains(t, string(data), "stream unsupported")

			var archived openAIDowngradeProbeArchiveEntry
			require.NoError(t, json.Unmarshal(data, &archived))
			require.Equal(t, account.ID, archived.AccountID)
			require.Equal(t, "normal", archived.Mode)
			require.Equal(t, result.HTTPStatus, archived.HTTPStatus)
			require.Equal(t, result.ErrorMessage, archived.ErrorMessage)
			require.Equal(t, result.TransportOK, archived.TransportOK)
			require.Equal(t, result.AnswerVerdict(), archived.AnswerCorrect)
			require.Equal(t, result.ReasoningTokens, archived.ReasoningTokens)
			require.Equal(t, result.Latency.Milliseconds(), archived.LatencyMS)
			switch name {
			case "transport_failure", "retry_transport_failure", "nil_response", "retry_body_failure":
				require.Empty(t, archived.ResponseTail)
				require.False(t, archived.TransportOK)
				require.NotEmpty(t, archived.ErrorMessage)
			default:
				require.NotEmpty(t, archived.ResponseTail)
			}
		})
	}
}
