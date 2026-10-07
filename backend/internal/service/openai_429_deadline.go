package service

import (
	"math"
	"net/http"
	"strconv"
	"time"
)

// All explicit limits are not-before boundaries. A shorter header must not
// override an exhausted quota window or a later error-body reset.
func openAI429ResetTimeAt(headers http.Header, body []byte, now time.Time) *time.Time {
	var latest *time.Time
	consider := func(candidate *time.Time) {
		if candidate != nil && candidate.After(now) && (latest == nil || candidate.After(*latest)) {
			latest = candidate
		}
	}
	consider(calculateOpenAI429ResetTimeAt(headers, now))
	consider(parseRetryAfterResetTime(headers, now))
	if unix := parseOpenAIRateLimitResetTimeAt(body, now); unix != nil {
		reset := time.Unix(*unix, 0)
		consider(&reset)
	}
	return latest
}

func rateLimitResetDuration(seconds float64) (time.Duration, bool) {
	if seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return 0, false
	}
	const maximum = time.Duration(1<<63 - 1)
	if seconds >= float64(maximum)/float64(time.Second) {
		return maximum, true
	}
	return time.Duration(seconds * float64(time.Second)), true
}

func rateLimitResetNumber(value any) (float64, bool) {
	var number float64
	switch value := value.(type) {
	case float64:
		number = value
	case string:
		var err error
		number, err = strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	return number, number >= 0 && !math.IsNaN(number) && !math.IsInf(number, 0)
}
