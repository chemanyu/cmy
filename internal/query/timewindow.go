package query

import (
	"fmt"
	"strings"
	"time"
)

const tsLayout = "2006-01-02 15:04:05"

// CST 是业务统一时区（Doris 侧 datetime 按东八区解释）。
var CST = time.FixedZone("CST", 8*3600)

// TimeWindow 是一个左闭右开的时间区间。
type TimeWindow struct {
	Start time.Time
	End   time.Time
}

// String 返回可读区间。
func (w TimeWindow) String() string {
	return fmt.Sprintf("%s ~ %s", w.Start.Format(tsLayout), w.End.Format(tsLayout))
}

// Days 返回区间跨度天数（向上取整）。
func (w TimeWindow) Days() int {
	d := w.End.Sub(w.Start).Hours() / 24
	if d != float64(int(d)) {
		return int(d) + 1
	}
	return int(d)
}

// ParseWindow 解析时间窗口参数。
//
// start/end 支持三种写法：
//   - "2026-07-28 10:00:00"
//   - "2026-07-28"（当日 00:00:00）
//   - 相对写法 "-2d" / "-6h" / "now"
//
// 两者都为空时，回退到 defaultHours 小时前至现在。end 为空时取 now。
// maxDays 为窗口跨度上限，超过则报错——这是防全表扫的最后一道闸。
func ParseWindow(start, end string, defaultHours, maxDays int) (TimeWindow, error) {
	now := time.Now().In(CST)

	var w TimeWindow
	var err error

	if strings.TrimSpace(end) == "" {
		w.End = now
	} else {
		w.End, err = parseTimePoint(end, now)
		if err != nil {
			return w, fmt.Errorf("invalid_time: end=%q 解析失败：%w", end, err)
		}
	}

	if strings.TrimSpace(start) == "" {
		w.Start = w.End.Add(-time.Duration(defaultHours) * time.Hour)
	} else {
		w.Start, err = parseTimePoint(start, now)
		if err != nil {
			return w, fmt.Errorf("invalid_time: start=%q 解析失败：%w", start, err)
		}
	}

	if !w.End.After(w.Start) {
		return w, fmt.Errorf("invalid_time: end (%s) 必须晚于 start (%s)",
			w.End.Format(tsLayout), w.Start.Format(tsLayout))
	}
	if days := w.Days(); maxDays > 0 && days > maxDays {
		return w, fmt.Errorf("window_too_large: 时间窗口跨度 %d 天，超过上限 %d 天。请收窄 start/end——OCPX 明细表按 req_time 排序，窗口越大扫描代价越高",
			days, maxDays)
	}
	return w, nil
}

// parseTimePoint 解析单个时间点。
func parseTimePoint(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return now, nil
	}
	if strings.EqualFold(s, "now") {
		return now, nil
	}
	// 相对时间："-2d" "-90m" "-6h" "+1d"
	if s[0] == '-' || s[0] == '+' {
		d, err := parseRelative(s)
		if err != nil {
			return time.Time{}, err
		}
		return now.Add(d), nil
	}
	// 绝对时间，按精度从长到短尝试
	layouts := []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04",
		"2006-01-02 15",
		"2006-01-02",
	}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, s, CST); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("支持格式：\"2006-01-02 15:04:05\" / \"2006-01-02\" / 相对时间 \"-2d\" \"-6h\" / \"now\"")
}

// parseRelative 解析 "-2d" 这类相对偏移，额外支持 d（天）。
func parseRelative(s string) (time.Duration, error) {
	sign := time.Duration(1)
	body := s
	switch s[0] {
	case '-':
		sign, body = -1, s[1:]
	case '+':
		body = s[1:]
	}
	if strings.HasSuffix(body, "d") {
		var days float64
		if _, err := fmt.Sscanf(strings.TrimSuffix(body, "d"), "%g", &days); err != nil {
			return 0, fmt.Errorf("无法解析天数偏移 %q", s)
		}
		return sign * time.Duration(days*24) * time.Hour, nil
	}
	d, err := time.ParseDuration(body)
	if err != nil {
		return 0, fmt.Errorf("无法解析时间偏移 %q，支持 d/h/m/s 后缀", s)
	}
	return sign * d, nil
}

// -----------------------------------------------------------------------------
// 时间分桶
// -----------------------------------------------------------------------------

// Granularity 是时间聚合粒度。
type Granularity string

const (
	GranMinute Granularity = "minute"
	GranHour   Granularity = "hour"
	GranDay    Granularity = "day"
	GranNone   Granularity = "none"
)

// ParseGranularity 校验粒度取值。
func ParseGranularity(s string) (Granularity, error) {
	switch Granularity(strings.ToLower(strings.TrimSpace(s))) {
	case "", GranNone:
		return GranNone, nil
	case GranMinute:
		return GranMinute, nil
	case GranHour:
		return GranHour, nil
	case GranDay:
		return GranDay, nil
	default:
		return "", fmt.Errorf("invalid_granularity: 不支持粒度 %q，可用：minute / hour / day / none", s)
	}
}

// BucketExpr 返回该粒度下的时间分桶表达式。timeCol 来自表定义，不是外部输入。
func BucketExpr(g Granularity, timeCol string) string {
	c := quoteIdent(timeCol)
	switch g {
	case GranMinute:
		return fmt.Sprintf("DATE_FORMAT(%s, '%%Y-%%m-%%d %%H:%%i:00')", c)
	case GranHour:
		return fmt.Sprintf("DATE_FORMAT(%s, '%%Y-%%m-%%d %%H:00:00')", c)
	case GranDay:
		return fmt.Sprintf("DATE_FORMAT(%s, '%%Y-%%m-%%d')", c)
	}
	return ""
}

// MaxBuckets 是分桶数量上限，避免 minute 粒度 + 长窗口炸出几万行。
const MaxBuckets = 5000

// CheckBucketCount 校验窗口与粒度的组合不会产生过多分桶。
func CheckBucketCount(w TimeWindow, g Granularity) error {
	if g == GranNone {
		return nil
	}
	span := w.End.Sub(w.Start)
	var n float64
	switch g {
	case GranMinute:
		n = span.Minutes()
	case GranHour:
		n = span.Hours()
	case GranDay:
		n = span.Hours() / 24
	}
	if int(n) > MaxBuckets {
		return fmt.Errorf("too_many_buckets: 窗口 %s 按 %s 粒度会产生约 %d 个时间桶，超过上限 %d。请换更粗的粒度或收窄窗口",
			w.String(), g, int(n), MaxBuckets)
	}
	return nil
}
