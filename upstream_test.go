package main

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func startTestDNS(t *testing.T, handler dns.HandlerFunc) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	done := make(chan error, 1)
	server := &dns.Server{
		PacketConn: conn, Handler: handler,
		NotifyStartedFunc: func() { close(ready) },
	}
	go func() { done <- server.ActivateAndServe() }()
	<-ready
	t.Cleanup(func() {
		if err := server.Shutdown(); err != nil {
			t.Error(err)
		}
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return conn.LocalAddr().String()
}

func testQuery() *dns.Msg {
	return new(dns.Msg).SetQuestion("example.org.", dns.TypeA)
}

func testAnswer(query *dns.Msg, ip string) *dns.Msg {
	resp := new(dns.Msg).SetReply(query)
	resp.Answer = []dns.RR{&dns.A{
		Hdr: dns.RR_Header{Name: query.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
		A:   net.ParseIP(ip),
	}}
	return resp
}

func TestQueryCancellationClosesInflightExchange(t *testing.T) {
	received := make(chan struct{}, 1)
	addr := startTestDNS(t, func(w dns.ResponseWriter, msg *dns.Msg) {
		received <- struct{}{}
	})
	server := newDNSServer(addr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := server.query(ctx, testQuery())
		done <- err
	}()
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive the query")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context cancellation, got %v", err)
		}
	case <-time.After(time.Second):
		// 等待本测试启动的查询退出，避免影响后续测试。
		<-done
		t.Fatal("cancellation did not interrupt the in-flight DNS read")
	}
}

func TestCanceledQueryDoesNotMarkServerUnavailable(t *testing.T) {
	server := newDNSServer("127.0.0.1:53")
	server.updateStatus(0, context.Canceled)
	if !server.isAvailable || server.failCount != 0 {
		t.Fatalf("cancellation changed health: available=%t failures=%d", server.isAvailable, server.failCount)
	}
}

func TestForwardWaitsForSuccessfulDNSResponse(t *testing.T) {
	failed := make(chan struct{})
	bad := startTestDNS(t, func(w dns.ResponseWriter, msg *dns.Msg) {
		resp := new(dns.Msg).SetRcode(msg, dns.RcodeServerFailure)
		_ = w.WriteMsg(resp)
		close(failed)
	})
	good := startTestDNS(t, func(w dns.ResponseWriter, msg *dns.Msg) {
		<-failed
		time.Sleep(20 * time.Millisecond)
		_ = w.WriteMsg(testAnswer(msg, "192.0.2.1"))
	})
	original := upstreamServers
	upstreamServers = []*DNSServer{newDNSServer(bad), newDNSServer(good)}
	t.Cleanup(func() { upstreamServers = original })
	isolateCache(t)
	resp, err := forwardToUpstreamDNS(context.Background(), testQuery())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
		t.Fatalf("want successful answer, got %s", resp)
	}
}

func TestHealthChecksStopAndPreserveCanceledServer(t *testing.T) {
	received := make(chan struct{}, 1)
	addr := startTestDNS(t, func(w dns.ResponseWriter, msg *dns.Msg) {
		received <- struct{}{}
	})
	original := upstreamServers
	server := newDNSServer(addr)
	upstreamServers = []*DNSServer{server}
	t.Cleanup(func() { upstreamServers = original })
	ctx, cancel := context.WithCancel(context.Background())
	done := startHealthCheck(ctx)
	t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("health check did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("health check did not stop after cancellation")
	}
	server.mu.RLock()
	defer server.mu.RUnlock()
	if !server.isAvailable || server.failCount != 0 {
		t.Fatal("shutdown cancellation marked the upstream unhealthy")
	}
}

func TestHealthCheckRecoversUnavailableServer(t *testing.T) {
	addr := startTestDNS(t, func(w dns.ResponseWriter, msg *dns.Msg) {
		_ = w.WriteMsg(testAnswer(msg, "192.0.2.1"))
	})
	original := upstreamServers
	server := newDNSServer(addr)
	server.updateStatus(0, errors.New("previous failure"))
	upstreamServers = []*DNSServer{server}
	t.Cleanup(func() { upstreamServers = original })
	if len(getAvailableServers()) != 0 {
		t.Fatal("failed server must be excluded")
	}
	checkAllServers(context.Background())
	if len(getAvailableServers()) != 1 || server.failCount != 0 {
		t.Fatal("successful health check did not restore the upstream")
	}
}

func TestForwardRespectsCallerDeadline(t *testing.T) {
	addr := startTestDNS(t, func(w dns.ResponseWriter, msg *dns.Msg) {})
	original := upstreamServers
	server := newDNSServer(addr)
	upstreamServers = []*DNSServer{server}
	t.Cleanup(func() { upstreamServers = original })
	isolateCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := forwardToUpstreamDNS(ctx, testQuery())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("forward ignored caller deadline")
	}
	server.mu.RLock()
	defer server.mu.RUnlock()
	if !server.isAvailable || server.failCount != 0 {
		t.Fatal("caller deadline marked a responsive upstream unavailable")
	}
}

func TestForwardPreservesDNSErrorWhenOtherUpstreamTimesOut(t *testing.T) {
	errorAddr := startTestDNS(t, func(w dns.ResponseWriter, msg *dns.Msg) {
		_ = w.WriteMsg(new(dns.Msg).SetRcode(msg, dns.RcodeServerFailure))
	})
	timeoutAddr := startTestDNS(t, func(w dns.ResponseWriter, msg *dns.Msg) {})
	original := upstreamServers
	responding := newDNSServer(errorAddr)
	upstreamServers = []*DNSServer{responding, newDNSServer(timeoutAddr)}
	t.Cleanup(func() { upstreamServers = original })
	isolateCache(t)
	resp, err := forwardToUpstreamDNS(context.Background(), testQuery())
	if err != nil || resp == nil || resp.Rcode != dns.RcodeServerFailure {
		t.Fatalf("want the received DNS failure despite the other timeout, got %v, %v", resp, err)
	}
	responding.mu.RLock()
	defer responding.mu.RUnlock()
	if !responding.isAvailable || dnsCache.ItemCount() != 0 {
		t.Fatal("DNS error changed upstream health or entered cache")
	}
}

func TestLoadUpstreamServersValidatesConfiguration(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:dns",
		"localhost:53", "127.0.0.1:53|99", "127.0.0.1:53,",
	} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("DOH_UPSTREAMS", raw)
			if _, err := loadUpstreamServers(); err == nil {
				t.Fatal("invalid configuration silently accepted")
			}
		})
	}
	t.Setenv("DOH_UPSTREAMS", "127.0.0.1:53,[::1]:53")
	servers, err := loadUpstreamServers()
	if err != nil || len(servers) != 2 {
		t.Fatalf("want IPv4 and IPv6 upstreams, got %v, %v", servers, err)
	}
	t.Setenv("DOH_UPSTREAMS", "")
	servers, err = loadUpstreamServers()
	if err != nil || len(servers) != len(defaultUpstreamAddrs) {
		t.Fatalf("default configuration failed: %v", err)
	}
}
