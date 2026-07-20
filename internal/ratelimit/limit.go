// Package ratelimit provides a simple in-memory fixed-window rate limiter.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter is a process-local fixed-window counter keyed by string.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	count  int
	window time.Time
}

// New returns an empty limiter.
func New() *Limiter {
	return &Limiter{buckets: make(map[string]*bucket)}
}

// Allow reports whether key may proceed (max events per window).
// max <= 0 means unlimited (always allow).
func (l *Limiter) Allow(key string, max int, window time.Duration) bool {
	if max <= 0 {
		return true
	}
	if window <= 0 {
		window = time.Minute
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil || now.Sub(b.window) >= window {
		l.buckets[key] = &bucket{count: 1, window: now}
		return true
	}
	if b.count >= max {
		return false
	}
	b.count++
	return true
}

// Cleanup drops idle buckets (optional periodic call).
func (l *Limiter) Cleanup(maxAge time.Duration) {
	if maxAge <= 0 {
		maxAge = time.Hour
	}
	cutoff := time.Now().Add(-maxAge)
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buckets {
		if b.window.Before(cutoff) {
			delete(l.buckets, k)
		}
	}
}
