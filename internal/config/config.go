// Package config 负责从 etc/config.yaml 装载 OCPX MCP 服务的运行参数。
//
// 配置文件的加载方式与 adt-go 下其他项目一致：go-zero 的 conf.MustLoad
// 读取 -f 指定的 yaml（默认 etc/config.yaml）。
package config

import (
	"fmt"
	"strings"
	"time"
)

// Config 项目总配置，对应 etc/config.yaml。
type Config struct {
	Name string
	Host string
	Port int

	MCP   MCPConfig
	Doris DorisConfig
	Query QueryConfig
}

// MCPConfig MCP 协议相关配置。
type MCPConfig struct {
	EndpointPath string `json:",default=/mcp"`
	// AuthToken 为空表示关闭鉴权，仅供本地调试，线上必须配置。
	AuthToken string `json:",optional"`
}

// DorisConfig Doris 连接配置。
//
// 支持两种后端，二选一：
//   - 直连（默认）：走 MySQL 协议连 FE，需要 Host/Port/User/Password；
//   - HTTP 网关：配了 QueryURL 就改走它转发 SELECT，用于拿不到 Doris 直连
//     凭据、只有一个查询 HTTP 接口的场景。此时 Host/User 不再必填。
type DorisConfig struct {
	Host string `json:",optional"`
	// Port 是 FE 的 MySQL 协议端口，不是 8030。
	Port     int    `json:",default=9030"`
	User     string `json:",optional"`
	Password string `json:",optional"`
	Database string `json:",default=ocpx"`
	Charset  string `json:",default=utf8mb4"`

	MaxOpenConns int `json:",default=16"`
	MaxIdleConns int `json:",default=4"`
	// ConnMaxLifetime 连接最大生命周期（秒）。
	ConnMaxLifetime int `json:",default=3600"`

	// QueryURL 非空则走 HTTP 网关而非直连。接口约定：GET 传 sql/key，
	// 返回 {"code":0,"data":[{列:值}...],"errInfo":[]}。
	QueryURL string `json:",optional"`
	// QueryKey HTTP 网关的鉴权 key，随请求一起发。
	QueryKey string `json:",optional"`

	// LogLevel SQL 日志级别: silent / error / warn / info。
	// 日志打到标准输出，由 systemd 落到 output.log，不单独写文件。
	LogLevel string `json:",default=info,options=silent|error|warn|info"`
}

// QueryConfig 查询安全限制。
type QueryConfig struct {
	// Timeout 单条查询超时（秒）。
	Timeout int `json:",default=120"`
	// MaxRows 单次返回行数上限。
	MaxRows int `json:",default=2000"`
	// MaxWindowDays 时间窗口最大跨度（天），防全表扫。
	MaxWindowDays int `json:",default=31"`
}

// UseHTTPBackend 表示走 HTTP 网关而非 MySQL 直连。
func (c *Config) UseHTTPBackend() bool {
	return strings.TrimSpace(c.Doris.QueryURL) != ""
}

// TableQualifier 返回 SQL 里表名的库限定前缀。
//
// 直连时返回 Database（生成 `monitor`.`ocpx_v1_clk`）；HTTP 网关模式返回空，
// 生成裸表名（`ocpx_v1_clk`）——那个网关只认裸表名，带库前缀会被它的表白名单
// 拒绝（把库名当成表名去匹配）。
func (c *Config) TableQualifier() string {
	if c.UseHTTPBackend() {
		return ""
	}
	return c.Doris.Database
}

// Validate 在 conf.MustLoad 之后做跨字段校验与兜底。
func (c *Config) Validate() error {
	if !strings.HasPrefix(c.MCP.EndpointPath, "/") {
		c.MCP.EndpointPath = "/" + c.MCP.EndpointPath
	}
	// HTTP 网关模式不需要直连凭据，只校验 URL；直连模式才要 Host/User。
	if c.UseHTTPBackend() {
		if !strings.HasPrefix(c.Doris.QueryURL, "http://") && !strings.HasPrefix(c.Doris.QueryURL, "https://") {
			return fmt.Errorf("Doris.QueryURL 必须是 http(s):// 开头的完整地址")
		}
	} else {
		if c.Doris.Host == "" {
			return fmt.Errorf("Doris.Host 必须配置（或配 Doris.QueryURL 走 HTTP 网关）")
		}
		if c.Doris.User == "" {
			return fmt.Errorf("Doris.User 必须配置（或配 Doris.QueryURL 走 HTTP 网关）")
		}
	}
	if c.Query.MaxRows <= 0 {
		return fmt.Errorf("Query.MaxRows 必须为正数")
	}
	if c.Query.MaxWindowDays <= 0 {
		return fmt.Errorf("Query.MaxWindowDays 必须为正数")
	}
	if c.Query.Timeout <= 0 {
		return fmt.Errorf("Query.Timeout 必须为正数")
	}
	// 空闲连接数不该超过打开上限，超了按上限收敛而不是报错。
	if c.Doris.MaxIdleConns > c.Doris.MaxOpenConns {
		c.Doris.MaxIdleConns = c.Doris.MaxOpenConns
	}
	return nil
}

// Addr 返回 HTTP 监听地址。
func (c *Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// QueryTimeout 单条查询超时。
func (c *Config) QueryTimeout() time.Duration {
	return time.Duration(c.Query.Timeout) * time.Second
}

// ConnMaxLife 连接最大生命周期。
func (c *Config) ConnMaxLife() time.Duration {
	return time.Duration(c.Doris.ConnMaxLifetime) * time.Second
}

// DSN 拼装 go-sql-driver/mysql 的连接串。
//
// 参数说明：
//
//	parseTime      让 datetime 直接扫成 time.Time
//	charset        Doris 建议 utf8mb4
//	loc/time_zone  统一按东八区解释 datetime，避免时区漂移
func (c *Config) DSN() string {
	charset := c.Doris.Charset
	if charset == "" {
		charset = "utf8mb4"
	}
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=%s&parseTime=true&loc=Asia%%2FShanghai&timeout=10s&readTimeout=300s&writeTimeout=30s",
		c.Doris.User, c.Doris.Password, c.Doris.Host, c.Doris.Port, c.Doris.Database, charset)
}

// Redacted 返回去掉密码的 DSN，用于日志输出。
func (c *Config) Redacted() string {
	dsn := c.DSN()
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return dsn
	}
	colon := strings.Index(dsn[:at], ":")
	if colon < 0 {
		return dsn
	}
	return dsn[:colon+1] + "***" + dsn[at:]
}

// BackendDesc 返回当前后端的可读描述（脱敏），用于启动日志。
func (c *Config) BackendDesc() string {
	if c.UseHTTPBackend() {
		desc := "HTTP 网关 " + c.Doris.QueryURL
		if c.Doris.QueryKey != "" {
			desc += "（key=***）"
		}
		return desc
	}
	return "MySQL 直连 " + c.Redacted()
}
