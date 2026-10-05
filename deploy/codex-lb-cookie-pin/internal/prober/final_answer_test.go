package prober

import "testing"

func TestExtractFinalAnswer(t *testing.T) {
	cases := []struct {
		text, want string
		known      bool
	}{
		{"21", "21", true}, {"最少取出 21 个", "21", true}, {"答案：21", "21", true},
		{"推导中用到21，但结论是22。\nFINAL_ANSWER: 22", "22", true},
		{"不能保证21，因此取22。", "", false},
		{"21或22", "", false}, {"21.5", "", false}, {"-21", "", false},
		{"推导里出现21，但没有结论", "", false},
		{"FINAL_ANSWER: 21\nFINAL_ANSWER: 22", "", false},
		{"FINAL_ANSWER: 21\n结论还不确定", "", false},
		{"推导21\nFINAL_ANSWER: 21\n", "21", true},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			got, known := ExtractFinalAnswer(tc.text)
			if got != tc.want || known != tc.known {
				t.Fatalf("got (%q,%v), want (%q,%v)", got, known, tc.want, tc.known)
			}
		})
	}
}
