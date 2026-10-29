package main

import (
	"context"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/gofiber/contrib/v3/monitor"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/log"
	"github.com/gofiber/fiber/v3/middleware/logger"
	"github.com/gofiber/fiber/v3/middleware/requestid"
)

// 版本信息通过构建参数注入。
var (
	AppVersion = "1.1.10"
	BuildTime  = "unknown"
	GitCommit  = ""
	GoVersion  = runtime.Version()
)

func newApp(ctx context.Context) *fiber.App {
	app := fiber.New(fiber.Config{ServerHeader: "Caddy2"})
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(ctx)
		return c.Next()
	})
	app.Use(requestid.New())
	app.Use(logger.New(logger.Config{
		Format:     "${time} ${locals:requestid} ${clientip} ${method} ${status} ${path} ${latency}\n",
		TimeFormat: "2006-01-02 15:04:05",
		TimeZone:   "Asia/Shanghai",
		CustomTags: map[string]logger.LogFunc{
			"clientip": func(output logger.Buffer, c fiber.Ctx, _ *logger.Data, _ string) (int, error) {
				return output.WriteString(getRealIP(c))
			},
		},
		Next: func(c fiber.Ctx) bool {
			ua := strings.ToLower(c.Get("User-Agent"))
			return strings.Contains(ua, "kube-probe") || strings.Contains(ua, "uptime-kuma")
		},
	}))
	app.Get("/live", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })
	app.Get("/ready", func(c fiber.Ctx) error {
		if len(getAvailableServers()) == 0 {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		return c.SendStatus(fiber.StatusOK)
	})
	app.Get("/metrics", monitor.New(monitor.Config{Title: "Caddy Metrics"}))
	for _, path := range []string{"/dns-query", "/dnspod", "/"} {
		app.Get(path, handle)
		app.Post(path, handle)
	}
	app.Get("/version", handleVersion)
	app.Route("/debug", func(router fiber.Router) {
		router.Use(debugMiddleware)
		router.Get("/cache", handleCacheInfo)
		router.Get("/status", handleStatus)
		router.Post("/reset", handleResetStats)
		router.Post("/check", handleManualCheck)
	})
	app.All("/deepseek", func(c fiber.Ctx) error {
		return c.SendString("ip: " + getRealIP(c))
	})
	return app
}

func main() {
	var err error
	upstreamServers, err = loadUpstreamServers()
	if err != nil {
		log.Fatal(err)
	}
	if os.Getenv("DOH_DEBUG_ENABLED") == "1" && strings.TrimSpace(os.Getenv("DOH_DEBUG_TOKEN")) == "" {
		log.Warn("未配置 DOH_DEBUG_TOKEN，调试接口将拒绝访问")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	healthDone := startHealthCheck(ctx)
	app := newApp(ctx)
	if err := app.Listen(":65001", fiber.ListenConfig{
		GracefulContext: ctx,
		ShutdownTimeout: 10 * time.Second,
	}); err != nil {
		log.Errorf("HTTP 服务退出: %v", err)
	}
	stop()
	<-healthDone
}
