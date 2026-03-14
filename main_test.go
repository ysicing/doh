package main

import (
	"encoding/base64"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/miekg/dns"
)

func TestCalculateCacheTTLPositiveAnswer(t *testing.T) {
	resp := new(dns.Msg)
	resp.SetReply(&dns.Msg{
		Question: []dns.Question{{Name: "example.org.", Qtype: dns.TypeA, Qclass: dns.ClassINET}},
	})
	resp.Answer = []dns.RR{
		&dns.A{
			Hdr: dns.RR_Header{Name: "example.org.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
		},
	}

	if got := calculateCacheTTL(resp); got != 30*time.Second {
		t.Fatalf("calculateCacheTTL() = %v, want %v", got, 30*time.Second)
	}
}

func TestCalculateCacheTTLNegativeResponse(t *testing.T) {
	resp := new(dns.Msg)
	resp.Rcode = dns.RcodeNameError
	resp.Ns = []dns.RR{
		&dns.SOA{
			Hdr:     dns.RR_Header{Name: "example.org.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 120},
			Ns:      "ns1.example.org.",
			Mbox:    "hostmaster.example.org.",
			Serial:  1,
			Refresh: 60,
			Retry:   60,
			Expire:  60,
			Minttl:  45,
		},
	}

	if got := calculateCacheTTL(resp); got != 45*time.Second {
		t.Fatalf("calculateCacheTTL() = %v, want %v", got, 45*time.Second)
	}
}

func TestGetCacheKeyIncludesDNSSECSignal(t *testing.T) {
	base := new(dns.Msg)
	base.SetQuestion("example.org.", dns.TypeA)

	withDO := base.Copy()
	withDO.SetEdns0(4096, true)

	if getCacheKey(base) == getCacheKey(withDO) {
		t.Fatal("cache key should differ when DO bit differs")
	}
}

func TestAdjustTTLSkipsOPTRecord(t *testing.T) {
	opt := &dns.OPT{
		Hdr: dns.RR_Header{
			Name:   ".",
			Rrtype: dns.TypeOPT,
			Class:  4096,
			Ttl:    0x8000,
		},
	}
	a := &dns.A{
		Hdr: dns.RR_Header{Name: "example.org.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300},
	}

	adjustTTL([]dns.RR{opt, a}, 60)

	if opt.Hdr.Ttl != 0x8000 {
		t.Fatalf("OPT TTL/flags = %d, want %d", opt.Hdr.Ttl, 0x8000)
	}
	if a.Hdr.Ttl != 60 {
		t.Fatalf("A TTL = %d, want %d", a.Hdr.Ttl, 60)
	}
}

func TestAllowDebugRequiresTokenWhenConfigured(t *testing.T) {
	t.Setenv("DOH_DEBUG_ENABLED", "1")
	t.Setenv("DOH_DEBUG_TOKEN", "secret")

	app := fiber.New()
	app.Get("/debug", func(c fiber.Ctx) error {
		if allowDebug(c) {
			return c.SendStatus(fiber.StatusOK)
		}
		return c.SendStatus(fiber.StatusForbidden)
	})

	req := httptest.NewRequest("GET", "/debug", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test() error = %v", err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusForbidden)
	}

	req = httptest.NewRequest("GET", "/debug", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err = app.Test(req)
	if err != nil {
		t.Fatalf("app.Test() error = %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusOK)
	}
}

func TestAllowDebugUsesRealClientIPWithoutToken(t *testing.T) {
	t.Setenv("DOH_DEBUG_ENABLED", "1")
	t.Setenv("DOH_DEBUG_TOKEN", "")

	app := fiber.New()
	app.Get("/debug", func(c fiber.Ctx) error {
		if allowDebug(c) {
			return c.SendStatus(fiber.StatusOK)
		}
		return c.SendStatus(fiber.StatusForbidden)
	})

	req := httptest.NewRequest("GET", "/debug", nil)
	req.RemoteAddr = "10.0.0.10:12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.8")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test() error = %v", err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusForbidden)
	}
}

func TestParseUpstreamServer(t *testing.T) {
	server, err := parseUpstreamServer("8.8.8.8:53|77", 0)
	if err != nil {
		t.Fatalf("parseUpstreamServer() error = %v", err)
	}
	if server.addr != "8.8.8.8:53" {
		t.Fatalf("addr = %q, want %q", server.addr, "8.8.8.8:53")
	}
	if server.priority != 77 {
		t.Fatalf("priority = %d, want %d", server.priority, 77)
	}
	if server.isIPv6 {
		t.Fatal("expected IPv4 upstream")
	}
}

func TestHandlePOSTRejectsUnsupportedMediaType(t *testing.T) {
	app := fiber.New()
	app.Post("/dns-query", handle)

	req := httptest.NewRequest("POST", "/dns-query", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/dns-message")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test() error = %v", err)
	}
	if resp.StatusCode != fiber.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusUnsupportedMediaType)
	}
}

func TestHandleGETRejectsUnsupportedAccept(t *testing.T) {
	app := fiber.New()
	app.Get("/dns-query", handle)

	msg := new(dns.Msg)
	msg.SetQuestion("example.org.", dns.TypeA)
	wire, err := msg.Pack()
	if err != nil {
		t.Fatalf("Pack() error = %v", err)
	}

	req := httptest.NewRequest("GET", "/dns-query?dns="+base64.RawURLEncoding.EncodeToString(wire), nil)
	req.Header.Set("Accept", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test() error = %v", err)
	}
	if resp.StatusCode != fiber.StatusNotAcceptable {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusNotAcceptable)
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
