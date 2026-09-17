package service

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"strconv"
	"time"
)

// 探针多题域题库（P2-11 二期）：糖果组合题在社区已知是中转侧常用 canary 题，
// 单一语义类别长期高频出现本身就是可聚类特征。题库扩为多个语义互不相交的
// 题域，每针均匀抽取，共用同一组判分阈值（答错或 rt<800 = degraded，
// rt≥1400 = recovered）。
//
// 题域准入硬标准（2026-09-15 五轮网关校准，1029 健康号 / gpt-6-astra / xhigh）：
// 糖果 rt=1419/1419/1532/1986✅；数字谜题 117❌、星期推算 34❌、天数计算 212❌、
// 网格路径 137❌、排列计数 183-208❌、选人计数 103-104❌、单维最坏情形 84-237❌。
// 结论：公式/计数/单维抽屉类在 xhigh 下推理量都不够 800 门槛，健康号会被误判
// 降智；只有「可感知维度 × 不可见维度 + 跨维配对目标」的双维两阶段最坏情形
// 分析（糖果结构）稳定落在健康带。答案实体若出现在题面（如逻辑谜题的人名）
// 会被模型复述时误配判分——题域必须使用带数字边界的数值答案。
//
// 每道题的期望答案由生成器用本地真值（全排列/组合枚举）现场算出，判分正则
// 按题动态构造。新题域上生产前必须先在健康账号上经网关校准（答对 +
// reasoning_tokens≥1400 + 无截断指纹），未通过校准的题域从
// openAIDowngradeProbeDomainGenerators 移除。

// openAIDowngradeProbeQuestion 一针探针题：题干 + 该题专用的答案匹配正则。
type openAIDowngradeProbeQuestion struct {
	Domain string
	Text   string
	// AnswerPattern 在模型输出文本里匹配期望答案。数值答案带边界
	// (^|[^0-9])N([^0-9]|$)，语义与旧版固定答案 21 的正则完全一致。
	AnswerPattern *regexp.Regexp
	// AnswerDisplay 期望答案的可读形态，仅供日志与校准记录。
	AnswerDisplay string
}

// openAIDowngradeNumericAnswerPattern 构造数字答案的边界正则。
func openAIDowngradeNumericAnswerPattern(answer int) *regexp.Regexp {
	return regexp.MustCompile(`(^|[^0-9])` + regexp.QuoteMeta(strconv.Itoa(answer)) + `([^0-9]|$)`)
}

func openAIDowngradeProbeLead() string {
	return openAIDowngradeProbeLeadPool[rand.IntN(len(openAIDowngradeProbeLeadPool))]
}

// openAIDowngradeProbeNextQuestion 均匀抽取一个题域生成一针探针题。
func openAIDowngradeProbeNextQuestion(now time.Time) openAIDowngradeProbeQuestion {
	generators := openAIDowngradeProbeDomainGenerators
	return generators[rand.IntN(len(generators))](now)
}

var openAIDowngradeProbeDomainGenerators = []func(time.Time) openAIDowngradeProbeQuestion{
	openAIDowngradeCandyQuestion,
	openAIDowngradeTwoDimQuestion,
}

// ─── 题域 1：糖果组合（保留原题，行为锚点数字不变） ─────────────────────────
//
// 所有变体共享同一组"行为锚点"数字（目标口味1: 7/7，目标口味2: 9/6，干扰口味:
// 8/4）。该数字组合已经过健康账号实测，稳定回答 21，因此数字不变、答案恒为 21。
// 随机化只作用于口味名、形状名、场景容器与形状行顺序——每针题目文本都不同，
// 防止上游按"同一道题"聚类。

var openAIDowngradeProbeFlavorPool = []string{
	"苹果", "桃子", "西瓜", "葡萄", "芒果", "草莓",
	"柠檬", "橙子", "荔枝", "蓝莓", "菠萝", "哈密瓜",
}

var openAIDowngradeProbeShapePool = [][2]string{
	{"圆形", "五角星形"},
	{"方形", "三角形"},
	{"心形", "菱形"},
	{"椭圆形", "六角星形"},
	{"半球形", "方块形"},
	{"月牙形", "十字形"},
}

var openAIDowngradeProbeContainerPool = []string{
	"一个黑色的袋子",
	"一个不透明的盒子",
	"一个密封的深色罐子",
	"一个看不见里面的口袋",
}

// 题干引导句与 instructions 同步轮换，避免 instructions 变了而题干首行
// 仍是同一句固定文本。
var openAIDowngradeProbeLeadPool = []string{
	"不使用任何外部工具回答以下问题：",
	"请不用工具,直接回答下面的问题：",
	"下面这道题请直接推理作答(不使用工具)：",
}

func openAIDowngradeCandyQuestion(_ time.Time) openAIDowngradeProbeQuestion {
	flavorIdx := rand.Perm(len(openAIDowngradeProbeFlavorPool))
	target1 := openAIDowngradeProbeFlavorPool[flavorIdx[0]]
	target2 := openAIDowngradeProbeFlavorPool[flavorIdx[1]]
	decoy := openAIDowngradeProbeFlavorPool[flavorIdx[2]]
	shapes := openAIDowngradeProbeShapePool[rand.IntN(len(openAIDowngradeProbeShapePool))]
	container := openAIDowngradeProbeContainerPool[rand.IntN(len(openAIDowngradeProbeContainerPool))]
	lead := openAIDowngradeProbeLeadPool[rand.IntN(len(openAIDowngradeProbeLeadPool))]
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
	return openAIDowngradeProbeQuestion{
		Domain:        "candy",
		Text:          text,
		AnswerPattern: openAIDowngradeNumericAnswerPattern(21),
		AnswerDisplay: "21",
	}
}

// ─── 题域 2：双维保证（糖果推理类的泛化表面，两阶段博弈闭式真值） ──────────
//
// 五轮校准（2026-09-15，1029 健康号）逐步收窄了 rt 健康带的来源：
//   公式/计数类 103-212❌；单维抽屉最值 84-237❌；糖果 1419/1419/1532/1986✅；
//   two_dim 短标签池 6/6✅ rt=1255-1930（第四、五轮），年份池 0/1❌ 已移除。
// 糖果的推理量来自双维结构——一个可感知维度（形状靠手感分辨，摸的人可选流）
// 叠加一个不可见维度（口味，对手决定），目标又是跨维配对（不同形状的两目标
// 口味），必须做两阶段最坏情形分析。闭式（经糖果锚点数字反推验证 21）：
//   先摸流 S：decoy_S + 1 拿到一个目标值 g（对手选 g 最大化后续负担），
//   再摸流 T：decoy_T + count_T(g) + 1 拿到另一目标值。
//   答案 = min( 先A后B, 先B后A )，两个方向各对 g 取 max。
// 本域保留该结构与锚点量级，仅扩散语义表面（球/积木/电池/茶包 × 材质/形状/
// 大小 × 颜色/口味/年份），数表数字随机化（答案随数字变动，闭式现场计算）。
// 测试用小规模博弈树全搜素 DFS 交叉验证闭式（TestOpenAIDowngradeTwoDimTruth）。

type openAIDowngradeTwoDimSpec struct {
	streamNames [2]string // 可感知维度：如 金属/塑料
	hiddenVals  [3]string // 不可见维度：目标1/目标2/干扰
	counts      [2][3]int // [流][隐藏值] 数量
}

// openAIDowngradeTwoDimCommit 「先摸一流再摸另一流」承诺策略的最坏代价。
// 不是一般最优（后续可在主流续摸双目标再换流），仅用于生成期的干净性筛选。
func openAIDowngradeTwoDimCommit(sp *openAIDowngradeTwoDimSpec) int {
	decoy := func(stream int) int { return sp.counts[stream][2] }
	order := func(first, second int) int {
		worst := 0
		for _, got := range []int{0, 1} {
			burden := decoy(first) + 1 + decoy(second) + sp.counts[second][got] + 1
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

// openAIDowngradeTwoDimGame 全信息极小极大博弈真值：摸的人每步选流（最小化
// 总摸取数），对手决定每次摸出的隐藏值（最大化）。持有状态可由「初始−剩余」
// 推导，memo 键只需剩余计数向量，状态数 ≤ Π(c_i+1)。
func openAIDowngradeTwoDimGame(sp *openAIDowngradeTwoDimSpec) int {
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

var openAIDowngradeTwoDimSurfaces = []struct {
	container   string
	item        string
	streamNames [2]string
}{
	{"一个不透明的大布袋", "球", [2]string{"金属的", "塑料的"}},
	{"一个乱放的玩具积木箱", "积木", [2]string{"圆柱形", "方块形"}},
	{"一个混放的杂物抽屉", "电池", [2]string{"5号的", "7号的"}},
	{"一个密封的茶叶罐", "茶包", [2]string{"三角袋", "方形袋"}},
}

var openAIDowngradeTwoDimHiddenPools = [][]string{
	{"红色", "蓝色", "黄色"},
	{"红色", "绿色", "白色"},
	{"蓝色", "紫色", "橙色"},
	{"柠檬味", "茉莉味", "薄荷味"},
	// 注意不使用「年份」类隐藏值：2026-09-15 校准针03（电池×年份，rt=1552
	// 健康带内）模型把 15/13 读成 14/12 答错——长标签需额外心射映射，健康号
	// 的滑误率上升；短颜色/口味标签四针全对。
}

func openAIDowngradeTwoDimQuestion(_ time.Time) openAIDowngradeProbeQuestion {
	for attempt := 0; attempt < 300; attempt++ {
		surface := openAIDowngradeTwoDimSurfaces[rand.IntN(len(openAIDowngradeTwoDimSurfaces))]
		hidden := openAIDowngradeTwoDimHiddenPools[rand.IntN(len(openAIDowngradeTwoDimHiddenPools))]
		spec := &openAIDowngradeTwoDimSpec{streamNames: surface.streamNames}
		perm := rand.Perm(3)
		for h := range spec.hiddenVals {
			spec.hiddenVals[h] = hidden[perm[h]]
		}
		for s := range spec.counts {
			for h := range spec.counts[s] {
				spec.counts[s][h] = 4 + rand.IntN(5) // 4-8，糖果锚点量级
			}
		}
		answer := openAIDowngradeTwoDimGame(spec)
		// 只接受「博弈最优 = 承诺策略」的教科书干净实例：若自适应续摸能改进，
		// 题目存在多个可辩护答案（如 [[3,1,5],[2,3,3]] 的 12 vs 13），模型答案
		// 不稳定，canary 会误判。糖果锚点数字同样满足该等式（21）。
		if answer != openAIDowngradeTwoDimCommit(spec) {
			continue
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
			openAIDowngradeProbeLead(), surface.container,
			surface.item, target1, target2, spec.hiddenVals[2],
			sa, sb, surface.item,
			target1, target2,
			sa, target1, sb, target2,
			sa, target2, sb, target1,
			target1, target2, spec.hiddenVals[2],
			sa, spec.counts[0][0], spec.counts[0][1], spec.counts[0][2],
			sb, spec.counts[1][0], spec.counts[1][1], spec.counts[1][2])
		return openAIDowngradeProbeQuestion{
			Domain:        "two_dim",
			Text:          text,
			AnswerPattern: openAIDowngradeNumericAnswerPattern(answer),
			AnswerDisplay: strconv.Itoa(answer),
		}
	}
	return openAIDowngradeCandyQuestion(time.Time{})
}
