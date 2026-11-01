package httpapi

import (
	"sync"
	"time"
)

// loginLimiter 限制同一来源的登录失败次数，避免令牌被在线爆破。
// 计数与封禁都在内存里，进程重启后清零（可接受）。
type loginLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	entries map[string]*loginEntry
	now     func() time.Time
}

type loginEntry struct {
	failures     int
	resetAt      time.Time
	blockedUntil time.Time
}

func newLoginLimiter(limit int, window time.Duration) *loginLimiter {
	if limit <= 0 {
		limit = 10
	}
	if window <= 0 {
		window = 5 * time.Minute
	}
	return &loginLimiter{
		limit:   limit,
		window:  window,
		entries: map[string]*loginEntry{},
		now:     time.Now,
	}
}

// check 返回是否允许尝试；被限制时给出剩余等待时间。
func (l *loginLimiter) check(keys ...string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.purgeLocked(now)
	var wait time.Duration
	for _, k := range keys {
		e := l.entries[k]
		if e == nil {
			continue
		}
		if now.Before(e.blockedUntil) {
			if d := e.blockedUntil.Sub(now); d > wait {
				wait = d
			}
		}
	}
	return wait == 0, wait
}

// fail 记录一次失败；达到阈值后封禁一个窗口。
func (l *loginLimiter) fail(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for _, k := range keys {
		e := l.entries[k]
		if e == nil || (!e.resetAt.IsZero() && now.After(e.resetAt) && !now.Before(e.blockedUntil)) {
			e = &loginEntry{resetAt: now.Add(l.window)}
			l.entries[k] = e
		}
		e.failures++
		if e.failures >= l.limit {
			e.failures = 0
			e.blockedUntil = now.Add(l.window)
			e.resetAt = now.Add(l.window)
		}
	}
}

// reset 在登录成功后清除计数。
func (l *loginLimiter) reset(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		delete(l.entries, k)
	}
}

// purgeLocked 清理已经过期且未封禁的条目，避免长期运行时 map 无限增长。
func (l *loginLimiter) purgeLocked(now time.Time) {
	if len(l.entries) < 1024 {
		return
	}
	for k, e := range l.entries {
		if now.After(e.blockedUntil) && (e.resetAt.IsZero() || now.After(e.resetAt)) {
			delete(l.entries, k)
		}
	}
}
