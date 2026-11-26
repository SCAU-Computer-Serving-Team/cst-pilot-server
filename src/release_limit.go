// 发行包分发 · 鉴权防暴破、单 IP 高频下载限次与滚动流量预算熔断器。
package main

import (
	"crypto/subtle"
	"math"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	adminAuthWindow      = 10 * time.Minute
	adminAuthMaxFailures = 10
	fileDownloadWindow   = 10 * time.Minute
	fileDownloadMaxPerIP = 5
	guardMapMaxEntries   = 2048
	// 多留 1 个边界分钟桶，保守保证满整 60 分钟 / 24 小时才释放尾部流量。
	budgetBucketSlots = 24*60 + 1
	budgetHourMinutes = 60
	budgetDayMinutes  = 24 * 60
)

type downloadDecision int

const (
	downloadAllowed downloadDecision = iota
	downloadDenyRateLimited
	downloadDenyBudgetExceeded
)

type windowCounter struct {
	windowStart time.Time
	count       int
}

type minuteTrafficBucket struct {
	minute int64
	bytes  int64
}

type releaseGuard struct {
	cfg releaseConfig
	now func() time.Time

	mu          sync.Mutex
	lastNow     time.Time
	adminFails  map[string]windowCounter
	ipDownloads map[string]windowCounter
	buckets     [budgetBucketSlots]minuteTrafficBucket

	directSlots chan struct{}
}

func newReleaseGuard(cfg releaseConfig, nowFn func() time.Time) *releaseGuard {
	if nowFn == nil {
		nowFn = time.Now
	}
	slots := cfg.MaxDirectDownloads
	if slots < 1 {
		slots = defaultReleaseMaxDirectDownloads
	}
	return &releaseGuard{
		cfg:         cfg,
		now:         nowFn,
		adminFails:  make(map[string]windowCounter),
		ipDownloads: make(map[string]windowCounter),
		directSlots: make(chan struct{}, slots),
	}
}

func (g *releaseGuard) monotonicNowLocked() time.Time {
	now := g.now()
	if !g.lastNow.IsZero() && now.Before(g.lastNow) {
		return g.lastNow
	}
	g.lastNow = now
	return now
}

func ceilRetrySeconds(d time.Duration) int {
	secs := int(math.Ceil(d.Seconds()))
	if secs < 1 {
		return 1
	}
	return secs
}

// checkAdminToken 锁外执行常数时间比对，锁内原子完成锁定判断与失败计数更新。
func (g *releaseGuard) checkAdminToken(r *http.Request) (ok bool, disabled bool, rateLimited bool, retryAfter int) {
	if g.cfg.AdminToken == "" {
		return false, true, false, 0
	}
	ip := releaseClientIP(r, g.cfg.TrustProxy)
	provided := bearerToken(r.Header.Get("Authorization"))
	tokenValid := provided != "" && len(provided) == len(g.cfg.AdminToken) &&
		subtle.ConstantTimeCompare([]byte(provided), []byte(g.cfg.AdminToken)) == 1

	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.monotonicNowLocked()

	entry, exists := g.adminFails[ip]
	if exists && now.Sub(entry.windowStart) >= adminAuthWindow {
		delete(g.adminFails, ip)
		exists = false
	}
	if exists && entry.count >= adminAuthMaxFailures {
		return false, false, true, ceilRetrySeconds(adminAuthWindow - now.Sub(entry.windowStart))
	}
	if tokenValid {
		delete(g.adminFails, ip)
		return true, false, false, 0
	}

	if !exists && len(g.adminFails) >= guardMapMaxEntries {
		for k, v := range g.adminFails {
			if now.Sub(v.windowStart) >= adminAuthWindow {
				delete(g.adminFails, k)
			}
		}
		if len(g.adminFails) >= guardMapMaxEntries {
			// 表满时对未跟踪失败来源直接返回 429 防绕过。
			return false, false, true, 60
		}
	}
	if !exists {
		entry = windowCounter{windowStart: now, count: 1}
	} else {
		entry.count++
	}
	g.adminFails[ip] = entry
	if entry.count >= adminAuthMaxFailures {
		return false, false, true, ceilRetrySeconds(adminAuthWindow - now.Sub(entry.windowStart))
	}
	return false, false, false, 0
}

func (g *releaseGuard) allowFileDownload(ip string, archiveBytes int64) (downloadDecision, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.monotonicNowLocked()

	// 1. 单 IP 10 分钟内高频拉包限制。
	entry, exists := g.ipDownloads[ip]
	if exists && now.Sub(entry.windowStart) < fileDownloadWindow {
		if entry.count >= fileDownloadMaxPerIP {
			return downloadDenyRateLimited, ceilRetrySeconds(fileDownloadWindow - now.Sub(entry.windowStart))
		}
	} else {
		entry = windowCounter{windowStart: now, count: 0}
		exists = false
	}
	if !exists && len(g.ipDownloads) >= guardMapMaxEntries {
		for k, v := range g.ipDownloads {
			if now.Sub(v.windowStart) >= fileDownloadWindow {
				delete(g.ipDownloads, k)
			}
		}
		if len(g.ipDownloads) >= guardMapMaxEntries {
			return downloadDenyRateLimited, 60
		}
	}

	// 2. 滚动 1 小时 / 24 小时归档流量预算检查（保守多保留 1 个边界分钟桶）。
	curMin := now.Unix() / 60
	hourRetry := g.windowWaitSecondsLocked(curMin, budgetHourMinutes, g.cfg.HourlyBudgetBytes, archiveBytes)
	dayRetry := g.windowWaitSecondsLocked(curMin, budgetDayMinutes, g.cfg.DailyBudgetBytes, archiveBytes)
	if hourRetry > 0 || dayRetry > 0 {
		retry := hourRetry
		if dayRetry > retry {
			retry = dayRetry
		}
		return downloadDenyBudgetExceeded, retry
	}

	// 3. 放行记账。
	entry.count++
	g.ipDownloads[ip] = entry
	idx := int(curMin % budgetBucketSlots)
	if idx < 0 {
		idx += budgetBucketSlots
	}
	if g.buckets[idx].minute != curMin {
		g.buckets[idx] = minuteTrafficBucket{minute: curMin, bytes: archiveBytes}
	} else {
		g.buckets[idx].bytes += archiveBytes
	}
	return downloadAllowed, 0
}

// windowWaitSecondsLocked 计算在窗口内为 archiveBytes 腾出足够余额所需等待的保守秒数；0 表示可立即放行。
func (g *releaseGuard) windowWaitSecondsLocked(curMin int64, windowMins int64, limit int64, need int64) int {
	minMinute := curMin - windowMins // 含边界分钟，确保满整 windowMins 才退出窗口
	var used int64
	active := make([]minuteTrafficBucket, 0, 16)
	for i := range g.buckets {
		b := g.buckets[i]
		if b.bytes <= 0 || b.minute < minMinute || b.minute > curMin {
			continue
		}
		used += b.bytes
		active = append(active, b)
	}
	if used+need <= limit {
		return 0
	}
	sort.Slice(active, func(i, j int) bool { return active[i].minute < active[j].minute })
	deficit := used + need - limit
	var freed int64
	for _, b := range active {
		freed += b.bytes
		if freed >= deficit {
			expireMin := b.minute + windowMins + 1
			waitSec := int((expireMin - curMin) * 60)
			if waitSec < 60 {
				waitSec = 60
			}
			return waitSec
		}
	}
	return int(windowMins * 60)
}

func (g *releaseGuard) trafficTotalsLocked(curMin int64) (hourlyUsed, dailyUsed int64) {
	hourStart := curMin - budgetHourMinutes
	dayStart := curMin - budgetDayMinutes
	for i := range g.buckets {
		b := g.buckets[i]
		if b.bytes <= 0 || b.minute < dayStart || b.minute > curMin {
			continue
		}
		dailyUsed += b.bytes
		if b.minute >= hourStart {
			hourlyUsed += b.bytes
		}
	}
	return
}

func (g *releaseGuard) budgetStatus() map[string]any {
	g.mu.Lock()
	curMin := g.monotonicNowLocked().Unix() / 60
	hourlyUsed, dailyUsed := g.trafficTotalsLocked(curMin)
	g.mu.Unlock()
	tripped := hourlyUsed >= g.cfg.HourlyBudgetBytes || dailyUsed >= g.cfg.DailyBudgetBytes
	return map[string]any{
		"hourlyUsedBytes":   hourlyUsed,
		"hourlyBudgetBytes": g.cfg.HourlyBudgetBytes,
		"dailyUsedBytes":    dailyUsed,
		"dailyBudgetBytes":  g.cfg.DailyBudgetBytes,
		"tripped":           tripped,
	}
}

func (g *releaseGuard) isBudgetTripped() bool {
	g.mu.Lock()
	curMin := g.monotonicNowLocked().Unix() / 60
	hourlyUsed, dailyUsed := g.trafficTotalsLocked(curMin)
	g.mu.Unlock()
	return hourlyUsed >= g.cfg.HourlyBudgetBytes || dailyUsed >= g.cfg.DailyBudgetBytes
}

func (g *releaseGuard) acquireDirectSlot() bool {
	select {
	case g.directSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (g *releaseGuard) releaseDirectSlot() {
	select {
	case <-g.directSlots:
	default:
	}
}

// releaseClientIP 仅在开启 RELEASE_TRUST_PROXY=1 且 TCP 来源为回环地址时信任 X-Real-IP。
func releaseClientIP(r *http.Request, trustProxy bool) string {
	peerHost := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		peerHost = host
	}
	if trustProxy {
		if addr, err := netip.ParseAddr(peerHost); err == nil && addr.IsLoopback() {
			realIP := strings.TrimSpace(r.Header.Get("X-Real-IP"))
			if parsed, err := netip.ParseAddr(realIP); err == nil {
				return parsed.Unmap().String()
			}
		}
	}
	if addr, err := netip.ParseAddr(peerHost); err == nil {
		return addr.Unmap().String()
	}
	return peerHost
}
