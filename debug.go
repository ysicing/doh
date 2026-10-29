package main

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"sort"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/miekg/dns"
)

type VersionInfo struct {
	Version   string `json:"version"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`
	GitCommit string `json:"git_commit"`
	Platform  string `json:"platform"`
	Node      string `json:"node"`
}

type ServerStatus struct {
	Address     string  `json:"address"`
	FailCount   int     `json:"fail_count"`
	LastCheck   string  `json:"last_check"`
	LatencyMS   float64 `json:"latency_ms"`
	IsIPv6      bool    `json:"is_ipv6"`
	IsAvailable bool    `json:"is_available"`
}

type CacheStats struct {
	Hits          int64   `json:"hits"`
	Misses        int64   `json:"misses"`
	HitRate       float64 `json:"hit_rate"`
	LastResetTime string  `json:"last_reset_time"`
	Node          string  `json:"node"`
}

type CacheEntry struct {
	Key       string   `json:"key"`
	Domain    string   `json:"domain"`
	Type      string   `json:"type"`
	TTL       int64    `json:"ttl"`
	CacheTime string   `json:"cache_time"`
	ExpireAt  string   `json:"expire_at"`
	Size      int      `json:"size"` // DNS wire 长度，不代表 Go 对象占用内存。
	Answers   []string `json:"answers"`
}

type CacheInfo struct {
	Summary struct {
		CacheStats
		TotalItems    int     `json:"total_items"`
		TotalSize     int64   `json:"total_size"`
		CapacityUsage float64 `json:"capacity_usage"`
	} `json:"summary"`
	Items []CacheEntry `json:"items"`
}

func nodeName() string {
	if node := os.Getenv("NODE_IP"); node != "" {
		return node
	}
	return "unknown"
}

func cacheStats() CacheStats {
	stats := CacheStats{
		Hits: cacheHits.Load(), Misses: cacheMisses.Load(), Node: nodeName(),
		LastResetTime: statsResetTime.Load().(time.Time).Format(time.RFC3339),
	}
	if total := stats.Hits + stats.Misses; total > 0 {
		stats.HitRate = float64(stats.Hits) / float64(total) * 100
	}
	return stats
}

func handleVersion(c fiber.Ctx) error {
	return c.JSON(VersionInfo{
		Version: AppVersion, BuildTime: BuildTime, GoVersion: GoVersion,
		GitCommit: GitCommit, Platform: runtime.GOOS + "/" + runtime.GOARCH, Node: nodeName(),
	})
}

func handleStatus(c fiber.Ctx) error {
	servers := make([]ServerStatus, 0, len(upstreamServers))
	for _, server := range upstreamServers {
		host, _, _ := net.SplitHostPort(server.addr)
		server.mu.RLock()
		servers = append(servers, ServerStatus{
			Address: server.addr, FailCount: server.failCount,
			LastCheck: server.lastCheck.Format(time.RFC3339),
			LatencyMS: float64(server.latency) / float64(time.Millisecond),
			IsIPv6:    net.ParseIP(host).To4() == nil, IsAvailable: server.isAvailable,
		})
		server.mu.RUnlock()
	}
	return c.JSON(fiber.Map{
		"servers": servers,
		"cache": struct {
			CacheStats
			ItemCount int `json:"item_count"`
		}{cacheStats(), dnsCache.ItemCount()},
	})
}

func handleManualCheck(c fiber.Ctx) error {
	ctx := c.Context()
	go checkAllServers(ctx)
	return c.JSON(fiber.Map{"message": "健康检查已触发"})
}

func handleResetStats(c fiber.Ctx) error {
	action := c.Query("action", "all")
	switch action {
	case "reset":
		resetStats()
	case "clear":
		clearCache()
	case "all":
		resetStats()
		clearCache()
	default:
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "action 必须为 reset、clear 或 all"})
	}
	return c.JSON(fiber.Map{"message": "操作完成", "action": action, "time": time.Now().Format(time.RFC3339)})
}

func handleCacheInfo(c fiber.Ctx) error {
	info := CacheInfo{Items: make([]CacheEntry, 0)}
	info.Summary.CacheStats = cacheStats()
	now := time.Now()
	for key, item := range dnsCache.Items() {
		entry, ok := item.Object.(*cachedResponse)
		if !ok || len(entry.Response.Question) == 0 {
			continue
		}
		expiresAt := time.Unix(0, item.Expiration)
		if !expiresAt.After(now) {
			continue
		}
		q := entry.Response.Question[0]
		answers := make([]string, 0, len(entry.Response.Answer))
		for _, rr := range entry.Response.Answer {
			answers = append(answers, rr.String())
		}
		size := entry.Response.Len()
		info.Summary.TotalSize += int64(size)
		info.Items = append(info.Items, CacheEntry{
			Key: key, Domain: q.Name, Type: dnsTypeToString(q.Qtype),
			TTL: int64(expiresAt.Sub(now) / time.Second), Size: size, Answers: answers,
			CacheTime: entry.CacheTime.Format(time.RFC3339), ExpireAt: expiresAt.Format(time.RFC3339),
		})
	}
	info.Summary.TotalItems = len(info.Items)
	info.Summary.CapacityUsage = float64(len(info.Items)) / float64(maxCacheItems) * 100
	sort.Slice(info.Items, func(i, j int) bool { return info.Items[i].Key < info.Items[j].Key })
	return c.JSON(info)
}

func dnsTypeToString(qtype uint16) string {
	if name, ok := dns.TypeToString[qtype]; ok {
		return name
	}
	return fmt.Sprintf("TYPE%d", qtype)
}
