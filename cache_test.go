package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/miekg/dns"
	"github.com/patrickmn/go-cache"
)

func isolateCache(t *testing.T) {
	t.Helper()
	original := dnsCache
	dnsCache = cache.New(maxCacheTTL, 0)
	t.Cleanup(func() { dnsCache = original })
}

func TestCacheRefreshAtCapacity(t *testing.T) {
	isolateCache(t)
	query := testQuery()
	addToCache(query, testAnswer(query, "192.0.2.1"))
	for i := 1; i < maxCacheItems; i++ {
		dnsCache.Set(fmt.Sprint(i), true, maxCacheTTL)
	}
	addToCache(query, testAnswer(query, "192.0.2.2"))
	got := getFromCache(query)
	if got == nil || got.Answer[0].(*dns.A).A.String() != "192.0.2.2" {
		t.Fatal("an existing entry must remain refreshable when cache is full")
	}
	other := new(dns.Msg).SetQuestion("other.example.", dns.TypeA)
	addToCache(other, testAnswer(other, "192.0.2.3"))
	if got := dnsCache.ItemCount(); got != maxCacheItems {
		t.Fatalf("want %d entries, got %d", maxCacheItems, got)
	}
	if getFromCache(other) != nil {
		t.Fatal("full cache accepted a new entry")
	}
}

func TestCacheRejectsOversizedAdditionalSection(t *testing.T) {
	isolateCache(t)
	query := testQuery()
	resp := testAnswer(query, "192.0.2.1")
	for range 20 {
		resp.Extra = append(resp.Extra, &dns.TXT{
			Hdr: dns.RR_Header{Name: "example.org.", Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60},
			Txt: []string{strings.Repeat("x", 250)},
		})
	}
	addToCache(query, resp)
	if dnsCache.ItemCount() != 0 {
		t.Fatal("cache size limit ignored the additional section")
	}
}

func TestCacheKeyNormalizesNameAndSeparatesFlags(t *testing.T) {
	query := testQuery()
	upper := query.Copy()
	upper.Question[0].Name = "EXAMPLE.org."
	if getCacheKey(query) != getCacheKey(upper) {
		t.Error("case variants should share a cache entry")
	}
	cd := query.Copy()
	cd.CheckingDisabled = true
	if getCacheKey(query) == getCacheKey(cd) {
		t.Error("checking-disabled queries must not share validated responses")
	}
	rd := query.Copy()
	rd.RecursionDesired = !query.RecursionDesired
	if getCacheKey(query) == getCacheKey(rd) {
		t.Error("recursive and non-recursive queries must not share responses")
	}
}

func TestCacheSkipsClientSpecificEDNSOptions(t *testing.T) {
	isolateCache(t)
	query := testQuery()
	query.SetEdns0(4096, false)
	query.IsEdns0().Option = []dns.EDNS0{&dns.EDNS0_LOCAL{
		Code: dns.EDNS0LOCALSTART, Data: []byte("client-specific"),
	}}
	addToCache(query, testAnswer(query, "192.0.2.1"))
	if dnsCache.ItemCount() != 0 {
		t.Fatal("client-specific options must bypass the shared cache")
	}
}

func TestCachedAdditionalTTLAges(t *testing.T) {
	isolateCache(t)
	query := testQuery()
	resp := testAnswer(query, "192.0.2.1")
	resp.Extra = []dns.RR{&dns.A{
		Hdr: dns.RR_Header{Name: "extra.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 5},
	}}
	dnsCache.Set(getCacheKey(query), &cachedResponse{
		Response: resp, CacheTime: time.Now().Add(-10 * time.Second),
	}, 50*time.Second)
	got := getFromCache(query)
	if got.Extra[0].Header().Ttl != 0 {
		t.Fatalf("want expired additional TTL 0, got %d", got.Extra[0].Header().Ttl)
	}
	if resp.Extra[0].Header().Ttl != 5 {
		t.Fatal("cache read mutated the stored message")
	}
}

func TestNegativeCacheHonorsZeroSOAMinimum(t *testing.T) {
	resp := new(dns.Msg)
	resp.Rcode = dns.RcodeNameError
	resp.Ns = []dns.RR{&dns.SOA{
		Hdr: dns.RR_Header{Name: "example.org.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 60},
	}}
	if got := calculateCacheTTL(resp); got != 0 {
		t.Fatalf("want no negative caching, got %v", got)
	}
}

func TestCacheInfoReportsRealExpiration(t *testing.T) {
	isolateCache(t)
	query := testQuery()
	addToCache(query, testAnswer(query, "192.0.2.1"))
	app := fiber.New()
	app.Get("/cache", handleCacheInfo)
	resp, err := app.Test(httptest.NewRequest("GET", "/cache", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var info CacheInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if len(info.Items) != 1 {
		t.Fatalf("want one cache entry, got %d", len(info.Items))
	}
	if ttl := info.Items[0].TTL; ttl < 0 || ttl > 60 {
		t.Fatalf("want remaining TTL in seconds between 0 and 60, got %d", ttl)
	}
}

func TestConcurrentCacheWritesRespectCapacity(t *testing.T) {
	isolateCache(t)
	for i := range maxCacheItems - 1 {
		dnsCache.Set(fmt.Sprint(i), true, maxCacheTTL)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for worker := range 2 {
		wg.Go(func() {
			<-start
			for i := range 16 {
				query := new(dns.Msg).SetQuestion(fmt.Sprintf("%d-%d.example.", worker, i), dns.TypeA)
				addToCache(query, testAnswer(query, "192.0.2.1"))
			}
		})
	}
	close(start)
	wg.Wait()
	if got := dnsCache.ItemCount(); got != maxCacheItems {
		t.Fatalf("want %d entries, got %d", maxCacheItems, got)
	}
}

func TestCacheReclaimsExpiredCapacity(t *testing.T) {
	isolateCache(t)
	for i := range maxCacheItems {
		dnsCache.Set(fmt.Sprint(i), true, time.Nanosecond)
	}
	query := testQuery()
	addToCache(query, testAnswer(query, "192.0.2.1"))
	if getFromCache(query) == nil {
		t.Fatal("expired entries prevented a new cache write")
	}
}

func TestCacheRejectsFailuresAndBoundsNegativeAliases(t *testing.T) {
	query := testQuery()
	for _, rcode := range []int{dns.RcodeServerFailure, dns.RcodeRefused} {
		resp := testAnswer(query, "192.0.2.1")
		resp.Rcode = rcode
		if calculateCacheTTL(resp) != 0 {
			t.Fatal("failed answer must not be cached")
		}
	}
	resp := new(dns.Msg).SetRcode(query, dns.RcodeNameError)
	resp.Answer = []dns.RR{&dns.CNAME{
		Hdr:    dns.RR_Header{Name: "example.org.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 300},
		Target: "missing.example.org.",
	}}
	resp.Ns = []dns.RR{&dns.SOA{
		Hdr:    dns.RR_Header{Name: "example.org.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 60},
		Minttl: 15,
	}}
	if got := calculateCacheTTL(resp); got != 15*time.Second {
		t.Fatalf("want negative alias lifetime 15s, got %v", got)
	}
	resp.Truncated = true
	if calculateCacheTTL(resp) != 0 {
		t.Fatal("truncated answer must not be cached")
	}
}
