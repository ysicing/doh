package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/miekg/dns"
)

func TestAcceptQualityAndSpecificity(t *testing.T) {
	for _, tt := range []struct {
		accept string
		want   bool
	}{
		{"", true},
		{"application/*;q=0.2", true},
		{"application/dns-message;q=0", false},
		{"application/dns-message;q=0, */*;q=1", false},
		{"application/dns-message;q=0.5, */*;q=0", true},
		{"application/json", false},
		{"application/dns-message;q=NaN", false},
	} {
		t.Run(tt.accept, func(t *testing.T) {
			app := fiber.New()
			app.Get("/", func(c fiber.Ctx) error {
				if acceptsDNSMessage(c) != tt.want {
					t.Errorf("want accepted=%t for %q", tt.want, tt.accept)
				}
				return c.SendStatus(fiber.StatusOK)
			})
			req := httptest.NewRequest("GET", "/", nil)
			req.Header.Set("Accept", tt.accept)
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
		})
	}
}

func TestDebugRequiresTokenDespiteForwardedHeaders(t *testing.T) {
	t.Setenv("DOH_DEBUG_ENABLED", "1")
	t.Setenv("DOH_DEBUG_TOKEN", "")
	app := fiber.New()
	app.Get("/debug", debugMiddleware, func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })
	for _, header := range []string{"CF-Connecting-IP", "True-Client-IP", "X-Real-IP", "X-Forwarded-For"} {
		req := httptest.NewRequest("GET", "/debug", nil)
		req.Header.Set(header, "127.0.0.1")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != fiber.StatusNotFound {
			t.Fatalf("%s bypassed authentication", header)
		}
	}
}

func TestDoHGETPOSTShareCacheAndPreserveRequestID(t *testing.T) {
	var queries atomic.Int32
	addr := startTestDNS(t, func(w dns.ResponseWriter, msg *dns.Msg) {
		queries.Add(1)
		_ = w.WriteMsg(testAnswer(msg, "192.0.2.1"))
	})
	original := upstreamServers
	upstreamServers = []*DNSServer{newDNSServer(addr)}
	t.Cleanup(func() { upstreamServers = original })
	isolateCache(t)
	app := fiber.New()
	app.Get("/dns-query", handle)
	app.Post("/dns-query", handle)
	for _, method := range []string{"GET", "POST"} {
		query := testQuery()
		if method == "POST" {
			query.Id++
			query.Question[0].Name = "EXAMPLE.org."
		}
		wire, err := query.Pack()
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, "/dns-query", bytes.NewReader(wire))
		if method == "GET" {
			req = httptest.NewRequest(method, "/dns-query?dns="+base64.RawURLEncoding.EncodeToString(wire), nil)
		}
		req.Header.Set("Content-Type", "application/dns-message")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != fiber.StatusOK || resp.Header.Get("Content-Type") != "application/dns-message" {
			t.Fatalf("unexpected HTTP response: %d %s", resp.StatusCode, body)
		}
		answer := new(dns.Msg)
		if err := answer.Unpack(body); err != nil {
			t.Fatal(err)
		}
		if answer.Id != query.Id || answer.Question[0] != query.Question[0] || len(answer.Answer) != 1 {
			t.Fatalf("response does not match the request: %s", answer)
		}
	}
	if queries.Load() != 1 {
		t.Fatalf("want one upstream query, got %d", queries.Load())
	}
}

func TestMalformedDNSRequestsAreRejected(t *testing.T) {
	app := fiber.New()
	app.Post("/dns-query", handle)
	multiple := testQuery()
	multiple.Question = append(multiple.Question, multiple.Question[0])
	response := new(dns.Msg).SetReply(testQuery())
	update := testQuery()
	update.Opcode = dns.OpcodeUpdate
	for _, msg := range []*dns.Msg{new(dns.Msg), multiple, response, update} {
		wire, err := msg.Pack()
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", "/dns-query", bytes.NewReader(wire))
		req.Header.Set("Content-Type", "application/dns-message")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("want bad request, got %d", resp.StatusCode)
		}
	}
}

func TestDNSErrorIsReturnedWithoutDisablingUpstream(t *testing.T) {
	addr := startTestDNS(t, func(w dns.ResponseWriter, msg *dns.Msg) {
		_ = w.WriteMsg(new(dns.Msg).SetRcode(msg, dns.RcodeServerFailure))
	})
	original := upstreamServers
	server := newDNSServer(addr)
	upstreamServers = []*DNSServer{server}
	t.Cleanup(func() { upstreamServers = original })
	isolateCache(t)
	app := fiber.New()
	app.Post("/dns-query", handle)
	wire, err := testQuery().Pack()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/dns-query", bytes.NewReader(wire))
	req.Header.Set("Content-Type", "application/dns-message")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("valid SERVFAIL must remain a DNS response, got HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	msg := new(dns.Msg)
	if err := msg.Unpack(body); err != nil || msg.Rcode != dns.RcodeServerFailure {
		t.Fatalf("want DNS SERVFAIL, got %v, %s", err, msg)
	}
	if len(getAvailableServers()) != 1 || dnsCache.ItemCount() != 0 {
		t.Fatal("DNS errors must not disable reachable upstreams or enter the cache")
	}
}

func TestDoHBoundsLargeResponse(t *testing.T) {
	addr := startTestDNS(t, func(w dns.ResponseWriter, msg *dns.Msg) {
		resp := new(dns.Msg).SetReply(msg)
		for range 20 {
			resp.Answer = append(resp.Answer, &dns.TXT{
				Hdr: dns.RR_Header{Name: "example.org.", Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60},
				Txt: []string{strings.Repeat("x", 250)},
			})
		}
		_ = w.WriteMsg(resp)
	})
	original := upstreamServers
	upstreamServers = []*DNSServer{newDNSServer(addr)}
	t.Cleanup(func() { upstreamServers = original })
	isolateCache(t)
	app := fiber.New()
	app.Post("/dns-query", handle)
	query := new(dns.Msg).SetQuestion("example.org.", dns.TypeTXT)
	query.SetEdns0(65535, false)
	wire, err := query.Pack()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/dns-query", bytes.NewReader(wire))
	req.Header.Set("Content-Type", "application/dns-message")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK || len(body) > maxDNSMessageSize {
		t.Fatalf("response is not bounded: status=%d size=%d", resp.StatusCode, len(body))
	}
	answer := new(dns.Msg)
	if err := answer.Unpack(body); err != nil || !answer.Truncated {
		t.Fatalf("want a truncated DNS response, got %v, %s", err, answer)
	}
	if dnsCache.ItemCount() != 0 {
		t.Fatal("oversized response should not enter cache")
	}
}

func TestOperationalRoutesAndReadiness(t *testing.T) {
	t.Setenv("DOH_DEBUG_ENABLED", "1")
	t.Setenv("DOH_DEBUG_TOKEN", "secret")
	original := upstreamServers
	server := newDNSServer("127.0.0.1:53")
	upstreamServers = []*DNSServer{server}
	t.Cleanup(func() { upstreamServers = original })
	isolateCache(t)
	app := newApp(context.Background())
	for _, path := range []string{"/live", "/ready", "/version", "/metrics", "/deepseek", "/debug/status", "/debug/cache"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer secret")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("%s returned %d", path, resp.StatusCode)
		}
	}
	server.updateStatus(0, errors.New("unreachable"))
	resp, err := app.Test(httptest.NewRequest("GET", "/ready", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("want unavailable readiness, got %d", resp.StatusCode)
	}
}
