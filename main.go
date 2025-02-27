package main

import (
	"context"
	"encoding/base64"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
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
)

var (
	// 版本信息变量,通过编译时注入
	AppVersion = "1.0.2"
	BuildTime  = "unknown"
	GitCommit  = ""
	GoVersion  = runtime.Version()
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

func forwardToUpstreamDNS(msg *dns.Msg) (*dns.Msg, error) {
	dnsServer := environ.GetEnv("DNS_SERVER", "100.90.80.20:53")
	client := &dns.Client{
		Net:          "udp",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	resp, _, err := client.Exchange(msg, dnsServer)
	if err != nil {
		return nil, err
	}

	// 如果响应被截断，切换到 TCP 重试
	if resp != nil && resp.Truncated {
		log.Info("UDP response truncated, retrying with TCP")
		client.Net = "tcp"
		resp, _, err = client.Exchange(msg, dnsServer)
		if err != nil {
			return nil, err
		}
	}

	return resp, nil
}

func handle(c *fiber.Ctx) error {
	var dnsMsg []byte
	var err error

	if c.Method() == "GET" {
		dnsParam := c.Query("dns")
		if dnsParam == "" {
			return c.Redirect("/copilot", 302)
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

	log.Infof("DNS Query: %s, Type: %s",
		msg.Question[0].Name,
		dns.TypeToString[msg.Question[0].Qtype])

	start := time.Now()
	resp, err := forwardToUpstreamDNS(msg)
	duration := time.Since(start)

	log.Infof("DNS Query completed in %v", duration)

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
		ServerHeader: "traefik",
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			return c.Redirect("/copilot", 302)
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
	app.Get("/metrics", monitor.New(monitor.Config{Title: "Traefik Metrics"}))

	app.Get("/dns-query", handle)
	app.Post("/dns-query", handle)
	app.Get("/dnspod", handle)
	app.Post("/dnspod", handle)
	app.Get("/", handle)
	app.Post("/", handle)

	// 添加版本接口
	app.Get("/version", handleVersion)

	app.All("/copilot", func(c *fiber.Ctx) error {
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

	if err := app.Listen(":65001"); err != nil {
		log.Error("服务器启动失败:", err)
	}
}
