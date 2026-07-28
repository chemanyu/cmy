// Package doris 封装对 Doris 的只读访问。
//
// 有两个后端实现，对上层暴露同一个 Querier 接口：
//   - Client：走 MySQL 协议直连 Doris FE（本文件）；
//   - HTTPClient：走 HTTP 查询网关转发 SELECT（http.go）。
package doris

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// Querier 是只读查询后端对上层暴露的统一接口。
// Client（直连）与 HTTPClient（HTTP 网关）都实现它，
// 因此工具层不感知底层用的是哪种后端。
type Querier interface {
	// Query 执行一条只读 SQL；args 为 ? 占位符的参数。
	Query(ctx context.Context, query string, args ...any) (*Result, error)
	// QueryOneRow 返回第一行的 map，没有行时返回 nil。
	QueryOneRow(ctx context.Context, query string, args ...any) (map[string]any, error)
	// Ping 探活。
	Ping(ctx context.Context) error
	// Close 释放资源。
	Close() error
}

// Client 是走 MySQL 协议直连 Doris 的只读查询客户端。
type Client struct {
	db       *sql.DB
	database string
	timeout  time.Duration
	maxRows  int
	sqlLog   *sqlLogger
}

// Options 构造 Client 所需参数。
type Options struct {
	DSN          string
	Database     string
	MaxOpenConns int
	MaxIdleConns int
	ConnMaxLife  time.Duration
	Timeout      time.Duration
	MaxRows      int
	// LogLevel SQL 日志级别，silent 表示不记录。日志固定打到标准输出。
	LogLevel string
}

// New 建立连接池。注意 sql.Open 不会真正拨号，需调用 Ping 验证。
func New(opts Options) (*Client, error) {
	db, err := sql.Open("mysql", opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("打开 Doris 连接失败: %w", err)
	}
	db.SetMaxOpenConns(opts.MaxOpenConns)
	db.SetMaxIdleConns(opts.MaxIdleConns)
	db.SetConnMaxLifetime(opts.ConnMaxLife)

	return &Client{
		db:       db,
		database: opts.Database,
		timeout:  opts.Timeout,
		maxRows:  opts.MaxRows,
		sqlLog:   newSQLLogger(opts.LogLevel),
	}, nil
}

// Ping 验证连通性。
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.db.PingContext(ctx)
}

// Close 关闭连接池。
func (c *Client) Close() error { return c.db.Close() }

// Result 是一次查询的结果集。
type Result struct {
	Columns     []string `json:"columns"`
	Rows        [][]any  `json:"rows"`
	RowCount    int      `json:"row_count"`
	Truncated   bool     `json:"truncated"`
	ExecutionMS int64    `json:"execution_ms"`
	SQL         string   `json:"sql"`
}

// Query 执行一条只读 SQL 并把结果读进内存。
//
// 调用方必须保证 SQL 已经过 query 包的构造/校验——本函数只做执行与
// 行数封顶，不做语义审查。
func (c *Client) Query(ctx context.Context, query string, args ...any) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	start := time.Now()
	// 无论从哪个分支返回，都记一条 SQL 日志。
	var (
		logRows int
		logErr  error
	)
	defer func() {
		c.sqlLog.trace(query, args, time.Since(start), logRows, logErr)
	}()

	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		logErr = err
		return nil, wrapQueryError(err, c.timeout, query)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		logErr = err
		return nil, fmt.Errorf("读取结果列名失败: %w", err)
	}

	res := &Result{Columns: cols, Rows: [][]any{}, SQL: query}
	// 多读一行用来判断是否被截断。
	limit := c.maxRows
	for rows.Next() {
		if len(res.Rows) >= limit {
			res.Truncated = true
			break
		}
		holders := make([]any, len(cols))
		for i := range holders {
			holders[i] = new(sql.RawBytes)
		}
		if err := rows.Scan(holders...); err != nil {
			logErr = err
			return nil, fmt.Errorf("扫描结果行失败: %w", err)
		}
		row := make([]any, len(cols))
		for i, h := range holders {
			row[i] = normalize(*(h.(*sql.RawBytes)))
		}
		res.Rows = append(res.Rows, row)
	}
	if err := rows.Err(); err != nil {
		logErr = err
		return nil, wrapQueryError(err, c.timeout, query)
	}

	res.RowCount = len(res.Rows)
	res.ExecutionMS = time.Since(start).Milliseconds()
	logRows = res.RowCount
	return res, nil
}

// QueryOneRow 执行查询并返回第一行的 map 形式，没有行时返回 nil。
func (c *Client) QueryOneRow(ctx context.Context, query string, args ...any) (map[string]any, error) {
	res, err := c.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	if len(res.Rows) == 0 {
		return nil, nil
	}
	out := map[string]any{}
	for i, col := range res.Columns {
		if i < len(res.Rows[0]) {
			out[col] = res.Rows[0][i]
		}
	}
	return out, nil
}

// RowMaps 把结果集转成 []map，便于组装业务返回。
func (r *Result) RowMaps() []map[string]any {
	out := make([]map[string]any, 0, len(r.Rows))
	for _, row := range r.Rows {
		m := map[string]any{}
		for i, col := range r.Columns {
			if i < len(row) {
				m[col] = row[i]
			}
		}
		out = append(out, m)
	}
	return out
}

// Markdown 把结果集渲染成 markdown 表格，作为工具的 fallback 文本。
func (r *Result) Markdown() string {
	if len(r.Rows) == 0 {
		return "（0 行）"
	}
	var b strings.Builder
	b.WriteString("| " + strings.Join(r.Columns, " | ") + " |\n")
	b.WriteString("|" + strings.Repeat(" --- |", len(r.Columns)) + "\n")
	for _, row := range r.Rows {
		cells := make([]string, len(r.Columns))
		for i := range r.Columns {
			if i < len(row) {
				cells[i] = fmtCell(row[i])
			}
		}
		b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
	}
	if r.Truncated {
		fmt.Fprintf(&b, "\n> 结果已截断至 %d 行，不是全部数据。\n", r.RowCount)
	}
	return b.String()
}

func fmtCell(v any) string {
	if v == nil {
		return "NULL"
	}
	s := fmt.Sprintf("%v", v)
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// normalize 把 RawBytes 转成 JSON 友好的 Go 值。
//
// 全列按 RawBytes 扫描再自行判型，避免 driver 对 Doris 的 tinyint/decimal
// 返回 []byte 时上层拿到 base64 字符串。
func normalize(b sql.RawBytes) any {
	if b == nil {
		return nil
	}
	s := string(b)
	if s == "" {
		return ""
	}
	// 整数
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	// 浮点（排除 "1e5" 这类会被误判的业务字符串：要求含小数点）
	if strings.Contains(s, ".") {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
	}
	return s
}

// wrapQueryError 把驱动错误翻译成对模型有指导意义的错误码 + 中文说明。
func wrapQueryError(err error, timeout time.Duration, query string) error {
	msg := err.Error()
	low := strings.ToLower(msg)

	switch {
	case strings.Contains(low, "context deadline exceeded"), strings.Contains(low, "invalid connection"), strings.Contains(low, "i/o timeout"):
		return fmt.Errorf("timeout: 查询超过 %s 未返回。请收窄时间窗口、增加过滤条件，或降低 limit。原始错误: %s", timeout, msg)
	case strings.Contains(low, "unknown column"):
		return fmt.Errorf("column_not_found: %s。请调用 describe_ocpx_table 核对列名，不要凭猜测写列", msg)
	case strings.Contains(low, "unknown table"), strings.Contains(low, "table") && strings.Contains(low, "doesn't exist"):
		return fmt.Errorf("table_not_found: %s。请调用 list_ocpx_tables 查看可用表", msg)
	case strings.Contains(low, "access denied"), strings.Contains(low, "denied to user"):
		return fmt.Errorf("permission_denied: %s。当前 Doris 账号对该库表无权限", msg)
	case strings.Contains(low, "syntax error"), strings.Contains(low, "parse error"):
		return fmt.Errorf("syntax_error: %s。生成的 SQL 为: %s", msg, query)
	case strings.Contains(low, "memory limit exceeded"), strings.Contains(low, "exceed memory"):
		return fmt.Errorf("result_too_large: Doris 内存超限。请收窄时间窗口或减少 GROUP BY 维度。原始错误: %s", msg)
	case strings.Contains(low, "connection refused"), strings.Contains(low, "no such host"), strings.Contains(low, "dial tcp"):
		return fmt.Errorf("connection_failed: 无法连接 Doris FE，请检查 config.yaml 里的 Doris.Host/Doris.Port（MySQL 协议端口通常是 9030）。原始错误: %s", msg)
	default:
		return fmt.Errorf("query_failed: %s", msg)
	}
}
