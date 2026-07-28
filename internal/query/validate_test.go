package query

import (
	"strings"
	"testing"
	"time"

	"github.com/chemanyu/mcp/internal/schema"
)

var allowed = schema.TableNames()

func TestValidateSelectAccepts(t *testing.T) {
	cases := []string{
		"SELECT count(*) FROM ocpx_v1_imp WHERE req_time >= '2026-07-01' AND req_time < '2026-07-02'",
		"select `product_channel`, count(*) from `ocpx`.`ocpx_v1_clk` where req_time > '2026-07-01' group by 1",
		// CTE 引用自身定义的名字
		"WITH a AS (SELECT unikey FROM ocpx_v1_imp WHERE req_time >= '2026-07-01') SELECT count(*) FROM a WHERE req_time IS NOT NULL",
		// 字符串字面量里含关键字，不应误杀
		"SELECT count(*) FROM ocpx_v1_track WHERE req_time >= '2026-07-01' AND err = 'drop table foo'",
		// 列名含被禁关键字的子串，词边界应放行
		"SELECT count(*) FROM ocpx_jd_imp WHERE req_time >= '2026-07-01' AND up_event_name = 'x'",
	}
	for _, sqlText := range cases {
		if _, err := ValidateSelect(sqlText, allowed, 100); err != nil {
			t.Errorf("应通过但被拒绝:\n  SQL: %s\n  err: %v", sqlText, err)
		}
	}
}

func TestValidateSelectRejects(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want string // 期望错误里包含的片段
	}{
		{"空语句", "   ", "不能为空"},
		{"非 SELECT 开头", "DROP TABLE ocpx_v1_imp", "SELECT"},
		{"多语句", "SELECT 1 FROM ocpx_v1_imp WHERE req_time > '2026-01-01'; DROP TABLE x", "多条语句"},
		{"注释藏分号", "SELECT 1 FROM ocpx_v1_imp WHERE req_time > '2026-01-01' /* x */ ; DELETE FROM y", "多条语句"},
		{"行注释藏写操作", "SELECT 1 FROM ocpx_v1_imp WHERE req_time > '2026-01-01'\n-- \nUPDATE ocpx_v1_imp SET uid=1", "UPDATE"},
		{"子查询里写操作", "SELECT * FROM (SELECT 1) x WHERE req_time > '2026-01-01' AND 1 IN (INSERT INTO a VALUES(1))", "INSERT"},
		{"未授权表", "SELECT * FROM other_db.secret_table WHERE req_time > '2026-01-01'", "不允许访问表"},
		{"无表引用", "SELECT 1 WHERE req_time > '2026-01-01'", "没有解析到任何表引用"},
		{"缺时间过滤", "SELECT count(*) FROM ocpx_v1_imp WHERE uid = 1", "missing_time_filter"},
		{"缺 WHERE", "SELECT count(*) FROM ocpx_v1_imp GROUP BY req_time", "missing_time_filter"},
		{"导出文件", "SELECT * FROM ocpx_v1_imp WHERE req_time > '2026-01-01' INTO OUTFILE '/tmp/x'", "OUTFILE"},
		{"会话变更", "SELECT @@version, 1 FROM ocpx_v1_imp WHERE req_time > '2026-01-01' AND (SET x=1)", "SET"},
		{"未闭合引号", "SELECT * FROM ocpx_v1_imp WHERE req_time > '2026-01-01", "闭合"},
		{"未闭合块注释", "SELECT * FROM ocpx_v1_imp WHERE req_time > '2026-01-01' /* x", "闭合"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ValidateSelect(c.sql, allowed, 100)
			if err == nil {
				t.Fatalf("应被拒绝但通过了: %s", c.sql)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误信息应包含 %q，实际: %v", c.want, err)
			}
		})
	}
}

func TestValidateSelectAppendsLimit(t *testing.T) {
	base := "SELECT count(*) FROM ocpx_v1_imp WHERE req_time >= '2026-07-01'"
	out, err := ValidateSelect(base, allowed, 123)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "LIMIT 123") {
		t.Errorf("应追加 LIMIT 123，实际: %s", out)
	}

	// 已有 LIMIT 时不应重复追加
	withLimit := base + " LIMIT 5"
	out, err = ValidateSelect(withLimit, allowed, 123)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(strings.ToUpper(out), "LIMIT") != 1 {
		t.Errorf("不应重复追加 LIMIT，实际: %s", out)
	}
}

func TestParseWindow(t *testing.T) {
	// 绝对区间
	w, err := ParseWindow("2026-07-01", "2026-07-03", 24, 31)
	if err != nil {
		t.Fatal(err)
	}
	if w.Days() != 2 {
		t.Errorf("跨度应为 2 天，实际 %d", w.Days())
	}
	if got := w.Start.Format("2006-01-02 15:04:05"); got != "2026-07-01 00:00:00" {
		t.Errorf("日期应补齐为零点，实际 %s", got)
	}

	// 相对区间：end 留空取 now
	w, err = ParseWindow("-6h", "", 24, 31)
	if err != nil {
		t.Fatal(err)
	}
	if d := w.End.Sub(w.Start); d < 5*time.Hour+50*time.Minute || d > 6*time.Hour+10*time.Minute {
		t.Errorf("-6h 的跨度应约为 6 小时，实际 %v", d)
	}

	// 两者都留空时使用 defaultHours
	w, err = ParseWindow("", "", 3, 31)
	if err != nil {
		t.Fatal(err)
	}
	if d := w.End.Sub(w.Start); d < 2*time.Hour+50*time.Minute || d > 3*time.Hour+10*time.Minute {
		t.Errorf("默认窗口应为 3 小时，实际 %v", d)
	}
}

func TestParseWindowErrors(t *testing.T) {
	if _, err := ParseWindow("2026-07-03", "2026-07-01", 24, 31); err == nil {
		t.Error("end 早于 start 应报错")
	}
	if _, err := ParseWindow("2026-01-01", "2026-07-01", 24, 31); err == nil {
		t.Error("超过 maxDays 应报错")
	} else if !strings.Contains(err.Error(), "window_too_large") {
		t.Errorf("应为 window_too_large，实际 %v", err)
	}
	if _, err := ParseWindow("not-a-date", "", 24, 31); err == nil {
		t.Error("非法时间格式应报错")
	}
}

func TestCheckBucketCount(t *testing.T) {
	w, _ := ParseWindow("2026-01-01", "2026-01-31", 24, 0)
	if err := CheckBucketCount(w, GranMinute); err == nil {
		t.Error("30 天 × minute 粒度应超过分桶上限")
	}
	if err := CheckBucketCount(w, GranHour); err != nil {
		t.Errorf("30 天 × hour 粒度应通过，实际 %v", err)
	}
	if err := CheckBucketCount(w, GranNone); err != nil {
		t.Errorf("none 粒度应直接通过，实际 %v", err)
	}
}
