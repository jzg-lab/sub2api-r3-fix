package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type manualQualityTestRepo struct {
	AccountRepository
	account  *Account
	saved    *ManualQualityResult
	begins   int
	finishes int
	busy     bool
}

func (r *manualQualityTestRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}
func (r *manualQualityTestRepo) BeginManualQuality(_ context.Context, result *ManualQualityResult) (bool, error) {
	r.begins++
	return !r.busy, nil
}
func (r *manualQualityTestRepo) FinishManualQuality(_ context.Context, result *ManualQualityResult) error {
	r.finishes++
	copy := *result
	r.saved = &copy
	return nil
}
func (r *manualQualityTestRepo) GetManualQuality(context.Context, int64, string, string) (*ManualQualityResult, error) {
	return r.saved, nil
}
func (r *manualQualityTestRepo) ListManualQuality(context.Context, []int64) ([]ManualQualityResult, error) {
	return nil, nil
}

// Unimplemented mutation methods intentionally panic: a diagnostic must not call them.

func TestManualQualityProtocolRequestsAndStateIsolation(t *testing.T) {
	for _, tc := range []struct{ name, platform, protocol, stream string }{
		{"responses", PlatformOpenAI, APIProtocolResponses, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":5}}}\n\n"},
		{"chat", PlatformOpenAI, APIProtocolChatCompletions, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"21\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":5}}\n\ndata: [DONE]\n\n"},
		{"claude", PlatformAnthropic, APIProtocolAnthropic, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"21\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"},
		{"grok", PlatformGrok, APIProtocolResponses, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":5}}}\n\n"},
		{"adaptive", PlatformZhipu, APIProtocolAdaptive, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"21\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":5}}\n\ndata: [DONE]\n\n"},
		{"cn-anthropic", PlatformZhipu, APIProtocolAnthropic, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"21\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"},
		{"gemini", PlatformGemini, "gemini", "data: {\"candidates\":[{\"content\":{\"parts\":[{\"thought\":true,\"text\":\"hidden\"},{\"text\":\"21\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":5}}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, status := range []int{200, 401, 403, 429} {
				t.Run(http.StatusText(status), func(t *testing.T) {
					account := &Account{ID: 42, Platform: tc.platform, Type: AccountTypeAPIKey,
						Credentials: map[string]any{"api_key": "secret", "base_url": "https://test.invalid", "api_protocol": tc.protocol, "model_mapping": map[string]any{"requested": "mapped"}},
						Extra:       map[string]any{"openai_api_mode": tc.protocol, "api_protocol": tc.protocol}}
					if tc.protocol == APIProtocolChatCompletions {
						account.Extra["openai_responses_mode"] = "force_chat_completions"
					}
					// The Responses selector is an existing per-account extra flag.
					if tc.protocol == APIProtocolResponses {
						account.Extra["openai_responses_mode"] = "force_responses"
					}
					repo := &manualQualityTestRepo{account: account}
					var body map[string]any
					calls := 0
					upstream := &systemOneHTTPUpstream{do: func(req *http.Request) (*http.Response, error) {
						calls++
						require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
						responseBody := tc.stream
						if status != 200 {
							responseBody = `{"error":"secret credential"}`
						}
						return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(responseBody))}, nil
					}}
					svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream, cfg: &config.Config{}}
					input := ManualQualityRequest{Model: "requested", ManualQualityTemplate: ManualQualityTemplate{Kind: "candy", Prompt: "my changed question", ReferenceAnswer: "not sent"}}
					result, err := svc.RunManualQuality(context.Background(), 42, input)
					require.NoError(t, err)
					require.Equal(t, 1, calls)
					encoded, _ := json.Marshal(body)
					require.Contains(t, string(encoded), input.Prompt)
					require.NotContains(t, string(encoded), input.ReferenceAnswer)
					require.Equal(t, 1, repo.finishes)
					if status == 200 {
						require.Equal(t, "completed", result.Status, result.Error)
						require.Equal(t, "21", result.Answer)
						require.Equal(t, "mapped", result.UpstreamModel)
					} else {
						require.Equal(t, "failed", result.Status)
						require.NotContains(t, result.Error, "secret")
					}
				})
			}
		})
	}
}

func TestManualQualityStreamRequiresCompleteAnswer(t *testing.T) {
	cases := []struct {
		name, protocol, stream string
		ok                     bool
	}{
		{"bare EOF", "anthropic", "", false},
		{"partial Claude", "anthropic", "data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"21\"}}\n\n", false},
		{"truncated Claude", "anthropic", "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n", false},
		{"bare DONE", "chat_completions", "data: [DONE]\n\n", false},
		{"length stop", "chat_completions", "data: {\"choices\":[{\"finish_reason\":\"length\"}]}\n\n", false},
		{"incomplete response", "responses", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"incomplete\"}}\n\n", false},
		{"terminal only answer", "responses", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"21\"}]}]}}", true},
		{"blocked Gemini", "gemini", "data: {\"candidates\":[{\"finishReason\":\"SAFETY\"}]}\n\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			run := &manualQualityRun{result: &ManualQualityResult{}}
			c.Set(manualQualityContextKey, run)
			err := processManualQualityStream(c, strings.NewReader(tc.stream), tc.protocol)
			if tc.ok {
				require.NoError(t, err)
				require.Equal(t, "21", run.result.Answer)
			} else {
				require.Error(t, err)
				require.NotEqual(t, "completed", run.result.Status)
			}
		})
	}
}

func TestManualQualityValidationAndBusyDoNotCallUpstream(t *testing.T) {
	repo := &manualQualityTestRepo{account: &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, busy: true}
	svc := &AccountTestService{accountRepo: repo}
	input := ManualQualityRequest{Model: "gpt-test", ManualQualityTemplate: DefaultManualQualityTemplates()[0]}
	_, err := svc.RunManualQuality(context.Background(), 1, input)
	require.ErrorContains(t, err, "already running")
	require.Zero(t, repo.finishes)
	input.Prompt = ""
	_, err = svc.RunManualQuality(context.Background(), 1, input)
	require.Error(t, err)
	require.Equal(t, 1, repo.begins)
}

func TestManualQualityCandyReferenceByExhaustiveCounts(t *testing.T) {
	// Enumerate every draw with no valid pair; test all precommitted shape counts.
	bad := map[[2]int]bool{}
	for a := 0; a <= 7; a++ {
		for p := 0; p <= 9; p++ {
			for w := 0; w <= 8; w++ {
				for x := 0; x <= 7; x++ {
					for y := 0; y <= 6; y++ {
						for z := 0; z <= 4; z++ {
							if (a == 0 || y == 0) && (p == 0 || x == 0) {
								bad[[2]int{a + p + w, x + y + z}] = true
							}
						}
					}
				}
			}
		}
	}
	minimum := 42
	for round := 0; round <= 24; round++ {
		for star := 0; star <= 17; star++ {
			if !bad[[2]int{round, star}] && round+star < minimum {
				minimum = round + star
			}
		}
	}
	require.Equal(t, 21, minimum)
	require.False(t, bad[[2]int{9, 12}])
	require.Equal(t, "21", DefaultManualQualityTemplates()[0].ReferenceAnswer)
}

func TestManualQualityAntigravitySendsFullQuestionOnce(t *testing.T) {
	for _, model := range []string{"gemini-2.5-flash", "claude-sonnet-4-5"} {
		t.Run(model, func(t *testing.T) {
			account := &Account{ID: 43, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "token", "project_id": "project", "model_mapping": map[string]any{model: model}}}
			repo := &manualQualityTestRepo{account: account}
			calls := 0
			upstream := &systemOneHTTPUpstream{do: func(req *http.Request) (*http.Response, error) {
				calls++
				var body map[string]any
				require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
				data, _ := json.Marshal(body)
				require.Contains(t, string(data), "custom pelican")
				request, ok := body["request"].(map[string]any)
				require.True(t, ok)
				generation, ok := request["generationConfig"].(map[string]any)
				require.True(t, ok)
				require.EqualValues(t, 8192, generation["maxOutputTokens"])
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"<svg></svg>\"}]},\"finishReason\":\"STOP\"}]}}\n\n"))}, nil
			}}
			gateway := &AntigravityGatewayService{tokenProvider: &AntigravityTokenProvider{}}
			svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream, antigravityGatewayService: gateway, cfg: &config.Config{}}
			result, err := svc.RunManualQuality(context.Background(), 43, ManualQualityRequest{Model: model, ManualQualityTemplate: ManualQualityTemplate{Kind: "pelican", Prompt: "custom pelican"}})
			require.NoError(t, err)
			require.Equal(t, "completed", result.Status, result.Error)
			require.Equal(t, 1, calls)
			require.Equal(t, model, result.UpstreamModel)
		})
	}
}

type manualQualitySettingRepo struct {
	SettingRepository
	values map[string]string
}

func (r *manualQualitySettingRepo) GetValue(_ context.Context, key string) (string, error) {
	value, ok := r.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}
func (r *manualQualitySettingRepo) Set(_ context.Context, key, value string) error {
	r.values[key] = value
	return nil
}

func TestManualQualityTemplatesPersistSeparatelyFromDefaults(t *testing.T) {
	repo := &manualQualitySettingRepo{values: map[string]string{}}
	svc := &AccountTestService{settingService: &SettingService{settingRepo: repo}}
	templates, err := svc.ManualQualityTemplates(context.Background())
	require.NoError(t, err)
	require.Equal(t, DefaultManualQualityTemplates(), templates)
	changed := templates[0]
	changed.Prompt = "modified candy"
	changed.ReferenceAnswer = ""
	require.NoError(t, svc.SaveManualQualityTemplate(context.Background(), changed))
	other := &AccountTestService{settingService: &SettingService{settingRepo: repo}}
	templates, err = other.ManualQualityTemplates(context.Background())
	require.NoError(t, err)
	require.Equal(t, changed, templates[0])
	require.Equal(t, DefaultManualQualityTemplates()[1], templates[1])
	require.NotEqual(t, "modified candy", DefaultManualQualityTemplates()[0].Prompt)
}
