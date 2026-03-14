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
	"github.com/gofiber/contrib/v3/monitor"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/log"
	"github.com/gofiber/fiber/v3/middleware/healthcheck"
	"github.com/gofiber/fiber/v3/middleware/logger"
	"github.com/gofiber/fiber/v3/middleware/requestid"

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
	upstreamServers   []*DNSServer
	serverMu          sync.RWMutex
	dnsCache          *cache.Cache
	cacheHits         int64 // 缓存命中次数
	cacheMisses       int64 // 缓存未命中次数
	statsResetTime    atomic.Value
	healthCheckTicker *time.Ticker
	cronScheduler     *cron.Cron
	monitorStopCh     chan struct{}

	// 版本信息变量,通过编译时注入
	AppVersion = "1.1.10"
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
	healthCheckInterval = 1 * time.Minute
	healthCheckTimeout  = 2 * time.Second
	healthCheckDomain   = "google.com."
	baseCheckDelay      = 30 * time.Second
	maxCheckDelay       = 30 * time.Minute

	// 缓存相关
	maxCacheTTL     = 5 * time.Minute
	cleanupInterval = 30 * time.Second
	maxCacheItems   = 10000
	maxItemSize     = 4096

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

type cachedResponse struct {
	Response  *dns.Msg
	CacheTime time.Time
}

var defaultUpstreamAddrs = []string{
	"8.8.8.8:53|99",
	"8.8.4.4:53|99",
	"[2001:4860:4860::8888]:53|99",
	"[2001:4860:4860::8844]:53|99",
	"1.1.1.1:53|98",
	"1.0.0.1:53|98",
	"[2606:4700:4700::1111]:53|98",
	"[2606:4700:4700::1001]:53|98",
}

func init() {
	upstreamServers = loadUpstreamServers()
	dnsCache = cache.New(maxCacheTTL, cleanupInterval)
	dnsCache.OnEvicted(func(key string, value any) {
		log.Debugf("缓存条目已过期或被驱逐 - Key: %s", key)
	})
	statsResetTime.Store(time.Now())
}

func loadUpstreamServers() []*DNSServer {
	raw := strings.TrimSpace(environ.GetEnv("DOH_UPSTREAMS", ""))
	if raw == "" {
		raw = strings.Join(defaultUpstreamAddrs, ",")
	}

	parts := strings.Split(raw, ",")
	servers := make([]*DNSServer, 0, len(parts))
	for idx, part := range parts {
		server, err := parseUpstreamServer(strings.TrimSpace(part), idx)
		if err != nil {
			log.Warnf("跳过无效上游配置 %q: %v", part, err)
			continue
		}
		servers = append(servers, server)
	}

	if len(servers) == 0 {
		log.Warn("未加载到有效上游，回退到默认配置")
		for idx, entry := range defaultUpstreamAddrs {
			server, err := parseUpstreamServer(entry, idx)
			if err == nil {
				servers = append(servers, server)
			}
		}
	}

	return servers
}

func parseUpstreamServer(entry string, idx int) (*DNSServer, error) {
	if entry == "" {
		return nil, fmt.Errorf("empty upstream entry")
	}

	priority := 100 - idx
	addr := entry
	if strings.Contains(entry, "|") {
		parts := strings.SplitN(entry, "|", 2)
		addr = strings.TrimSpace(parts[0])
		if addr == "" {
			return nil, fmt.Errorf("empty address")
		}
		if strings.TrimSpace(parts[1]) != "" {
			parsedPriority, err := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err != nil {
				return nil, fmt.Errorf("invalid priority: %w", err)
			}
			priority = parsedPriority
		}
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid host:port: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("host is not an IP address")
	}

	return newDNSServer(addr, priority, ip.To4() == nil), nil
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
		delay := min(time.Duration(s.failCount)*baseCheckDelay, maxCheckDelay)
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

	// 一次性读取所有数据，避免排序时加锁
	type serverSnapshot struct {
		server   *DNSServer
		priority int
		weight   float64
	}
	var snapshots []serverSnapshot

	for _, s := range upstreamServers {
		s.mu.RLock()
		if s.isAvailable {
			snapshots = append(snapshots, serverSnapshot{s, s.priority, s.weight})
		}
		s.mu.RUnlock()
	}
	serverMu.RUnlock()

	grouped := make(map[int][]serverSnapshot)
	priorities := make([]int, 0, len(snapshots))
	for _, snap := range snapshots {
		if _, ok := grouped[snap.priority]; !ok {
			priorities = append(priorities, snap.priority)
		}
		grouped[snap.priority] = append(grouped[snap.priority], snap)
	}

	sort.Slice(priorities, func(i, j int) bool {
		return priorities[i] > priorities[j]
	})

	result := make([]*DNSServer, len(snapshots))
	idx := 0
	for _, priority := range priorities {
		group := grouped[priority]
		rand.Shuffle(len(group), func(i, j int) {
			group[i], group[j] = group[j], group[i]
		})
		sort.SliceStable(group, func(i, j int) bool {
			return group[i].weight > group[j].weight
		})
		for _, snap := range group {
			result[idx] = snap.server
			idx++
		}
	}
	return result
}

// 启动健康检查
func startHealthCheck() {
	healthCheckTicker = time.NewTicker(healthCheckInterval)
	go func() {
		// 启动时立即进行一次检查，使用 true 来忽略 nextCheck 时间
		checkAllServers(true)

		for range healthCheckTicker.C {
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

	_, rtt, err := server.query(ctx, msg.Copy()) // 使用消息的副本
	server.updateStatus(rtt, err)
}

func logHealthCheckResults() {
	serverMu.RLock()
	defer serverMu.RUnlock()

	var available, unavailable int
	var totalWeight float64

	for _, server := range upstreamServers {
		server.mu.RLock()
		if server.isAvailable {
			available++
			totalWeight += server.weight
		} else {
			unavailable++
		}
		server.mu.RUnlock()
	}

	log.Debugf("健康检查结果：可用=%d 不可用=%d 总权重=%.2f", available, unavailable, totalWeight)
}

func startStatsReset() {
	cronScheduler = cron.New()
	_, err := cronScheduler.AddFunc(statsResetCron, func() {
		now := resetStats()
		log.Infof("缓存统计已自动重置 - 时间: %s", now.Format("2006-01-02 15:04:05"))
	})
	if err != nil {
		log.Errorf("添加统计重置定时任务失败: %v", err)
		return
	}
	cronScheduler.Start()
}

func handle(c fiber.Ctx) error {
	var dnsMsg []byte
	var err error

	if c.Method() == "GET" {
		if !acceptsDNSMessage(c) {
			return c.Status(fiber.StatusNotAcceptable).JSON(fiber.Map{
				"error": "客户端不接受DNS消息响应",
			})
		}

		dnsParam := c.Query("dns")
		if dnsParam == "" {
			return c.Redirect().Status(fiber.StatusFound).To("/deepseek")
		}
		dnsMsg, err = base64.RawURLEncoding.DecodeString(dnsParam)
		if err != nil {
			log.Warnf("base64解码失败: %v", err)
			return c.Status(400).JSON(fiber.Map{
				"error": "请求参数无效",
			})
		}
	} else {
		if !isDNSMessageContentType(c.Get("Content-Type")) {
			return c.Status(fiber.StatusUnsupportedMediaType).JSON(fiber.Map{
				"error": "请求 Content-Type 必须为 application/dns-message",
			})
		}
		if !acceptsDNSMessage(c) {
			return c.Status(fiber.StatusNotAcceptable).JSON(fiber.Map{
				"error": "客户端不接受DNS消息响应",
			})
		}
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

	q := msg.Question[0]
	log.Debugf("DNS查询 - 客户端: %s, 域名: %s, 类型: %s", getRealIP(c), q.Name, dnsTypeToString(q.Qtype))

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

func isDNSMessageContentType(contentType string) bool {
	mediaType := strings.TrimSpace(strings.ToLower(strings.SplitN(contentType, ";", 2)[0]))
	return mediaType == "application/dns-message"
}

func acceptsDNSMessage(c fiber.Ctx) bool {
	accept := strings.TrimSpace(strings.ToLower(c.Get("Accept")))
	if accept == "" || accept == "*/*" {
		return true
	}

	for _, part := range strings.Split(accept, ",") {
		mediaType := strings.TrimSpace(strings.ToLower(strings.SplitN(part, ";", 2)[0]))
		if mediaType == "*/*" || mediaType == "application/*" || mediaType == "application/dns-message" {
			return true
		}
	}
	return false
}

// 获取缓存key
func getCacheKey(msg *dns.Msg) string {
	if len(msg.Question) == 0 {
		return ""
	}
	q := msg.Question[0]
	do := false
	if opt := msg.IsEdns0(); opt != nil {
		do = opt.Do()
	}
	return fmt.Sprintf("%d@%d@%t@%s", q.Qclass, q.Qtype, do, q.Name)
}

// 从缓存获取响应
func getFromCache(msg *dns.Msg) *dns.Msg {
	key := getCacheKey(msg)
	if key == "" {
		atomic.AddInt64(&cacheMisses, 1)
		return nil
	}

	if cached, expiresAt, found := dnsCache.GetWithExpiration(key); found {
		if entry, ok := cached.(*cachedResponse); ok {
			// 调整TTL值
			elapsed := time.Since(entry.CacheTime)
			resp := entry.Response.Copy()
			remainingTTL := uint32(0)
			if !expiresAt.IsZero() {
				remaining := time.Until(expiresAt)
				if remaining > 0 {
					remainingTTL = uint32(remaining / time.Second)
				}
			}

			// 使用缓存真实剩余时间回写 TTL，且不超过原始 TTL。
			adjustTTL(resp.Answer, remainingTTL)
			adjustTTL(resp.Ns, remainingTTL)
			adjustTTL(resp.Extra, remainingTTL)

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

	cacheEntry := &cachedResponse{
		Response:  cachedResp,
		CacheTime: cacheTime,
	}

	ttl := calculateCacheTTL(resp)
	if ttl <= 0 {
		return
	}
	dnsCache.Set(key, cacheEntry, ttl)
}

func calculateCacheTTL(resp *dns.Msg) time.Duration {
	if resp == nil || resp.Truncated {
		return 0
	}

	if len(resp.Answer) > 0 {
		// 获取响应中最小的TTL
		minTTL := ^uint32(0)
		for _, rr := range resp.Answer {
			if rr.Header().Ttl < minTTL {
				minTTL = rr.Header().Ttl
			}
		}

		if minTTL == 0 {
			return 0
		}

		ttl := time.Duration(minTTL) * time.Second
		if ttl > maxCacheTTL {
			return maxCacheTTL
		}
		return ttl
	}

	if resp.Rcode == dns.RcodeNameError || resp.Rcode == dns.RcodeSuccess {
		if ttl := negativeCacheTTL(resp); ttl > 0 {
			return ttl
		}
	}

	return 0
}

func negativeCacheTTL(resp *dns.Msg) time.Duration {
	var minTTL uint32
	for _, rr := range resp.Ns {
		soa, ok := rr.(*dns.SOA)
		if !ok {
			continue
		}

		candidate := soa.Hdr.Ttl
		if soa.Minttl > 0 && soa.Minttl < candidate {
			candidate = soa.Minttl
		}
		if candidate == 0 {
			continue
		}
		if minTTL == 0 || candidate < minTTL {
			minTTL = candidate
		}
	}

	ttl := time.Duration(minTTL) * time.Second
	if ttl <= 0 {
		return 0
	}
	if ttl > maxCacheTTL {
		return maxCacheTTL
	}
	return ttl
}

func adjustTTL(records []dns.RR, remainingTTL uint32) {
	for _, rr := range records {
		if rr.Header().Rrtype == dns.TypeOPT {
			continue
		}
		if remainingTTL == 0 || rr.Header().Ttl > remainingTTL {
			rr.Header().Ttl = remainingTTL
		}
	}
}

// 监控缓存大小
func monitorCacheSize() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			itemCount := dnsCache.ItemCount()
			if itemCount >= maxCacheItems {
				// 并发写入可能短暂超过阈值，定期修剪回上限以内。
				removeCount := max(1, itemCount-maxCacheItems+1)
				log.Warnf("缓存条目数量(%d)超过限制(%d)，准备清理%d个条目",
					itemCount, maxCacheItems, removeCount)
				pruneCache(removeCount)
			}

			// 记录当前缓存状态
			log.Infof("当前缓存状态 - 条目数: %d, 使用率: %.2f%%",
				itemCount, float64(itemCount)/float64(maxCacheItems)*100)
		case <-monitorStopCh:
			return
		}
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
	workerCount := len(servers)
	responses := make(chan dnsResponse, workerCount)
	ctx, cancel := context.WithTimeout(context.Background(), totalTimeout)
	defer cancel()
	jobs := make(chan *DNSServer)
	var wg sync.WaitGroup

	for range workerCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for server := range jobs {
				if ctx.Err() != nil {
					return
				}

				start := time.Now()
				resp, rtt, err := server.query(ctx, msg)

				// 验证响应
				if err == nil && resp != nil {
					if packed, packErr := resp.Pack(); packErr == nil && len(packed) > 4096 {
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
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, server := range servers {
			select {
			case <-ctx.Done():
				return
			case jobs <- server:
			}
		}
	}()

	go func() {
		wg.Wait()
		close(responses)
	}()

	var lastError error
	for response := range responses {
		if response.err == nil && response.resp != nil {
			log.Infof("DNS查询成功 - 服务器: %s, 域名: %s, 类型: %s, 延迟: %v",
				response.server,
				msg.Question[0].Name,
				dnsTypeToString(msg.Question[0].Qtype),
				response.latency)

			addToCache(msg, response.resp)
			cancel()
			return response.resp, nil
		}
		lastError = response.err
	}

	return nil, fmt.Errorf("所有DNS服务器查询失败，最后错误: %v", lastError)
}

func (s *DNSServer) query(ctx context.Context, msg *dns.Msg) (*dns.Msg, time.Duration, error) {
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
			resp, rtt, err := dnsClient.ExchangeContext(ctx, queryMsg, s.addr)
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
			backoff := min(time.Duration(math.Pow(2, float64(retry)))*50*time.Millisecond, time.Second)
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return nil, 0, ctx.Err()
			case <-timer.C:
			}
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

func handleStatus(c fiber.Ctx) error {
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
	status.Cache.ResetTime = statsResetTime.Load().(time.Time).Format("2006-01-02 15:04:05")

	return c.JSON(status)
}

// 修改手动触发健康检查的处理函数
func handleManualCheck(c fiber.Ctx) error {
	// 手动触发时也使用 true，确保检查所有服务器
	go checkAllServers(true)
	return c.JSON(fiber.Map{
		"message": "健康检查已触发，将检查所有服务器",
	})
}

func resetStats() time.Time {
	atomic.StoreInt64(&cacheHits, 0)
	atomic.StoreInt64(&cacheMisses, 0)
	now := time.Now()
	statsResetTime.Store(now)
	return now
}

func handleResetStats(c fiber.Ctx) error {
	action := c.Query("action", "all")

	switch action {
	case "reset":
		now := resetStats()
		return c.JSON(fiber.Map{
			"message": "统计已重置",
			"time":    now.Format("2006-01-02 15:04:05"),
		})

	case "clear":
		dnsCache.Flush()
		return c.JSON(fiber.Map{
			"message": "缓存已清空",
			"time":    time.Now().Format("2006-01-02 15:04:05"),
		})

	case "all":
		now := resetStats()
		dnsCache.Flush()
		return c.JSON(fiber.Map{
			"message": "统计已重置且缓存已清空",
			"time":    now.Format("2006-01-02 15:04:05"),
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
	return max(minWeight, min(weight, maxWeight))
}

func handleVersion(c fiber.Ctx) error {
	return c.JSON(VersionInfo{
		Version:   AppVersion,
		BuildTime: BuildTime,
		GoVersion: GoVersion,
		GitCommit: GitCommit,
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		Node:      environ.GetEnv("NODE_IP", "unknown"),
	})
}

func handleCacheInfo(c fiber.Ctx) error {
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
	cacheInfo.Summary.LastResetTime = statsResetTime.Load().(time.Time).Format("2006-01-02 15:04:05")

	// 如果请求详细信息，则填充缓存条目
	// 预分配切片以提高性能
	cacheInfo.Items = make([]CacheEntry, 0, validItems)

	for k, item := range items {
		// 跳过已过期的条目
		if item.Expired() {
			continue
		}

		if entry, ok := item.Object.(*cachedResponse); ok {
			// 解析缓存键：格式为 "{Qclass}@{Qtype}@{DO}@{Name}"
			parts := strings.SplitN(k, "@", 4)
			if len(parts) != 4 {
				continue
			}

			qtype, _ := strconv.Atoi(parts[1])
			domain := parts[3]

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
				ExpireAt:  time.Unix(item.Expiration, 0).Format("2006-01-02 15:04:05"),
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

func getRealIP(c fiber.Ctx) string {
	if ip := parseHeaderIP(c.Get("CF-Connecting-IP")); ip != "" {
		return ip
	}
	if ip := parseHeaderIP(c.Get("True-Client-IP")); ip != "" {
		return ip
	}
	if ip := parseHeaderIP(c.Get("X-Real-IP")); ip != "" {
		return ip
	}
	if forwarded := c.Get("X-Forwarded-For"); forwarded != "" {
		ips := strings.Split(forwarded, ",")
		for _, ip := range ips {
			if parsed := parseHeaderIP(ip); parsed != "" && !exnet.IsPrivateNetIP(net.ParseIP(parsed)) {
				return parsed
			}
		}
	}
	if ip := parseHeaderIP(c.IP()); ip != "" {
		return ip
	}
	return c.IP()
}

func parseHeaderIP(raw string) string {
	ip := net.ParseIP(strings.TrimSpace(raw))
	if ip == nil {
		return ""
	}
	return ip.String()
}

func allowDebug(c fiber.Ctx) bool {
	if environ.GetEnv("DOH_DEBUG_ENABLED", "") != "1" {
		return false
	}

	expectedToken := strings.TrimSpace(environ.GetEnv("DOH_DEBUG_TOKEN", ""))
	if expectedToken != "" {
		authHeader := strings.TrimSpace(c.Get("Authorization"))
		if strings.HasPrefix(authHeader, "Bearer ") && strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer ")) == expectedToken {
			return true
		}
		if strings.TrimSpace(c.Get("X-Debug-Token")) == expectedToken {
			return true
		}
		return false
	}

	ip := net.ParseIP(getRealIP(c))
	return ip != nil && (ip.IsLoopback() || exnet.IsPrivateNetIP(ip))
}

func debugMiddleware(c fiber.Ctx) error {
	if !allowDebug(c) {
		return c.SendStatus(fiber.StatusNotFound)
	}
	return c.Next()
}

func main() {
	app := fiber.New(fiber.Config{
		ServerHeader: "Caddy2",
		ErrorHandler: func(c fiber.Ctx, err error) error {
			return c.Redirect().Status(fiber.StatusFound).To("/deepseek")
		},
	})
	app.Use(requestid.New())
	app.Use(logger.New(logger.Config{
		Format:     "${time} ${locals:requestid} ${clientip} ${ua} - ${method} ${status} ${host} ${path} ${latency}\n",
		TimeFormat: "2006-01-02 15:04:05",
		TimeZone:   "Asia/Shanghai",
		CustomTags: map[string]logger.LogFunc{
			"clientip": func(output logger.Buffer, c fiber.Ctx, data *logger.Data, extraParam string) (int, error) {
				return output.WriteString(getRealIP(c))
			},
		},
		Next: func(c fiber.Ctx) bool {
			ua := strings.ToLower(c.Get("User-Agent"))
			return strings.Contains(ua, "kube-probe") || strings.Contains(ua, "uptime-kuma")
		},
	}))
	app.Get("/live", healthcheck.New(healthcheck.Config{
		Probe: func(c fiber.Ctx) bool {
			return true
		},
	}))
	app.Get("/ready", healthcheck.New(healthcheck.Config{
		Probe: func(c fiber.Ctx) bool {
			return true
		},
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
		router.Use(debugMiddleware)
		// 添加缓存信息接口
		router.Get("/cache", handleCacheInfo)
		// 添加重置统计的接口
		router.Post("/reset", handleResetStats)
		// 添加手动触发健康检查的接口
		router.Post("/check", handleManualCheck)
		// 添加新的状态查询接口
		router.Get("/status", handleStatus)
	})

	app.All("/deepseek", func(c fiber.Ctx) error {
		return c.SendString("ip: " + getRealIP(c))
	})

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-quit
		log.Info("正在关闭服务器...")

		// 停止健康检查
		if healthCheckTicker != nil {
			healthCheckTicker.Stop()
		}
		// 停止 cron 任务
		if cronScheduler != nil {
			cronScheduler.Stop()
		}
		// 停止缓存监控
		if monitorStopCh != nil {
			close(monitorStopCh)
		}

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

	// 启动缓存监控
	monitorStopCh = make(chan struct{})
	go monitorCacheSize()

	if err := app.Listen(":65001"); err != nil {
		log.Error("服务器启动失败:", err)
	}
}
