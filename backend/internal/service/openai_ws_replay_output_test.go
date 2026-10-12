package service

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIWSReplayOutputPreservesCompleteHistory(t *testing.T) {
	var collector openAIWSToolCallReplayCollector
	output := `[
		{"type":"reasoning","id":"rs_1","encrypted_content":"opaque","summary":[]},
		{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Working"}]},
		{"type":"function_call","id":"fc_1","call_id":"call_1","name":"inspect","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":"ready"},
		{"type":"compaction","id":"cmp_1","encrypted_content":"compact-opaque"}
	]`
	for index, item := range gjson.Parse(output).Array() {
		collector.AddEvent("response.output_item.done", []byte(fmt.Sprintf(
			`{"output_index":%d,"item":%s}`, index, item.Raw)))
	}
	collector.AddEvent("response.completed", []byte(`{"response":{"output":`+output+`}}`))
	for _, items := range [][]json.RawMessage{collector.Items(), collector.AllItems()} {
		require.Len(t, items, 5)
		for index, expected := range gjson.Parse(output).Array() {
			require.JSONEq(t, expected.Raw, string(items[index]))
		}
	}
	copy := collector.Items()
	copy[0][0] = '!'
	require.True(t, json.Valid(collector.Items()[0]), "callers must not mutate collector state")
}

func TestOpenAIWSReplayOutputFiltersUnusableReasoningAfterEnrichment(t *testing.T) {
	var collector openAIWSToolCallReplayCollector
	collector.AddEvent("response.output_item.done", []byte(
		`{"output_index":1,"item":{"type":"reasoning","id":"rs_rich","summary":[]}}`))
	collector.AddEvent("response.completed", []byte(`{"response":{"output":[
		{"type":"message","id":"msg_1","role":"assistant","content":[]},
		{"type":"reasoning","id":"rs_rich","encrypted_content":"opaque","summary":[]},
		{"type":"reasoning","id":"rs_missing"},
		{"type":"reasoning","id":"rs_empty","encrypted_content":""},
		{"type":"reasoning","id":"rs_null","encrypted_content":null},
		{"type":"reasoning","id":"rs_wrong","encrypted_content":123},
		{"type":"reasoning","id":"rs_blank","encrypted_content":"  "}
	]}}`))
	// A duplicated late item must not erase the completed encrypted payload.
	collector.AddEvent("response.output_item.done", []byte(
		`{"output_index":1,"item":{"type":"reasoning","id":"rs_rich","summary":[]}}`))
	require.Len(t, collector.Items(), 2)
	require.Equal(t, collector.Items(), collector.AllItems())
	require.Equal(t, "message", gjson.GetBytes(collector.Items()[0], "type").String())
	require.Equal(t, "opaque", gjson.GetBytes(collector.Items()[1], "encrypted_content").String())
}

func TestOpenAIWSReplayOutputOrderAndDistinctToolResults(t *testing.T) {
	var collector openAIWSToolCallReplayCollector
	events := []string{
		`{"output_index":2,"item":{"type":"function_call_output","call_id":"shared","output":"ok"}}`,
		`{"output_index":0,"item":{"type":"reasoning","id":"rs","encrypted_content":"opaque","summary":[]}}`,
		`{"output_index":1,"item":{"type":"function_call","call_id":"shared","name":"inspect","arguments":"{}"}}`,
	}
	for _, event := range events {
		collector.AddEvent("response.output_item.done", []byte(event))
	}
	for _, event := range events {
		collector.AddEvent("response.output_item.done", []byte(event))
	}
	items := collector.Items()
	require.Len(t, items, 3)
	require.Equal(t, "reasoning", gjson.GetBytes(items[0], "type").String())
	require.Equal(t, "function_call", gjson.GetBytes(items[1], "type").String())
	require.Equal(t, "function_call_output", gjson.GetBytes(items[2], "type").String())
}

func TestOpenAIWSReplayOutputFeedsOrdinaryAndFailoverPayloads(t *testing.T) {
	var collector openAIWSToolCallReplayCollector
	collector.AddEvent("response.done", []byte(`{"response":{"output":[
		{"type":"reasoning","id":"rs_1","encrypted_content":"opaque","summary":[]},
		{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Working"}]},
		{"type":"function_call","id":"fc_1","call_id":"call_1","name":"inspect","arguments":"{}"}
	]}}`))
	first := json.RawMessage(`{"role":"user","content":"Inspect"}`)
	next := []byte(`{"type":"response.create","model":"gpt-6-astra","previous_response_id":"resp_1","store":false,"input":[{"type":"function_call_output","call_id":"call_1","output":"ready"}]}`)
	for _, output := range [][]json.RawMessage{collector.Items(), collector.AllItems()} {
		history := append([]json.RawMessage{first}, output...)
		full, exists, err := buildOpenAIWSReplayInputSequence(history, true, next, true)
		require.NoError(t, err)
		require.True(t, exists)
		require.Len(t, full, 5)
		body, safe, err := buildOpenAIWSCurrentTurnRetryPayload(next, full, exists, "gpt-6-astra")
		require.NoError(t, err)
		require.True(t, safe)
		require.False(t, gjson.GetBytes(body, "previous_response_id").Exists())
		require.Equal(t, "opaque", gjson.GetBytes(body, "input.1.encrypted_content").String())
		require.Equal(t, "assistant", gjson.GetBytes(body, "input.2.role").String())
		require.Equal(t, "function_call", gjson.GetBytes(body, "input.3.type").String())
		require.Equal(t, "function_call_output", gjson.GetBytes(body, "input.4.type").String())
		repeated, _, err := buildOpenAIWSReplayInputSequence(history, true, body, true)
		require.NoError(t, err)
		require.Equal(t, full, repeated, "full client history must not be prepended twice")
	}
}

func TestOpenAIWSReplayOutputCompletionEnrichesIdentityWithoutDuplicating(t *testing.T) {
	var collector openAIWSToolCallReplayCollector
	collector.AddEvent("response.output_item.done", []byte(
		`{"output_index":0,"item":{"type":"reasoning","summary":[]}}`))
	collector.AddEvent("response.output_item.done", []byte(
		`{"output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"inspect","arguments":"{}"}}`))
	collector.AddEvent("response.completed", []byte(`{"response":{"output":[
		{"type":"reasoning","id":"rs_1","encrypted_content":"opaque","summary":[]},
		{"type":"function_call","id":"fc_1","call_id":"call_1","name":"inspect","arguments":"{}"}
	]}}`))
	require.Len(t, collector.Items(), 2)
	require.Equal(t, "rs_1", gjson.GetBytes(collector.Items()[0], "id").String())
	require.Equal(t, "fc_1", gjson.GetBytes(collector.Items()[1], "id").String())

	// An invalid repeated index is not authority to conflate distinct items.
	collector.AddEvent("response.output_item.done", []byte(
		`{"output_index":1,"item":{"type":"function_call","id":"fc_other","call_id":"other","name":"inspect","arguments":"{}"}}`))
	require.Len(t, collector.Items(), 3)
}
