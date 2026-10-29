package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3/log"
	"github.com/miekg/dns"
)

const (
	queryTimeout        = 3 * time.Second
	healthCheckInterval = time.Minute
	healthCheckTimeout  = 2 * time.Second
	healthCheckDomain   = "google.com."
)

var (
	dnsClient       = &dns.Client{Timeout: queryTimeout}
	upstreamServers []*DNSServer // 启动后只读；每个上游的健康状态由自身的锁保护。
	healthCheckMu   sync.Mutex
)

var defaultUpstreamAddrs = []string{
	"8.8.8.8:53", "8.8.4.4:53",
	"[2001:4860:4860::8888]:53", "[2001:4860:4860::8844]:53",
	"1.1.1.1:53", "1.0.0.1:53",
	"[2606:4700:4700::1111]:53", "[2606:4700:4700::1001]:53",
}

// DNSServer 仅保存查询所需地址及用于故障隔离的健康状态。
type DNSServer struct {
	addr        string
	mu          sync.RWMutex
	isAvailable bool
	failCount   int
	lastCheck   time.Time
	latency     time.Duration
}

func loadUpstreamServers() ([]*DNSServer, error) {
	raw := strings.TrimSpace(os.Getenv("DOH_UPSTREAMS"))
	if raw == "" {
		raw = strings.Join(defaultUpstreamAddrs, ",")
	}
	var servers []*DNSServer
	for _, entry := range strings.Split(raw, ",") {
		server, err := parseUpstreamServer(strings.TrimSpace(entry))
		if err != nil {
			return nil, fmt.Errorf("无效的 DOH_UPSTREAMS 配置 %q: %w", entry, err)
		}
		servers = append(servers, server)
	}
	return servers, nil
}

func parseUpstreamServer(addr string) (*DNSServer, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("expected IP:port: %w", err)
	}
	if net.ParseIP(host) == nil {
		return nil, fmt.Errorf("host must be an IP address")
	}
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil || number == 0 {
		return nil, fmt.Errorf("port must be between 1 and 65535")
	}
	return newDNSServer(addr), nil
}

func newDNSServer(addr string) *DNSServer {
	return &DNSServer{addr: addr, isAvailable: true}
}

func (s *DNSServer) updateStatus(latency time.Duration, err error) {
	// 竞速胜出或调用方取消都不代表上游故障。
	if errors.Is(err, context.Canceled) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	available := err == nil
	if available != s.isAvailable {
		log.Infof("上游 %s 可用状态变更: %t", s.addr, available)
	}
	s.isAvailable = available
	s.lastCheck = time.Now()
	s.latency = latency
	if available {
		s.failCount = 0
	} else {
		s.failCount++
	}
}

func getAvailableServers() []*DNSServer {
	servers := make([]*DNSServer, 0, len(upstreamServers))
	for _, server := range upstreamServers {
		server.mu.RLock()
		available := server.isAvailable
		server.mu.RUnlock()
		if available {
			servers = append(servers, server)
		}
	}
	return servers
}

func (s *DNSServer) query(ctx context.Context, msg *dns.Msg) (*dns.Msg, time.Duration, error) {
	if msg == nil || len(msg.Question) != 1 {
		return nil, 0, fmt.Errorf("DNS 查询必须包含一个 Question")
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	query := msg.Copy()
	if query.IsEdns0() == nil {
		query.SetEdns0(maxDNSMessageSize, false)
	}
	conn, err := dnsClient.DialContext(ctx, s.addr)
	if err != nil {
		return nil, 0, err
	}
	defer conn.Close()
	// ExchangeWithConnContext 只设置 deadline；主动关闭连接才能及时中断取消的读取。
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	resp, rtt, err := dnsClient.ExchangeWithConnContext(ctx, query, conn)
	if ctx.Err() != nil {
		return nil, rtt, ctx.Err()
	}
	// socket deadline 可能先于 context 定时器触发，统一对外的超时语义。
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return nil, rtt, context.DeadlineExceeded
	}
	if err != nil {
		return nil, rtt, err
	}
	if resp == nil {
		return nil, rtt, fmt.Errorf("上游返回空响应")
	}
	return resp, rtt, nil
}

func forwardToUpstreamDNS(ctx context.Context, msg *dns.Msg) (*dns.Msg, error) {
	if msg == nil || len(msg.Question) != 1 {
		return nil, fmt.Errorf("DNS 查询必须包含一个 Question")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if resp := getFromCache(msg); resp != nil {
		return resp, nil
	}
	servers := getAvailableServers()
	if len(servers) == 0 {
		return nil, fmt.Errorf("没有可用的上游 DNS")
	}
	queryCtx, cancel := context.WithTimeout(ctx, queryTimeout)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	type result struct {
		response *dns.Msg
		err      error
	}
	// 每个任务只发送一次，缓冲保证调用方提前返回后，剩余任务仍能退出。
	results := make(chan result, len(servers))
	for _, server := range servers {
		wg.Go(func() {
			resp, rtt, err := server.query(queryCtx, msg)
			if ctx.Err() == nil {
				server.updateStatus(rtt, err)
			}
			results <- result{resp, err}
		})
	}
	var lastErr error
	var dnsFailure *dns.Msg
	for range servers {
		// 每个查询自行处理超时并发送结果；不另设 deadline 分支丢弃已收到的 DNS 错误。
		res := <-results
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if res.err != nil {
			lastErr = res.err
			continue
		}
		if res.response.Rcode != dns.RcodeSuccess && res.response.Rcode != dns.RcodeNameError {
			dnsFailure = res.response
			continue
		}
		addToCache(msg, res.response)
		return res.response, nil
	}
	if dnsFailure != nil {
		return dnsFailure, nil
	}
	return nil, fmt.Errorf("所有上游 DNS 查询失败: %w", lastErr)
}

func startHealthCheck(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(healthCheckInterval)
		defer ticker.Stop()
		checkAllServers(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				checkAllServers(ctx)
			}
		}
	}()
	return done
}

func checkAllServers(ctx context.Context) {
	// 定时检查与手动检查共用一次探测，避免重复触发放大流量。
	if !healthCheckMu.TryLock() {
		return
	}
	defer healthCheckMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()
	msg := new(dns.Msg).SetQuestion(healthCheckDomain, dns.TypeA)
	var wg sync.WaitGroup
	for _, server := range upstreamServers {
		wg.Go(func() {
			_, rtt, err := server.query(ctx, msg)
			server.updateStatus(rtt, err)
		})
	}
	wg.Wait()
}
