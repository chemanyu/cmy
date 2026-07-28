package doris

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"
)

// newTestLogger 构造一个写进 buf 的日志器，等价于 newSQLLogger 但输出可捕获。
// 保持与 newSQLLogger 相同的级别语义，级别逻辑本身由 ParseLogLevel 覆盖。
func newTestLogger(buf *bytes.Buffer, level string) *sqlLogger {
	lv := ParseLogLevel(level)
	if lv == LogSilent {
		return &sqlLogger{level: LogSilent}
	}
	return &sqlLogger{logger: log.New(buf, "[sql] ", log.LstdFlags), level: lv}
}

// 多行 SQL 压成一行 + 参数渲染
func TestFlattenAndArgs(t *testing.T) {
	var buf bytes.Buffer
	l := newTestLogger(&buf, "info")
	l.trace("SELECT a\n  FROM t\n  WHERE d >= ? AND n > ?", []any{"2026-07-01", 42}, 12*time.Millisecond, 7, nil)
	got := buf.String()
	t.Logf("输出: %s", strings.TrimSpace(got))
	if lines := strings.SplitN(strings.TrimSpace(got), "\n", 2); len(lines) > 1 {
		t.Errorf("SQL 没压成一行, 有 %d 行", len(lines))
	}
	for _, want := range []string{"SELECT a FROM t WHERE d >= ? AND n > ?", `args=["2026-07-01", 42]`, "rows=7", "[OK]"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺 %q", want)
		}
	}
}

// silent 什么都不输出
func TestSilent(t *testing.T) {
	var buf bytes.Buffer
	l := newTestLogger(&buf, "silent")
	l.trace("SELECT 1", nil, time.Second, 1, nil)
	l.trace("SELECT 2", nil, time.Second, 1, errors.New("boom"))
	if buf.Len() != 0 {
		t.Errorf("silent 不该有输出: %s", buf.String())
	}
}

// newSQLLogger 在 silent 下不应持有 logger（走 trace 的空判断）
func TestNewSQLLoggerSilent(t *testing.T) {
	l := newSQLLogger("silent")
	if l.logger != nil {
		t.Error("silent 不该构造 logger")
	}
	l.trace("SELECT 1", nil, time.Millisecond, 1, nil) // 不 panic 即通过
}

// error 级别：只记失败
func TestErrorLevel(t *testing.T) {
	var buf bytes.Buffer
	l := newTestLogger(&buf, "error")
	l.trace("SELECT ok", nil, 5*time.Millisecond, 1, nil)
	l.trace("SELECT slow", nil, 10*time.Second, 1, nil)
	l.trace("SELECT bad", nil, 5*time.Millisecond, 0, errors.New("boom"))
	got := buf.String()
	if strings.Contains(got, "SELECT ok") {
		t.Error("error 级别不该记成功语句")
	}
	if strings.Contains(got, "SELECT slow") {
		t.Error("error 级别不该记慢查询")
	}
	if !strings.Contains(got, "SELECT bad") || !strings.Contains(got, "[ERROR]") {
		t.Error("应记失败语句")
	}
}

// warn 级别：记失败 + 慢查询，不记正常
func TestWarnLevel(t *testing.T) {
	var buf bytes.Buffer
	l := newTestLogger(&buf, "warn")
	l.trace("SELECT fast", nil, 5*time.Millisecond, 1, nil)
	l.trace("SELECT slow", nil, 3*time.Second, 1, nil)
	got := buf.String()
	if strings.Contains(got, "SELECT fast") {
		t.Error("warn 不该记正常语句")
	}
	if !strings.Contains(got, "SELECT slow") || !strings.Contains(got, "SLOW") {
		t.Errorf("应记慢查询: %s", got)
	}
}

// info 级别下慢查询也要带 SLOW 标记
func TestInfoMarksSlow(t *testing.T) {
	var buf bytes.Buffer
	l := newTestLogger(&buf, "info")
	l.trace("SELECT slow", nil, 5*time.Second, 3, nil)
	if got := buf.String(); !strings.Contains(got, "SLOW") {
		t.Errorf("info 级别的慢查询应带 SLOW 标记: %s", got)
	}
}

func TestParseLogLevel(t *testing.T) {
	cases := map[string]LogLevel{
		"silent": LogSilent, "error": LogError, "warn": LogWarn, "info": LogInfo,
		"": LogInfo, "GARBAGE": LogInfo, " Info ": LogInfo,
	}
	for in, want := range cases {
		if got := ParseLogLevel(in); got != want {
			t.Errorf("ParseLogLevel(%q)=%v want %v", in, got, want)
		}
	}
}

// 验证 client.Query 真的会调用日志（连不上的本地端口，只测串联）。
// 直接替换 client 的 sqlLog 以捕获输出。
func TestQueryWiresIntoLog(t *testing.T) {
	c, err := New(Options{
		DSN:      "u:p@tcp(127.0.0.1:1)/db?timeout=1s",
		Database: "db", MaxOpenConns: 1, MaxIdleConns: 1,
		ConnMaxLife: time.Minute, Timeout: 3 * time.Second, MaxRows: 10,
		LogLevel: "info",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	var buf bytes.Buffer
	c.sqlLog = newTestLogger(&buf, "info")

	if _, qErr := c.Query(context.Background(), "SELECT count(*) FROM t WHERE d >= ?", "2026-07-01"); qErr == nil {
		t.Fatal("连不上却没报错？")
	}
	got := buf.String()
	t.Logf("日志内容: %s", strings.TrimSpace(got))
	for _, want := range []string{"[ERROR]", "SELECT count(*) FROM t WHERE d >= ?", `args=["2026-07-01"]`, "err="} {
		if !strings.Contains(got, want) {
			t.Errorf("缺 %q", want)
		}
	}
}
