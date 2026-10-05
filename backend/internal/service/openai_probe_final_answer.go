package service

import (
	"regexp"
	"strings"
)

const openAIProbeFinalAnswerInstruction = "\n\nEnd your response with exactly one line: FINAL_ANSWER: <integer>."

var openAIProbeFinalAnswerLine = regexp.MustCompile(`^FINAL_ANSWER:\s*([0-9]+)$`)
var openAIProbeLegacyAnswerLine = regexp.MustCompile(`^(?:答案[:：是]\s*|最少取出\s*)?([0-9]+)(?:\s*个)?[。.]?$`)

// extractOpenAIProbeFinalAnswer accepts a single explicit final line or an entire short
// answer. Numbers in reasoning, negation, or conflicting final lines are not votes.
func extractOpenAIProbeFinalAnswer(text string) (string, bool) {
	text = strings.TrimSpace(text)
	lines := strings.Split(text, "\n")
	markers := strings.Count(text, "FINAL_ANSWER:")
	if markers > 0 {
		if markers != 1 {
			return "", false
		}
		match := openAIProbeFinalAnswerLine.FindStringSubmatch(strings.TrimSpace(lines[len(lines)-1]))
		if match == nil {
			return "", false
		}
		return match[1], true
	}
	match := openAIProbeLegacyAnswerLine.FindStringSubmatch(text)
	if match == nil {
		return "", false
	}
	return match[1], true
}
