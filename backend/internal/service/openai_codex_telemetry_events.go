package service

// Codex 分析事件构造：thread 初始化、turn 事件、hook 与工具事件。
// 事件身份字段全部由遥测 profile（= 最终出站请求头）派生。

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

type openAICodexAnalyticsEvent struct {
	EventType   string         `json:"event_type"`
	EventParams map[string]any `json:"event_params"`
	client      openAICodexTelemetryIdentity
}

type openAICodexThreadSpec struct {
	threadID       string
	sessionID      string
	parentThreadID any
	source         string
	subagentSource any
	model          string
	ephemeral      bool
}

type openAICodexTurnSpec struct {
	threadID string
	turnID   string
	model    string
	effort   string
}

type openAICodexToolSpec struct {
	terminal []byte
	itemID   string
	duration int64
	status   string
}

func newOpenAICodexAnalyticsEvent(profile openAICodexTelemetryProfile, eventType string, params map[string]any) openAICodexAnalyticsEvent {
	return openAICodexAnalyticsEvent{EventType: eventType, EventParams: params, client: profile.client}
}

// openAICodexInitializationEvents 生成用户、guardian 与标题线程的初始化事件。
func openAICodexInitializationEvents(profile openAICodexTelemetryProfile) []openAICodexAnalyticsEvent {
	events := make([]openAICodexAnalyticsEvent, 0, 4)
	if profile.firstThread {
		events = append(events, openAICodexThreadInitialized(profile, openAICodexThreadSpec{
			threadID: profile.threadID, sessionID: profile.sessionID, source: "user", model: profile.model,
		}))
	}
	guardianID := uuid.NewString()
	events = append(events, openAICodexThreadInitialized(profile, openAICodexThreadSpec{
		threadID: guardianID, sessionID: profile.sessionID, parentThreadID: profile.threadID,
		source: "guardian_review", subagentSource: "guardian", model: "codex-auto-review",
	}))
	if !profile.firstThread {
		return events
	}
	titleThreadID := uuid.NewString()
	events = append(events, openAICodexThreadInitialized(profile, openAICodexThreadSpec{
		threadID: titleThreadID, sessionID: titleThreadID, source: "thread_title", model: "gpt-5.6-luna", ephemeral: true,
	}))
	events = append(events, openAICodexTitleTurnEvent(profile, titleThreadID))
	return events
}

// openAICodexThreadInitialized 按线程规格生成初始化事件。
func openAICodexThreadInitialized(profile openAICodexTelemetryProfile, spec openAICodexThreadSpec) openAICodexAnalyticsEvent {
	appServer := openAICodexAppServerClient(profile)
	if spec.source == "guardian_review" {
		appServer["rpc_transport"], appServer["experimental_api_enabled"] = "in_process", nil
	}
	params := map[string]any{
		"app_server_client": appServer, "created_at": profile.started.Unix(),
		"ephemeral": spec.ephemeral, "forked_from_thread_id": nil, "initialization_mode": "new",
		"model": spec.model, "parent_thread_id": spec.parentThreadID, "runtime": openAICodexRuntime(profile),
		"session_id": spec.sessionID, "subagent_source": spec.subagentSource, "thread_id": spec.threadID,
		"thread_source": spec.source,
	}
	return newOpenAICodexAnalyticsEvent(profile, "codex_thread_initialized", params)
}

// openAICodexTitleTurnEvent 模拟首次会话的标题生成 turn。
func openAICodexTitleTurnEvent(profile openAICodexTelemetryProfile, threadID string) openAICodexAnalyticsEvent {
	duration := int64(800 + openAICodexSimulatedInt(profile.turnID+":title", 1800))
	started := profile.started.Unix()
	turnID := uuid.NewString()
	titleProfile := profile
	titleProfile.sessionID, titleProfile.threadID, titleProfile.rootTurnID = threadID, threadID, turnID
	titleProfile.dynamicTool, titleProfile.command, titleProfile.fileChange = false, false, false
	params := openAICodexTurnEventBase(titleProfile, openAICodexTurnSpec{threadID, turnID, "gpt-5.6-luna", "low"})
	params["approval_policy"], params["approvals_reviewer"] = "never", "user"
	params["before_first_sampling_ms"], params["sampling_ms"] = duration/2, duration/2
	params["completed_at"], params["duration_ms"] = started+duration/1000, duration
	params["ephemeral"], params["is_first_turn"] = true, true
	params["sandbox_policy"], params["status"] = "read_only", "completed"
	params["thread_source"], params["turn_trigger"] = "thread_title", "thread_title"
	params["reasoning_summary"] = nil
	params["workspace_kind"] = nil
	return newOpenAICodexAnalyticsEvent(profile, "codex_turn_event", params)
}

// openAICodexTerminalEvents 生成工具、hook 与主 turn 的结束事件。
func openAICodexTerminalEvents(profile openAICodexTelemetryProfile, result openAICodexTelemetryTerminal) []openAICodexAnalyticsEvent {
	events := make([]openAICodexAnalyticsEvent, 0, 9)
	if profile.command {
		events = append(events, openAICodexCommandEvent(profile, result.body))
	}
	if profile.dynamicTool {
		events = append(events, openAICodexDynamicToolEvent(profile, result.body))
	}
	if profile.fileChange {
		events = append(events, openAICodexFileChangeEvent(profile, result.body), openAICodexAcceptedLinesEvent(profile))
	}
	for range 4 {
		events = append(events, openAICodexHookEvent(profile, result.status))
	}
	events = append(events, openAICodexMainTurnEvent(profile, result))
	return events
}

// openAICodexMainTurnEvent 使用真实响应状态与用量生成主 turn 事件。
func openAICodexMainTurnEvent(profile openAICodexTelemetryProfile, result openAICodexTelemetryTerminal) openAICodexAnalyticsEvent {
	now := time.Now()
	params := openAICodexTurnEventBase(profile, openAICodexTurnSpec{profile.threadID, profile.turnID, profile.model, profile.effort})
	response := openAICodexTerminalResponse(result.body)
	params["approval_policy"] = firstNonEmptyOpenAIString(profile.turnMeta.Get("approval_policy").String(), "on-request")
	params["approvals_reviewer"], params["completed_at"] = "auto_review", now.Unix()
	params["duration_ms"] = openAICodexMaxInt64(now.Sub(profile.started).Milliseconds(), 0)
	params["before_first_sampling_ms"] = openAICodexElapsedMillis(profile.started, result.firstEvent, now)
	params["sampling_ms"] = openAICodexElapsedMillis(result.firstEvent, now, now)
	params["after_last_sampling_ms"], params["between_sampling_overhead_ms"] = 0, 0
	params["status"], params["service_tier"] = result.status, firstNonEmptyOpenAIString(response.Get("service_tier").String(), profile.serviceTier)
	params["sandbox_policy"] = firstNonEmptyOpenAIString(profile.turnMeta.Get("sandbox").String(), "workspace_write")
	params["explicit_client_interrupt_requested_at_ms"] = nil
	if result.status == "interrupted" {
		params["explicit_client_interrupt_requested_at_ms"] = now.UnixMilli()
	}
	openAICodexSetTurnUsage(params, response)
	if !result.firstToken.IsZero() {
		params["sampling_ms"] = openAICodexMaxInt64(now.Sub(result.firstToken).Milliseconds(), 0)
	}
	return newOpenAICodexAnalyticsEvent(profile, "codex_turn_event", params)
}

// openAICodexTurnEventBase 构造 turn 事件的公共字段。
func openAICodexTurnEventBase(profile openAICodexTelemetryProfile, spec openAICodexTurnSpec) map[string]any {
	dynamicCount, commandCount, fileCount := openAICodexBoolInt(profile.dynamicTool), openAICodexBoolInt(profile.command), openAICodexBoolInt(profile.fileChange)
	return map[string]any{
		"app_server_client": openAICodexAppServerClient(profile), "cache_write_input_tokens": 0,
		"cached_input_tokens": 0, "codex_error_http_status_code": nil, "codex_error_kind": nil,
		"codex_turn_source": nil, "collaboration_mode": "default", "compaction_ms": 0,
		"dynamic_tool_call_count": dynamicCount, "ephemeral": false, "file_change_count": fileCount,
		"guardian_v2_enabled": true, "image_generation_count": 0, "image_preparations": []any{},
		"initialization_mode": "new", "input_tokens": 0, "is_first_turn": profile.firstThread,
		"mcp_tool_call_count": 0, "model": spec.model, "model_provider": "openai", "num_input_images": 0,
		"output_tokens": 0, "parent_thread_id": nil, "personality": "pragmatic",
		"reasoning_effort": spec.effort, "reasoning_output_tokens": 0, "reasoning_summary": "detailed",
		"root_turn_id": profile.rootTurnID, "runtime": openAICodexRuntime(profile), "sampling_request_count": 1,
		"sampling_retry_count": 0, "sandbox_network_access": false, "service_tier": profile.serviceTier,
		"session_id": profile.sessionID, "shell_command_count": commandCount, "started_at": profile.started.Unix(),
		"steer_count": 0, "subagent_source": nil, "subagent_tool_call_count": 0, "submission_type": nil,
		"thread_id": spec.threadID, "thread_source": "user", "tool_blocking_ms": 0,
		"total_tokens": 0, "total_tool_call_count": dynamicCount + fileCount, "turn_error": nil,
		"turn_id": spec.turnID, "turn_trigger": "composer", "web_search_count": 0, "workspace_kind": "projectless",
	}
}

// openAICodexSetTurnUsage 将 Responses 真实用量写入 turn 参数。
func openAICodexSetTurnUsage(params map[string]any, response gjson.Result) {
	usage := response.Get("usage")
	input, output := usage.Get("input_tokens").Int(), usage.Get("output_tokens").Int()
	total := usage.Get("total_tokens").Int()
	if total == 0 {
		total = input + output
	}
	params["input_tokens"], params["output_tokens"], params["total_tokens"] = input, output, total
	params["cached_input_tokens"] = usage.Get("input_tokens_details.cached_tokens").Int()
	params["reasoning_output_tokens"] = usage.Get("output_tokens_details.reasoning_tokens").Int()
}

// openAICodexHookEvent 模拟一次 Stop 或 Interrupt hook 执行。
func openAICodexHookEvent(profile openAICodexTelemetryProfile, status string) openAICodexAnalyticsEvent {
	hookName := "Stop"
	if status != "completed" {
		hookName = "Interrupt"
	}
	params := map[string]any{
		"execution_mode": "sync", "handler_type": "mcp_tool", "hook_name": hookName,
		"hook_source": "plugin", "model_slug": profile.model, "product_client_id": openAICodexClientName(profile),
		"status": "completed", "thread_id": profile.threadID, "turn_id": profile.turnID,
	}
	return newOpenAICodexAnalyticsEvent(profile, "codex_hook_run", params)
}

// openAICodexDynamicToolEvent 模拟一次动态工具调用事件。
func openAICodexDynamicToolEvent(profile openAICodexTelemetryProfile, terminal []byte) openAICodexAnalyticsEvent {
	itemID, duration := uuid.NewString(), int64(200+openAICodexSimulatedInt(profile.turnID+":dynamic", 1800))
	status := "completed"
	if profile.command && openAICodexSimulatedInt(profile.turnID+":command-status", 10) == 0 {
		status = "failed"
	}
	params := openAICodexToolEventBase(profile, openAICodexToolSpec{terminal, itemID, duration, status})
	params["dynamic_tool_name"], params["tool_name"] = "exec", "exec"
	params["success"] = status == "completed"
	for _, key := range []string{"output_audio_item_count", "output_content_item_count", "output_image_item_count", "output_text_item_count"} {
		params[key] = nil
	}
	return newOpenAICodexAnalyticsEvent(profile, "codex_dynamic_tool_call_event", params)
}

// openAICodexCommandEvent 模拟一次 unified exec 命令执行事件。
func openAICodexCommandEvent(profile openAICodexTelemetryProfile, terminal []byte) openAICodexAnalyticsEvent {
	duration := int64(100 + openAICodexSimulatedInt(profile.turnID+":command-duration", 1200))
	failed := openAICodexSimulatedInt(profile.turnID+":command-status", 10) == 0
	status, exitCode, failure := "completed", 0, any(nil)
	if failed {
		status, exitCode, failure = "failed", 1, "tool_error"
	}
	params := openAICodexToolEventBase(profile, openAICodexToolSpec{terminal, uuid.NewString(), duration, status})
	params["cell_id"], params["command_execution_source"] = "1", "unifiedExecStartup"
	params["exit_code"], params["failure_kind"], params["tool_name"] = exitCode, failure, "unified_exec"
	params["plugin_id"] = nil
	params["execution_duration_ms"], params["script_path"] = duration, nil
	openAICodexSetCommandCounts(params, openAICodexSimulatedInt(profile.turnID+":command-kind", 4))
	return newOpenAICodexAnalyticsEvent(profile, "codex_command_execution_event", params)
}

// openAICodexSetCommandCounts 设置模拟命令的动作类型计数。
func openAICodexSetCommandCounts(params map[string]any, kind int) {
	params["command_total_action_count"] = 1
	keys := []string{"command_read_action_count", "command_list_files_action_count", "command_search_action_count", "command_unknown_action_count"}
	for index, key := range keys {
		params[key] = openAICodexBoolInt(index == kind)
	}
}

// openAICodexFileChangeEvent 模拟一次文件修改事件。
func openAICodexFileChangeEvent(profile openAICodexTelemetryProfile, terminal []byte) openAICodexAnalyticsEvent {
	total := 1 + openAICodexSimulatedInt(profile.turnID+":file-total", 3)
	kind := openAICodexSimulatedInt(profile.turnID+":file-kind", 4)
	params := openAICodexToolEventBase(profile, openAICodexToolSpec{terminal, uuid.NewString(), int64(500 + openAICodexSimulatedInt(profile.turnID+":file-duration", 4000)), "completed"})
	keys := []string{"file_add_count", "file_update_count", "file_delete_count", "file_move_count"}
	for index, key := range keys {
		params[key] = total * openAICodexBoolInt(index == kind)
	}
	params["file_change_count"], params["tool_name"] = total, "apply_patch"
	return newOpenAICodexAnalyticsEvent(profile, "codex_file_change_event", params)
}

// openAICodexAcceptedLinesEvent 模拟接受代码行指纹的事件。
func openAICodexAcceptedLinesEvent(profile openAICodexTelemetryProfile) openAICodexAnalyticsEvent {
	params := map[string]any{
		"accepted_added_lines":   1 + openAICodexSimulatedInt(profile.turnID+":added", 120),
		"accepted_deleted_lines": openAICodexSimulatedInt(profile.turnID+":deleted", 24),
		"completed_at":           time.Now().Unix(), "event_type": "codex.accepted_line_fingerprints",
		"line_fingerprints": []any{}, "model_slug": profile.model, "product_surface": "codex",
		"repo_hash": nil, "thread_id": profile.threadID, "turn_id": profile.turnID,
	}
	return newOpenAICodexAnalyticsEvent(profile, "codex_accepted_line_fingerprints", params)
}

// openAICodexToolEventBase 构造工具类事件共享的时序和身份字段。
func openAICodexToolEventBase(profile openAICodexTelemetryProfile, spec openAICodexToolSpec) map[string]any {
	completed := time.Now()
	return map[string]any{
		"app_server_client": openAICodexAppServerClient(profile), "cell_id": spec.itemID, "completed_at_ms": completed.UnixMilli(),
		"duration_ms": spec.duration, "execution_duration_ms": spec.duration, "failure_kind": nil,
		"final_approval_outcome": "unknown", "guardian_review_count": 0, "item_id": spec.itemID,
		"originating_response_id": openAICodexResponseID(spec.terminal), "parent_call_id": nil, "parent_thread_id": nil,
		"requested_additional_permissions": false, "requested_network_access": false,
		"review_count": 0, "root_turn_id": profile.rootTurnID, "runtime": openAICodexRuntime(profile),
		"session_id": profile.sessionID, "started_at_ms": completed.Add(-time.Duration(spec.duration) * time.Millisecond).UnixMilli(),
		"subagent_source": nil, "subsequent_response_id": nil, "terminal_status": spec.status,
		"thread_id": profile.threadID, "thread_source": "user", "turn_id": profile.turnID, "user_review_count": 0,
	}
}

// openAICodexTerminalResponse 从终止事件或非流式响应中提取 response 对象。
func openAICodexTerminalResponse(terminal []byte) gjson.Result {
	root := gjson.ParseBytes(terminal)
	if response := root.Get("response"); response.Exists() {
		return response
	}
	return root
}

// openAICodexResponseID 返回真实响应 ID，缺失时生成同形占位值。
func openAICodexResponseID(terminal []byte) string {
	if value := openAICodexTerminalResponse(terminal).Get("id").String(); value != "" {
		return value
	}
	return "resp_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

// openAICodexAppServerClient 按客户端指纹构造 App Server 身份字段。
func openAICodexAppServerClient(profile openAICodexTelemetryProfile) map[string]any {
	clientName, appVersion, _, _, _ := openAICodexUserAgentParts(profile.client.userAgent, profile.client.version)
	transport := "in_process"
	if strings.EqualFold(clientName, "Codex Desktop") {
		transport = "stdio"
	}
	return map[string]any{"product_client_id": clientName, "client_name": clientName, "client_version": appVersion, "rpc_transport": transport, "experimental_api_enabled": true}
}

// openAICodexRuntime 从客户端指纹生成 Codex runtime 属性。
func openAICodexRuntime(profile openAICodexTelemetryProfile) map[string]any {
	_, _, osName, osVersion, arch := openAICodexUserAgentParts(profile.client.userAgent, profile.client.version)
	runtimeOS := strings.ToLower(osName)
	switch runtimeOS {
	case "mac os":
		runtimeOS = "macos"
	case "ubuntu", "debian", "arch linux":
		runtimeOS = "linux"
	}
	return map[string]any{"codex_rs_version": profile.client.version, "runtime_os": runtimeOS, "runtime_os_version": osVersion, "runtime_arch": arch}
}

// openAICodexClientName 返回遥测使用的产品客户端名称。
func openAICodexClientName(profile openAICodexTelemetryProfile) string {
	name, _, _, _, _ := openAICodexUserAgentParts(profile.client.userAgent, profile.client.version)
	return name
}

// openAICodexUserAgentParts 解析 Codex User-Agent 中的客户端、系统与架构信息。
func openAICodexUserAgentParts(userAgent, fallbackVersion string) (client, appVersion, osName, osVersion, arch string) {
	client = strings.TrimSpace(strings.SplitN(userAgent, "/", 2)[0])
	appVersion = fallbackVersion
	open, close := strings.Index(userAgent, "("), strings.Index(userAgent, ")")
	if open >= 0 && close > open {
		platform := strings.SplitN(userAgent[open+1:close], ";", 2)
		osName, osVersion = splitOpenAICodexOS(strings.TrimSpace(platform[0]))
		if len(platform) == 2 {
			arch = strings.TrimSpace(platform[1])
		}
	}
	if lastOpen := strings.LastIndex(userAgent, "("); lastOpen > open && strings.HasSuffix(userAgent, ")") {
		app := strings.SplitN(userAgent[lastOpen+1:len(userAgent)-1], ";", 2)
		if len(app) == 2 {
			appVersion = strings.TrimSpace(app[1])
		}
	}
	return
}

// splitOpenAICodexOS 将平台描述拆分为系统名称和版本。
func splitOpenAICodexOS(platform string) (string, string) {
	for _, name := range []string{"Mac OS", "Windows", "Ubuntu", "Linux", "Debian", "Arch Linux"} {
		if strings.HasPrefix(platform, name+" ") {
			return name, strings.TrimSpace(strings.TrimPrefix(platform, name))
		}
	}
	parts := strings.SplitN(platform, " ", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return platform, "Unknown"
}

// openAICodexSimulatedInt 从随机 turn 标识稳定派生有界模拟值。
func openAICodexSimulatedInt(seed string, limit int) int {
	if limit <= 1 {
		return 0
	}
	sum := sha256.Sum256([]byte(seed))
	return int(binary.BigEndian.Uint64(sum[:8]) % uint64(limit))
}

func openAICodexBoolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// openAICodexElapsedMillis 计算非负毫秒间隔并处理缺失终点。
func openAICodexElapsedMillis(start, end, fallback time.Time) int64 {
	if end.IsZero() {
		end = fallback
	}
	return openAICodexMaxInt64(end.Sub(start).Milliseconds(), 0)
}
