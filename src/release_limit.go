// 发行包分发 · 鉴权防暴破、单 IP 高频下载限次与滚动流量预算熔断器。
package main

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/netip"
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
	budgetBucketMinutes  = 24 * 60
	budgetHourMinutes    = 60
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
	adminFails  map[string]windowCounter
	ipDownloads map[string]windowCounter
	buckets     [budgetBucketMinutes]minuteTrafficBucket

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

func (g *releaseGuard) checkAdminToken(r *http.Request) (ok bool, disabled bool, rateLimited bool, retryAfter int) {
	if g.cfg.AdminToken == "" {
		return false, true, false, 0
	}
	ip := releaseClientIP(r, g.cfg.TrustProxy)
	now := g.now()

	g.mu.Lock()
	if entry, exists := g.adminFails[ip]; exists {
		if now.Sub(entry.windowStart) < adminAuthWindow {
			if entry.count >= adminAuthMaxFailures {
				rem := int(adminAuthWindow.Seconds() - now.Sub(entry.windowStart).Seconds())
				if rem < 1 {
					rem = 1
				}
				g.mu.Unlock()
				return false, false, true, rem
			}
		} else {
			delete(g.adminFails, ip)
		}
	}
	g.mu.Unlock()

	provided := bearerToken(r.Header.Get("Authorization"))
	if provided != "" && len(provided) == len(g.cfg.AdminToken) &&
		subtle.ConstantTimeCompare([]byte(provided), []byte(g.cfg.AdminToken)) == 1 {
		g.mu.Lock()
		delete(g.adminFails, ip)
		g.mu.Unlock()
		return true, false, false, 0
	}

	g.mu.Lock()
	if len(g.adminFails) >= guardMapMaxEntries {
		for k, v := range g.adminFails {
			if now.Sub(v.windowStart) >= adminAuthWindow {
				delete(g.adminFails, k)
			}
		}
	}
	if len(g.adminFails) < guardMapMaxEntries {
		entry := g.adminFails[ip]
		if entry.windowStart.IsZero() || now.Sub(entry.windowStart) >= adminAuthWindow {
			entry = windowCounter{windowStart: now, count: 1}
		} else {
			entry.count++
		}
		g.adminFails[ip] = entry
	}
	g.mu.Unlock()
	return false, false, false, 0
}

func (g *releaseGuard) allowFileDownload(ip string, archiveBytes int64) (downloadDecision, int) {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()

	// 1. 单 IP 10 分钟内高频拉包限制。
	entry, exists := g.ipDownloads[ip]
	if exists && now.Sub(entry.windowStart) < fileDownloadWindow {
		if entry.count >= fileDownloadMaxPerIP {
			rem := int(fileDownloadWindow.Seconds() - now.Sub(entry.windowStart).Seconds())
			if rem < 1 {
				rem = 1
			}
			return downloadDenyRateLimited, rem
		}
	} else {
		entry = windowCounter{windowStart: now, count: 0}
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

	// 2. 滚动 1 小时 / 24 小时归档流量预算熔断检查。
	curMin := now.Unix() / 60
	hourlyUsed, dailyUsed, oldestHourMin, oldestDayMin := g.trafficTotalsLocked(curMin)
	if hourlyUsed+archiveBytes > g.cfg.HourlyBudgetBytes {
		retry := int((oldestHourMin + budgetHourMinutes - curMin) * 60)
		if retry < 60 {
			retry = 60
		}
		return downloadDenyBudgetExceeded, retry
	}
	if dailyUsed+archiveBytes > g.cfg.DailyBudgetBytes {
		retry := int((oldestDayMin + budgetBucketMinutes - curMin) * 60)
		if retry < 60 {
			retry = 60
		}
		return downloadDenyBudgetExceeded, retry
	}

	// 3. 放行记账。
	entry.count++
	g.ipDownloads[ip] = entry
	idx := int(curMin % budgetBucketMinutes)
	if idx < 0 {
		idx += budgetBucketMinutes
	}
	if g.buckets[idx].minute != curMin {
		g.buckets[idx] = minuteTrafficBucket{minute: curMin, bytes: archiveBytes}
	} else {
		g.buckets[idx].bytes += archiveBytes
	}
	return downloadAllowed, 0
}

func (g *releaseGuard) trafficTotalsLocked(curMin int64) (hourlyUsed, dailyUsed, oldestHourMin, oldestDayMin int64) {
	hourStart := curMin - budgetHourMinutes + 1
	dayStart := curMin - budgetBucketMinutes + 1
	oldestHourMin = curMin
	oldestDayMin = curMin
	for i := range g.buckets {
		b := g.buckets[i]
		if b.bytes <= 0 || b.minute < dayStart || b.minute > curMin {
			continue
		}
		dailyUsed += b.bytes
		if b.minute < oldestDayMin {
			oldestDayMin = b.minute
		}
		if b.minute >= hourStart {
			hourlyUsed += b.bytes
			if b.minute < oldestHourMin {
				oldestHourMin = b.minute
			}
		}
	}
	return
}

func (g *releaseGuard) budgetStatus() map[string]any {
	curMin := g.now().Unix() / 60
	g.mu.Lock()
	hourlyUsed, dailyUsed, _, _ := g.trafficTotalsLocked(curMin)
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
	curMin := g.now().Unix() / 60
	g.mu.Lock()
	hourlyUsed, dailyUsed, _, _ := g.trafficTotalsLocked(curMin)
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
