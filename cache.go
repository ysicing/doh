package main

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"github.com/patrickmn/go-cache"
)

const (
	maxCacheTTL     = 5 * time.Minute
	cleanupInterval = 30 * time.Second
	maxCacheItems   = 10000
	maxItemSize     = maxDNSMessageSize
)

var (
	dnsCache       = cache.New(maxCacheTTL, cleanupInterval)
	cacheWriteMu   sync.Mutex
	cacheHits      atomic.Int64
	cacheMisses    atomic.Int64
	statsResetTime atomic.Value
)

type cachedResponse struct {
	Response  *dns.Msg
	CacheTime time.Time
}

func init() {
	statsResetTime.Store(time.Now())
}

func getCacheKey(msg *dns.Msg) string {
	if msg == nil || msg.Response || msg.Opcode != dns.OpcodeQuery || len(msg.Question) != 1 || msg.IsTsig() != nil {
		return ""
	}
	do := false
	if opt := msg.IsEdns0(); opt != nil {
		// ECS、Cookie 等选项携带客户端上下文，不放入公共缓存。
		if len(opt.Option) != 0 {
			return ""
		}
		do = opt.Do()
	}
	q := msg.Question[0]
	return fmt.Sprintf("%d@%d@%t@%t@%t@%s",
		q.Qclass, q.Qtype, do, msg.CheckingDisabled, msg.RecursionDesired, strings.ToLower(q.Name))
}

func getFromCache(msg *dns.Msg) *dns.Msg {
	key := getCacheKey(msg)
	if key != "" {
		if cached, expiresAt, found := dnsCache.GetWithExpiration(key); found {
			entry := cached.(*cachedResponse)
			resp := entry.Response.Copy()
			elapsed := uint32(max(0, time.Since(entry.CacheTime)/time.Second))
			remaining := uint32(max(0, time.Until(expiresAt)/time.Second))
			adjustTTL(resp.Answer, elapsed, remaining)
			adjustTTL(resp.Ns, elapsed, remaining)
			adjustTTL(resp.Extra, elapsed, remaining)
			resp.Id = msg.Id
			resp.Question = append([]dns.Question(nil), msg.Question...)
			cacheHits.Add(1)
			return resp
		}
	}
	cacheMisses.Add(1)
	return nil
}

func addToCache(msg *dns.Msg, resp *dns.Msg) {
	key := getCacheKey(msg)
	ttl := calculateCacheTTL(resp)
	if key == "" || ttl <= 0 || resp.Len() > maxItemSize || resp.IsTsig() != nil {
		return
	}
	if opt := resp.IsEdns0(); opt != nil && len(opt.Option) != 0 {
		return
	}
	entry := &cachedResponse{Response: resp.Copy(), CacheTime: time.Now()}

	// 容量检查和写入必须串行，防止并发 miss 突破上限；满时仍允许更新已有 key。
	cacheWriteMu.Lock()
	defer cacheWriteMu.Unlock()
	if _, found := dnsCache.Get(key); !found && dnsCache.ItemCount() >= maxCacheItems {
		dnsCache.DeleteExpired()
		if dnsCache.ItemCount() >= maxCacheItems {
			return
		}
	}
	dnsCache.Set(key, entry, ttl)
}

func calculateCacheTTL(resp *dns.Msg) time.Duration {
	if resp == nil || resp.Truncated || (resp.Rcode != dns.RcodeSuccess && resp.Rcode != dns.RcodeNameError) {
		return 0
	}
	ttl := maxCacheTTL
	if resp.Rcode == dns.RcodeNameError || len(resp.Answer) == 0 {
		ttl = negativeCacheTTL(resp)
	}
	for _, rr := range resp.Answer {
		ttl = min(ttl, time.Duration(rr.Header().Ttl)*time.Second)
	}
	return ttl
}

func negativeCacheTTL(resp *dns.Msg) time.Duration {
	ttl := maxCacheTTL
	found := false
	for _, rr := range resp.Ns {
		if soa, ok := rr.(*dns.SOA); ok {
			ttl = min(ttl, time.Duration(min(soa.Hdr.Ttl, soa.Minttl))*time.Second)
			found = true
		}
	}
	if !found {
		return 0
	}
	return ttl
}

func adjustTTL(records []dns.RR, elapsed, remaining uint32) {
	for _, rr := range records {
		header := rr.Header()
		if header.Rrtype == dns.TypeOPT {
			continue
		}
		if header.Ttl <= elapsed {
			header.Ttl = 0
		} else {
			header.Ttl = min(header.Ttl-elapsed, remaining)
		}
	}
}

func resetStats() time.Time {
	cacheHits.Store(0)
	cacheMisses.Store(0)
	now := time.Now()
	statsResetTime.Store(now)
	return now
}

func clearCache() {
	cacheWriteMu.Lock()
	defer cacheWriteMu.Unlock()
	dnsCache.Flush()
}
