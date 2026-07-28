package doris

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// HTTPClient 走 HTTP 查询网关执行只读 SELECT。
//
// 用于拿不到 Doris 直连凭据、只有一个转发 SELECT 的 HTTP 接口的场景。
// 接口约定（GET）：
//
//	GET <QueryURL>?sql=<SQL>&key=<Key>
//	200 {"code":0,"data":[{"col":"val",...}, ...],"errInfo":[]}
//
// 与直连相比的两处固有差异，已在代码里补偿：
//  1. 网关不支持 ? 占位符，故本客户端先把 args 插值回 SQL 再发；
//  2. 返回是 JSON 对象数组，列顺序丢失，故从 SELECT 子句解析列序还原。
type HTTPClient struct {
	url     string
	key     string
	http    *http.Client
	timeout time.Duration
	maxRows int
	sqlLog  *sqlLogger
}

// HTTPOptions 构造 HTTPClient 所需参数。
type HTTPOptions struct {
	URL      string
	Key      string
	Timeout  time.Duration
	MaxRows  int
	LogLevel string
}

// NewHTTP 构造 HTTP 网关客户端。
func NewHTTP(opts HTTPOptions) (*HTTPClient, error) {
	if opts.URL == "" {
		return nil, fmt.Errorf("HTTP 网关 URL 为空")
	}
	return &HTTPClient{
		url:     opts.URL,
		key:     opts.Key,
		http:    &http.Client{Timeout: opts.Timeout},
		timeout: opts.Timeout,
		maxRows: opts.MaxRows,
		sqlLog:  newSQLLogger(opts.LogLevel),
	}, nil
}

// gatewayResp 是网关的响应体。
//
// Data 用 json.RawMessage（而非 []json.RawMessage）延迟解析：网关成功时
// data 是对象数组，失败时却是空字符串 ""，若直接声明成数组，遇到错误响应
// 会整体反序列化失败，把真正的 errInfo 报错吞掉。所以先看 code，再按需解析 data。
type gatewayResp struct {
	Code    int             `json:"code"`
	Data    json.RawMessage `json:"data"`
	ErrInfo json.RawMessage `json:"errInfo"`
}

// Query 把 SQL 发给网关并解析结果。
func (c *HTTPClient) Query(ctx context.Context, query string, args ...any) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// 网关不支持占位符，先把参数插值成完整 SQL。
	full, err := interpolate(query, args)
	if err != nil {
		return nil, fmt.Errorf("query_failed: 参数插值失败: %v", err)
	}

	start := time.Now()
	var (
		logRows int
		logErr  error
	)
	defer func() {
		// 日志记插值后的完整 SQL，便于直接复制到网关复现。
		c.sqlLog.trace(full, nil, time.Since(start), logRows, logErr)
	}()

	res, err := c.do(ctx, full)
	if err != nil {
		logErr = err
		return nil, err
	}
	logRows = res.RowCount
	return res, nil
}

// do 发起一次请求并把网关响应转成 *Result。
func (c *HTTPClient) do(ctx context.Context, sql string) (*Result, error) {
	u, err := url.Parse(c.url)
	if err != nil {
		return nil, fmt.Errorf("connection_failed: 网关 URL 非法: %v", err)
	}
	q := u.Query()
	q.Set("sql", sql)
	if c.key != "" {
		q.Set("key", c.key)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("query_failed: 构造请求失败: %v", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, wrapHTTPError(err, c.timeout)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("query_failed: 读取网关响应失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("query_failed: 网关返回 HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}

	var gr gatewayResp
	if err := json.Unmarshal(body, &gr); err != nil {
		return nil, fmt.Errorf("query_failed: 网关响应不是预期 JSON: %s", truncate(string(body), 300))
	}
	// 先判 code：非 0 时 data 可能是 ""（不是数组），直接读 errInfo 报错，不碰 data。
	if gr.Code != 0 {
		return nil, wrapQueryError(fmt.Errorf("%s", gatewayErrText(gr.ErrInfo)), c.timeout, sql)
	}

	// code==0 才把 data 解析成对象数组。空 data 也按 0 行处理。
	var rows []json.RawMessage
	if len(gr.Data) > 0 && string(gr.Data) != `""` && string(gr.Data) != "null" {
		if err := json.Unmarshal(gr.Data, &rows); err != nil {
			return nil, fmt.Errorf("query_failed: 网关 data 不是对象数组: %s", truncate(string(gr.Data), 300))
		}
	}
	return c.decodeRows(rows, sql)
}

// decodeRows 把 [{列:值}] 转成有序的 Columns + Rows。
//
// 列顺序优先按 SQL 的 SELECT 子句还原；解析不出（如 SELECT *）时，
// 退回按首行 key 排序，保证多次查询列序稳定、可预测。
func (c *HTTPClient) decodeRows(data []json.RawMessage, sql string) (*Result, error) {
	res := &Result{Columns: []string{}, Rows: [][]any{}, SQL: sql}
	if len(data) == 0 {
		return res, nil
	}

	// 逐行解析成 map，同时记录首行出现的 key 顺序。
	rowMaps := make([]map[string]any, 0, len(data))
	var firstKeys []string
	for _, raw := range data {
		m, keys, err := decodeObject(raw)
		if err != nil {
			return nil, fmt.Errorf("query_failed: 解析结果行失败: %v", err)
		}
		if firstKeys == nil {
			firstKeys = keys
		}
		rowMaps = append(rowMaps, m)
	}

	res.Columns = resolveColumns(sql, firstKeys)

	for _, m := range rowMaps {
		if len(res.Rows) >= c.maxRows {
			res.Truncated = true
			break
		}
		row := make([]any, len(res.Columns))
		for i, col := range res.Columns {
			if v, ok := m[col]; ok {
				row[i] = normalizeJSON(v)
			}
		}
		res.Rows = append(res.Rows, row)
	}
	res.RowCount = len(res.Rows)
	return res, nil
}

// QueryOneRow 执行查询并返回第一行的 map 形式，没有行时返回 nil。
func (c *HTTPClient) QueryOneRow(ctx context.Context, query string, args ...any) (map[string]any, error) {
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

// Ping 发一条最小查询探活。网关没有独立健康检查端点，用 SELECT 1 代替。
func (c *HTTPClient) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := c.do(ctx, "SELECT 1")
	return err
}

// Close HTTP 后端无长连接需要释放。
func (c *HTTPClient) Close() error { return nil }

// -----------------------------------------------------------------------------
// 辅助
// -----------------------------------------------------------------------------

// decodeObject 解析一行 JSON 对象，返回 map 与 key 的出现顺序。
// 用 json.Decoder 逐 token 读，才能拿到原始 key 顺序（map 会丢顺序）。
func decodeObject(raw json.RawMessage) (map[string]any, []string, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, fmt.Errorf("期望 JSON 对象")
	}
	m := map[string]any{}
	var keys []string
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, _ := keyTok.(string)
		var val any
		if err := dec.Decode(&val); err != nil {
			return nil, nil, err
		}
		if _, dup := m[key]; !dup {
			keys = append(keys, key)
		}
		m[key] = val
	}
	return m, keys, nil
}

// selectColsRe 抓取 SELECT 与 FROM 之间的列列表。忽略大小写、允许换行。
var selectColsRe = regexp.MustCompile(`(?is)^\s*select\s+(.*?)\s+from\s`)

// resolveColumns 尽力还原列顺序：先按 SELECT 子句里的 AS 别名/列名，
// 失败或对不齐时退回按首行 key（已排序）。
func resolveColumns(sql string, fallbackKeys []string) []string {
	sorted := append([]string(nil), fallbackKeys...)
	sortStrings(sorted)

	m := selectColsRe.FindStringSubmatch(sql)
	if m == nil {
		return sorted
	}
	parsed := parseSelectList(m[1])
	// 解析出的列名必须与网关返回的 key 集合一一对应，否则不可信，退回排序。
	if len(parsed) != len(fallbackKeys) {
		return sorted
	}
	keySet := make(map[string]bool, len(fallbackKeys))
	for _, k := range fallbackKeys {
		keySet[k] = true
	}
	for _, p := range parsed {
		if !keySet[p] {
			return sorted
		}
	}
	return parsed
}

// parseSelectList 把 SELECT 列表拆成列名/别名，只在顶层逗号处分割
// （不切括号内的逗号，如 count(*)、coalesce(a,b)）。
func parseSelectList(list string) []string {
	var cols []string
	depth := 0
	start := 0
	for i, r := range list {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				cols = append(cols, colName(list[start:i]))
				start = i + 1
			}
		}
	}
	cols = append(cols, colName(list[start:]))
	for _, c := range cols {
		if c == "" || c == "*" {
			return nil // 带 * 或空段，放弃解析
		}
	}
	return cols
}

// colName 从单个 select 项里取最终列名：有 AS 别名用别名，
// 否则用表达式原文（去掉表限定前缀与反引号），与网关 JSON key 对齐。
func colName(expr string) string {
	e := strings.TrimSpace(expr)
	// 处理 " ... AS alias" 或 " ... alias"（末尾标识符作别名）。
	if idx := lastAsIndex(e); idx >= 0 {
		return unquoteIdent(strings.TrimSpace(e[idx+4:]))
	}
	return unquoteIdent(e)
}

// lastAsIndex 找顶层的 " as "（忽略大小写），括号内的不算。
func lastAsIndex(s string) int {
	lower := strings.ToLower(s)
	depth := 0
	for i := 0; i+4 <= len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && lower[i:i+4] == " as " {
			return i
		}
	}
	return -1
}

// unquoteIdent 去掉反引号与表限定前缀（a.b -> b）。
func unquoteIdent(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`")
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	return strings.Trim(s, "`")
}

// gatewayErrText 把 errInfo（可能是字符串、数组或对象）转成可读文本。
func gatewayErrText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "网关返回 code!=0"
	}
	// 字符串："..."
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return s
	}
	// 对象：{"sql":"表不在白名单内：..."} —— 拼接所有值。
	var obj map[string]any
	if json.Unmarshal(raw, &obj) == nil && len(obj) > 0 {
		parts := make([]string, 0, len(obj))
		for _, v := range obj {
			parts = append(parts, fmt.Sprintf("%v", v))
		}
		return strings.Join(parts, "; ")
	}
	// 数组：["...", "..."]
	var arr []any
	if json.Unmarshal(raw, &arr) == nil && len(arr) > 0 {
		parts := make([]string, 0, len(arr))
		for _, v := range arr {
			parts = append(parts, fmt.Sprintf("%v", v))
		}
		return strings.Join(parts, "; ")
	}
	return truncate(string(raw), 300)
}

// wrapHTTPError 把 HTTP 传输层错误翻译成与直连一致的错误码前缀。
func wrapHTTPError(err error, timeout time.Duration) error {
	msg := err.Error()
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "context deadline exceeded"), strings.Contains(low, "timeout"), strings.Contains(low, "deadline"):
		return fmt.Errorf("timeout: 网关查询超过 %s 未返回。请收窄时间窗口、增加过滤条件，或降低 limit。原始错误: %s", timeout, msg)
	case strings.Contains(low, "no such host"), strings.Contains(low, "connection refused"), strings.Contains(low, "dial tcp"), strings.Contains(low, "eof"):
		return fmt.Errorf("connection_failed: 无法连接查询网关，请检查 config.yaml 里的 Doris.QueryURL。原始错误: %s", msg)
	default:
		return fmt.Errorf("query_failed: %s", msg)
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
