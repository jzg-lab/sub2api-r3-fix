// Package cookiestore 实现按账号隔离的 LB 粘性 Cookie 罐。
//
// 机制：OpenAI 边缘负载均衡（Cloudflare __cflb / OpenAI __oailb）用会话粘性
// Cookie 把客户端钉在特定后端实例上；被分到弱后端 = 路由级降智（模型替换、
// 推理截断）。本罐只被动捕获业务响应里的白名单 Cookie、按需注入出站请求，
// 不产生任何额外探针流量。Cookie 过期或被信号判坏即丢弃，下一笔真实业务
// 请求自然重摇出新 Cookie。
package cookiestore

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/pluginconfig"
)

// Entry 是单个 Cookie 的存储形态。
type Entry struct {
	Name       string    `json:"name"`
	Value      string    `json:"value"`
	Attributes string    `json:"attributes,omitempty"`
	CapturedAt time.Time `json:"captured_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// AccountStatus 是单账号的状态快照（不含 Cookie 值，防泄漏）。
type AccountStatus struct {
	AccountID       int64      `json:"account_id"`
	Names           string     `json:"names"`
	NewestRemaining int64      `json:"newest_remaining_seconds"`
	Fresh           bool       `json:"fresh"`
	SignCapturedAt  time.Time  `json:"sign_captured_at"`              // 最新签捕获时刻（零值=无签）
	LifetimeStats   *SignStats `json:"sign_lifetime_stats,omitempty"` // 实测寿命统计（有样本才带）
}

// SignStats 是单账号实测签寿命的滚动统计（单位秒）。样本来自三类死亡信号
// （v0.3 计量学）：捕获覆写（LB 刷新换签）、TTL 到期清理、主动重摇
// （faster-model / 探针答错 / 手动）。会话 Cookie 无 Expires，兜底 TTL 只是
// 下限推断——实测分布才是真窗口。
type SignStats struct {
	P50     float64 `json:"p50"`
	P80     float64 `json:"p80"`
	Min     float64 `json:"min"`
	Samples int     `json:"samples"`
}

// signMeterWindow 滚动窗口大小：32 个样本足够覆盖一整天的换签节奏，
// 又能让统计快速忘记久远分布（账号被迁移/惩罚窗变化后不拖泥带水）。
const signMeterWindow = 32

// signMeter 是单账号的寿命环形账本（调用方持锁）。
type signMeter struct {
	lifetimes []float64
	next      int
}

// record 追记一条寿命（秒）。
func (m *signMeter) record(seconds float64) {
	if len(m.lifetimes) < signMeterWindow {
		m.lifetimes = append(m.lifetimes, seconds)
		return
	}
	m.lifetimes[m.next] = seconds
	m.next = (m.next + 1) % signMeterWindow
}

// stats 就地排序副本算最近秩百分位。
func (m *signMeter) stats() SignStats {
	if len(m.lifetimes) == 0 {
		return SignStats{}
	}
	sorted := append([]float64(nil), m.lifetimes...)
	sort.Float64s(sorted)
	rank := func(p float64) float64 {
		idx := int(p*float64(len(sorted)-1) + 0.5)
		return sorted[idx]
	}
	return SignStats{
		P50:     rank(0.50),
		P80:     rank(0.80),
		Min:     sorted[0],
		Samples: len(sorted),
	}
}

// Status 是整罐状态快照。
type Status struct {
	Enabled           bool            `json:"enabled"`
	Accounts          []AccountStatus `json:"accounts"`
	Captures          int64           `json:"captures"`
	Injects           int64           `json:"injects"`
	Rerolls           int64           `json:"rerolls"`
	Expiries          int64           `json:"expiries"`
	ResponsesObserved int64           `json:"responses_observed"`
}

type jar struct {
	Entries map[string]Entry
}

// Store 是并发安全的 Cookie 罐。
type Store struct {
	mu       sync.Mutex
	cfg      pluginconfig.Config
	jars     map[int64]*jar
	versions map[int64]uint64
	meters   map[int64]*signMeter
	captures int64
	injects  int64
	rerolls  int64
	expiries int64
	observed int64
	dirty    bool
}

// New 创建空罐。
func New() *Store {
	return &Store{cfg: pluginconfig.Default(), jars: map[int64]*jar{}, versions: map[int64]uint64{}, meters: map[int64]*signMeter{}}
}

// meterDeath 归档一条签的死亡（调用方持锁）。寿命 = 捕获到死亡的时间；
// 零值/未来捕获不记（防时钟跳变污染统计）。统计只含时长，无任何 Cookie 值。
func (s *Store) meterDeath(accountID int64, capturedAt, now time.Time) {
	if capturedAt.IsZero() || !now.After(capturedAt) {
		return
	}
	m := s.meters[accountID]
	if m == nil {
		m = &signMeter{}
		s.meters[accountID] = m
	}
	m.record(now.Sub(capturedAt).Seconds())
}

// SignInfo 是单账号当前签的观测（卡点排程与状态桥共用）。
type SignInfo struct {
	HasSign    bool      // 罐内是否有白名单 Cookie
	CapturedAt time.Time // 最新一条的捕获时刻（HasSign 才有意义）
	ExpiresAt  time.Time // 同一条目的 ExpiresAt（会话 Cookie 为兜底 TTL 推得）
	Stats      SignStats // 实测寿命统计（进程内滚动，重启从零积累）
}

// SignInfo 返回单账号当前签观测（零额外流量，纯内存）。
func (s *Store) SignInfo(accountID int64, now time.Time) SignInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := SignInfo{Stats: SignStats{}}
	if m := s.meters[accountID]; m != nil {
		info.Stats = m.stats()
	}
	accountJar := s.jars[accountID]
	if accountJar == nil {
		return info
	}
	for _, entry := range accountJar.Entries {
		if entry.CapturedAt.After(info.CapturedAt) {
			info.CapturedAt = entry.CapturedAt
			info.ExpiresAt = entry.ExpiresAt
		}
	}
	info.HasSign = !info.CapturedAt.IsZero()
	return info
}

// SetConfig 原子切换配置。
func (s *Store) SetConfig(cfg pluginconfig.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
}

// Config 返回当前配置副本。
func (s *Store) Config() pluginconfig.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// nameSet 返回白名单集合（调用方持锁）。
func (s *Store) nameSet() map[string]struct{} {
	set := make(map[string]struct{}, len(s.cfg.CookieNames))
	for _, name := range s.cfg.CookieNames {
		set[name] = struct{}{}
	}
	return set
}

// Capture 从一次响应的 Set-Cookie 列表里吸收白名单 Cookie。
// 返回本次实际更新的 Cookie 名（小写）。会话 Cookie（无 Expires/Max-Age）
// 用配置的兜底 TTL；空值或 Max-Age=0 视为删除指令。
func (s *Store) Capture(accountID int64, setCookies []string, now time.Time) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.captureLocked(accountID, setCookies, now)
}

// CaptureIfCurrent prevents an old response from replacing a newer business cookie.
func (s *Store) CaptureIfCurrent(accountID int64, setCookies []string, now time.Time, version uint64) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.versions[accountID] != version {
		return s.versions[accountID], false
	}
	s.captureLocked(accountID, setCookies, now)
	return s.versions[accountID], true
}

// ObserveIfCurrent applies a business response and its reroll signal atomically.
func (s *Store) ObserveIfCurrent(accountID int64, setCookies []string, now time.Time, version uint64, reroll bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.cfg.Enabled || s.versions[accountID] != version {
		return false
	}
	s.captureLocked(accountID, setCookies, now)
	if reroll {
		if _, ok := s.jars[accountID]; ok {
			s.dropLocked(accountID)
			s.rerolls++
		}
	}
	return true
}

func (s *Store) captureLocked(accountID int64, setCookies []string, now time.Time) []string {
	if accountID <= 0 || len(setCookies) == 0 {
		return nil
	}
	if !s.cfg.Enabled {
		return nil
	}
	allow := s.nameSet()
	updated := make([]string, 0, 2)
	accountJar := s.jars[accountID]
	if accountJar == nil {
		accountJar = &jar{Entries: map[string]Entry{}}
		s.jars[accountID] = accountJar
	}
	for _, raw := range setCookies {
		parts := strings.Split(raw, ";")
		name, value, ok := splitCookiePair(parts[0])
		if !ok {
			continue
		}
		if _, allowed := allow[name]; !allowed {
			continue
		}
		expires, explicit := cookieExpiry(parts[1:], now)
		if !explicit {
			expires = now.Add(time.Duration(s.cfg.DefaultTTLSeconds) * time.Second)
		}
		if value == "" || expires.Equal(now) || expires.Before(now) {
			// 删除指令或即刻过期：清掉存量（旧签寿命归档）。
			if old, exists := accountJar.Entries[name]; exists {
				s.meterDeath(accountID, old.CapturedAt, now)
				delete(accountJar.Entries, name)
				s.versions[accountID]++
				s.expiries++
				s.dirty = true
			}
			continue
		}
		capturedAt := now
		if old, exists := accountJar.Entries[name]; exists {
			if old.Value == value && now.Before(old.ExpiresAt) {
				// Renewing the same live cookie is not a new routing signature.
				capturedAt = old.CapturedAt
			} else {
				s.meterDeath(accountID, old.CapturedAt, now)
			}
		}
		accountJar.Entries[name] = Entry{
			Name:       name,
			Value:      value,
			Attributes: strings.Join(parts[1:], ";"),
			CapturedAt: capturedAt,
			ExpiresAt:  expires,
		}
		s.versions[accountID]++
		updated = append(updated, name)
		s.captures++
		s.dirty = true
	}
	return updated
}

// cookieExpiry 解析 Set-Cookie 属性里的 Expires/Max-Age。
// 返回 (过期时刻, 是否显式携带)。
func cookieExpiry(attrs []string, now time.Time) (time.Time, bool) {
	maxAge := int64(-1)
	var expires time.Time
	hasExpires := false
	for _, attr := range attrs {
		key, value, ok := strings.Cut(strings.TrimSpace(attr), "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "max-age":
			if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
				maxAge = seconds
			}
		case "expires":
			if parsed, err := http.ParseTime(strings.TrimSpace(value)); err == nil {
				expires = parsed
				hasExpires = true
			}
		}
	}
	if maxAge >= 0 {
		return now.Add(time.Duration(maxAge) * time.Second), true
	}
	if hasExpires {
		return expires, true
	}
	return time.Time{}, false
}

// freshEntries 返回仍新鲜的条目（调用方持锁）。陈旧条目顺带清理。
func (s *Store) freshEntries(accountID int64, now time.Time) []Entry {
	accountJar := s.jars[accountID]
	if accountJar == nil {
		return nil
	}
	horizon := now.Add(time.Duration(s.cfg.RefreshBeforeSeconds) * time.Second)
	fresh := make([]Entry, 0, len(accountJar.Entries))
	for name, entry := range accountJar.Entries {
		if entry.ExpiresAt.After(horizon) {
			fresh = append(fresh, entry)
			continue
		}
		s.meterDeath(accountID, entry.CapturedAt, now)
		delete(accountJar.Entries, name)
		s.versions[accountID]++
		s.expiries++
		s.dirty = true
	}
	if len(accountJar.Entries) == 0 {
		delete(s.jars, accountID)
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].Name < fresh[j].Name })
	return fresh
}

// MergeHeader 把该账号的新鲜 Cookie 合并进既有 Cookie 头（保留无关条目，
// 同名以罐内新值为准）。无新鲜 Cookie 时原样返回。
func (s *Store) MergeHeader(accountID int64, existing string, now time.Time) string {
	header, _ := s.HeaderSnapshot(accountID, existing, now)
	return header
}

// HeaderSnapshot binds a probe to the exact cookie generation it sends.
func (s *Store) HeaderSnapshot(accountID int64, existing string, now time.Time) (string, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	header := s.mergeHeaderLocked(accountID, existing, now)
	return header, s.versions[accountID]
}

func (s *Store) mergeHeaderLocked(accountID int64, existing string, now time.Time) string {
	if accountID <= 0 || !s.cfg.Enabled {
		return existing
	}
	fresh := s.freshEntries(accountID, now)
	if len(fresh) == 0 {
		return existing
	}
	pairs := map[string]string{}
	order := make([]string, 0, 8)
	for _, part := range strings.Split(existing, ";") {
		name, value, ok := splitCookiePair(part)
		if !ok {
			continue
		}
		if _, exists := pairs[name]; !exists {
			order = append(order, name)
		}
		pairs[name] = name + "=" + value
	}
	for _, entry := range fresh {
		if _, exists := pairs[entry.Name]; !exists {
			order = append(order, entry.Name)
		}
		pairs[entry.Name] = entry.Name + "=" + entry.Value
	}
	s.injects++
	out := make([]string, 0, len(order))
	for _, name := range order {
		if pairs[name] != "" {
			out = append(out, pairs[name])
		}
	}
	return strings.Join(out, "; ")
}

// Drop 丢弃某账号整罐 Cookie（手动或信号重摇）。被丢弃签的寿命归档
// （探针答错重摇 = 质量死亡信号）。
func (s *Store) Drop(accountID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropLocked(accountID)
}

// AcceptProbe linearizes acceptance and an optional reroll against cookie updates.
func (s *Store) AcceptProbe(accountID int64, version uint64, reroll bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.versions[accountID] != version {
		return false
	}
	if reroll {
		s.dropLocked(accountID)
	}
	return true
}

func (s *Store) dropLocked(accountID int64) {
	s.versions[accountID]++
	accountJar, ok := s.jars[accountID]
	if !ok {
		return
	}
	for _, entry := range accountJar.Entries {
		s.meterDeath(accountID, entry.CapturedAt, time.Now())
	}
	delete(s.jars, accountID)
	s.dirty = true
}

// Reroll 丢弃并计入重摇计数（响应信号触发；faster-model = 实时死亡信号）。
func (s *Store) Reroll(accountID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jars[accountID]; ok {
		s.dropLocked(accountID)
		s.rerolls++
	}
}

// ObserveResponse 计一次响应观测（用于状态面板的活度指标）。
func (s *Store) ObserveResponse() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observed++
}

// TakeDirty 报告并清除持久化脏标记。
func (s *Store) TakeDirty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	dirty := s.dirty
	s.dirty = false
	return dirty
}

// Status 返回脱敏状态快照。
func (s *Store) Status(now time.Time) Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	accounts := make([]AccountStatus, 0, len(s.jars))
	for accountID := range s.jars {
		names := make([]string, 0, 4)
		newestRemaining := int64(-1)
		var signCapturedAt time.Time
		horizon := now.Add(time.Duration(s.cfg.RefreshBeforeSeconds) * time.Second)
		for name, entry := range s.jars[accountID].Entries {
			names = append(names, name)
			if remaining := int64(entry.ExpiresAt.Sub(now).Seconds()); remaining > newestRemaining {
				newestRemaining = remaining
			}
			if entry.CapturedAt.After(signCapturedAt) {
				signCapturedAt = entry.CapturedAt
			}
		}
		sort.Strings(names)
		fresh := false
		for _, entry := range s.jars[accountID].Entries {
			if entry.ExpiresAt.After(horizon) {
				fresh = true
				break
			}
		}
		status := AccountStatus{
			AccountID:       accountID,
			Names:           strings.Join(names, ","),
			NewestRemaining: newestRemaining,
			Fresh:           fresh,
			SignCapturedAt:  signCapturedAt,
		}
		if m := s.meters[accountID]; m != nil {
			if stats := m.stats(); stats.Samples > 0 {
				lifetime := stats
				status.LifetimeStats = &lifetime
			}
		}
		accounts = append(accounts, status)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].AccountID < accounts[j].AccountID })
	return Status{
		Enabled:           s.cfg.Enabled,
		Accounts:          accounts,
		Captures:          s.captures,
		Injects:           s.injects,
		Rerolls:           s.rerolls,
		Expiries:          s.expiries,
		ResponsesObserved: s.observed,
	}
}

// SnapshotJSON 导出可持久化的罐快照（含 Cookie 值，只进宿主 KV，绝不进日志）。
func (s *Store) SnapshotJSON() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]map[string]Entry, len(s.jars))
	for id, accountJar := range s.jars {
		entries := make(map[string]Entry, len(accountJar.Entries))
		for name, entry := range accountJar.Entries {
			entries[name] = entry
		}
		out[strconv.FormatInt(id, 10)] = entries
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return raw
}

// RestoreJSON 从快照恢复罐内容（只接受仍有寿命的条目）。
func (s *Store) RestoreJSON(raw []byte, now time.Time) {
	var restored map[string]map[string]Entry
	if err := json.Unmarshal(raw, &restored); err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.cfg.Enabled {
		return
	}
	allow := s.nameSet()
	for id, entries := range restored {
		accountID, err := strconv.ParseInt(id, 10, 64)
		if err != nil || accountID <= 0 {
			continue
		}
		// A delayed startup restore cannot revive a dropped or replaced jar.
		if s.versions[accountID] != 0 {
			continue
		}
		accountJar := s.jars[accountID]
		if accountJar == nil {
			accountJar = &jar{Entries: map[string]Entry{}}
			s.jars[accountID] = accountJar
		}
		for name, entry := range entries {
			if _, allowed := allow[name]; !allowed {
				continue
			}
			if !entry.ExpiresAt.After(now) {
				continue
			}
			accountJar.Entries[name] = entry
			s.versions[accountID]++
		}
	}
}

// splitCookiePair 拆 "name=value"，名字小写化，非法名字拒绝。
func splitCookiePair(part string) (string, string, bool) {
	part = strings.TrimSpace(part)
	name, value, ok := strings.Cut(part, "=")
	name = strings.ToLower(strings.TrimSpace(name))
	if !ok || name == "" || strings.ContainsAny(name, " \t\r\n;") {
		return "", "", false
	}
	return name, strings.TrimSpace(value), true
}
