// OCPX Doris MCP Server
//
// 一个基于 mark3labs/mcp-go 的 MCP 服务，把 Doris 上的 OCPX 广告归因明细表
// 包装成业务语义工具（漏斗、下钻、趋势、设备反查、丢失分析），
// 通过 Streamable HTTP + Bearer token 对外提供。
//
// 启动示例:
//
//	go run . -f etc/config.yaml
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/zeromicro/go-zero/core/conf"

	"github.com/chemanyu/mcp/internal/config"
	"github.com/chemanyu/mcp/internal/doris"
	"github.com/chemanyu/mcp/internal/tools"
)

const (
	serverName    = "ocpx-doris-mcp"
	serverVersion = "1.0.0"
)

// instructions 是发给客户端的服务级说明，会出现在 initialize 响应里。
// 它约束模型的整体调用策略，单个工具的细节写在各自的 description 里。
const instructions = `本服务提供 OCPX 广告归因数据（Doris）的只读查询能力。

推荐调用顺序:
  1. list_ocpx_tables       —— 先拿表地图，确认业务线（v1 通用 / jd 京东）
  2. describe_ocpx_table    —— 需要写过滤条件或选维度时，先读列定义
  3. 业务工具               —— ocpx_funnel（漏斗转化率）/ ocpx_breakdown（维度下钻）
                               / ocpx_trend（时间趋势）/ ocpx_device_lookup（单设备排查）
                               / ocpx_loss_analysis（丢失与报错）/ ocpx_sample_rows（看样本）
  4. ocpx_run_sql           —— 仅当上面工具都表达不了时的兜底

统一口径（业务工具已内置，无需自己处理）:
  - 正式统计剔除 test_status != 0（测试流量）与 is_loss = 1（丢失记录）
  - 转化量按 action_pv 求和，不是 count(*)
  - 所有查询都必须带 req_time 范围过滤；时间窗口有跨度上限

不要凭记忆猜测表名或列名——猜错会直接报错，读一次元数据更快。`

var configFile = flag.String("f", "etc/config.yaml", "the config file")

func main() {
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[ocpx-mcp] ")

	var cfg config.Config
	conf.MustLoad(*configFile, &cfg)
	if err := cfg.Validate(); err != nil {
		log.Fatalf("配置校验失败: %v", err)
	}

	// 按配置选后端：配了 Doris.QueryURL 走 HTTP 网关，否则 MySQL 直连。
	var db doris.Querier
	var err error
	if cfg.UseHTTPBackend() {
		db, err = doris.NewHTTP(doris.HTTPOptions{
			URL:      cfg.Doris.QueryURL,
			Key:      cfg.Doris.QueryKey,
			Timeout:  cfg.QueryTimeout(),
			MaxRows:  cfg.Query.MaxRows,
			LogLevel: cfg.Doris.LogLevel,
		})
	} else {
		db, err = doris.New(doris.Options{
			DSN:          cfg.DSN(),
			Database:     cfg.Doris.Database,
			MaxOpenConns: cfg.Doris.MaxOpenConns,
			MaxIdleConns: cfg.Doris.MaxIdleConns,
			ConnMaxLife:  cfg.ConnMaxLife(),
			Timeout:      cfg.QueryTimeout(),
			MaxRows:      cfg.Query.MaxRows,
			LogLevel:     cfg.Doris.LogLevel,
		})
	}
	if err != nil {
		log.Fatalf("Doris 客户端初始化失败: %v", err)
	}
	defer db.Close()

	// 启动时探活。连不上不直接退出——后端可能晚于本服务就绪，
	// 真正的错误会在工具调用时以 connection_failed 返回给客户端。
	if err := db.Ping(context.Background()); err != nil {
		log.Printf("警告: 后端探活失败（服务继续启动，请检查连接配置）: %v", err)
	} else {
		log.Printf("后端连接成功: %s", cfg.BackendDesc())
	}

	mcpServer := server.NewMCPServer(
		serverName, serverVersion,
		server.WithToolCapabilities(true),
		server.WithInstructions(instructions),
		server.WithRecovery(), // 单个工具 panic 不拖垮整个服务
		server.WithLogging(),
	)
	tools.Register(mcpServer, &tools.Deps{DB: db, Cfg: &cfg})

	httpServer := server.NewStreamableHTTPServer(mcpServer,
		server.WithEndpointPath(cfg.MCP.EndpointPath),
		server.WithHeartbeatInterval(25*time.Second),
	)

	handler := withAuth(cfg.MCP.AuthToken, withRequestLog(httpServer))
	mux := http.NewServeMux()
	mux.Handle(cfg.MCP.EndpointPath, handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("doris unreachable: " + err.Error()))
			return
		}
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// 不设 WriteTimeout：MCP 的 SSE 流是长连接，会被写超时切断。
	}

	if cfg.MCP.AuthToken == "" {
		log.Printf("警告: 未配置 MCP.AuthToken，服务将不做鉴权。生产环境请务必配置")
	}
	log.Printf("MCP 端点: http://%s%s", displayAddr(cfg.Addr()), cfg.MCP.EndpointPath)
	log.Printf("健康检查: http://%s/healthz", displayAddr(cfg.Addr()))
	log.Printf("目标库: %s | 单查询超时 %s | 行数上限 %d | 窗口上限 %d 天",
		cfg.Doris.Database, cfg.QueryTimeout(), cfg.Query.MaxRows, cfg.Query.MaxWindowDays)

	// 优雅退出：收到信号后给在途请求 10 秒收尾。
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Printf("收到退出信号，正在关闭…")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("HTTP 服务异常退出: %v", err)
	}
	log.Printf("已关闭")
}

// withAuth 校验 Bearer token。token 为空时放行（本地调试用）。
//
// 对比用 subtle.ConstantTimeCompare 而不是 ==，避免按字符早退泄漏 token 前缀。
func withAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := extractBearer(r)
		if got == "" || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			// 按 MCP 授权规范返回 WWW-Authenticate，客户端据此提示重新鉴权。
			w.Header().Set("WWW-Authenticate", `Bearer realm="ocpx-mcp"`)
			http.Error(w, "unauthorized: 缺少或错误的 Bearer token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// extractBearer 从 Authorization 头取 token，兼容客户端直接把裸 token 放进头里的情况。
func extractBearer(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if h == "" {
		return ""
	}
	if len(h) >= 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return h
}

// withRequestLog 记录请求耗时，方便定位慢工具。
func withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s from %s in %s", r.Method, r.URL.Path, clientIP(r), time.Since(start).Round(time.Millisecond))
	})
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.Index(v, ","); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	if i := strings.LastIndex(r.RemoteAddr, ":"); i > 0 {
		return r.RemoteAddr[:i]
	}
	return r.RemoteAddr
}

func displayAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "0.0.0.0" + addr
	}
	return addr
}
