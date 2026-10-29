package main

import (
	"crypto/subtle"
	"encoding/base64"
	"mime"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/log"
	"github.com/miekg/dns"
)

const maxDNSMessageSize = 4096

func handle(c fiber.Ctx) error {
	if c.Method() != fiber.MethodGet && !isDNSMessageContentType(c.Get("Content-Type")) {
		return c.Status(fiber.StatusUnsupportedMediaType).JSON(fiber.Map{"error": "Content-Type 必须为 application/dns-message"})
	}
	if !acceptsDNSMessage(c) {
		return c.Status(fiber.StatusNotAcceptable).JSON(fiber.Map{"error": "客户端不接受 DNS 消息"})
	}
	wire := c.Body()
	if c.Method() == fiber.MethodGet {
		encoded := c.Query("dns")
		if encoded == "" {
			return c.Redirect().Status(fiber.StatusFound).To("/deepseek")
		}
		var err error
		wire, err = base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "无效的 base64 参数"})
		}
	}
	if len(wire) > maxDNSMessageSize {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "请求数据过大"})
	}
	msg := new(dns.Msg)
	if err := msg.Unpack(wire); err != nil || msg.Response || msg.Opcode != dns.OpcodeQuery || len(msg.Question) != 1 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "请求必须为单 Question DNS 查询"})
	}
	resp, err := forwardToUpstreamDNS(c.Context(), msg)
	if err != nil {
		log.Warnf("DNS 查询失败: %v", err)
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "上游 DNS 不可用"})
	}
	resp.Id = msg.Id
	// 上游只负责查询，HTTP 出口统一负责响应大小；缓存独立执行准入限制。
	if resp.Len() > maxDNSMessageSize {
		resp.Truncate(maxDNSMessageSize)
	}
	packed, err := resp.Pack()
	if err != nil {
		log.Errorf("DNS 响应打包失败: %v", err)
		return c.SendStatus(fiber.StatusInternalServerError)
	}
	if len(packed) > maxDNSMessageSize {
		return c.SendStatus(fiber.StatusBadGateway)
	}
	c.Set("Content-Type", "application/dns-message")
	// 避免 HTTP 中间缓存再次延长 DNS 记录的有效期。
	c.Set("Cache-Control", "no-store")
	return c.Send(packed)
}

func isDNSMessageContentType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "application/dns-message"
}

func acceptsDNSMessage(c fiber.Ctx) bool {
	accept := strings.TrimSpace(c.Get("Accept"))
	if accept == "" {
		return true
	}
	bestSpecificity, quality := -1, 0.0
	for _, part := range strings.Split(accept, ",") {
		mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		specificity, supported := map[string]int{
			"*/*": 0, "application/*": 1, "application/dns-message": 2,
		}[mediaType]
		if !supported {
			continue
		}
		q := 1.0
		if raw, exists := params["q"]; exists {
			q, err = strconv.ParseFloat(raw, 64)
			if err != nil || !(q >= 0 && q <= 1) {
				continue
			}
		}
		// 更精确的媒体类型优先，显式 q=0 不能被通配符覆盖。
		if specificity > bestSpecificity {
			bestSpecificity, quality = specificity, q
		} else if specificity == bestSpecificity {
			quality = max(quality, q)
		}
	}
	return quality > 0
}

func getRealIP(c fiber.Ctx) string {
	// 转发头仅用于展示和日志，不能作为鉴权依据。
	for _, header := range []string{"CF-Connecting-IP", "True-Client-IP", "X-Real-IP", "X-Forwarded-For"} {
		raw, _, _ := strings.Cut(c.Get(header), ",")
		if ip := net.ParseIP(strings.TrimSpace(raw)); ip != nil {
			return ip.String()
		}
	}
	return c.IP()
}

func allowDebug(c fiber.Ctx) bool {
	if os.Getenv("DOH_DEBUG_ENABLED") != "1" {
		return false
	}
	expected := strings.TrimSpace(os.Getenv("DOH_DEBUG_TOKEN"))
	if expected == "" {
		return false
	}
	token, bearer := strings.CutPrefix(strings.TrimSpace(c.Get("Authorization")), "Bearer ")
	if bearer && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), []byte(expected)) == 1 {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(c.Get("X-Debug-Token"))), []byte(expected)) == 1
}

func debugMiddleware(c fiber.Ctx) error {
	if !allowDebug(c) {
		return c.SendStatus(fiber.StatusNotFound)
	}
	return c.Next()
}
