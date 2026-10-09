package service

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Stage only the prefix so all text adapters share one deadline without changing their wire conversion.
func (s *OpenAIGatewayService) doOpenAITextUpstream(ctx context.Context, c *gin.Context, req *http.Request, proxy string, account *Account, body []byte, start time.Time, clientStream bool) (*http.Response, error) {
	upstream, release := withUpstreamDrain(ctx, req.Context(), openAIUsageDrainTimeout)
	model := gjson.GetBytes(body, "model").String()
	effort := ""
	if value := extractOpenAIReasoningEffortFromBody(body, model); value != nil {
		effort = *value
	}
	timeout := time.Duration(0)
	if account.Platform == PlatformOpenAI && !IsImageGenerationIntent(req.URL.Path, model, body) {
		timeout = s.openAIFirstOutputTimeout(effort)
	}
	var headerGuard *openAIFirstOutputHeaderGuard
	if timeout > 0 {
		upstream, headerGuard = newOpenAIFirstOutputHeaderGuard(upstream, release, start.Add(timeout))
	}
	cleanup := release
	if headerGuard != nil {
		cleanup = headerGuard.close
	}
	upstreamStart := time.Now()
	resp, err := s.doOpenAIUpstream(ctx, req.WithContext(upstream), proxy, account)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
	account = openAIResponseAccount(resp, account)
	if headerGuard != nil && headerGuard.stopHeaderWait() {
		cleanup()
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, s.newOpenAIFirstOutputTimeoutError(ctx, c, account, start, model, effort, timeout, "response_headers", nil)
	}
	if err != nil || resp == nil || resp.Body == nil {
		cleanup()
		return resp, err
	}
	resp.Body = &openAIRequestContextReadCloser{ReadCloser: resp.Body, cleanup: cleanup}
	if timeout <= 0 || resp.StatusCode >= 400 {
		return resp, nil
	}

	deadline := newOpenAIReadDeadline(resp.Body, start.Add(timeout))
	defer deadline.stop()
	upstreamStream := isEventStreamResponse(resp.Header) || (gjson.GetBytes(body, "stream").Bool() && !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "application/json"))
	stopKeepalive := func() {}
	if clientStream && upstreamStream && s.cfg != nil && s.cfg.Gateway.StreamKeepaliveInterval > 0 {
		if c.GetBool("openai_passthrough") {
			writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
			if extractOpenAICodexTurnState(resp.Header) != "" {
				s.noteOpenAICodexTurnStateProvenance(c, account)
			}
		} else if s.responseHeaderFilter != nil {
			responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
		}
		stopKeepalive = startOpenAISSEKeepalive(c, time.Duration(s.cfg.Gateway.StreamKeepaliveInterval)*time.Second)
	}
	defer stopKeepalive()
	if upstreamStream {
		err = stageOpenAITextPrefix(resp)
	} else {
		var data []byte
		data, err = readUpstreamResponseBodyLimited(resp.Body, resolveUpstreamResponseReadLimit(s.cfg))
		if err == nil {
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(data))
		}
	}
	if !deadline.stop() {
		_ = resp.Body.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, s.newOpenAIFirstOutputTimeoutError(ctx, c, account, start, model, effort, timeout, "semantic_output", resp.Header)
	}
	if err != nil {
		_ = resp.Body.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		failure := s.newOpenAIStreamFailoverErrorWithModel(c, account, c.GetBool("openai_passthrough"), resp.Header.Get("x-request-id"), nil, "Upstream body failed before output: "+err.Error(), model, resp.Header)
		return nil, failure
	}
	return resp, nil
}

func stageOpenAITextPrefix(resp *http.Response) (err error) {
	original := resp.Body
	stage := newDefaultOpenAIFirstOutputStage()
	defer func() {
		if err != nil {
			_ = stage.Close()
		}
	}()
	reader := bufio.NewReaderSize(original, 64*1024)
	var line bytes.Buffer
	var parser openAICompatSSEFrameParser
	for {
		fragment, readErr := reader.ReadSlice('\n')
		if _, err = stage.Write(fragment); err != nil {
			return err
		}
		_, _ = line.Write(fragment)
		if errors.Is(readErr, bufio.ErrBufferFull) {
			continue
		}
		frame, complete := parser.AddLine(strings.TrimRight(line.String(), "\r\n"))
		line.Reset()
		if (complete && openAITextFrameStartsOutput(frame.Data, frame.EventType)) || errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	var prefix io.Reader = bytes.NewReader(stage.memory.Bytes())
	if stage.tempFile != nil {
		prefix = io.NewSectionReader(stage.tempFile, 0, stage.size)
	}
	resp.Body = &openAIStagedTextBody{Reader: io.MultiReader(prefix, reader), original: original, stage: stage}
	return nil
}

type openAIStagedTextBody struct {
	io.Reader
	original io.ReadCloser
	stage    *openAIFirstOutputStage
}

func (b *openAIStagedTextBody) Close() error {
	return errors.Join(b.original.Close(), b.stage.Close())
}

func openAITextFrameStartsOutput(data, eventType string) bool {
	if strings.TrimSpace(data) == "[DONE]" {
		return true
	}
	if strings.TrimSpace(data) == "" {
		return false
	}
	payload := gjson.Parse(data)
	if !gjson.Valid(data) {
		return true
	} // Let the existing parser report malformed upstream data.
	eventType = firstNonEmpty(payload.Get("type").String(), eventType)
	if eventType == "error" || openAIStreamEventTypeIsTerminal(eventType) {
		return true
	}
	if choices := payload.Get("choices"); choices.Exists() {
		for _, choice := range choices.Array() {
			if strings.TrimSpace(choice.Get("finish_reason").String()) != "" {
				return true
			}
		}
		return openAIChatDataHasOutput(data)
	}
	return openAIStreamDataStartsClientOutput(data, eventType)
}

func openAIChatDataHasOutput(data string) bool {
	for _, choice := range gjson.Get(data, "choices").Array() {
		for _, field := range []string{"delta.content", "delta.reasoning_content", "delta.refusal"} {
			if strings.TrimSpace(choice.Get(field).String()) != "" {
				return true
			}
		}
		if len(choice.Get("delta.tool_calls").Array()) > 0 || choice.Get("delta.function_call").IsObject() {
			return true
		}
	}
	return false
}

func readOpenAIResponseBodyThroughTerminal(resp *http.Response, limit int64) ([]byte, error) {
	if !isEventStreamResponse(resp.Header) {
		return readUpstreamResponseBodyLimited(resp.Body, limit)
	}
	reader := bufio.NewReader(io.LimitReader(resp.Body, limit+1))
	var out, line bytes.Buffer
	var parser openAICompatSSEFrameParser
	for {
		fragment, err := reader.ReadSlice('\n')
		if int64(out.Len()+len(fragment)) > limit {
			return nil, ErrUpstreamResponseBodyTooLarge
		}
		_, _ = out.Write(fragment)
		_, _ = line.Write(fragment)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		frame, complete := parser.AddLine(strings.TrimRight(line.String(), "\r\n"))
		line.Reset()
		if complete {
			eventType := firstNonEmpty(gjson.Get(frame.Data, "type").String(), frame.EventType)
			if strings.TrimSpace(frame.Data) == "[DONE]" || (eventType != "error" && openAIStreamEventTypeIsTerminal(eventType)) {
				return out.Bytes(), nil
			}
		}
		if errors.Is(err, io.EOF) {
			return out.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
	}
}
