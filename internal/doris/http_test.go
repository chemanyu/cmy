package doris

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mockGateway 起一个模拟网关，回声实际收到的 sql，并按预置响应返回。
func mockGateway(t *testing.T, handler func(sql, key string) (int, any)) (*httptest.Server, *[]string) {
	t.Helper()
	var gotSQLs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sql := r.URL.Query().Get("sql")
		key := r.URL.Query().Get("key")
		gotSQLs = append(gotSQLs, sql)
		code, data := handler(sql, key)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(data)
	}))
	t.Cleanup(srv.Close)
	return srv, &gotSQLs
}

func newTestHTTP(t *testing.T, u, key string) *HTTPClient {
	t.Helper()
	c, err := NewHTTP(HTTPOptions{URL: u, Key: key, Timeout: 5 * time.Second, MaxRows: 100, LogLevel: "silent"})
	if err != nil {
		t.Fatalf("NewHTTP: %v", err)
	}
	return c
}

// 用用户给的真实响应格式验证解析
func TestHTTPParsesRealFormat(t *testing.T) {
	srv, _ := mockGateway(t, func(sql, key string) (int, any) {
		return 200, map[string]any{
			"code": 0,
			"data": []map[string]any{
				{"up_event_name": "224", "count(*)": "63"},
				{"up_event_name": "0", "count(*)": "1010"},
				{"up_event_name": "", "count(*)": "8"},
			},
			"errInfo": []any{},
		}
	})
	c := newTestHTTP(t, srv.URL, "k")

	res, err := c.Query(context.Background(), "select up_event_name, count(*) from ocpx_v1_track where req_time >= '2026-07-16' group by up_event_name")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.RowCount != 3 {
		t.Errorf("RowCount=%d want 3", res.RowCount)
	}
	// 列序应按 SELECT 子句：up_event_name 在前，count(*) 在后
	if len(res.Columns) != 2 || res.Columns[0] != "up_event_name" || res.Columns[1] != "count(*)" {
		t.Errorf("列序错: %v", res.Columns)
	}
	// "63" 应被 normalize 成 int64(63)
	if got := res.Rows[0][1]; got != int64(63) {
		t.Errorf("count 值 = %#v，期望 int64(63)", got)
	}
	if got := res.Rows[0][0]; got != int64(224) {
		t.Errorf("up_event_name '224' normalize 后 = %#v", got)
	}
}

// key 与 sql 确实发给了网关
func TestHTTPSendsKeyAndSQL(t *testing.T) {
	srv, gotSQLs := mockGateway(t, func(sql, key string) (int, any) {
		if key != "secret" {
			t.Errorf("key 未透传: %q", key)
		}
		return 200, map[string]any{"code": 0, "data": []any{}, "errInfo": []any{}}
	})
	c := newTestHTTP(t, srv.URL, "secret")
	if _, err := c.Query(context.Background(), "SELECT 1 FROM t WHERE a = 1"); err != nil {
		t.Fatal(err)
	}
	if len(*gotSQLs) != 1 || (*gotSQLs)[0] != "SELECT 1 FROM t WHERE a = 1" {
		t.Errorf("网关收到的 SQL: %v", *gotSQLs)
	}
}

// 占位符插值：args 拼进 SQL 后再发（网关不支持 ?）
func TestHTTPInterpolatesArgs(t *testing.T) {
	srv, gotSQLs := mockGateway(t, func(sql, key string) (int, any) {
		return 200, map[string]any{"code": 0, "data": []any{}, "errInfo": []any{}}
	})
	c := newTestHTTP(t, srv.URL, "")
	ts := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	_, err := c.Query(context.Background(),
		"SELECT x FROM t WHERE req_time >= ? AND unikey = ? AND n > ?",
		ts, "7439474138", 5)
	if err != nil {
		t.Fatal(err)
	}
	want := "SELECT x FROM t WHERE req_time >= '2026-07-16 00:00:00' AND unikey = '7439474138' AND n > 5"
	if (*gotSQLs)[0] != want {
		t.Errorf("插值结果:\n  got  %s\n  want %s", (*gotSQLs)[0], want)
	}
}

// 注入防护：字符串里的单引号被转义
func TestHTTPEscapesQuote(t *testing.T) {
	srv, gotSQLs := mockGateway(t, func(sql, key string) (int, any) {
		return 200, map[string]any{"code": 0, "data": []any{}, "errInfo": []any{}}
	})
	c := newTestHTTP(t, srv.URL, "")
	if _, err := c.Query(context.Background(), "SELECT x FROM t WHERE a = ?", "o'brien'; DROP"); err != nil {
		t.Fatal(err)
	}
	want := "SELECT x FROM t WHERE a = 'o''brien''; DROP'"
	if (*gotSQLs)[0] != want {
		t.Errorf("转义结果: %s", (*gotSQLs)[0])
	}
}

// code != 0 应翻译成错误码
func TestHTTPGatewayError(t *testing.T) {
	srv, _ := mockGateway(t, func(sql, key string) (int, any) {
		return 200, map[string]any{"code": 1, "data": []any{}, "errInfo": "Unknown column 'xx' in 'field list'"}
	})
	c := newTestHTTP(t, srv.URL, "")
	_, err := c.Query(context.Background(), "SELECT xx FROM t WHERE a = 1")
	if err == nil {
		t.Fatal("code!=0 应报错")
	}
	if !strings.Contains(err.Error(), "column_not_found") {
		t.Errorf("错误码翻译失败: %v", err)
	}
}

// 复现线上 bug：失败响应里 data 是空字符串 ""（不是数组），errInfo 是对象。
// 修复前会误报"网关响应不是预期 JSON"，把真正的白名单报错吞掉。
func TestHTTPErrorDataIsEmptyString(t *testing.T) {
	srv, _ := mockGateway(t, func(sql, key string) (int, any) {
		return 200, map[string]any{
			"code":    1,
			"data":    "",
			"errInfo": map[string]any{"sql": "表不在白名单内：monitor（仅允许 ocpx_v1_imp / ocpx_v1_clk ...）"},
		}
	})
	c := newTestHTTP(t, srv.URL, "")
	_, err := c.Query(context.Background(), "SELECT x FROM `monitor`.`ocpx_v1_clk` WHERE a = 1")
	if err == nil {
		t.Fatal("code!=0 应报错")
	}
	if strings.Contains(err.Error(), "不是预期 JSON") {
		t.Errorf("不该误报 JSON 解析失败: %v", err)
	}
	if !strings.Contains(err.Error(), "白名单") {
		t.Errorf("应带出网关的真实报错: %v", err)
	}
}

// data 为 null 时按 0 行处理，不报错
func TestHTTPNullData(t *testing.T) {
	srv, _ := mockGateway(t, func(sql, key string) (int, any) {
		return 200, map[string]any{"code": 0, "data": nil, "errInfo": []any{}}
	})
	c := newTestHTTP(t, srv.URL, "")
	res, err := c.Query(context.Background(), "SELECT x FROM t WHERE a = 1")
	if err != nil {
		t.Fatalf("data=null 不该报错: %v", err)
	}
	if res.RowCount != 0 {
		t.Errorf("应为 0 行: %d", res.RowCount)
	}
}

// HTTP 500 应报错
func TestHTTPStatusError(t *testing.T) {
	srv, _ := mockGateway(t, func(sql, key string) (int, any) {
		return 500, map[string]any{"msg": "boom"}
	})
	c := newTestHTTP(t, srv.URL, "")
	if _, err := c.Query(context.Background(), "SELECT 1 FROM t WHERE a=1"); err == nil {
		t.Fatal("HTTP 500 应报错")
	}
}

// SELECT * 解析不出列名时，退回按 key 排序，且稳定
func TestHTTPFallbackColumnOrder(t *testing.T) {
	srv, _ := mockGateway(t, func(sql, key string) (int, any) {
		return 200, map[string]any{
			"code":    0,
			"data":    []map[string]any{{"zebra": "1", "apple": "2", "mango": "3"}},
			"errInfo": []any{},
		}
	})
	c := newTestHTTP(t, srv.URL, "")
	res, err := c.Query(context.Background(), "select * from t where a = 1")
	if err != nil {
		t.Fatal(err)
	}
	// 排序后应是 apple, mango, zebra
	want := []string{"apple", "mango", "zebra"}
	for i, w := range want {
		if res.Columns[i] != w {
			t.Errorf("列序 %v，期望 %v", res.Columns, want)
			break
		}
	}
}

// Ping 发 SELECT 1
func TestHTTPPing(t *testing.T) {
	srv, gotSQLs := mockGateway(t, func(sql, key string) (int, any) {
		return 200, map[string]any{"code": 0, "data": []map[string]any{{"1": "1"}}, "errInfo": []any{}}
	})
	c := newTestHTTP(t, srv.URL, "")
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if len(*gotSQLs) != 1 || (*gotSQLs)[0] != "SELECT 1" {
		t.Errorf("Ping 发的 SQL: %v", *gotSQLs)
	}
}

// MaxRows 截断
func TestHTTPTruncate(t *testing.T) {
	srv, _ := mockGateway(t, func(sql, key string) (int, any) {
		rows := make([]map[string]any, 5)
		for i := range rows {
			rows[i] = map[string]any{"n": "1"}
		}
		return 200, map[string]any{"code": 0, "data": rows, "errInfo": []any{}}
	})
	c, _ := NewHTTP(HTTPOptions{URL: srv.URL, Timeout: 5 * time.Second, MaxRows: 2, LogLevel: "silent"})
	res, err := c.Query(context.Background(), "select n from t where a = 1")
	if err != nil {
		t.Fatal(err)
	}
	if res.RowCount != 2 || !res.Truncated {
		t.Errorf("截断失败: RowCount=%d Truncated=%v", res.RowCount, res.Truncated)
	}
}

// URL 非法时查询应报错（防御性）
func TestHTTPBadURL(t *testing.T) {
	c := newTestHTTP(t, "http://127.0.0.1:1", "")
	if _, err := c.Query(context.Background(), "SELECT 1 FROM t WHERE a=1"); err == nil {
		t.Fatal("连不上应报错")
	}
}
