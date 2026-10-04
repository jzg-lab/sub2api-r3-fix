package prober

import (
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/pluginconfig"
)

func TestContainsNumberToken(t *testing.T) {
	cases := []struct {
		text string
		want string
		ok   bool
	}{
		{"3", "3", true},
		{" 3 ", "3", true},
		{"答案是3。", "3", true},
		{"\"3\"", "3", true},
		{"434", "434", true},
		{"434.0", "434", true},  // 尾零宽容
		{"434.00", "434", true}, // 尾零宽容
		{"=434。", "434", true},  // 全角标点切词
		{"13", "3", false},      // 13 ≠ 3（r_count 假阳性防线）
		{"30", "3", false},
		{"3.14", "3", false}, // 真小数不等于 3
		{"0.3", "3", false},
		{"3.0", "3", true},     // 3.0 = 3
		{"4340", "434", false}, // 无小数点不做尾零裁剪
		{"314", "434", false},
		{"r 出现 3 次", "3", true},
		{"3.1.4 版本", "3", false}, // 畸形答案拒绝（假阴性比假阳性安全）
		{"没有数字", "3", false},
		{"", "3", false},
	}
	for _, c := range cases {
		if got := ContainsNumberToken(c.text, c.want); got != c.ok {
			t.Errorf("ContainsNumberToken(%q, %q) = %v, want %v", c.text, c.want, got, c.ok)
		}
	}
}

// ---------- v0.3.5 canary 题库（宿主资格针同卷移植） ----------

// TestCandyQuestionAnchors：糖果题锚点恒定——答案恒 21（社区 canary 的经典
// 错答 29 必须判错），题面必含结构关键词，且随机表面真的在变（≥10 个不同
// 题面，防「同一道题」聚类）。
func TestCandyQuestionAnchors(t *testing.T) {
	distinct := map[string]bool{}
	for i := 0; i < 40; i++ {
		q := candyQuestion()
		if q.ID != "candy" {
			t.Fatalf("ID 应为 candy: %q", q.ID)
		}
		if !q.Grade("21") || !q.Grade("最少取出 21 个") {
			t.Fatal("糖果题判分应接受 21")
		}
		if q.Grade("29") || q.Grade("2") || q.Grade("210") {
			t.Fatal("糖果题判分应拒绝经典错答")
		}
		for _, kw := range []string{"糖果", "形状", "口味"} {
			if !strings.Contains(q.Prompt, kw) {
				t.Fatalf("题面缺结构关键词 %s: %q", kw, q.Prompt)
			}
		}
		distinct[q.Prompt] = true
	}
	if len(distinct) < 10 {
		t.Fatalf("随机表面应有足够散布（40 针 ≥10 个不同题面），得 %d", len(distinct))
	}
}

// TestTwoDimCandyAnchorTruth：糖果锚点数表 [[7,9,8],[7,6,4]] 上博弈树真值 =
// 承诺闭式 = 21——移植保真锚点（宿主同款自检，闭式经此数字反推验证）。
func TestTwoDimCandyAnchorTruth(t *testing.T) {
	sp := &twoDimSpec{counts: [2][3]int{{7, 9, 8}, {7, 6, 4}}}
	if got := twoDimGame(sp); got != 21 {
		t.Fatalf("糖果锚点博弈真值应为 21，得 %d", got)
	}
	if got := TwoDimCommitAnswer(sp.counts); got != 21 {
		t.Fatalf("糖果锚点承诺闭式应为 21，得 %d", got)
	}
}

// TestTwoDimGameBoundedByCommit：博弈最优 ≤ 承诺策略代价（承诺是可行策略
// 之一）；生成器只接受等号成立（且 ≥10）的干净实例。
func TestTwoDimGameBoundedByCommit(t *testing.T) {
	clean := 0
	for i := 0; i < 2000; i++ {
		var counts [2][3]int
		for s := range counts {
			for h := range counts[s] {
				counts[s][h] = 4 + rand.Intn(5)
			}
		}
		sp := &twoDimSpec{counts: counts}
		game, commit := twoDimGame(sp), TwoDimCommitAnswer(counts)
		if game > commit {
			t.Fatalf("博弈最优 %d 超过承诺代价 %d（不变量被破坏）", game, commit)
		}
		if game == commit && game >= 10 {
			clean++
		}
	}
	if clean < 100 {
		t.Fatalf("干净实例占比异常偏低（2000 样本仅 %d 个），生成器会退化回糖果", clean)
	}
}

// TestTwoDimQuestionEndToEnd：题面 → 解析数表 → 闭式复算 → 判分自洽，
// 且 ID/结构关键词正确、题面有散布。
func TestTwoDimQuestionEndToEnd(t *testing.T) {
	distinct := map[string]bool{}
	for i := 0; i < 40; i++ {
		q := twoDimQuestion()
		if q.ID != "two_dim" {
			t.Fatalf("ID 应为 two_dim: %q", q.ID)
		}
		for _, kw := range []string{"类别", "才能保证"} {
			if !strings.Contains(q.Prompt, kw) {
				t.Fatalf("题面缺结构关键词 %s: %q", kw, q.Prompt)
			}
		}
		counts, ok := parseTwoDimCounts(q.Prompt)
		if !ok {
			t.Fatalf("题面数表解析失败: %q", q.Prompt)
		}
		answer := TwoDimCommitAnswer(counts)
		if answer < 10 {
			t.Fatalf("生成实例答案应 ≥10，得 %d", answer)
		}
		if !q.Grade(strconv.Itoa(answer)) || !q.Grade("答案："+strconv.Itoa(answer)) {
			t.Fatalf("判分应接受自算答案 %d", answer)
		}
		if q.Grade(strconv.Itoa(answer+1)) || q.Grade(strconv.Itoa(answer-1)) {
			t.Fatalf("判分应拒绝相邻错答（%d 的 ±1）", answer)
		}
		distinct[q.Prompt] = true
	}
	if len(distinct) < 10 {
		t.Fatalf("随机表面应有足够散布（40 针 ≥10 个不同题面），得 %d", len(distinct))
	}
}

// parseTwoDimCounts 从题面提取数表：题尾两行各取行内最后 3 个数字 token
// （行首是流标签——电池表面的「5号的/7号的」标签自带数字，故不能整题取
// 最后 6 个）。行序 = 流序，行内列序 = 目标1/目标2/干扰。
func parseTwoDimCounts(prompt string) ([2][3]int, bool) {
	var counts [2][3]int
	lines := strings.Split(prompt, "\n")
	found := 0
	for i := len(lines) - 1; i >= 0 && found < 2; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		toks := numTokenRe.FindAllString(line, -1)
		if len(toks) < 3 {
			continue
		}
		row := toks[len(toks)-3:]
		s := 1 - found // 末行=流1，倒数第二行=流0
		for h := 0; h < 3; h++ {
			v, err := strconv.Atoi(row[h])
			if err != nil {
				return counts, false
			}
			counts[s][h] = v
		}
		found++
	}
	if found != 2 {
		return counts, false
	}
	return counts, true
}

var numTokenRe = regexp.MustCompile(`[0-9]+`)

// TestQuestionForAlternates：题域按针数交替（偶糖果/奇双维），接口完整。
func TestQuestionForAlternates(t *testing.T) {
	for i := int64(0); i < 6; i++ {
		q := QuestionFor(i)
		if q.ID == "" || q.Prompt == "" || q.Grade == nil {
			t.Fatalf("QuestionFor(%d) 返回不完整题目", i)
		}
		want := "candy"
		if i%2 == 1 {
			want = "two_dim"
		}
		if q.ID != want {
			t.Fatalf("QuestionFor(%d) 题域应为 %s，得 %s", i, want, q.ID)
		}
	}
}

func testCfg() pluginconfig.Config {
	cfg := pluginconfig.Default()
	cfg.QualityProbeEnabled = true
	cfg.ProbeIntervalSeconds = 300
	cfg.MaxConsecutiveProbeFailures = 3
	cfg.ProbeBackoffSeconds = 3600
	return cfg
}

func TestStatePassPath(t *testing.T) {
	cfg := testCfg()
	now := time.Now()
	st := NewState(1)
	d := st.Record(VerdictPass, "bag_perm", "3", now, cfg)
	if d != (Decision{}) {
		t.Errorf("pass 不应触发动作，得 %+v", d)
	}
	if st.ConsecFails != 0 || st.SuspectAccountLevel {
		t.Errorf("pass 应清失败状态: %+v", st)
	}
	// v0.3.2 双档：新号连过 1 < 退出线 3，走密集档 120s。
	if !st.NextProbeAt.Equal(now.Add(120 * time.Second)) {
		t.Errorf("未验证态 pass 应按密集档排期: %v", st.NextProbeAt)
	}
	if !st.InBurst || st.BurstProbes != 1 {
		t.Errorf("应处于密集档: in_burst=%v burst_probes=%d", st.InBurst, st.BurstProbes)
	}
	if st.Due(now.Add(119 * time.Second)) {
		t.Errorf("间隔内不应到期")
	}
	if !st.Due(now.Add(121 * time.Second)) {
		t.Errorf("间隔后应到期")
	}
}

// ---------- v0.3.2 双档排程（密集档/稳态档） ----------

// 验收：签捕获→上岗 ≈ 3×120s——前两针密集档、第三针达标出档回稳态档，
// 出档清密集预算；再答错重开回合恢复密集预算。
func TestBurstCadence(t *testing.T) {
	cfg := testCfg()
	cfg.ProbeBurstUntilPasses = 3 // Explicit legacy cadence remains supported.
	now := time.Now()
	st := NewState(10)
	// 第 1、2 针：未验证态，密集档。
	st.Record(VerdictPass, "bag_perm", "3", now, cfg)
	if !st.NextProbeAt.Equal(now.Add(120*time.Second)) || !st.InBurst {
		t.Fatalf("连过1 应密集档 120s: %v in_burst=%v", st.NextProbeAt, st.InBurst)
	}
	t2 := now.Add(120 * time.Second)
	st.Record(VerdictPass, "arith", "434", t2, cfg)
	if !st.NextProbeAt.Equal(t2.Add(120*time.Second)) || !st.InBurst {
		t.Fatalf("连过2 应仍密集档 120s: %v in_burst=%v", st.NextProbeAt, st.InBurst)
	}
	// 第 3 针：达标出档——回稳态档 300s，密集预算清零。
	t3 := t2.Add(120 * time.Second)
	st.Record(VerdictPass, "weekday27", "星期二", t3, cfg)
	if !st.NextProbeAt.Equal(t3.Add(300*time.Second)) || st.InBurst {
		t.Fatalf("连过3 应出档回稳态 300s: %v in_burst=%v", st.NextProbeAt, st.InBurst)
	}
	if st.BurstProbes != 0 {
		t.Fatalf("出档应清密集预算: %d", st.BurstProbes)
	}
	// 稀疏档持续：第 4 针仍 300s。
	t4 := t3.Add(300 * time.Second)
	st.Record(VerdictPass, "bag_perm", "3", t4, cfg)
	if !st.NextProbeAt.Equal(t4.Add(300 * time.Second)) {
		t.Fatalf("已验证态应稳态 300s: %v", st.NextProbeAt)
	}
	// 降智回归：答错清连过、密集预算重开——兜底排程落回密集档（新回合
	// 重新快速攒证据；复探链正常时该排期仅作断链兜底）。
	t5 := t4.Add(300 * time.Second)
	st.Record(VerdictFail, "arith", "366", t5, cfg)
	if st.ConsecPasses != 0 {
		t.Fatalf("答错应清连过: %+v", st)
	}
	if !st.InBurst || st.BurstProbes != 1 {
		t.Fatalf("新回合兜底应落密集档: in_burst=%v burst_probes=%d", st.InBurst, st.BurstProbes)
	}
}

func TestDefaultBurstCoversFullGraduationThreshold(t *testing.T) {
	cfg := testCfg()
	now := time.Now()
	st := NewState(10)
	for passes := 1; passes <= 6; passes++ {
		st.Record(VerdictPass, "bag_perm", "3", now, cfg)
		expected := time.Duration(cfg.ProbeBurstIntervalSeconds) * time.Second
		if passes == 6 {
			expected = time.Duration(cfg.ProbeIntervalSeconds) * time.Second
		}
		if st.InBurst != (passes < 6) || !st.NextProbeAt.Equal(now.Add(expected)) {
			t.Fatalf("passes=%d in_burst=%v next=%v expected_delay=%v", passes, st.InBurst, st.NextProbeAt, expected)
		}
		now = st.NextProbeAt
	}
}

// 验收：error 空转有封顶——连续 error 耗尽密集预算后退稳态档，不再 120s
// 一针地烧（模型 400 死循环是 2026-10-02 生产实测场景）。
func TestBurstCapOnErrorLoop(t *testing.T) {
	cfg := testCfg()
	cfg.ProbeBurstMaxProbes = 3
	now := time.Now()
	st := NewState(11)
	var at time.Time = now
	for i := 1; i <= 3; i++ {
		st.Record(VerdictError, "arith", "http:400", at, cfg)
		if want := at.Add(120 * time.Second); !st.NextProbeAt.Equal(want) {
			t.Fatalf("第 %d 个 error 应密集档: got %v want %v", i, st.NextProbeAt, want)
		}
		at = st.NextProbeAt
	}
	st.Record(VerdictError, "arith", "http:400", at, cfg)
	if want := at.Add(300 * time.Second); !st.NextProbeAt.Equal(want) {
		t.Fatalf("密集预算耗尽应退稳态档: got %v want %v", st.NextProbeAt, want)
	}
	if st.InBurst {
		t.Fatalf("封顶后应出密集档")
	}
}

func TestStateFailRerollPath(t *testing.T) {
	cfg := testCfg()
	now := time.Now()
	st := NewState(2)
	// 第 1、2 错：重摇 + 复探；第 3 错：账号级退避。
	d1 := st.Record(VerdictFail, "r_count", "2", now, cfg)
	if !d1.ShouldReroll || !d1.ProbeAgainNow || d1.EnterBackoff {
		t.Fatalf("第 1 错应重摇+复探: %+v", d1)
	}
	if st.QualityRerolls != 1 || st.ConsecFails != 1 {
		t.Errorf("计数错误: %+v", st)
	}
	d2 := st.Record(VerdictFail, "arith", "366", now.Add(3*time.Second), cfg)
	if !d2.ShouldReroll || !d2.ProbeAgainNow || d2.EnterBackoff {
		t.Fatalf("第 2 错应重摇+复探: %+v", d2)
	}
	d3 := st.Record(VerdictFail, "weekday27", "星期三", now.Add(6*time.Second), cfg)
	if d3.ShouldReroll || d3.ProbeAgainNow || !d3.EnterBackoff {
		t.Fatalf("第 3 错应退避而非继续烧额度: %+v", d3)
	}
	if !st.SuspectAccountLevel {
		t.Errorf("应标记疑似账号级")
	}
	if st.QualityRerolls != 2 {
		t.Errorf("退避不再重摇，累计重摇应 2: %d", st.QualityRerolls)
	}
	if st.Due(now.Add(3599 * time.Second)) {
		t.Errorf("退避期内不应到期")
	}
	if !st.Due(now.Add(3607 * time.Second)) {
		t.Errorf("退避期满应复探（给新机会）")
	}
	// 退避期满后答对：摘嫌疑帽、恢复常规排期。
	d4 := st.Record(VerdictPass, "bag_perm", "3", now.Add(3607*time.Second), cfg)
	if d4 != (Decision{}) || st.SuspectAccountLevel {
		t.Errorf("退避后 pass 应清嫌疑: %+v / %+v", d4, st)
	}
}

func TestStateErrorNotQualitySignal(t *testing.T) {
	cfg := testCfg()
	now := time.Now()
	st := NewState(3)
	st.Record(VerdictFail, "r_count", "2", now, cfg)
	d := st.Record(VerdictError, "arith", "http:503", now.Add(time.Second), cfg)
	if d != (Decision{}) {
		t.Errorf("error 不应触发动作: %+v", d)
	}
	if st.ConsecFails != 1 || st.Fails != 1 {
		t.Errorf("error 不应动质量计数: %+v", st)
	}
}

func TestAnswerTruncation(t *testing.T) {
	long := ""
	for i := 0; i < 200; i++ {
		long += "字"
	}
	got := TruncateAnswer(long)
	runes := []rune(got)
	if len(runes) != 81 { // 80 + 省略号
		t.Errorf("摘要应截 80+1，得 %d", len(runes))
	}
}

// ---------- v0.3 连过计数 + 卡点排程 ----------

func TestConsecPassesCounting(t *testing.T) {
	cfg := testCfg()
	now := time.Now()
	st := NewState(4)
	for i := 0; i < 3; i++ {
		st.Record(VerdictPass, "bag_perm", "3", now.Add(time.Duration(i)*time.Minute), cfg)
	}
	if st.ConsecPasses != 3 {
		t.Fatalf("三连对应为 3: %d", st.ConsecPasses)
	}
	d := st.Record(VerdictError, "arith", "http:503", now.Add(3*time.Minute), cfg)
	if st.ConsecPasses != 0 {
		t.Fatalf("error 应清零连续通过证据: %d", st.ConsecPasses)
	}
	if d != (Decision{}) || st.Fails != 0 || st.ConsecFails != 0 || st.QualityRerolls != 0 || st.SuspectAccountLevel {
		t.Fatalf("error 不得算作质量失败或触发重摇: decision=%+v state=%+v", d, st)
	}
	if !st.InBurst || !st.NextProbeAt.Equal(now.Add(3*time.Minute+120*time.Second)) {
		t.Fatalf("error 后应回到有界密集档: %+v", st)
	}
	st.Record(VerdictFail, "arith", "366", now.Add(4*time.Minute), cfg)
	if st.ConsecPasses != 0 {
		t.Fatalf("答错应清零连过: %d", st.ConsecPasses)
	}
	st.Record(VerdictPass, "bag_perm", "3", now.Add(5*time.Minute), cfg)
	if st.ConsecPasses != 1 {
		t.Fatalf("再答对从 1 起算: %d", st.ConsecPasses)
	}
}

func TestErrorPreservesQualityFailureAndBackoff(t *testing.T) {
	cfg := testCfg()
	now := time.Now()
	for _, failures := range []int{1, cfg.MaxConsecutiveProbeFailures} {
		t.Run(strconv.Itoa(failures), func(t *testing.T) {
			st := NewState(4)
			for i := 0; i < failures; i++ {
				st.Record(VerdictFail, "arith", "366", now.Add(time.Duration(i)*time.Minute), cfg)
			}
			before := *st
			d := st.Record(VerdictError, "arith", "http:503", now.Add(4*time.Minute), cfg)
			if d != (Decision{}) || st.Fails != before.Fails || st.ConsecFails != before.ConsecFails ||
				st.QualityRerolls != before.QualityRerolls || st.SuspectAccountLevel != before.SuspectAccountLevel ||
				!st.BackoffUntil.Equal(before.BackoffUntil) {
				t.Fatalf("error changed quality or backoff state: before=%+v after=%+v decision=%+v", before, st, d)
			}
			if st.ConsecPasses != 0 || st.Probes != before.Probes+1 || st.LastVerdict != VerdictError {
				t.Fatalf("error was not recorded: %+v", st)
			}
		})
	}
}

func fixedRnd() func() float64 { return func() float64 { return 0.5 } }

func TestAdaptiveColdStartFallsBack(t *testing.T) {
	cfg := testCfg() // interval 300s, margin 默认 120s, adaptive 默认 true
	now := time.Now()
	captured := now.Add(-time.Minute)
	cases := []struct {
		name       string
		p80        time.Duration
		capturedAt time.Time
		adaptive   bool
	}{
		{"无实测寿命", 0, captured, true},
		{"无签", 30 * time.Minute, time.Time{}, true},
		{"开关关闭", 30 * time.Minute, captured, false},
	}
	for _, c := range cases {
		cfg.AdaptiveProbeScheduling = c.adaptive
		got := AdaptiveNextProbe(now, c.capturedAt, c.p80, cfg, fixedRnd())
		if !got.Equal(now.Add(300 * time.Second)) {
			t.Errorf("%s 应退回固定间隔: %v", c.name, got.Sub(now))
		}
	}
}

func TestAdaptiveYoungSignSavesQuota(t *testing.T) {
	cfg := testCfg()
	now := time.Now()
	captured := now.Add(-time.Minute)
	// p80=30min → 目标 = 捕获+30min-120s = now+27min；间隔上限内按目标排。
	got := AdaptiveNextProbe(now, captured, 30*time.Minute, cfg, fixedRnd())
	target := captured.Add(30*time.Minute - 120*time.Second)
	if !got.Equal(target.Add(-60 * time.Second)) { // rnd=0.5 → 目标-0.5×120s
		t.Fatalf("年轻签应排到目标-jitter（中点）: got %v want %v", got.Sub(now), target.Sub(now)-60*time.Second)
	}
	// 远超上限（p80=3h）封顶 7200s。
	got = AdaptiveNextProbe(now, captured, 3*time.Hour, cfg, fixedRnd())
	if !got.Equal(now.Add(7200 * time.Second)) {
		t.Fatalf("超上限应封顶 7200s: %v", got.Sub(now))
	}
}

func TestAdaptiveDenseNearExpectedDeath(t *testing.T) {
	cfg := testCfg()
	now := time.Now()
	// 签龄已超 p80（目标已过）：最低间隔起密集盯防。
	captured := now.Add(-31 * time.Minute)
	got := AdaptiveNextProbe(now, captured, 30*time.Minute, cfg, fixedRnd())
	if !got.Equal(now.Add(300*time.Second + 60*time.Second)) {
		t.Fatalf("临死密集应 300s+jitter(中点60s): %v", got.Sub(now))
	}
}

// 验收标准 9：落点带 jitter、与换签时刻无强相关——固定种子下 2000 次抽样，
// 全部落在 [目标-120s, 目标] 窗口内、偏移分布有足够散布（≥100 个 1 秒粒度
// 不同值、均值贴近均匀分布中点），不存在锁死单一时刻的聚类指纹。
func TestAdaptiveJitterSpread(t *testing.T) {
	cfg := testCfg()
	now := time.Now()
	captured := now.Add(-time.Minute)
	target := captured.Add(30*time.Minute - 120*time.Second)
	rnd := rand.New(rand.NewSource(42)).Float64
	distinct := map[int64]bool{}
	var sum time.Duration
	const draws = 2000
	for i := 0; i < draws; i++ {
		got := AdaptiveNextProbe(now, captured, 30*time.Minute, cfg, rnd)
		offset := target.Sub(got) // (0, 120s]
		if offset < 0 || offset > 120*time.Second {
			t.Fatalf("落点越界 [目标-120s, 目标]: %v", offset)
		}
		distinct[int64(offset/time.Second)] = true
		sum += offset
	}
	if len(distinct) < 100 {
		t.Fatalf("jitter 散布不足（%d 个 1 秒粒度值），疑似聚类指纹", len(distinct))
	}
	mean := sum / draws
	if mean < 45*time.Second || mean > 75*time.Second {
		t.Fatalf("均匀分布均值应≈60s，得到 %v", mean)
	}
}
