package prober

// v0.3.5 canary 题库——从宿主资格针题库（backend/internal/service/
// openai_downgrade_probe_questions.go，2026-09-15 五轮网关校准）移植。
// v0.3.3 的三道公式/计数题全在被宿主校准淘汰的种类（健康号 rt 只到
// 84-237，降智号反而全对）——2026-10-02/03 生产实证：插件连过 / 宿主针
// 40 连败，根因 = 考卷难度错位。唯一落在健康带的题域是「可感知维度 ×
// 不可见维度 + 跨维配对目标」的双维两阶段最坏情形分析（糖果结构）；
// 社区侧（linux.do、sub2api issue #3644）糖果题也正是降智检测的公认
// canary。判分只接受明确最终答案，不以推导中出现的数字作为通过证据。
//
// 题域按针数确定性轮换（candy/two_dim 交替），每针的语义表面（口味/
// 形状/容器/数表数字）现场随机重生成——题面每针不同、结构与锚点不变，
// 防止上游按「同一道题」聚类。
//
// 题域准入硬标准（宿主五轮校准，1029 健康号 / gpt-6-astra / xhigh）：
// 糖果 rt=1419/1419/1532/1986✅、two_dim rt=1255-1930 6/6✅；数字谜题/
// 星期推算/天数计算/网格路径/排列计数/选人计数/单维最坏情形 84-237❌。

import (
	"fmt"
	"math/rand/v2"
	"strconv"
)

// QuestionFor 按针数取题：偶数针糖果、奇数针双维保证，每针现场生成
// 全新随机实例。两题域共用同一组判分阈值（答对 + rt≥800，执行器侧），
// 难度与宿主资格针完全同卷——插件连过才是宿主真能毕业的证据。
func QuestionFor(probeCount int64) Question {
	if probeCount%2 == 1 {
		return twoDimQuestion()
	}
	return candyQuestion()
}

// ─── 题域 1：糖果组合（行为锚点数字不变，答案恒为 21） ────────────────────
//
// 所有变体共享同一组「行为锚点」数字（7/9/8 与 7/6/4）：该组合经健康号
// 实测稳定回答 21，数字不变、答案恒定。随机化只作用于口味名、形状名、
// 场景容器、引导句与形状行顺序。

var candyFlavorPool = []string{
	"苹果", "桃子", "西瓜", "葡萄", "芒果", "草莓",
	"柠檬", "橙子", "荔枝", "蓝莓", "菠萝", "哈密瓜",
}

var candyShapePool = [][2]string{
	{"圆形", "五角星形"},
	{"方形", "三角形"},
	{"心形", "菱形"},
	{"椭圆形", "六角星形"},
	{"半球形", "方块形"},
	{"月牙形", "十字形"},
}

var candyContainerPool = []string{
	"一个黑色的袋子",
	"一个不透明的盒子",
	"一个密封的深色罐子",
	"一个看不见里面的口袋",
}

// probeLeadPool 题干引导句（与宿主同步轮换，防题面首行固定文本聚类）。
var probeLeadPool = []string{
	"不使用任何外部工具回答以下问题：",
	"请不用工具,直接回答下面的问题：",
	"下面这道题请直接推理作答(不使用工具)：",
}

func probeLead() string {
	return probeLeadPool[rand.IntN(len(probeLeadPool))]
}

func candyQuestion() Question {
	flavorIdx := rand.Perm(len(candyFlavorPool))
	target1 := candyFlavorPool[flavorIdx[0]]
	target2 := candyFlavorPool[flavorIdx[1]]
	decoy := candyFlavorPool[flavorIdx[2]]
	shapes := candyShapePool[rand.IntN(len(candyShapePool))]
	container := candyContainerPool[rand.IntN(len(candyContainerPool))]
	lead := probeLead()
	shapeA, shapeB := shapes[0], shapes[1]
	rowA, rowB := [3]int{7, 9, 8}, [3]int{7, 6, 4}
	if rand.IntN(2) == 1 {
		shapeA, shapeB = shapeB, shapeA
	}
	text := fmt.Sprintf(`%s

在%s里放有三种口味的糖果，每种糖果有两种不同的形状（%s和%s，不同的形状靠手感可以分辨）。现已知不同口味的糖和不同形状的数量统计如下表。参赛者需要在活动前决定摸出的糖果数目，那么，最少取出多少个糖果才能保证手中同时拥有不同形状的%s味和%s味的糖？（同时手中有%s%s味匹配%s%s味糖果，或者有%s%s味匹配%s%s味糖果都满足要求）

        %s味  %s味  %s味
%s       %d      %d      %d
%s       %d      %d      %d`,
		lead, container, shapeA, shapeB,
		target1, target2,
		shapeA, target1, shapeB, target2,
		shapeA, target2, shapeB, target1,
		target1, target2, decoy,
		shapeA, rowA[0], rowA[1], rowA[2],
		shapeB, rowB[0], rowB[1], rowB[2])
	return Question{
		ID:     "candy",
		Prompt: text + finalAnswerInstruction,
		Grade: func(text string) bool {
			answer, known := ExtractFinalAnswer(text)
			return known && answer == "21"
		},
	}
}

// ─── 题域 2：双维保证（糖果推理类的泛化表面，博弈真值交叉验证） ────────────
//
// 闭式（经糖果锚点数字反推验证 21）：先摸一流拿到某目标值 g（对手选 g
// 最大化后续负担），再摸另一流拿到另一目标值。答案 = min(先A后B, 先B后A)，
// 两个方向各对 g 取 max。生成期用全信息极小极大博弈树 DFS 交叉验证闭式，
// 只接受「博弈最优 = 承诺策略」且答案 ≥10 的教科书干净实例——否则题目
// 存在多个可辩护答案，canary 会误判（宿主校准结论原样保留）。
// 隐藏值不用长标签（年份类）：健康号滑误率上升（宿主校准针03 实证）。

type twoDimSpec struct {
	streamNames [2]string // 可感知维度：如 金属/塑料
	hiddenVals  [3]string // 不可见维度：目标1/目标2/干扰
	counts      [2][3]int // [流][隐藏值] 数量
}

// TwoDimCommitAnswer 「先摸一流再摸另一流」承诺策略的最坏代价（闭式）。
// 不是一般最优（后续可在主流续摸双目标再换流），仅用于生成期的干净性
// 筛选与测试侧真值复算。导出供 transport 测试按题面数表复算答案。
func TwoDimCommitAnswer(counts [2][3]int) int {
	decoy := func(stream int) int { return counts[stream][2] }
	order := func(first, second int) int {
		worst := 0
		for _, got := range []int{0, 1} {
			burden := decoy(first) + 1 + decoy(second) + counts[second][got] + 1
			if burden > worst {
				worst = burden
			}
		}
		return worst
	}
	a, b := order(0, 1), order(1, 0)
	if a < b {
		return a
	}
	return b
}

// twoDimGame 全信息极小极大博弈真值：摸的人每步选流（最小化总摸取数），
// 对手决定每次摸出的隐藏值（最大化）。memo 键只需剩余计数向量，
// 状态数 ≤ Π(c_i+1)。
func twoDimGame(sp *twoDimSpec) int {
	memo := make(map[[6]int]int)
	met := func(counts [2][3]int) bool {
		for s := 0; s < 2; s++ {
			drawnT1 := sp.counts[s][0] > counts[s][0]
			drawnT2 := sp.counts[s][1] > counts[s][1]
			other := 1 - s
			otherT1 := sp.counts[other][0] > counts[other][0]
			otherT2 := sp.counts[other][1] > counts[other][1]
			if (drawnT1 && otherT2) || (drawnT2 && otherT1) {
				return true
			}
		}
		return false
	}
	var dfs func(counts [2][3]int) int
	dfs = func(counts [2][3]int) int {
		if met(counts) {
			return 0
		}
		key := [6]int{counts[0][0], counts[0][1], counts[0][2], counts[1][0], counts[1][1], counts[1][2]}
		if v, ok := memo[key]; ok {
			return v
		}
		best := 1 << 28
		for s := 0; s < 2; s++ {
			worst := -(1 << 28)
			for h := 0; h < 3; h++ {
				if counts[s][h] == 0 {
					continue
				}
				next := counts
				next[s][h]--
				v := 1 + dfs(next)
				if v > worst {
					worst = v
				}
			}
			if worst == -(1 << 28) {
				continue // 该流已摸空
			}
			if worst < best {
				best = worst
			}
		}
		if best == 1<<28 {
			best = 1 << 27 // 双流皆空且未达成（防御；正常规格不可达）
		}
		memo[key] = best
		return best
	}
	return dfs(sp.counts)
}

var twoDimSurfaces = []struct {
	container   string
	item        string
	streamNames [2]string
}{
	{"一个不透明的大布袋", "球", [2]string{"金属的", "塑料的"}},
	{"一个乱放的玩具积木箱", "积木", [2]string{"圆柱形", "方块形"}},
	{"一个混放的杂物抽屉", "电池", [2]string{"5号的", "7号的"}},
	{"一个密封的茶叶罐", "茶包", [2]string{"三角袋", "方形袋"}},
}

var twoDimHiddenPools = [][]string{
	{"红色", "蓝色", "黄色"},
	{"红色", "绿色", "白色"},
	{"蓝色", "紫色", "橙色"},
	{"柠檬味", "茉莉味", "薄荷味"},
}

// twoDimQuestion 随机生成一道双维保证题：随机表面 × 随机隐藏值排列 ×
// 随机数表（4-8，糖果锚点量级），真值经博弈树验证后按数字边界判分。
func twoDimQuestion() Question {
	for attempt := 0; attempt < 300; attempt++ {
		surface := twoDimSurfaces[rand.IntN(len(twoDimSurfaces))]
		hidden := twoDimHiddenPools[rand.IntN(len(twoDimHiddenPools))]
		spec := &twoDimSpec{streamNames: surface.streamNames}
		perm := rand.Perm(3)
		for h := range spec.hiddenVals {
			spec.hiddenVals[h] = hidden[perm[h]]
		}
		for s := range spec.counts {
			for h := range spec.counts[s] {
				spec.counts[s][h] = 4 + rand.IntN(5) // 4-8
			}
		}
		answer := twoDimGame(spec)
		if answer != TwoDimCommitAnswer(spec.counts) {
			continue // 只接受「博弈最优 = 承诺策略」的干净实例
		}
		if answer < 10 {
			continue
		}
		target1, target2 := spec.hiddenVals[0], spec.hiddenVals[1]
		sa, sb := spec.streamNames[0], spec.streamNames[1]
		text := fmt.Sprintf(`%s

在%s里放有三种%s（%s、%s、%s），每种又有两种不同的类别（%s和%s，类别靠手感可以分辨，但%s分辨不出来）。现已知不同类别和各种类别的数量统计如下表。那么，最少取出多少个才能保证手中同时拥有不同类别的%s和%s？（同时手中有一个%s%s匹配一个%s%s，或者有一个%s%s匹配一个%s%s，都满足要求）

        %s  %s  %s
%s      %d     %d     %d
%s      %d     %d     %d`,
			probeLead(), surface.container,
			surface.item, target1, target2, spec.hiddenVals[2],
			sa, sb, surface.item,
			target1, target2,
			sa, target1, sb, target2,
			sa, target2, sb, target1,
			target1, target2, spec.hiddenVals[2],
			sa, spec.counts[0][0], spec.counts[0][1], spec.counts[0][2],
			sb, spec.counts[1][0], spec.counts[1][1], spec.counts[1][2])
		return Question{
			ID:     "two_dim",
			Prompt: text + finalAnswerInstruction,
			Grade: func(text string) bool {
				final, known := ExtractFinalAnswer(text)
				return known && final == strconv.Itoa(answer)
			},
		}
	}
	return candyQuestion() // 300 次仍未命中干净实例（概率上不可达）：回糖果
}
