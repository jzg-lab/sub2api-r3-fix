package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const manualQualityContextKey = "manual_quality_test"
const manualQualityTimeout = 10 * time.Minute
const manualQualityMaxAnswer = 256 << 10

// ManualQualityStore keeps diagnostics separate from scheduling/account snapshots.
type ManualQualityStore interface {
	BeginManualQuality(context.Context, *ManualQualityResult) (bool, error)
	FinishManualQuality(context.Context, *ManualQualityResult) error
	GetManualQuality(context.Context, int64, string, string) (*ManualQualityResult, error)
	ListManualQuality(context.Context, []int64) ([]ManualQualityResult, error)
}

type ManualQualityTemplate struct {
	Kind            string `json:"kind"`
	Prompt          string `json:"prompt"`
	ReferenceAnswer string `json:"reference_answer"`
}

type ManualQualityRequest struct {
	ManualQualityTemplate
	Model string `json:"model"`
}

type ManualQualityResult struct {
	ManualQualityRequest
	AccountID     int64          `json:"account_id"`
	RunID         string         `json:"run_id"`
	StartedAt     time.Time      `json:"started_at"`
	DurationMS    int64          `json:"duration_ms"`
	Status        string         `json:"status"`
	Answer        string         `json:"answer"`
	Error         string         `json:"error,omitempty"`
	UpstreamModel string         `json:"upstream_model"`
	Parameters    map[string]any `json:"parameters,omitempty"`
	Usage         map[string]any `json:"usage,omitempty"`
}

type manualQualityRun struct {
	result   *ManualQualityResult
	prepared bool
}

func manualQuality(c *gin.Context) *manualQualityRun {
	v, _ := c.Get(manualQualityContextKey)
	run, _ := v.(*manualQualityRun)
	return run
}

func DefaultManualQualityTemplates() []ManualQualityTemplate {
	return []ManualQualityTemplate{
		{Kind: "candy", Prompt: `在一个黑色的袋子里放有三种口味的糖果，每种糖果有两种不同的形状（圆形和五角星形，不同的形状靠手感可以分辨）。现已知不同口味的糖和不同形状的数量统计如下表。参赛者需要在活动前决定摸出的糖果数目，那么，最少取出多少个糖果才能保证手中同时拥有不同形状的苹果味和桃子味的糖？（同时手中有圆形苹果味匹配五角星桃子味糖果，或者有圆形桃子味匹配五角星苹果味糖果都满足要求）

| 形状 | 苹果味 | 桃子味 | 西瓜味 |
| --- | --- | --- | --- |
| 圆形 | 7 | 9 | 8 |
| 五角星形 | 7 | 6 | 4 |`, ReferenceAnswer: "21"},
		{Kind: "pelican", Prompt: "创建一个HTML，内容是SVG绘制一个鹈鹕骑自行车的2D动画，你不需要任何测试，只靠你自己完成，不要使用任何工具"},
	}
}

func (t ManualQualityTemplate) Validate() error {
	if t.Kind != "candy" && t.Kind != "pelican" {
		return errors.New("unsupported test kind")
	}
	if strings.TrimSpace(t.Prompt) == "" || len(t.Prompt) > 16000 {
		return errors.New("prompt must contain 1–16000 bytes")
	}
	if len(t.ReferenceAnswer) > 1000 {
		return errors.New("reference answer is too long")
	}
	return nil
}

func (s *AccountTestService) ManualQualityTemplates(ctx context.Context) ([]ManualQualityTemplate, error) {
	if s.settingService == nil {
		return nil, errors.New("settings service unavailable")
	}
	templates := DefaultManualQualityTemplates()
	for i := range templates {
		value, err := s.settingService.settingRepo.GetValue(ctx, "manual_quality_template_"+templates[i].Kind)
		if errors.Is(err, ErrSettingNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var saved ManualQualityTemplate
		if err = json.Unmarshal([]byte(value), &saved); err != nil {
			return nil, err
		}
		if err = saved.Validate(); err != nil || saved.Kind != templates[i].Kind {
			return nil, errors.New("invalid saved quality template")
		}
		templates[i] = saved
	}
	return templates, nil
}

func (s *AccountTestService) SaveManualQualityTemplate(ctx context.Context, t ManualQualityTemplate) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if s.settingService == nil {
		return errors.New("settings service unavailable")
	}
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return s.settingService.settingRepo.Set(ctx, "manual_quality_template_"+t.Kind, string(data))
}

func (s *AccountTestService) ManualQualityStore() (ManualQualityStore, error) {
	store, ok := s.accountRepo.(ManualQualityStore)
	if !ok {
		return nil, errors.New("quality test storage unavailable")
	}
	return store, nil
}

func (s *AccountTestService) RunManualQuality(ctx context.Context, accountID int64, input ManualQualityRequest) (*ManualQualityResult, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}
	input.Model = strings.TrimSpace(input.Model)
	if input.Model == "" || len(input.Model) > 200 {
		return nil, errors.New("select a text model (maximum 200 bytes)")
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	mapped := account.GetMappedModel(input.Model)
	if account.IsTypeSafe() || account.IsSyntheticUITest() {
		return nil, errors.New("this account does not support manual text quality tests")
	}
	if isOpenAIImageModel(mapped) || isImageGenerationModel(mapped) || isGrokVideoGenerationModel(mapped) || isGrokImageGenerationModel(mapped) {
		return nil, errors.New("select a text model; SVG is generated as text")
	}
	store, err := s.ManualQualityStore()
	if err != nil {
		return nil, err
	}
	result := &ManualQualityResult{ManualQualityRequest: input, AccountID: accountID, RunID: uuid.NewString(), StartedAt: time.Now().UTC(), Status: "running"}
	acquired, err := store.BeginManualQuality(ctx, result)
	if err != nil {
		return nil, err
	}
	if !acquired {
		return nil, errors.New("this account/model/test is already running")
	}
	runCtx, cancel := context.WithTimeout(ctx, manualQualityTimeout)
	defer cancel()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = (&http.Request{Header: make(http.Header)}).WithContext(runCtx)
	run := &manualQualityRun{result: result}
	c.Set(manualQualityContextKey, run)
	testErr := s.TestAccountConnection(c, accountID, input.Model, input.Prompt, AccountTestModeGrokText)
	result.DurationMS = time.Since(result.StartedAt).Milliseconds()
	if testErr != nil || result.Status != "completed" || !run.prepared || strings.TrimSpace(result.Answer) == "" {
		result.Status = "failed"
		if result.Error == "" {
			result.Error = "No complete text response was received."
		}
	}
	if runCtx.Err() != nil {
		result.Status = "failed"
		result.Error = "Test canceled or timed out; the upstream may still charge for this request."
	}
	// A disconnected browser must not leave a completed result permanently running.
	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer persistCancel()
	if err := store.FinishManualQuality(persistCtx, result); err != nil {
		return result, fmt.Errorf("test finished but saving its result failed: %w", err)
	}
	return result, nil
}

// Called before serialization/signing. It never modifies the automatic probe's
// question bank or infers quality from connectivity success.
func prepareManualQualityPayload(c *gin.Context, payload map[string]any, protocol string) {
	run := manualQuality(c)
	if run == nil {
		return
	}
	prompt := run.result.Prompt
	switch protocol {
	case "responses":
		payload["input"] = []map[string]any{{"role": "user", "content": []map[string]any{{"type": "input_text", "text": prompt}}}}
	case "chat_completions", "anthropic":
		payload["messages"] = []map[string]any{{"role": "user", "content": prompt}}
		if protocol == "anthropic" {
			payload["max_tokens"] = 8192
		}
	case "gemini":
		payload["contents"] = []map[string]any{{"role": "user", "parts": []map[string]any{{"text": prompt}}}}
		generation, _ := payload["generationConfig"].(map[string]any)
		if generation == nil {
			generation = map[string]any{}
		}
		generation["maxOutputTokens"] = 8192
		payload["generationConfig"] = generation
	}
	run.prepared = true
	run.result.Parameters = map[string]any{"protocol": protocol}
	for _, key := range []string{"reasoning", "reasoning_effort", "temperature", "max_tokens", "max_output_tokens", "generationConfig"} {
		if value, ok := payload[key]; ok {
			run.result.Parameters[key] = value
		}
	}
	if model, ok := payload["model"].(string); ok {
		run.result.UpstreamModel = model
	}
}

// Antigravity connectivity tests intentionally send one-token probes. Manual
// questions instead reuse its identity/URL builders with a full text budget,
// without entering the retry/credits/state-recovery loop.
func (s *AccountTestService) testAntigravityManualQuality(c *gin.Context, account *Account, model string) error {
	gateway := s.antigravityGatewayService
	if gateway == nil || gateway.tokenProvider == nil {
		return s.sendErrorAndEnd(c, "Antigravity token provider is unavailable")
	}
	ctx := c.Request.Context()
	token, err := gateway.tokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return s.sendErrorAndEnd(c, "Unable to obtain Antigravity token")
	}
	project, err := resolveAntigravityProjectID(account)
	if err != nil {
		return s.sendErrorAndEnd(c, "Antigravity project is unavailable")
	}
	mapped := gateway.getMappedModel(account, model)
	if mapped == "" || isImageGenerationModel(mapped) {
		return s.sendErrorAndEnd(c, "Select an allowed text model")
	}
	var body []byte
	if shouldUseAntigravityGeminiTestRequest(model, mapped) {
		body, err = gateway.buildGeminiTestRequestWithPrompt(project, mapped, "")
	} else {
		body, err = gateway.buildClaudeTestRequest(project, mapped)
	}
	if err != nil {
		return s.sendErrorAndEnd(c, "Unable to build Antigravity request")
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return err
	}
	request, ok := payload["request"].(map[string]any)
	if !ok {
		return errors.New("invalid Antigravity request")
	}
	prepareManualQualityPayload(c, request, "gemini")
	manualQuality(c).result.UpstreamModel = mapped
	body, err = json.Marshal(payload)
	if err != nil {
		return err
	}
	baseURL, err := s.validateUpstreamBaseURL(resolveAntigravityForwardBaseURL(account))
	if err != nil {
		return s.sendErrorAndEnd(c, "Invalid Antigravity base URL")
	}
	req, err := antigravity.NewAPIRequestWithURL(ctx, baseURL, "streamGenerateContent", token, body)
	if err != nil {
		return s.sendErrorAndEnd(c, "Unable to build Antigravity request")
	}
	proxy := ""
	if account.Proxy != nil && account.ProxyID != nil {
		proxy = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(req, proxy, account.ID, account.Concurrency)
	if err != nil {
		return s.sendErrorAndEnd(c, "Antigravity request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return s.sendErrorAndEnd(c, fmt.Sprintf("API returned %d", resp.StatusCode))
	}
	return processManualQualityStream(c, resp.Body, "gemini")
}

func readAccountTestResponse(c *gin.Context, body io.Reader) ([]byte, error) {
	if manualQuality(c) == nil {
		return io.ReadAll(body)
	}
	data, err := io.ReadAll(io.LimitReader(body, (2<<20)+1))
	if len(data) > 2<<20 {
		return nil, errors.New("upstream response exceeded the size limit")
	}
	return data, err
}
