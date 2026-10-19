package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

// PlazaOfficialPricing 模型广场展示用的官方参考价（USD per token），与计费同源：
// LiteLLM → 内置兜底价卡 → 模型策略。字段为 nil 表示该项缺失（0 视为未配置）。
type PlazaOfficialPricing struct {
	InputPrice        *float64
	OutputPrice       *float64
	CacheWritePrice   *float64 // 5m 缓存写入（= LiteLLM cache_creation）
	CacheWrite1hPrice *float64 // 1h 缓存写入，仅计费会区分 5m/1h 时给出
	CacheReadPrice    *float64
	// Intervals 官方长上下文阶梯（多档时给出），不受分组开关影响。
	Intervals []PricingInterval
}

// PlazaModel 模型广场中单个模型条目：按实收口径合成的展示定价 + 官方参考价。
type PlazaModel struct {
	Name            string
	Platform        string
	Pricing         *ChannelModelPricing
	OfficialPricing *PlazaOfficialPricing
	// LongContextBasis 多档时的计价基准（整单 / 仅超出部分），单档为空。
	LongContextBasis ContextPricingBasis
	// TimePricing 计费会生效的分时倍率时段；无分时为 nil。
	TimePricing *TimePricingSchedule
	// CurrentTimeMultiplier is evaluated by the billing resolver, never by browser time.
	CurrentTimeMultiplier float64
}

// PlazaGroup 模型广场中以分组为顶层的条目。
//
// Models 汇总分组价格卡、渠道配置和账号支持的模型目录，按具体平台隔离。
type PlazaGroup struct {
	ID                 int64
	Name               string
	Description        string
	I18n               GroupI18n
	Platform           string
	SubscriptionType   string
	RateMultiplier     float64
	PeakRateEnabled    bool
	PeakStart          string
	PeakEnd            string
	PeakRateMultiplier float64
	IsExclusive        bool
	// 图片按次实付倍率：ImageRateIndependent 为 true 时，图片计费模型的实付
	// = 档位价 × ImageRateMultiplier，不乘分组/用户专属倍率（与计费口径一致）。
	ImageRateIndependent bool
	ImageRateMultiplier  float64
	VideoRateIndependent bool
	VideoRateMultiplier  float64
	// LongContextPricingEnabled 分组是否按上下文长度应用阶梯价；关闭时模型展示的是最低档。
	LongContextPricingEnabled bool
	Models                    []PlazaModel
}

// ModelPlazaService 聚合模型广场数据。
//
// 模型枚举来自分组、渠道及账号配置；token 模型的展示单价与阶梯由 BillingService 的阶梯表
// 查询给出（与扣费走同一条解析链与计费函数），图片/按次模型沿用渠道/分组档位价。
type ModelPlazaService struct {
	channelRepo    ChannelRepository
	groupRepo      GroupRepository
	accountRepo    AccountRepository
	pricingService *PricingService
	billingService *BillingService
	resolver       *ModelPricingResolver
}

// NewModelPlazaService 创建模型广场服务。
func NewModelPlazaService(
	channelRepo ChannelRepository,
	groupRepo GroupRepository,
	accountRepo AccountRepository,
	pricingService *PricingService,
	billingService *BillingService,
	resolver *ModelPricingResolver,
) *ModelPlazaService {
	return &ModelPlazaService{
		channelRepo:    channelRepo,
		groupRepo:      groupRepo,
		accountRepo:    accountRepo,
		pricingService: pricingService,
		billingService: billingService,
		resolver:       resolver,
	}
}

// ListGroups 返回模型广场数据：每个活跃分组附带其可用模型与定价。
//
// 枚举渠道、分组价格卡和账号目录，遵守分组白名单及渠道模型限制：
//   - 渠道按 lower(name) 排序后遍历，保证同名模型去重结果确定；
//   - 同分组同名模型「先见者胜」，仅当已存条目无定价而新条目有定价时升级替换；
//   - token 模型的单价与阶梯按实收口径合成（见 ResolveContextPricingSchedule），
//     图片计费模型的档位价按实收口径合成（见 plazaImageDisplayPricing）；
//   - 每个模型附带官方参考价（查不到为 nil）；
//   - 只返回 Models 非空的分组；分组按 RateMultiplier 升序（同倍率按名称），
//     组内模型按名称排序。
//
// 可见性过滤（专属分组）不在此层做，由 handler 按登录态裁剪。
func (s *ModelPlazaService) ListGroups(ctx context.Context) ([]PlazaGroup, error) {
	channels, err := s.channelRepo.ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	groups, err := s.groupRepo.ListActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("list active groups: %w", err)
	}

	// Build a request-local snapshot from the same fresh rows used to enumerate models.
	// Reuse the billing lookup implementation without its ten-minute process cache.
	displayService := *s
	platforms := make(map[int64]string, len(groups))
	for i := range groups {
		platforms[groups[i].ID] = groups[i].Platform
	}
	snapshot := &ChannelService{}
	snapshot.cache.Store(populateChannelCache(append([]Channel(nil), channels...), platforms))
	if s.resolver != nil && s.billingService != nil {
		displayService.resolver = NewModelPricingResolver(snapshot, s.billingService)
	}
	sort.SliceStable(channels, func(i, j int) bool {
		return strings.ToLower(channels[i].Name) < strings.ToLower(channels[j].Name)
	})

	byGroup := make(map[int64]*PlazaGroup, len(groups))
	groupEnt := make(map[int64]*Group, len(groups))
	order := make([]int64, 0, len(groups))
	for i := range groups {
		g := &groups[i]
		byGroup[g.ID] = &PlazaGroup{
			ID:                        g.ID,
			Name:                      g.Name,
			Description:               g.Description,
			I18n:                      g.I18n,
			Platform:                  g.Platform,
			SubscriptionType:          g.SubscriptionType,
			RateMultiplier:            g.RateMultiplier,
			PeakRateEnabled:           g.PeakRateEnabled,
			PeakStart:                 g.PeakStart,
			PeakEnd:                   g.PeakEnd,
			PeakRateMultiplier:        g.PeakRateMultiplier,
			IsExclusive:               g.IsExclusive,
			ImageRateIndependent:      g.ImageRateIndependent,
			ImageRateMultiplier:       g.ImageRateMultiplier,
			VideoRateIndependent:      g.VideoRateIndependent,
			VideoRateMultiplier:       g.VideoRateMultiplier,
			LongContextPricingEnabled: g.LongContextPricingEnabled,
		}
		groupEnt[g.ID] = g
		order = append(order, g.ID)
	}

	type modelKey struct {
		platform string
		name     string
	}
	// modelIdx[groupID][platform+modelName] = index into byGroup[groupID].Models
	modelIdx := make(map[int64]map[modelKey]int, len(groups))
	addModel := func(gid int64, m SupportedModel) {
		pg, ok := byGroup[gid]
		m.Name = strings.TrimSpace(m.Name)
		if !ok || m.Name == "" || strings.Contains(m.Name, "*") || !isPlatformPricingMatch(pg.Platform, m.Platform) {
			return
		}
		modelCtx := WithResolvedTargetPlatform(ctx, m.Platform)
		mapping := snapshot.ResolveChannelMapping(modelCtx, gid, m.Name)
		restrictedModel := billingModelForRestriction(mapping.BillingModelSource, m.Name, mapping.MappedModel)
		if !groupEnt[gid].ModelAllowlist.Allows(m.Name) || (restrictedModel != "" && snapshot.IsModelRestricted(modelCtx, gid, restrictedModel)) {
			return
		}
		idx := modelIdx[gid]
		if idx == nil {
			idx = make(map[modelKey]int)
			modelIdx[gid] = idx
		}
		key := modelKey{platform: m.Platform, name: strings.ToLower(m.Name)}
		if at, seen := idx[key]; seen {
			if pg.Models[at].Pricing == nil && m.Pricing != nil {
				pg.Models[at].Pricing = m.Pricing
			}
			return
		}
		idx[key] = len(pg.Models)
		pg.Models = append(pg.Models, PlazaModel{Name: m.Name, Platform: m.Platform, Pricing: m.Pricing})
	}
	for i := range channels {
		ch := &channels[i]
		if ch.Status != StatusActive {
			continue
		}
		ch.normalizeBillingModelSource()
		supported := ch.SupportedModels()
		fillGlobalPricingFallback(s.pricingService, supported)
		for _, gid := range ch.GroupIDs {
			for _, m := range supported {
				addModel(gid, m)
			}
		}
	}
	for i := range groups {
		g := &groups[i]
		for j := range g.ModelPricing {
			pricing := &g.ModelPricing[j]
			platform := g.Platform
			if platform == PlatformComposite {
				platform = pricing.Platform
			}
			for _, name := range pricing.Models {
				addModel(g.ID, SupportedModel{Name: name, Platform: platform, Pricing: pricing})
			}
		}
	}
	if s.accountRepo != nil {
		// One fresh read for all groups; transient limits must not remove price cards.
		accounts, err := s.accountRepo.ListActive(ctx)
		if err != nil {
			return nil, fmt.Errorf("list plaza accounts: %w", err)
		}
		catalogs := make(map[string][]string)
		for i := range accounts {
			a := &accounts[i]
			if a.Status != StatusActive || !a.Schedulable {
				continue
			}
			models := s.plazaAccountModelNames(a, catalogs)
			for _, gid := range a.GroupIDs {
				g := groupEnt[gid]
				if g == nil {
					continue
				}
				candidates := append([]string(nil), models...)
				if g.ModelAllowlist.Enabled {
					candidates = append(candidates, g.ModelAllowlist.Models...)
				}
				for _, name := range candidates {
					modelCtx := WithResolvedTargetPlatform(ctx, a.Platform)
					mapping := snapshot.ResolveChannelMapping(modelCtx, gid, name)
					if !a.IsModelSupported(mapping.MappedModel) {
						continue
					}
					if mapping.BillingModelSource == BillingModelSourceUpstream && snapshot.IsModelRestricted(modelCtx, gid, a.GetMappedModel(mapping.MappedModel)) {
						continue
					}
					addModel(gid, SupportedModel{Name: name, Platform: a.Platform})
				}
			}
		}
	}

	officialMemo := make(map[string]*PlazaOfficialPricing)
	out := make([]PlazaGroup, 0, len(order))
	for _, gid := range order {
		pg := byGroup[gid]
		if len(pg.Models) == 0 {
			continue
		}
		sort.SliceStable(pg.Models, func(i, j int) bool {
			if pg.Models[i].Name != pg.Models[j].Name {
				return pg.Models[i].Name < pg.Models[j].Name
			}
			return pg.Models[i].Platform < pg.Models[j].Platform
		})
		g := groupEnt[gid]
		for j := range pg.Models {
			displayService.fillDisplayPricing(ctx, &pg.Models[j], g)
			pg.Models[j].OfficialPricing = s.lookupOfficialPricing(ctx, pg.Models[j].Name, officialMemo)
		}
		out = append(out, *pg)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].RateMultiplier != out[j].RateMultiplier {
			return out[i].RateMultiplier < out[j].RateMultiplier
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// fillDisplayPricing 把模型的展示定价换成实收口径：
// token 模型取计费阶梯表（单价与档位均由真实计费函数得出），
// 图片/按次模型（或阶梯表不可用时）沿用渠道定价与分组图片档位价。
func (s *ModelPlazaService) fillDisplayPricing(ctx context.Context, m *PlazaModel, g *Group) {
	if groupPricing := matchGroupModelPricing(g, m.Name); groupPricing != nil {
		m.Pricing = groupPricing
	}
	m.CurrentTimeMultiplier = 1
	if s.billingService != nil && s.resolver != nil {
		pricingCtx := WithResolvedTargetPlatform(ctx, m.Platform)
		resolved := s.resolver.Resolve(pricingCtx, PricingInput{Model: m.Name, Group: g, GroupID: &g.ID})
		if resolved.channelPricing != nil {
			pricing := resolved.channelPricing.Clone()
			m.Pricing = &pricing
		}
		m.CurrentTimeMultiplier = resolvedChannelTimeMultiplier(resolved, timezone.Now())
		sched, err := s.billingService.ResolveContextPricingSchedule(ctx, s.resolver, ContextPricingScheduleInput{
			Model:    m.Name,
			Group:    g,
			Platform: m.Platform,
		})
		if err != nil {
			m.Pricing = nil
			return
		}
		if sched != nil && len(sched.Tiers) > 0 {
			m.Pricing = plazaPricingFromSchedule(m.Pricing, sched)
			if len(sched.Tiers) > 1 {
				m.LongContextBasis = sched.Basis
			}
			m.TimePricing = sched.TimePricing
			return
		}
	}
	m.Pricing = plazaImageDisplayPricing(m.Pricing, g)
	m.Pricing = plazaVideoDisplayPricing(m.Pricing, g, m.Name)
}

// plazaPricingFromSchedule 把阶梯表压成展示用的 ChannelModelPricing：
// 平价取首档单价，多档时 Intervals 逐档给出绝对单价；图片/按次字段沿用原始定价。
func plazaPricingFromSchedule(raw *ChannelModelPricing, sched *ContextPricingSchedule) *ChannelModelPricing {
	out := &ChannelModelPricing{BillingMode: BillingModeToken}
	if raw != nil {
		out.ImageInputPrice = raw.ImageInputPrice
		out.ImageOutputPrice = raw.ImageOutputPrice
		out.PerRequestPrice = raw.PerRequestPrice
		out.ReasoningEffortMultipliers = reasoningEffortMultipliersFromPricing(raw)
	}
	first := sched.Tiers[0]
	out.InputPrice = first.Input
	out.OutputPrice = first.Output
	out.CacheWritePrice = first.CacheWrite
	out.CacheWrite1hPrice = first.CacheWrite1h
	out.CacheReadPrice = first.CacheRead
	if len(sched.Tiers) > 1 {
		out.Intervals = plazaIntervalsFromTiers(sched.Tiers)
	}
	return out
}

func plazaIntervalsFromTiers(tiers []ContextPricingTier) []PricingInterval {
	intervals := make([]PricingInterval, 0, len(tiers))
	for i, t := range tiers {
		intervals = append(intervals, PricingInterval{
			MinTokens:         t.MinTokens,
			MaxTokens:         t.MaxTokens,
			TierLabel:         t.Label,
			InputPrice:        t.Input,
			OutputPrice:       t.Output,
			CacheWritePrice:   t.CacheWrite,
			CacheWrite1hPrice: t.CacheWrite1h,
			CacheReadPrice:    t.CacheRead,
			SortOrder:         i,
		})
	}
	return intervals
}

// plazaImageDisplayPricing 为图片计费模型合成展示定价，使档位价与实收口径一致：
// 每档（1K/2K/4K）单价 = 分组图片价 > 渠道同档位价 > 渠道默认按次价，无价的档不展示。
// 分组未配任何图片价、或定价非图片模式时原样返回。返回克隆，不修改入参
// （渠道定价指针指向缓存共享数据）。
func plazaImageDisplayPricing(p *ChannelModelPricing, g *Group) *ChannelModelPricing {
	if p == nil || g == nil || p.BillingMode != BillingModeImage {
		return p
	}
	if g.ImagePrice1K == nil && g.ImagePrice2K == nil && g.ImagePrice4K == nil {
		return p
	}
	channelTierPrice := func(label string) *float64 {
		for i := range p.Intervals {
			if p.Intervals[i].TierLabel == label && p.Intervals[i].PerRequestPrice != nil {
				return p.Intervals[i].PerRequestPrice
			}
		}
		return p.PerRequestPrice
	}
	tiers := []struct {
		label      string
		groupPrice *float64
	}{
		{"1K", g.ImagePrice1K},
		{"2K", g.ImagePrice2K},
		{"4K", g.ImagePrice4K},
	}
	clone := *p
	clone.Intervals = make([]PricingInterval, 0, len(tiers))
	for i, t := range tiers {
		price := t.groupPrice
		if price == nil {
			price = channelTierPrice(t.label)
		}
		if price == nil {
			continue
		}
		v := *price
		clone.Intervals = append(clone.Intervals, PricingInterval{
			TierLabel:       t.label,
			PerRequestPrice: &v,
			SortOrder:       i,
		})
	}
	return &clone
}

// lookupOfficialPricing 查询模型的官方参考价（与计费同源：LiteLLM → 内置兜底 → 模型策略），
// 带 memo 避免同名模型重复解析。官方阶梯按无分组、无渠道的口径查阶梯表。
// billingService 为 nil（测试场景）或查不到时返回 nil。
func (s *ModelPlazaService) lookupOfficialPricing(ctx context.Context, modelName string, memo map[string]*PlazaOfficialPricing) *PlazaOfficialPricing {
	if s.billingService == nil {
		return nil
	}
	if cached, ok := memo[modelName]; ok {
		return cached
	}
	var result *PlazaOfficialPricing
	if mp, err := s.billingService.GetModelPricing(modelName); err == nil && mp != nil {
		result = &PlazaOfficialPricing{
			InputPrice:      nonZeroPtr(mp.InputPricePerToken),
			OutputPrice:     nonZeroPtr(mp.OutputPricePerToken),
			CacheWritePrice: nonZeroPtr(mp.CacheCreationPricePerToken),
			CacheReadPrice:  nonZeroPtr(mp.CacheReadPricePerToken),
		}
		// 计费只在支持 5m/1h 分档时使用 1h 价，其余情况 1h 价对用户无意义。
		if mp.SupportsCacheBreakdown {
			result.CacheWrite1hPrice = nonZeroPtr(mp.CacheCreation1hPrice)
		}
		if s.resolver != nil {
			sched, schedErr := s.billingService.ResolveContextPricingSchedule(ctx, s.resolver, ContextPricingScheduleInput{Model: modelName})
			if schedErr == nil && sched != nil && len(sched.Tiers) > 1 {
				result.Intervals = plazaIntervalsFromTiers(sched.Tiers)
			}
		}
		if result.InputPrice == nil && result.OutputPrice == nil && result.CacheWritePrice == nil &&
			result.CacheWrite1hPrice == nil && result.CacheReadPrice == nil && len(result.Intervals) == 0 {
			result = nil
		}
	}
	memo[modelName] = result
	return result
}

// CurrentRateFor shares peak/image precedence with request billing. Token base prices
// already include the directory's current policy; only channel time is added here.
func (g *PlazaGroup) CurrentRateFor(m *PlazaModel, base float64, at time.Time) float64 {
	if base < 0 {
		base = 0
	}
	group := &Group{SubscriptionType: g.SubscriptionType, PeakRateEnabled: g.PeakRateEnabled,
		PeakStart: g.PeakStart, PeakEnd: g.PeakEnd, PeakRateMultiplier: g.PeakRateMultiplier,
		ImageRateIndependent: g.ImageRateIndependent, ImageRateMultiplier: g.ImageRateMultiplier,
		VideoRateIndependent: g.VideoRateIndependent, VideoRateMultiplier: g.VideoRateMultiplier}
	if m.Pricing != nil && m.Pricing.BillingMode == BillingModeVideo {
		return resolveVideoRateMultiplier(&APIKey{Group: group}, base)
	}
	tokenRate, imageRate := computePeakAwareMultipliers(&APIKey{Group: group}, base, at)
	if m.Pricing != nil && m.Pricing.BillingMode == BillingModeImage {
		return imageRate
	}
	if m.Pricing == nil || m.Pricing.BillingMode == "" || m.Pricing.BillingMode == BillingModeToken {
		factor := m.CurrentTimeMultiplier
		if factor == 0 {
			factor = 1
		} // Channel time configuration only accepts positive factors.
		return tokenRate * factor
	}
	return tokenRate
}

// Legacy per-resolution video prices apply after a group model card and before channel prices.
func plazaVideoDisplayPricing(p *ChannelModelPricing, g *Group, model string) *ChannelModelPricing {
	if p == nil || g == nil || p.BillingMode != BillingModeVideo || matchGroupModelPricing(g, model) != nil {
		return p
	}
	labels := []string{VideoBillingResolution480P, VideoBillingResolution720P, VideoBillingResolution1080P}
	configured := false
	for _, label := range labels {
		configured = configured || g.GetVideoPriceForModel(model, label) != nil
	}
	if !configured {
		return p
	}
	clone := *p
	clone.Intervals = nil
	for i, label := range labels {
		price := g.GetVideoPriceForModel(model, label)
		if price == nil {
			price = p.PerRequestPrice
			for j := range p.Intervals {
				if p.Intervals[j].TierLabel == label && p.Intervals[j].PerRequestPrice != nil {
					price = p.Intervals[j].PerRequestPrice
					break
				}
			}
		}
		if price != nil {
			v := *price
			clone.Intervals = append(clone.Intervals, PricingInterval{TierLabel: label, PerRequestPrice: &v, SortOrder: i})
		}
	}
	return &clone
}
