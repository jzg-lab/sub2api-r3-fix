package service

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func appendManualQualityAnswer(run *manualQualityRun, text string) error {
	if len(run.result.Answer)+len(text) > manualQualityMaxAnswer {
		return errors.New("answer exceeded the 256 KiB storage limit")
	}
	run.result.Answer += text
	return nil
}

func mergeManualQualityUsage(run *manualQualityRun, usage gjson.Result) {
	if !usage.IsObject() {
		return
	}
	if run.result.Usage == nil {
		run.result.Usage = map[string]any{}
	}
	// Only numeric counters are persisted; arbitrary upstream metadata is not.
	usage.ForEach(func(key, value gjson.Result) bool {
		if value.Type == gjson.Number {
			run.result.Usage[key.String()] = value.Int()
		}
		if value.IsObject() {
			value.ForEach(func(k, v gjson.Result) bool {
				if v.Type == gjson.Number {
					run.result.Usage[key.String()+"."+k.String()] = v.Int()
				}
				return true
			})
		}
		return true
	})
}

// Connectivity parsers intentionally accept some loose responses. A quality
// result requires a complete final answer, not a bare EOF or partial reasoning.
func processManualQualityStream(c *gin.Context, body io.Reader, protocol string) error {
	run := manualQuality(c)
	scanner := bufio.NewScanner(io.LimitReader(body, 2<<20))
	scanner.Buffer(make([]byte, 4096), 512<<10)
	dataLines := []string{}
	finished := false
	consume := func(raw string) error {
		if raw == "[DONE]" {
			return nil
		}
		if !gjson.Valid(raw) {
			return errors.New("invalid upstream SSE JSON")
		}
		data := gjson.Parse(raw)
		if data.Get("error").Exists() || data.Get("type").String() == "error" {
			return errors.New("upstream returned an error event")
		}
		switch protocol {
		case "responses":
			switch data.Get("type").String() {
			case "response.output_text.delta":
				return appendManualQualityAnswer(run, data.Get("delta").String())
			case "response.completed", "response.done":
				status := data.Get("response.status").String()
				if status != "" && status != "completed" {
					return errors.New("upstream response was not completed")
				}
				mergeManualQualityUsage(run, data.Get("response.usage"))
				if run.result.Answer == "" {
					for _, item := range data.Get("response.output").Array() {
						if item.Get("type").String() != "message" {
							continue
						}
						for _, part := range item.Get("content").Array() {
							if part.Get("type").String() == "output_text" {
								if err := appendManualQualityAnswer(run, part.Get("text").String()); err != nil {
									return err
								}
							}
						}
					}
				}
				finished = true
			case "response.failed", "response.incomplete":
				return errors.New("upstream response failed or was truncated")
			}
		case "chat_completions":
			mergeManualQualityUsage(run, data.Get("usage"))
			for _, choice := range data.Get("choices").Array() {
				if choice.Get("index").Int() != 0 {
					continue
				}
				if err := appendManualQualityAnswer(run, choice.Get("delta.content").String()); err != nil {
					return err
				}
				reason := choice.Get("finish_reason").String()
				if reason != "" && reason != "stop" {
					return errors.New("upstream stopped without a complete text answer: " + reason)
				}
				if reason == "stop" {
					finished = true
				}
			}
		case "anthropic":
			mergeManualQualityUsage(run, data.Get("message.usage"))
			mergeManualQualityUsage(run, data.Get("usage"))
			switch data.Get("type").String() {
			case "content_block_delta":
				if err := appendManualQualityAnswer(run, data.Get("delta.text").String()); err != nil {
					return err
				}
			case "message_delta":
				reason := data.Get("delta.stop_reason").String()
				if reason != "" && reason != "end_turn" && reason != "stop_sequence" {
					return errors.New("upstream stopped without a complete text answer: " + reason)
				}
			case "message_stop":
				finished = true
			}
		case "gemini":
			if data.Get("response").Exists() {
				data = data.Get("response")
			}
			mergeManualQualityUsage(run, data.Get("usageMetadata"))
			for _, candidate := range data.Get("candidates").Array() {
				if candidate.Get("index").Int() != 0 {
					continue
				}
				for _, part := range candidate.Get("content.parts").Array() {
					if !part.Get("thought").Bool() {
						if err := appendManualQualityAnswer(run, part.Get("text").String()); err != nil {
							return err
						}
					}
				}
				reason := candidate.Get("finishReason").String()
				if reason != "" && reason != "STOP" {
					return errors.New("upstream stopped without a complete text answer: " + reason)
				}
				if reason == "STOP" {
					finished = true
				}
			}
		}
		return nil
	}
	for scanner.Scan() {
		if err := c.Request.Context().Err(); err != nil {
			return err
		}
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if len(dataLines) == 0 {
				continue
			}
			raw := strings.Join(dataLines, "\n")
			dataLines = nil
			if err := consume(raw); err != nil {
				return err
			}
			// Chat usage may arrive after finish_reason, before [DONE].
			if finished && (protocol != "chat_completions" || raw == "[DONE]") {
				run.result.Status = "completed"
				return nil
			}
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read upstream stream: %w", err)
	}
	if len(dataLines) > 0 {
		if err := consume(strings.Join(dataLines, "\n")); err != nil {
			return err
		}
	}
	if !finished {
		return errors.New("stream ended before a completion event")
	}
	run.result.Status = "completed"
	return nil
}

func processManualQualityBedrock(c *gin.Context, body []byte) error {
	if !json.Valid(body) {
		return errors.New("invalid upstream JSON")
	}
	run := manualQuality(c)
	data := gjson.ParseBytes(body)
	if reason := data.Get("stop_reason").String(); reason != "end_turn" && reason != "stop_sequence" {
		return errors.New("upstream did not complete the answer")
	}
	for _, part := range data.Get("content").Array() {
		if part.Get("type").String() == "text" {
			if err := appendManualQualityAnswer(run, part.Get("text").String()); err != nil {
				return err
			}
		}
	}
	mergeManualQualityUsage(run, data.Get("usage"))
	run.result.Status = "completed"
	return nil
}
