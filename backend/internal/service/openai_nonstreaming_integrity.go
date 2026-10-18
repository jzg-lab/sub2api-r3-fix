package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/tidwall/gjson"
)

type openAIResponsesSSECompletion struct {
	eventType string
	payload   []byte
	response  []byte
}

// Inspect the whole buffered stream before selecting its response. Selecting
// the first answer and the last terminal independently can mix two executions.
func inspectOpenAIResponsesSSECompletion(body string) (openAIResponsesSSECompletion, error) {
	var result openAIResponsesSSECompletion
	var integrityErr error
	var responseID, terminalState string
	var terminalResponse []byte
	sawResponses, sawLegacyChat, malformed := false, false, false

	forEachOpenAISSEFrame(body, func(eventType string, data []byte) {
		if integrityErr != nil {
			return
		}
		isResponses := strings.HasPrefix(eventType, "response.")
		sawResponses = sawResponses || isResponses
		if !gjson.ValidBytes(data) {
			malformed = malformed || isResponses || eventType == ""
			return
		}
		sawLegacyChat = sawLegacyChat || gjson.GetBytes(data, "choices").IsArray()
		if isResponses {
			for _, field := range []string{"response.id", "response_id"} {
				id := gjson.GetBytes(data, field).String()
				if id == "" {
					continue
				}
				if responseID != "" && responseID != id {
					integrityErr = errors.New("Upstream Responses stream contains conflicting response identities")
					return
				}
				responseID = id
			}
		}
		if terminalState != "" && (responsesStreamEventMayContributeToOutput(eventType) ||
			eventType == "response.output_item.done" || eventType == "response.created" || eventType == "response.in_progress") {
			integrityErr = errors.New("Upstream Responses stream contains output after its terminal response")
			return
		}

		var state string
		switch eventType {
		case "response.completed", "response.done":
			state = "completed"
		case "response.failed":
			state = "failed"
		case "response.incomplete":
			state = "incomplete"
		case "response.cancelled", "response.canceled":
			state = "cancelled"
		case "error":
			// Compatible providers may report a recoverable bare error before
			// the final response. Preserve the existing last-terminal rule.
			if terminalState != "" {
				integrityErr = errors.New("Upstream Responses stream contains conflicting terminal responses")
				return
			}
			result = openAIResponsesSSECompletion{eventType: eventType, payload: data}
			return
		default:
			return
		}

		response := gjson.GetBytes(data, "response")
		if !response.IsObject() && state != "failed" {
			integrityErr = errors.New("Upstream Responses terminal is missing a response object")
			return
		}
		status := strings.TrimSpace(response.Get("status").String())
		if status == "canceled" {
			status = "cancelled"
		}
		// Older compatible providers omit status. An explicit contradictory
		// status must not be relabeled as completed.
		if status != "" && status != state {
			integrityErr = errors.New("Upstream Responses terminal status conflicts with its event")
			return
		}
		responseBytes := []byte(response.Raw)
		if terminalState != "" &&
			(terminalState != state || !equivalentOpenAIResponseJSON(terminalResponse, responseBytes)) {
			integrityErr = errors.New("Upstream Responses stream contains conflicting terminal responses")
			return
		}
		terminalState, terminalResponse = state, responseBytes
		result = openAIResponsesSSECompletion{eventType: eventType, payload: data}
		if state != "failed" {
			normalized, _ := normalizeCompletedImageGenerationStatus(data)
			result.response = []byte(gjson.GetBytes(normalized, "response").Raw)
		}
	})

	if integrityErr != nil {
		return openAIResponsesSSECompletion{}, integrityErr
	}
	if sawResponses && malformed {
		return openAIResponsesSSECompletion{}, errors.New("Upstream Responses stream contains malformed event data")
	}
	if result.eventType == "" && (sawResponses || !sawLegacyChat) {
		return openAIResponsesSSECompletion{}, errors.New("Upstream Responses stream is incomplete: missing terminal event")
	}
	return result, nil
}

func equivalentOpenAIResponseJSON(left, right []byte) bool {
	if bytes.Equal(left, right) {
		return true
	}
	// Only duplicate terminals need structural comparison. UseNumber avoids
	// rounding distinct large usage counts or identifiers into the same value.
	var leftValue, rightValue any
	leftDecoder := json.NewDecoder(bytes.NewReader(left))
	leftDecoder.UseNumber()
	rightDecoder := json.NewDecoder(bytes.NewReader(right))
	rightDecoder.UseNumber()
	return leftDecoder.Decode(&leftValue) == nil &&
		rightDecoder.Decode(&rightValue) == nil &&
		reflect.DeepEqual(leftValue, rightValue)
}
