// 冷却与熔断：Cooldown（固定时长账号级冷却）、CooldownSoftRate（账号级软冷却，对齐
// 上游重置时间或有界退避）、CooldownSoftForModel（模型级软冷却，对齐重置墙钟）、
// BlockModelBackoff/Clear（11102 负缓存）、软冷却封顶、熔断失败累计、签到解冻。
package pool

import (
	"strings"
	"time"
)

func (p *Pool) SetCredits(uid string, credits, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
		e.creditsTotal = total
		p.dirty.Store(true)
	}
}

// SetCreditsDetailed 更新账号余额/总额 + 快过架子集（签到与余额刷新时调用，
// 供选号优先消耗快过期积分）。expiring 会被钳到 [0, credits]：上游分桶异常时
// 不污染权重。
func (p *Pool) SetCreditsDetailed(uid string, credits, total, expiring int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if expiring < 0 {
			expiring = 0
		}
		if expiring > credits {
			expiring = credits
		}
		e.credits = credits
		e.creditsTotal = total
		e.creditsExpiring = expiring
		p.dirty.Store(true)
	}
}

// Cooldown 冷却账号至 now+d（即时冷却：CoolHard 余额耗尽 / CoolSoft 固定短冷却）。
//
// 重构后本入口是「固定时长的账号级冷却」，不再做两件旧事：
//   - 不再喂熔断器失败计数：熔断器只对「反复失败」（NoteError，5xx）退避。
//     软限流/余额耗尽各有权威恢复时刻（重置墙钟 / 04:00 签到），再并入"连续失败"
//     会让用户正常重试越堆越厚。熔断语义由 NoteError 唯一驱动（与 until 正交保持）。
//   - 不再做 softStreak 指数堆加：固定 d 即最终时长。CoolSoft 的精确对齐请用
//     CooldownSoftRate（有界、对齐上游重置时间）。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.coolKind = kind
		e.reason = reason
		// 非模型级冷却入口：清空模型级独立冷却表（modelCooldowns），
		// 避免上一次模型级限流的模型豁免泄漏到本次**账号级**限流上
		// （否则换模型请求会错误绕过本次冷却）。
		e.modelCooldowns = nil
		p.dirty.Store(true)
	}
}

// CooldownSoftForModel 429 的**模型级**软冷却入口（issue #31）：只影响 (账号, 模型)
// 这一组合，账号级状态（until/coolKind/softStreak）一律不动——该账号的其它模型照常
// 参与调度；到期由 pruneExpiredModelCooldowns 自动回收，该组合即回到调度。
//
//   - resetAt 非零（带「将在 … 重置」文案）→ modelCooldowns[model].Until = resetAt，
//     **直接采信上游墙钟**：不做指数堆加，也不再被账号级 soft_rate_max 截断（6004 的
//     重置窗口常达数小时，截断会提前解冻、解冻即再撞 429）。仅当显式配置了
//     cooldown.model_rate_max > 0 时才封顶（防上游异常远时刻把组合长期锁死）。
//     ResetAt 记录上游原始墙钟（台账展示用）。
//   - resetAt 零值（6004 无文案 / 文案变体）→ **模型级**有界退避：base 起按该模型
//     条目的 Hits 翻倍、封顶 model_rate_max（未配置则退回 soft_rate_max）。此前这个
//     分支做的是账号级冷却并清空模型台账——一个模型没文案就把整个账号停掉，与
//     「模型级」语义相反。
//   - model 为空（请求没带模型名，无法归因到模型）→ 退化为账号级 CooldownSoftRate。
func (p *Pool) CooldownSoftForModel(uid string, base time.Duration, resetAt time.Time, model, reason string) {
	if model == "" {
		p.CooldownSoftRate(uid, base, resetAt, reason)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	now := time.Now()
	if e.modelCooldowns == nil {
		e.modelCooldowns = map[string]modelCooldown{}
	}
	if !resetAt.IsZero() {
		e.modelCooldowns[model] = modelCooldown{
			Until:   p.cappedModelUntilLocked(now, resetAt),
			ResetAt: resetAt,
			Reason:  reason,
		}
		p.dirty.Store(true)
		return
	}
	// 无重置文案：模型级有界退避。**已在冷却中**（同模型条目未到期）时不推进/不延长，
	// 避免兜底探测把冷却越堆越厚（口径与 CooldownSoftRate 的"冷却中不翻倍"一致）。
	if mc, ok := e.modelCooldowns[model]; ok && !mc.Until.IsZero() && now.Before(mc.Until) {
		return
	}
	hits := e.modelCooldowns[model].Hits + 1
	d := base
	for i := 1; i < hits && i <= softStreakShiftMax; i++ {
		d <<= 1
	}
	if capD := p.modelBackoffCapLocked(); capD > 0 && (d > capD || d <= 0) {
		d = capD // d<=0：左移溢出，同样按封顶兜底
	}
	e.modelCooldowns[model] = modelCooldown{
		Until:  now.Add(d),
		Reason: reason,
		Hits:   hits,
	}
	p.dirty.Store(true)
}

// cappedModelUntilLocked 模型级冷却截止：默认**直接采信上游重置墙钟**（model_rate_max
// 为 0 = 不封顶）；显式配置了上限才截断。已过期/时钟回拨给最小冷却（1ms），
// 避免"冷却在写入瞬间就到期"导致同请求内反复重试。
// 调用方必须已持有 p.mu。
func (p *Pool) cappedModelUntilLocked(now, resetAt time.Time) time.Time {
	if p.modelRateMax > 0 {
		if capped := now.Add(p.modelRateMax); resetAt.After(capped) {
			return capped
		}
	}
	if resetAt.After(now) {
		return resetAt
	}
	return now.Add(time.Millisecond)
}

// modelBackoffCapLocked 模型级**无文案**退避的封顶：配置了 model_rate_max 用它，
// 否则退回账号级 soft_rate_max（默认 2h）——保证退避不会无上限增长。
// 调用方必须已持有 p.mu。
func (p *Pool) modelBackoffCapLocked() time.Duration {
	if p.modelRateMax > 0 {
		return p.modelRateMax
	}
	return p.softRateMaxOr()
}

// ModelBlockSummary 报告某模型在池中的限额情况（只读，供 handler 在该模型无可用
// 账号时返回 429 + 精确原因）：
//   - limited：因该模型处于模型级冷却（未到期）的账号数；
//   - earliest：这些限额账号里最早恢复的时刻（无则零值）；
//   - healthyIgnoring：**忽略模型限额后**账号级仍可用的账号数（healthy + 在途未满，
//     禁用的不计）。它是"号本身是好的、只是这个模型都被上游限额了"的判据。
func (p *Pool) ModelBlockSummary(model string) (limited int, earliest time.Time, healthyIgnoring int) {
	if model == "" {
		return 0, time.Time{}, 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if e.disabled {
			continue
		}
		if mc, ok := e.modelCooldowns[model]; ok && !mc.Until.IsZero() && now.Before(mc.Until) {
			limited++
			if earliest.IsZero() || mc.Until.Before(earliest) {
				earliest = mc.Until
			}
		}
		if e.healthy(now) && !p.inFlightFull(e) {
			healthyIgnoring++
		}
	}
	return limited, earliest, healthyIgnoring
}

// modelBlock TTL 常量（11102 负缓存退避）：
// 首次命中 6h；半开到期后允许放行重试，再次命中 TTL = base × 2^min(hits-1, shift)；
// 封顶 24h（最多一天再试一次）。该模型请求成功即由 BlockModelClear 清除。
const (
	modelBlockBaseTTL = 6 * time.Hour
	modelBlockShift   = 4
	modelBlockMaxTTL  = 24 * time.Hour
)

// BlockModelBackoff 11102「该后端无此模型」的 (账号, 模型) 负缓存入口
// （handler.applyErrorPolicy 调用）。复用 modelCooldowns 机制（不新建平行状态）：
// 写 modelCooldowns[model]，Until 为指数退避 TTL，选号侧 healthyForModel 自动对该
// 账号避开该模型。
//
// 语义与 6004 正交：6004 是「模型被限流、对齐重置墙钟」，本入口是「官方确定该后端
// 无此模型、重试无意义，只能换模型/换账号」。resetAt 无需传（11102 无重置文案），
// ResetAt 保持零值，与 6004 台账共用 Until 判定——11102 条目会以 11102 reason 出现在
// /status 台账，运维可见。
func (p *Pool) BlockModelBackoff(uid, model, reason string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	now := time.Now()
	hits := 0
	if e.modelCooldowns != nil {
		hits = e.modelCooldowns[model].Hits
	}
	hits++
	ttl := modelBlockBaseTTL
	if d := ttl * (1 << uint(min(hits-1, modelBlockShift))); d < modelBlockMaxTTL {
		ttl = d
	} else {
		ttl = modelBlockMaxTTL
	}
	if e.modelCooldowns == nil {
		e.modelCooldowns = map[string]modelCooldown{}
	}
	e.modelCooldowns[model] = modelCooldown{
		Until:  now.Add(ttl),
		Reason: reason,
		Hits:   hits,
	}
	p.dirty.Store(true)
}

// BlockModelClear 清除 (账号, 模型) 的 11102 负缓存条目（该模型实测又通了）。半开探测
// 或正常请求对该模型成功后调用（handler 成功路径）。只清 11102 条目、不碰 6004 独立
// 冷却表——6004 有自身上游重置墙钟语义，成功不该抹掉。reason 前缀判定区分两者：
// 11102 条目的 reason 恒以 "11102" 开头（见 upstream.BlockModelReason）。
func (p *Pool) BlockModelClear(uid, model string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || len(e.modelCooldowns) == 0 {
		return
	}
	mc, exists := e.modelCooldowns[model]
	if !exists || !strings.HasPrefix(mc.Reason, "11102") {
		return
	}
	delete(e.modelCooldowns, model)
	if len(e.modelCooldowns) == 0 {
		e.modelCooldowns = nil
	}
	p.dirty.Store(true)
}

// CooldownSoftRate 429/限流文案的**账号级**软冷却入口（handler.applyErrorPolicy 调用）。
//
// 语义：
//   - resetAt 非零（上游带权威重置时间，无论 6004 还是 11140 rate-limiting）→
//     账号级直到该墙钟（截断到 softRateMax，绝不指数堆加）；**不**在
//     modelCooldowns 记模型（账号级语义，不产生切模型豁免——普通账号级限流不该
//     因切模型绕过）。
//   - resetAt 零值且**不在冷却中**（首次/恢复后的新限流）→ 有界退避：按 softStreak
//     指数退避并封顶 softRateMax。softStreak 只在真正进入一次新冷却时计数，由
//     NoteSuccess/reviveCoolingLocked 清零（既有恢复语义）。
//   - resetAt 零值且**已在软冷却中**（兜底探测再次撞 429）→ 不推进 streak、不延长
//     until：用户重试/并发兜底探测不得把冷却越堆越厚——这正是旧实现「越重试越冷、
//     全池被推到 2h 封顶」的元凶（每次探测都 softStreak++ 指数翻倍）。
func (p *Pool) CooldownSoftRate(uid string, base time.Duration, resetAt time.Time, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		now := time.Now()
		if !resetAt.IsZero() {
			e.until = p.cappedSoftUntilLocked(now, resetAt)
		} else if e.coolKind != CoolSoft || !now.Before(e.until) {
			// 新限流（不在有效软冷却中）：推进有界退避；兜底探测（仍在软冷却中）不翻倍。
			d := p.softDurationLocked(base, e.softStreak+1)
			e.softStreak++
			e.until = now.Add(d)
		}
		e.coolKind = CoolSoft
		e.reason = reason
		e.modelCooldowns = nil // 账号级软冷却：清空模型豁免（切模型不绕过）
		p.dirty.Store(true)
	}
}

// cappedSoftUntilLocked 把上游重置墙钟截断到 softRateMax（now+softRateMax 与 resetAt
// 取较早者）。resetAt 已过期（时钟偏移/文案过期）时时长钳到时间零点附近，立即恢复。
// 调用方必须已持有 p.mu。
func (p *Pool) cappedSoftUntilLocked(now, resetAt time.Time) time.Time {
	cap := now.Add(p.softRateMaxOr())
	if resetAt.After(cap) {
		return cap
	}
	if resetAt.After(now) {
		return resetAt
	}
	return now.Add(time.Millisecond)
}

// softRateMaxOr 返回生效的 softRateMax（未注入时按默认 2h），供封顶计算。
// 调用方必须已持有 p.mu。
func (p *Pool) softRateMaxOr() time.Duration {
	if p.softRateMax > 0 {
		return p.softRateMax
	}
	return defaultSoftRateMax
}

// softDurationLocked 按连续软冷却次数把基数 d 指数放大：d << (streak-1)，封顶 softRateMax。
// softRateMax 未注入（<=0）时按 defaultSoftRateMax 算。streak<=1 时原样返回 d。
// 左移位数受 softStreakShiftMax 限制，避免 streak 极大时移位溢出。
// 调用方必须已持有 p.mu。
func (p *Pool) softDurationLocked(d time.Duration, streak int) time.Duration {
	if streak <= 1 {
		return d
	}
	shift := streak - 1
	if shift > softStreakShiftMax {
		shift = softStreakShiftMax
	}
	d <<= shift
	max := p.softRateMax
	if max <= 0 {
		max = defaultSoftRateMax
	}
	if d > max || d <= 0 { // d<=0：左移溢出成负数/零，同样按封顶兜底
		d = max
	}
	return d
}

// recordBreakerFailureLocked 累计一次熔断失败；达到阈值则按指数退避熔断。
// 熔断与冷却（until）解耦：冷却按错误类别给固定时长，熔断则对"反复失败"逐次加长封禁。
// 调用方必须已持有 p.mu。
func (p *Pool) recordBreakerFailureLocked(e *entry) {
	e.fails++
	if e.fails < p.breakerThreshold {
		return
	}
	d := p.breakerCooldown
	for i := 0; i < e.retryCount; i++ {
		d *= 2
		if d >= p.breakerCooldownMax {
			d = p.breakerCooldownMax
			break
		}
	}
	// 触发熔断：重置失败计数供下一轮重新累计；retryCount 递增放大退避指数。
	e.fails = 0
	e.retryCount++
	e.breakerUntil = time.Now().Add(d)
}

// CooldownUntilTomorrow4AM 冷却到下一个 04:00（本地时区）。
// 用于 ErrHardCredit 场景：积分耗尽账号等签到任务（09:00/21:00）恢复。
func (p *Pool) CooldownUntilTomorrow4AM(uid string, reason string) {
	now := time.Now()
	p.Cooldown(uid, CoolHard, nextDay4AM(now).Sub(now), reason)
}

// nextDay4AM 返回 now 之后最近的一个 04:00（与 now 同一时区）。
// now 在当天 04:00 之前（凌晨 00:00~04:00）时返回当天 04:00——此时签到尚未执行，
// 该窗内触发的硬冷却等当天签到即可恢复；返回次日会白冷约一天。
// 04:00 整及之后返回次日 04:00。
// time.Date 对日溢出自动进位（月末→下月 1 号、年末→下年 1 号），天然覆盖跨日/跨月/跨年。
func nextDay4AM(now time.Time) time.Time {
	if now.Hour() < 4 {
		return time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
	}
	return time.Date(now.Year(), now.Month(), now.Day()+1, 4, 0, 0, 0, now.Location())
}

// ReenableIfCredits 签到/余额刷新后解冻：仅当 remain > 0 且账号非禁用时，解冻
// **余额耗尽冷却**（CoolHard）。软限流（CoolSoft）与模型级台账（modelCooldowns）
// 不在此清除——它们的恢复证据是上游重置墙钟到期，不是余额恢复（余额刷新周期
// 任务每 5 分钟到达这里，全清会把限流冷却实际寿命压到一个刷新周期内）。
// 注意：不碰熔断器——熔断到期（breakerUntil 过期）或下次 chat 成功（NoteSuccess）才恢复。
// reviveCoolingLocked 已迁至 transition.go（状态机迁移唯一权威实现）。
