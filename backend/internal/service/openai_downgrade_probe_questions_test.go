package service

import (
	"math/rand/v2"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAIDowngradeProbeNextQuestionCoversAllDomains(t *testing.T) {
	seen := make(map[string]int)
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 800; i++ {
		q := openAIDowngradeProbeNextQuestion(now)
		require.NotEmpty(t, q.Text)
		require.NotNil(t, q.AnswerPattern)
		require.NotEmpty(t, q.AnswerDisplay)
		require.Contains(t, q.Text, "工具") // 引导句：不使用工具直接作答
		seen[q.Domain]++
	}
	// 均匀抽取的宽松分布下限：2 个生成器 → 2 个域，各期望 400。
	require.Len(t, seen, 2)
	for domain, count := range seen {
		require.GreaterOrEqualf(t, count, 200, "domain %s underrepresented: %d/800", domain, count)
	}
}

func TestOpenAIDowngradeTwoDimCandyAnchorsAreClean(t *testing.T) {
	// 糖果锚点数字回归：承诺策略与博弈真值都必须给出生产行为锚定的 21，
	// 即糖果本身是「教科书干净」实例，生成器的干净性筛选与其一致。
	candyAnchors := &openAIDowngradeTwoDimSpec{
		hiddenVals: [3]string{"a", "b", "decoy"},
		counts:     [2][3]int{{7, 9, 8}, {7, 6, 4}},
	}
	require.Equal(t, 21, openAIDowngradeTwoDimCommit(candyAnchors))
	require.Equal(t, 21, openAIDowngradeTwoDimGame(candyAnchors))
}

func TestOpenAIDowngradeTwoDimGameNeverExceedsCommit(t *testing.T) {
	// 已知歧义实例：自适应续摸改进承诺策略（12 < 13），生成器必须滤掉。
	ambiguous := &openAIDowngradeTwoDimSpec{
		counts: [2][3]int{{3, 1, 5}, {2, 3, 3}},
	}
	require.Equal(t, 13, openAIDowngradeTwoDimCommit(ambiguous))
	require.Equal(t, 12, openAIDowngradeTwoDimGame(ambiguous))

	// 随机小规格：博弈最优 ≤ 承诺代价（承诺是合法策略，不可能更差）。
	for i := 0; i < 200; i++ {
		spec := &openAIDowngradeTwoDimSpec{}
		for s := range spec.counts {
			for h := range spec.counts[s] {
				spec.counts[s][h] = 1 + rand.IntN(5)
			}
		}
		require.LessOrEqualf(t,
			openAIDowngradeTwoDimGame(spec), openAIDowngradeTwoDimCommit(spec),
			"counts=%v", spec.counts)
	}
}

func TestOpenAIDowngradeTwoDimQuestionBounds(t *testing.T) {
	generated := 0
	for i := 0; i < 300; i++ {
		q := openAIDowngradeTwoDimQuestion(time.Time{})
		if q.Domain != "two_dim" {
			continue
		}
		generated++
		answer, err := strconv.Atoi(q.AnswerDisplay)
		require.NoError(t, err)
		require.GreaterOrEqual(t, answer, 10)
		require.Contains(t, q.Text, "靠手感可以分辨")
		require.Contains(t, q.Text, "最少取出多少个")
		require.True(t, q.AnswerPattern.MatchString("答案是"+q.AnswerDisplay))
		require.False(t, q.AnswerPattern.MatchString("答案是"+q.AnswerDisplay+"0"))
	}
	// 干净实例接受率下限：过低说明 300 次尝试会频繁兜底退回糖果题，
	// 多题域退化回单题域。
	require.GreaterOrEqual(t, generated, 150)
}

func TestOpenAIDowngradeNumericAnswerPatternBoundaries(t *testing.T) {
	p21 := openAIDowngradeNumericAnswerPattern(21)
	require.True(t, p21.MatchString("答案是21"))
	require.True(t, p21.MatchString("21"))
	require.True(t, p21.MatchString("（21）"))
	require.False(t, p21.MatchString("答案是210"))
	require.False(t, p21.MatchString("答案是121"))
	require.False(t, p21.MatchString("答案是5213"))

	p4digit := openAIDowngradeNumericAnswerPattern(2357)
	require.True(t, p4digit.MatchString("这个数是2357"))
	require.False(t, p4digit.MatchString("这个数是12357"))
}
