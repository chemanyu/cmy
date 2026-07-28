// SQL 日志：把每条实际执行的语句、参数、耗时、返回行数打到标准输出，
// 由 systemd 收进 output.log，便于排查模型生成的 SQL。
package doris

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// LogLevel SQL 日志级别。
type LogLevel int

const (
	// LogSilent 不记录。
	LogSilent LogLevel = iota
	// LogError 只记录执行失败的语句。
	LogError
	// LogWarn 记录失败与慢查询。
	LogWarn
	// LogInfo 记录全部语句。
	LogInfo
)

// slowThreshold 超过该耗时算慢查询。
//
// 取 2s 而不是 alsc 的 200ms：那边是 OLTP 点查，这边是 OLAP 聚合，
// 几百毫秒是常态，200ms 会把每条查询都标成慢查询。
const slowThreshold = 2 * time.Second

// ParseLogLevel 解析配置里的级别字符串，无法识别时返回 LogInfo。
func ParseLogLevel(level string) LogLevel {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "silent":
		return LogSilent
	case "error":
		return LogError
	case "warn":
		return LogWarn
	case "info":
		return LogInfo
	default:
		return LogInfo
	}
}

// sqlLogger 把 SQL 写到标准输出；logger 为 nil 表示不记录。
type sqlLogger struct {
	logger *log.Logger
	level  LogLevel
}

// newSQLLogger 构造 SQL 日志器。
//
// 固定写标准输出，由 systemd 单元的 StandardOutput=append: 落到
// /data/log/go/adt-go/ocpx_mcp/ocpx_mcp.output.log，和应用日志同一个文件——
// 不自己管文件句柄，也就没有目录不存在、权限不足、句柄泄漏这些问题。
func newSQLLogger(logLevel string) *sqlLogger {
	level := ParseLogLevel(logLevel)
	if level == LogSilent {
		return &sqlLogger{level: LogSilent}
	}
	return &sqlLogger{logger: log.New(os.Stdout, "[sql] ", log.LstdFlags), level: level}
}

// trace 记录一条查询。err 非 nil 表示执行失败，rows 为返回行数。
func (l *sqlLogger) trace(query string, args []any, elapsed time.Duration, rows int, err error) {
	if l == nil || l.logger == nil || l.level == LogSilent {
		return
	}

	slow := elapsed >= slowThreshold
	switch {
	case err != nil: // LogError 及以上都记
	case slow && l.level < LogWarn:
		return
	case !slow && l.level < LogInfo:
		return
	}

	tag := "OK"
	if slow {
		tag = fmt.Sprintf("SLOW>=%s", slowThreshold)
	}
	if err != nil {
		tag = "ERROR"
	}

	// SQL 压成一行，否则多行语句会把日志切得难以 grep。
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s | rows=%d | %s", tag, elapsed.Round(time.Millisecond), rows, flatten(query))
	if len(args) > 0 {
		fmt.Fprintf(&b, " | args=%s", formatArgs(args))
	}
	if err != nil {
		fmt.Fprintf(&b, " | err=%v", err)
	}
	l.logger.Print(b.String())
}

// flatten 把多行 SQL 压成单行并合并连续空白。
func flatten(q string) string {
	return strings.Join(strings.Fields(q), " ")
}

// formatArgs 渲染占位符参数，字符串加引号以便区分类型。
func formatArgs(args []any) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		switch v := a.(type) {
		case string:
			parts = append(parts, strconv.Quote(v))
		case time.Time:
			parts = append(parts, strconv.Quote(v.Format("2006-01-02 15:04:05")))
		default:
			parts = append(parts, fmt.Sprintf("%v", v))
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
