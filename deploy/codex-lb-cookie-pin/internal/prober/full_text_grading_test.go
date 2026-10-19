package prober

import "testing"

func TestCanaryFullTextNumericGrading(t *testing.T) {
	q := candyQuestion()
	for _, tc := range []struct {
		text    string
		correct bool
	}{
		{"21", true},
		{"推导中用到21，但结论是22。\nFINAL_ANSWER: 22", true},
		{"不是21", true},
		{"无法确定答案", false},
		{"121", false},
		{"22", false},
	} {
		if got := q.Grade(tc.text); got != tc.correct {
			t.Errorf("Grade(%q) = %v, want %v", tc.text, got, tc.correct)
		}
	}
}
