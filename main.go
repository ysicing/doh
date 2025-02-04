package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ergoapi/util/environ"
	"github.com/ergoapi/util/exnet"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/log"
	"github.com/gofiber/fiber/v2/middleware/healthcheck"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/monitor"
	"github.com/gofiber/fiber/v2/middleware/requestid"

	"github.com/miekg/dns"
	"github.com/patrickmn/go-cache"
	"github.com/robfig/cron/v3"
)

var dnsClient = &dns.Client{
	Timeout:      queryTimeout,
	ReadTimeout:  queryTimeout,
	WriteTimeout: queryTimeout,
	Dialer: &net.Dialer{
		Timeout:   queryTimeout,
		KeepAlive: 30 * time.Second,
	},
	SingleInflight: true,
}

// DNSServer 表示一个上游DNS服务器的配置和状态
type DNSServer struct {
	addr        string
	priority    int
	weight      float64   // 权重，基于历史响应时间动态调整
	failCount   int       // 连续失败次数
	lastCheck   time.Time // 最后一次检查时间
	nextCheck   time.Time // 新增：下次检查时间
	avgLatency  float64   // 平均延迟（毫秒）
	isIPv6      bool      // 是否是IPv6服务器
	isAvailable bool      // 服务器是否可用
	mu          sync.RWMutex
}

var (
	upstreamServers []*DNSServer
	serverMu        sync.RWMutex
	dnsCache        *cache.Cache
	cacheHits       int64     // 缓存命中次数
	cacheMisses     int64     // 缓存未命中次数
	statsResetTime  time.Time // 统计重置时间

	// 版本信息变量,通过编译时注入
	AppVersion = "1.0.2"
	BuildTime  = "unknown"
	GitCommit  = ""
	GoVersion  = runtime.Version()
)

const (
	// DNS相关配置
	maxRetries   = 2
	queryTimeout = 3 * time.Second
	totalTimeout = 5 * time.Second

	// 健康检查相关
	healthTimeout       = 1 * time.Second
	healthCheckInterval = 1 * time.Minute
	healthCheckTimeout  = 2 * time.Second
	healthCheckDomain   = "google.com."
	baseCheckDelay      = 30 * time.Second
	maxCheckDelay       = 30 * time.Minute

	// 缓存相关
	defaultCacheTTL = 5 * time.Minute
	minCacheTTL     = 10 * time.Second
	maxCacheTTL     = 300 * time.Second
	cleanupInterval = 30 * time.Second
	maxCacheItems   = 10000 // 最大缓存条目数
	maxItemSize     = 4096  // 单个缓存项的最大大小（字节）

	// 统计相关
	statsResetCron = "0 0 * * 0"

	// 权重计算相关
	baseWeight = 1000.0
	minWeight  = 0.1
	maxWeight  = 100.0
)

// VersionInfo 表示版本信息的结构体
type VersionInfo struct {
	Version   string `json:"version"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`
	GitCommit string `json:"git_commit"`
	Platform  string `json:"platform,omitempty"`
	Node      string `json:"node,omitempty"`
}

// CacheEntry 表示缓存条目的结构体
type CacheEntry struct {
	Key       string   `json:"key"`
	Domain    string   `json:"domain"`
	Type      string   `json:"type"`
	TTL       int64    `json:"ttl"`        // 剩余TTL（秒）
	CacheTime string   `json:"cache_time"` // 缓存时间
	ExpireAt  string   `json:"expire_at"`  // 过期时间
	Size      int      `json:"size"`       // 估算大小（字节）
	Answers   []string `json:"answers"`    // 解析结果
}

// CacheInfo 表示缓存信息的结构体
type CacheInfo struct {
	Summary struct {
		TotalItems    int     `json:"total_items"`
		TotalSize     int64   `json:"total_size"`   // 总大小（字节）
		MemoryUsage   float64 `json:"memory_usage"` // 内存使用率
		Hits          int64   `json:"hits"`
		Misses        int64   `json:"misses"`
		HitRate       float64 `json:"hit_rate"`
		LastResetTime string  `json:"last_reset_time"`
		Node          string  `json:"node,omitempty"`
	} `json:"summary"`
	Items []CacheEntry `json:"items"`
}

func init() {
	upstreamServers = []*DNSServer{
		newDNSServer("8.8.8.8:53", 99, false),
		newDNSServer("8.8.4.4:53", 99, false),
		newDNSServer("[2001:4860:4860::8888]:53", 99, true),
		newDNSServer("[2001:4860:4860::8844]:53", 99, true),
		newDNSServer("1.1.1.1:53", 98, false),
		newDNSServer("1.0.0.1:53", 98, false),
		newDNSServer("[2606:4700:4700::1111]:53", 98, true),
		newDNSServer("[2606:4700:4700::1001]:53", 98, true),
	}
	rand.Seed(time.Now().UnixNano())
	dnsCache = cache.New(defaultCacheTTL, cleanupInterval)
	dnsCache.OnEvicted(func(key string, value interface{}) {
		log.Debugf("缓存条目已过期或被驱逐 - Key: %s", key)
	})
	go monitorCacheSize()
	statsResetTime = time.Now()
}

func newDNSServer(addr string, priority int, isIPv6 bool) *DNSServer {
	now := time.Now()
	return &DNSServer{
		addr:        addr,
		priority:    priority,
		weight:      1.0,
		isIPv6:      isIPv6,
		isAvailable: true,
		lastCheck:   now,
		nextCheck:   now, // 初始化下次检查时间
	}
}

// 更新服务器状态
func (s *DNSServer) updateStatus(latency time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastCheck = time.Now()

	if err != nil {
		s.failCount++
		s.isAvailable = false
		s.weight = minWeight

		// 计算下次检查延迟
		delay := time.Duration(s.failCount) * baseCheckDelay
		if delay > maxCheckDelay {
			delay = maxCheckDelay
		}
		s.nextCheck = time.Now().Add(delay)

		log.Warnf("上游DNS %s 探测失败, 错误: %v, 连续失败次数: %d",
			s.addr, err, s.failCount)
	} else {
		if !s.isAvailable {
			log.Infof("上游DNS %s 恢复", s.addr)
		}

		s.isAvailable = true
		s.failCount = 0
		s.nextCheck = time.Now().Add(healthCheckInterval)

		// 更新平均延迟
		alpha := 0.2
		newLatency := float64(latency.Milliseconds())
		if s.avgLatency == 0 {
			s.avgLatency = newLatency
		} else {
			s.avgLatency = alpha*newLatency + (1-alpha)*s.avgLatency
		}
		s.weight = s.calculateWeight(s.avgLatency)

		log.Infof("上游DNS %s, 延迟: %v, 当前权重: %.2f",
			s.addr, latency, s.weight)
	}
}

// 获取当前可用的服务器列表，使用加权随机算法选择
func getAvailableServers() []*DNSServer {
	serverMu.RLock()
	defer serverMu.RUnlock()

	var available []*DNSServer
	var totalWeight float64

	// 第一次遍历：收集可用服务器和计算总权重
	for _, server := range upstreamServers {
		server.mu.RLock()
		if server.isAvailable {
			available = append(available, server)
			totalWeight += server.weight
		}
		server.mu.RUnlock()
	}

	// 按照权重随机排序
	sort.Slice(available, func(i, j int) bool {
		si, sj := available[i], available[j]
		si.mu.RLock()
		sj.mu.RLock()
		defer si.mu.RUnlock()
		defer sj.mu.RUnlock()

		// 如果优先级不同，高优先级的始终排在前面
		if si.priority != sj.priority {
			return si.priority > sj.priority
		}

		// 在相同优先级内，使用加权随机
		wi := si.weight / totalWeight
		wj := sj.weight / totalWeight

		// 生成随机数进行加权比较
		return rand.Float64() < wi/(wi+wj)
	})

	// 对结果进行分组，保证相同优先级的服务器在一起
	var result []*DNSServer
	if len(available) > 0 {
		currentPriority := available[0].priority
		var samePriorityServers []*DNSServer

		for _, server := range available {
			if server.priority != currentPriority {
				// 对相同优先级的服务器进行加权随机排序
				result = append(result, samePriorityServers...)
				samePriorityServers = []*DNSServer{server}
				currentPriority = server.priority
			} else {
				samePriorityServers = append(samePriorityServers, server)
			}
		}
		// 添加最后一组
		result = append(result, samePriorityServers...)
	}

	return result
}

// 启动健康检查
func startHealthCheck() {
	ticker := time.NewTicker(healthCheckInterval)
	go func() {
		// 启动时立即进行一次检查，使用 true 来忽略 nextCheck 时间
		checkAllServers(true)

		for range ticker.C {
			// 定时检查时使用 false，遵守 nextCheck 时间限制
			checkAllServers(false)
		}
	}()
}

func checkAllServers(ignoreNextCheck bool) {
	ctx, cancel := context.WithTimeout(context.Background(), healthCheckTimeout)
	defer cancel()

	// 创建健康检查用的DNS查询
	msg := new(dns.Msg)
	msg.SetQuestion(healthCheckDomain, dns.TypeA)
	msg.RecursionDesired = true

	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup

	serverMu.RLock()
	for _, server := range upstreamServers {
		wg.Add(1)
		go func(s *DNSServer) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			checkServer(ctx, s, msg, ignoreNextCheck)
		}(server)
	}
	serverMu.RUnlock()

	wg.Wait()
	logHealthCheckResults()
}

// checkServer 检查单个服务器的健康状态
// ignoreNextCheck 为 true 时忽略 nextCheck 时间限制
func checkServer(ctx context.Context, server *DNSServer, msg *dns.Msg, ignoreNextCheck bool) {
	// 添加消息有效性检查
	if msg == nil || len(msg.Question) == 0 {
		log.Warn("健康检查:无效的DNS查询消息")
		return
	}

	server.mu.Lock()
	if !ignoreNextCheck && !time.Now().After(server.nextCheck) {
		server.mu.Unlock()
		return
	}
	server.mu.Unlock()

	_, rtt, err := server.query(msg.Copy(), ctx) // 使用消息的副本
	server.updateStatus(rtt, err)
}

func logHealthCheckResults() {
	serverMu.RLock()
	defer serverMu.RUnlock()

	var stats struct {
		Available   int
		Unavailable int
		TotalWeight float64
		Servers     []struct {
			Addr    string
			Status  string
			Weight  float64
			Latency float64
		}
	}

	for _, server := range upstreamServers {
		server.mu.RLock()
		serverStat := struct {
			Addr    string
			Status  string
			Weight  float64
			Latency float64
		}{
			Addr:    server.addr,
			Status:  map[bool]string{true: "可用", false: "不可用"}[server.isAvailable],
			Weight:  server.weight,
			Latency: server.avgLatency,
		}

		if server.isAvailable {
			stats.Available++
			stats.TotalWeight += server.weight
		} else {
			stats.Unavailable++
		}
		stats.Servers = append(stats.Servers, serverStat)
		server.mu.RUnlock()
	}

	log.Debugf("健康检查结果：可用=%d 不可用=%d 总权重=%.2f\n详细信息：%+v",
		stats.Available, stats.Unavailable, stats.TotalWeight, stats.Servers)
}

// 启动统计自动重置
func startStatsReset() {
	c := cron.New()

	// 添加定时任务
	_, err := c.AddFunc(statsResetCron, func() {
		atomic.StoreInt64(&cacheHits, 0)
		atomic.StoreInt64(&cacheMisses, 0)
		statsResetTime = time.Now()
		log.Infof("缓存统计已自动重置 - 时间: %s", statsResetTime.Format("2006-01-02 15:04:05"))
	})

	if err != nil {
		log.Errorf("添加统计重置定时任务失败: %v", err)
		return
	}

	c.Start()
}

func handle(c *fiber.Ctx) error {
	var dnsMsg []byte
	var err error

	if c.Method() == "GET" {
		dnsParam := c.Query("dns")
		if dnsParam == "" {
			return c.Redirect("/deepseek", 302)
		}
		dnsMsg, err = base64.RawURLEncoding.DecodeString(dnsParam)
		if err != nil {
			log.Warnf("base64解码失败: %v", err)
			return c.Status(400).JSON(fiber.Map{
				"error": "请求参数无效",
			})
		}
	} else {
		dnsMsg = c.Body()
	}

	// 添加消息大小检查
	if len(dnsMsg) > 4096 {
		log.Warnf("消息过大: %d bytes", len(dnsMsg))
		return c.Status(400).JSON(fiber.Map{
			"error": "请求数据过大",
		})
	}

	msg := new(dns.Msg)
	if err := msg.Unpack(dnsMsg); err != nil {
		log.Warnf("消息解析失败: %v", err)
		return c.Status(400).JSON(fiber.Map{
			"error": "请求格式无效",
		})
	}

	if len(msg.Question) == 0 {
		log.Warn("查询没有Question段")
		return c.Status(400).JSON(fiber.Map{
			"error": "请求格式无效",
		})
	}

	msg.SetEdns0(4096, false)

	resp, err := forwardToUpstreamDNS(msg)
	if err != nil {
		log.Errorf("查询失败: %v", err)
		return c.Status(500).JSON(fiber.Map{
			"error": "服务暂时不可用",
		})
	}

	if resp == nil {
		log.Error("收到空响应")
		return c.Status(500).JSON(fiber.Map{
			"error": "服务暂时不可用",
		})
	}

	resp.Id = msg.Id

	packed, err := resp.Pack()
	if err != nil {
		log.Errorf("响应打包失败: %v", err)
		return c.Status(500).JSON(fiber.Map{
			"error": "服务暂时不可用",
		})
	}

	if len(packed) > 4096 {
		log.Warnf("响应过大: %d bytes", len(packed))
		resp.Truncate(4096)
		packed, err = resp.Pack()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{
				"error": "服务暂时不可用",
			})
		}
	}

	c.Set("Content-Type", "application/dns-message")
	return c.Send(packed)
}

// 获取缓存key
func getCacheKey(msg *dns.Msg) string {
	if len(msg.Question) == 0 {
		return ""
	}
	q := msg.Question[0]
	return fmt.Sprintf("%d@%s", q.Qtype, q.Name)
}

// 从缓存获取响应
func getFromCache(msg *dns.Msg) *dns.Msg {
	key := getCacheKey(msg)
	if key == "" {
		atomic.AddInt64(&cacheMisses, 1)
		return nil
	}

	if cached, found := dnsCache.Get(key); found {
		if entry, ok := cached.(*struct {
			Response  *dns.Msg
			CacheTime time.Time
		}); ok {
			// 调整TTL值
			elapsed := time.Since(entry.CacheTime)
			resp := entry.Response.Copy()

			// 更新所有记录的TTL，使用实际剩余的缓存时间
			for _, rr := range resp.Answer {
				newTTL := uint32(math.Max(0, float64(maxCacheTTL.Seconds())-elapsed.Seconds()))
				rr.Header().Ttl = newTTL
			}

			// 更新响应的ID以匹配请求
			resp.Id = msg.Id

			atomic.AddInt64(&cacheHits, 1)

			log.Infof("缓存命中 - 域名: %s, 类型: %s, 已缓存时间: %v",
				msg.Question[0].Name,
				dnsTypeToString(msg.Question[0].Qtype),
				elapsed)

			return resp
		}
	}
	atomic.AddInt64(&cacheMisses, 1)
	return nil
}

// 添加响应到缓存
func addToCache(msg *dns.Msg, resp *dns.Msg) {
	key := getCacheKey(msg)
	if key == "" {
		return
	}

	// 检查缓存条目数量
	if dnsCache.ItemCount() >= maxCacheItems {
		log.Warnf("缓存已满，跳过缓存 - 域名: %s", msg.Question[0].Name)
		return
	}

	// 创建响应的副本
	cachedResp := resp.Copy()

	// 估算响应大小
	size := estimateResponseSize(cachedResp)
	if size > maxItemSize {
		log.Warnf("响应大小(%d bytes)超过限制(%d bytes)，跳过缓存 - 域名: %s",
			size, maxItemSize, msg.Question[0].Name)
		return
	}

	// 记录缓存时间
	cacheTime := time.Now()

	cacheEntry := &struct {
		Response  *dns.Msg
		CacheTime time.Time
	}{
		Response:  cachedResp,
		CacheTime: cacheTime,
	}

	ttl := calculateCacheTTL(resp)
	dnsCache.Set(key, cacheEntry, ttl)
}

func calculateCacheTTL(resp *dns.Msg) time.Duration {
	if len(resp.Answer) == 0 {
		return defaultCacheTTL
	}

	// 获取响应中最小的TTL
	minTTL := uint32(maxCacheTTL.Seconds())
	for _, rr := range resp.Answer {
		if rr.Header().Ttl < minTTL {
			minTTL = rr.Header().Ttl
		}
	}

	ttl := time.Duration(minTTL) * time.Second

	// 如果上游返回的TTL小于maxCacheTTL，则使用maxCacheTTL
	if ttl < maxCacheTTL {
		// 修改响应中的TTL值为maxCacheTTL
		for _, rr := range resp.Answer {
			rr.Header().Ttl = uint32(maxCacheTTL.Seconds())
		}
		return maxCacheTTL
	}

	// 使用上游返回的TTL
	return ttl
}

// 监控缓存大小
func monitorCacheSize() {
	ticker := time.NewTicker(1 * time.Minute)
	for range ticker.C {
		itemCount := dnsCache.ItemCount()
		if itemCount > maxCacheItems {
			// 如果超过最大条目数，清除一半的缓存
			removeCount := itemCount - maxCacheItems
			log.Warnf("缓存条目数量(%d)超过限制(%d)，准备清理%d个条目",
				itemCount, maxCacheItems, removeCount)
			pruneCache(removeCount)
		}

		// 记录当前缓存状态
		log.Infof("当前缓存状态 - 条目数: %d, 使用率: %.2f%%",
			itemCount, float64(itemCount)/float64(maxCacheItems)*100)
	}
}

// 清理缓存
func pruneCache(removeCount int) {
	items := dnsCache.Items()
	type cacheItem struct {
		key    string
		expiry time.Time // 过期时间
	}
	var itemList []cacheItem

	for k, v := range items {
		if v.Expired() {
			dnsCache.Delete(k)
			continue
		}
		itemList = append(itemList, cacheItem{
			key:    k,
			expiry: time.Unix(v.Expiration, 0),
		})
	}

	// 按过期时间排序，优先删除更快过期的条目
	sort.Slice(itemList, func(i, j int) bool {
		return itemList[i].expiry.Before(itemList[j].expiry)
	})

	// 删除指定数量的条目
	for i := 0; i < removeCount && i < len(itemList); i++ {
		dnsCache.Delete(itemList[i].key)
		log.Debugf("缓存清理：删除条目 %s", itemList[i].key)
	}
}

// 估算响应大小
func estimateResponseSize(resp *dns.Msg) int {
	size := 12 // DNS header size

	// 估算 Question 段大小
	for _, q := range resp.Question {
		size += len(q.Name) + 4 // 4 bytes for type and class
	}

	// 估算 Answer 段大小
	for _, rr := range resp.Answer {
		size += len(rr.Header().Name) + 10 // 10 bytes for header
		switch v := rr.(type) {
		case *dns.A:
			size += 4 // IPv4 address
		case *dns.AAAA:
			size += 16 // IPv6 address
		case *dns.CNAME:
			size += len(v.Target)
		case *dns.TXT:
			for _, txt := range v.Txt {
				size += len(txt)
			}
		}
	}

	return size
}

// 修改 forwardToUpstreamDNS 函数以使用缓存
func forwardToUpstreamDNS(msg *dns.Msg) (*dns.Msg, error) {
	if len(msg.Question) == 0 {
		return nil, fmt.Errorf("无效的DNS查询：空的Question段")
	}

	// 添加大小限制
	msg.SetEdns0(4096, false)

	// 首先尝试从缓存获取
	if cachedResp := getFromCache(msg); cachedResp != nil {
		return cachedResp, nil
	}

	servers := getAvailableServers()
	if len(servers) == 0 {
		return nil, fmt.Errorf("没有可用的上游DNS服务器")
	}

	type dnsResponse struct {
		resp    *dns.Msg
		err     error
		server  string
		latency time.Duration
	}
	responses := make(chan dnsResponse, len(servers))
	ctx, cancel := context.WithTimeout(context.Background(), totalTimeout)
	defer cancel()

	for _, server := range servers {
		go func(server *DNSServer) {
			start := time.Now()
			resp, rtt, err := server.query(msg, ctx)

			// 验证响应
			if err == nil && resp != nil {
				// 检查响应大小
				if packed, err := resp.Pack(); err == nil && len(packed) > 4096 {
					resp.Truncate(4096)
				}
			}

			server.updateStatus(rtt, err)

			select {
			case <-ctx.Done():
				return
			case responses <- dnsResponse{
				resp:    resp,
				err:     err,
				server:  server.addr,
				latency: time.Since(start),
			}:
			}
		}(server)
	}

	var lastError error
	for i := 0; i < len(servers); i++ {
		response := <-responses
		if response.err == nil && response.resp != nil {
			log.Infof("DNS查询成功 - 服务器: %s, 域名: %s, 类型: %s, 延迟: %v",
				response.server,
				msg.Question[0].Name,
				dnsTypeToString(msg.Question[0].Qtype),
				response.latency)

			addToCache(msg, response.resp)
			return response.resp, nil
		}
		lastError = response.err
	}

	return nil, fmt.Errorf("所有DNS服务器查询失败，最后错误: %v", lastError)
}

func (s *DNSServer) query(msg *dns.Msg, ctx context.Context) (*dns.Msg, time.Duration, error) {
	// 添加消息有效性检查
	if msg == nil {
		return nil, 0, fmt.Errorf("无效的DNS查询消息")
	}

	// 确保消息中包含Question
	if len(msg.Question) == 0 {
		return nil, 0, fmt.Errorf("DNS查询消息中没有Question段")
	}

	// 创建消息副本以避免修改原始消息
	queryMsg := msg.Copy()
	if queryMsg == nil {
		return nil, 0, fmt.Errorf("无法创建DNS消息副本")
	}

	// 添加EDNS0支持
	if queryMsg.IsEdns0() == nil {
		queryMsg.SetEdns0(4096, false)
	}

	var lastErr error
	for retry := 0; retry <= maxRetries; retry++ {
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		default:
			resp, rtt, err := dnsClient.Exchange(queryMsg, s.addr)
			if err == nil && resp != nil {
				// 验证响应
				if resp.IsEdns0() == nil {
					resp.SetEdns0(4096, false)
				}

				// 验证响应大小
				if packed, err := resp.Pack(); err == nil && len(packed) > 4096 {
					resp.Truncate(4096)
				}
				return resp, rtt, nil
			}
			lastErr = err
			backoff := time.Duration(math.Pow(2, float64(retry))) * 50 * time.Millisecond
			if backoff > time.Second {
				backoff = time.Second
			}
			time.Sleep(backoff)
		}
	}
	return nil, 0, lastErr
}

func dnsTypeToString(qtype uint16) string {
	switch qtype {
	case dns.TypeA:
		return "A"
	case dns.TypeAAAA:
		return "AAAA"
	case dns.TypeCNAME:
		return "CNAME"
	case dns.TypeMX:
		return "MX"
	case dns.TypeTXT:
		return "TXT"
	case dns.TypeNS:
		return "NS"
	case dns.TypeSOA:
		return "SOA"
	case dns.TypePTR:
		return "PTR"
	case dns.TypeSRV:
		return "SRV"
	case dns.TypeCAA:
		return "CAA"
	default:
		return fmt.Sprintf("TYPE%d", qtype)
	}
}

// ServerStatus 表示服务器状态的结构体
type ServerStatus struct {
	Address     string  `json:"address"`
	Priority    int     `json:"priority"`
	Weight      float64 `json:"weight"`
	FailCount   int     `json:"fail_count"`
	LastCheck   string  `json:"last_check"`
	NextCheck   string  `json:"next_check"` // 新增：下次检查时间
	AvgLatency  float64 `json:"avg_latency"`
	IsIPv6      bool    `json:"is_ipv6"`
	IsAvailable bool    `json:"is_available"`
}

// 合并的状态结构体
type SystemStatus struct {
	// 服务器状态
	Servers []ServerStatus `json:"servers"`
	// 缓存统计
	Cache struct {
		ItemCount int64   `json:"item_count"`     // 当前缓存项数量
		Hits      int64   `json:"hits"`           // 命中次数
		Misses    int64   `json:"misses"`         // 未命中次数
		HitRate   float64 `json:"hit_rate"`       // 命中率
		ResetTime string  `json:"reset_time"`     // 上次重置时间
		Node      string  `json:"node,omitempty"` // 节点名称
	} `json:"cache"`
}

func handleStatus(c *fiber.Ctx) error {
	serverMu.RLock()
	defer serverMu.RUnlock()

	status := SystemStatus{}

	// 获取服务器状态
	status.Servers = make([]ServerStatus, 0, len(upstreamServers))
	for _, server := range upstreamServers {
		server.mu.RLock()
		status.Servers = append(status.Servers, ServerStatus{
			Address:     server.addr,
			Priority:    server.priority,
			Weight:      server.weight,
			FailCount:   server.failCount,
			LastCheck:   server.lastCheck.Format("2006-01-02 15:04:05"),
			NextCheck:   server.nextCheck.Format("2006-01-02 15:04:05"),
			AvgLatency:  server.avgLatency,
			IsIPv6:      server.isIPv6,
			IsAvailable: server.isAvailable,
		})
		server.mu.RUnlock()
	}

	// 按优先级和权重排序
	sort.Slice(status.Servers, func(i, j int) bool {
		if status.Servers[i].Priority != status.Servers[j].Priority {
			return status.Servers[i].Priority > status.Servers[j].Priority
		}
		return status.Servers[i].Weight > status.Servers[j].Weight
	})

	// 获取缓存统计
	hits := atomic.LoadInt64(&cacheHits)
	misses := atomic.LoadInt64(&cacheMisses)
	total := hits + misses

	status.Cache.ItemCount = int64(dnsCache.ItemCount())
	status.Cache.Hits = hits
	status.Cache.Misses = misses
	status.Cache.Node = environ.GetEnv("NODE_IP", "unknown")
	if total > 0 {
		status.Cache.HitRate = float64(hits) / float64(total) * 100
	}
	status.Cache.ResetTime = statsResetTime.Format("2006-01-02 15:04:05")

	return c.JSON(status)
}

// 修改手动触发健康检查的处理函数
func handleManualCheck(c *fiber.Ctx) error {
	// 手动触发时也使用 true，确保检查所有服务器
	go checkAllServers(true)
	return c.JSON(fiber.Map{
		"message": "健康检查已触发，将检查所有服务器",
	})
}

// 添加重置统计的接口
func handleResetStats(c *fiber.Ctx) error {
	action := c.Query("action", "all")

	switch action {
	case "reset":
		// 重置统计数据
		atomic.StoreInt64(&cacheHits, 0)
		atomic.StoreInt64(&cacheMisses, 0)
		statsResetTime = time.Now()

		return c.JSON(fiber.Map{
			"message": "统计已重置",
			"time":    statsResetTime.Format("2006-01-02 15:04:05"),
		})

	case "clear":
		// 清空缓存
		dnsCache.Flush()

		return c.JSON(fiber.Map{
			"message": "缓存已清空",
			"time":    time.Now().Format("2006-01-02 15:04:05"),
		})

	case "all":
		// 同时执行重置统计和清空缓存
		atomic.StoreInt64(&cacheHits, 0)
		atomic.StoreInt64(&cacheMisses, 0)
		statsResetTime = time.Now()
		dnsCache.Flush()

		return c.JSON(fiber.Map{
			"message": "统计已重置且缓存已清空",
			"time":    statsResetTime.Format("2006-01-02 15:04:05"),
		})

	default:
		return c.Status(400).JSON(fiber.Map{
			"error":         "无效的操作类型",
			"valid_actions": []string{"reset", "clear", "all"},
		})
	}
}

func (s *DNSServer) calculateWeight(avgLatency float64) float64 {
	weight := baseWeight / (avgLatency + 10)

	// 添加权重范围限制
	if weight < minWeight {
		return minWeight
	} else if weight > maxWeight {
		return maxWeight
	}
	return weight
}

func handleVersion(c *fiber.Ctx) error {
	return c.JSON(VersionInfo{
		Version:   AppVersion,
		BuildTime: BuildTime,
		GoVersion: GoVersion,
		GitCommit: GitCommit,
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		Node:      environ.GetEnv("NODE_IP", "unknown"),
	})
}

func handleCacheInfo(c *fiber.Ctx) error {
	cacheInfo := CacheInfo{}
	items := dnsCache.Items()

	// 填充摘要信息
	var validItems int
	for _, item := range items {
		if !item.Expired() {
			validItems++
		}
	}

	// 基础统计信息
	cacheInfo.Summary.Node = environ.GetEnv("NODE_IP", "unknown")
	cacheInfo.Summary.TotalItems = validItems
	cacheInfo.Summary.Hits = atomic.LoadInt64(&cacheHits)
	cacheInfo.Summary.Misses = atomic.LoadInt64(&cacheMisses)
	total := cacheInfo.Summary.Hits + cacheInfo.Summary.Misses
	if total > 0 {
		cacheInfo.Summary.HitRate = float64(cacheInfo.Summary.Hits) / float64(total) * 100
	}
	cacheInfo.Summary.LastResetTime = statsResetTime.Format("2006-01-02 15:04:05")

	// 如果请求详细信息，则填充缓存条目
	// 预分配切片以提高性能
	cacheInfo.Items = make([]CacheEntry, 0, validItems)

	for k, item := range items {
		// 跳过已过期的条目
		if item.Expired() {
			continue
		}

		if entry, ok := item.Object.(*struct {
			Response  *dns.Msg
			CacheTime time.Time
		}); ok {
			// 解析缓存键：格式为 "{Qtype}@{Name}"
			parts := strings.SplitN(k, "@", 2)
			if len(parts) != 2 {
				continue
			}

			qtype, _ := strconv.Atoi(parts[0])
			domain := parts[1]

			// 计算剩余TTL
			remainingTTL := int64(item.Expiration - time.Now().Unix())
			if remainingTTL < 0 {
				continue
			}

			// 获取解析结果
			var answers []string
			for _, ans := range entry.Response.Answer {
				answers = append(answers, ans.String())
			}

			// 估算大小
			size := estimateResponseSize(entry.Response)
			cacheInfo.Summary.TotalSize += int64(size)

			cacheEntry := CacheEntry{
				Key:       k,
				Domain:    domain,
				Type:      dnsTypeToString(uint16(qtype)),
				TTL:       remainingTTL,
				CacheTime: entry.CacheTime.Format("2006-01-02 15:04:05"),
				ExpireAt:  time.Unix(0, item.Expiration).Format("2006-01-02 15:04:05"),
				Size:      size,
				Answers:   answers,
			}

			cacheInfo.Items = append(cacheInfo.Items, cacheEntry)
		}
	}

	// 计算内存使用率
	if maxCacheItems > 0 {
		cacheInfo.Summary.MemoryUsage = float64(validItems) / float64(maxCacheItems) * 100
	}

	// 按过期时间排序
	sort.Slice(cacheInfo.Items, func(i, j int) bool {
		return cacheInfo.Items[i].TTL < cacheInfo.Items[j].TTL
	})

	return c.JSON(cacheInfo)
}

func getRealIP(c *fiber.Ctx) string {
	if ip := c.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	if ip := c.Get("True-Client-IP"); ip != "" {
		return ip
	}
	if ip := c.Get("X-Real-IP"); ip != "" {
		return ip
	}
	if forwarded := c.Get("X-Forwarded-For"); forwarded != "" {
		ips := strings.Split(forwarded, ",")
		for _, ip := range ips {
			ip = strings.TrimSpace(ip)
			if ip != "" && !exnet.IsPrivateNetIP(net.ParseIP(ip)) {
				return ip
			}
		}
	}
	return c.IP()
}

func main() {
	app := fiber.New(fiber.Config{
		ServerHeader: "Caddy",
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			return c.Redirect("/deepseek", 302)
		},
	})
	app.Use(requestid.New())
	app.Use(logger.New(logger.Config{
		Format:     "${time} ${locals:requestid} ${clientip} ${ua} - ${method} ${status} ${host} ${path} ${latency}\n",
		TimeFormat: "2006-01-02 15:04:05",
		TimeZone:   "Asia/Shanghai",
		CustomTags: map[string]logger.LogFunc{
			"clientip": func(output logger.Buffer, c *fiber.Ctx, data *logger.Data, extraParam string) (int, error) {
				return output.WriteString(getRealIP(c))
			},
		},
		Next: func(c *fiber.Ctx) bool {
			ua := strings.ToLower(c.Get("User-Agent"))
			if strings.Contains(ua, "kube-probe") || strings.Contains(ua, "uptime-kuma") {
				return true
			}
			return false
		},
	}))
	app.Use(healthcheck.New(healthcheck.Config{
		LivenessProbe: func(c *fiber.Ctx) bool {
			return true
		},
		LivenessEndpoint: "/live",
		ReadinessProbe: func(c *fiber.Ctx) bool {
			return true
		},
		ReadinessEndpoint: "/ready",
	}))
	app.Get("/metrics", monitor.New(monitor.Config{Title: "Caddy Metrics"}))

	app.Get("/dns-query", handle)
	app.Post("/dns-query", handle)
	app.Get("/dnspod", handle)
	app.Post("/dnspod", handle)
	app.Get("/", handle)
	app.Post("/", handle)

	// 添加版本接口
	app.Get("/version", handleVersion)

	app.Route("/debug", func(router fiber.Router) {
		// 添加缓存信息接口
		router.Get("/cache", handleCacheInfo)
		// 添加重置统计的接口
		router.Post("/reset", handleResetStats)
		// 添加手动触发健康检查的接口
		router.Post("/check", handleManualCheck)
		// 添加新的状态查询接口
		router.Get("/status", handleStatus)
	})

	app.All("/deepseek", func(c *fiber.Ctx) error {
		return c.SendString("ip: " + getRealIP(c))
	})

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-quit
		log.Info("正在关闭服务器...")

		// 设置关闭超时
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := app.ShutdownWithContext(ctx); err != nil {
			log.Error("服务器关闭出错:", err)
		}
	}()

	// 启动健康检查
	startHealthCheck()

	// 启动统计自动重置
	startStatsReset()

	if err := app.Listen(":65001"); err != nil {
		log.Error("服务器启动失败:", err)
	}
}
