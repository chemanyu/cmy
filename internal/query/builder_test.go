package query

import (
	"strings"
	"testing"
	"time"

	"github.com/chemanyu/mcp/internal/schema"
)

func impTable(t *testing.T) *schema.Table {
	t.Helper()
	tbl, err := schema.Get("ocpx_v1_imp")
	if err != nil {
		t.Fatal(err)
	}
	return tbl
}

func TestBuilderBasics(t *testing.T) {
	b := New("ocpx", impTable(t))
	if err := b.GroupByColumn("product_channel"); err != nil {
		t.Fatal(err)
	}
	b.SelectExpr("COUNT(*)", "rows")
	b.TimeRange(time.Date(2026, 7, 1, 0, 0, 0, 0, CST), time.Date(2026, 7, 2, 0, 0, 0, 0, CST))
	b.ExcludeTestTraffic().ExcludeLoss()
	b.OrderByAlias("rows", true).Limit(10)

	sqlText, args, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"SELECT `product_channel`, COUNT(*) AS `rows`",
		"FROM `ocpx`.`ocpx_v1_imp`",
		"`req_time` >= ? AND `req_time` < ?",
		"GROUP BY `product_channel`",
		"ORDER BY `rows` DESC",
		"LIMIT 10",
	} {
		if !strings.Contains(sqlText, want) {
			t.Errorf("SQL 应包含 %q，实际:\n%s", want, sqlText)
		}
	}
	if len(args) != 2 {
		t.Errorf("时间范围应产生 2 个占位参数，实际 %d 个: %v", len(args), args)
	}
}

// 未知列必须被拒绝——这是防注入的核心机制。
func TestBuilderRejectsUnknownColumn(t *testing.T) {
	b := New("ocpx", impTable(t))
	if err := b.GroupByColumn("uid; DROP TABLE x"); err == nil {
		t.Fatal("注入式列名应被拒绝")
	} else if !strings.Contains(err.Error(), "column_not_found") {
		t.Errorf("应为 column_not_found，实际 %v", err)
	}
	if _, err := b.SelectColumn("nonexistent_col"); err == nil {
		t.Error("不存在的列应被拒绝")
	}
}

// 大字段不能作为聚合维度。
func TestBuilderRejectsRawDimension(t *testing.T) {
	b := New("ocpx", impTable(t))
	err := b.GroupByColumn("ua")
	if err == nil {
		t.Fatal("text 大字段 ua 不应允许 GROUP BY")
	}
	if !strings.Contains(err.Error(), "invalid_dimension") {
		t.Errorf("应为 invalid_dimension，实际 %v", err)
	}
}

// 无 WHERE 的查询必须被拒绝，避免全表扫。
func TestBuilderRejectsMissingWhere(t *testing.T) {
	b := New("ocpx", impTable(t))
	b.SelectExpr("COUNT(*)", "rows")
	if _, _, err := b.Build(); err == nil {
		t.Error("没有 WHERE 条件的查询应被拒绝")
	}
}

func TestApplyFilters(t *testing.T) {
	cases := []struct {
		name      string
		filter    Filter
		wantSQL   string
		wantArgs  int
		wantError bool
	}{
		{"等值", Filter{Column: "uid", Operator: "=", Value: 1}, "`uid` = ?", 1, false},
		{"默认等值", Filter{Column: "uid", Value: 1}, "`uid` = ?", 1, false},
		{"别名 gte", Filter{Column: "uid", Operator: "gte", Value: 1}, "`uid` >= ?", 1, false},
		{"IN", Filter{Column: "os", Operator: "in", Values: []any{"android", "ios"}}, "`os` IN (?, ?)", 2, false},
		{"IN 逗号串", Filter{Column: "os", Operator: "in", Value: "android,ios"}, "`os` IN (?, ?)", 2, false},
		{"IS NULL 无参数", Filter{Column: "err", Operator: "is_null"}, "`err` IS NULL", 0, false},
		{"LIKE 自动包裹", Filter{Column: "product_channel", Operator: "like", Value: "abc"}, "`product_channel` LIKE ?", 1, false},
		{"未知列", Filter{Column: "evil", Value: 1}, "", 0, true},
		{"未知操作符", Filter{Column: "uid", Operator: "REGEXP", Value: "x"}, "", 0, true},
		{"IN 空取值", Filter{Column: "os", Operator: "in"}, "", 0, true},
		{"等值缺 value", Filter{Column: "uid", Operator: "="}, "", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := New("ocpx", impTable(t))
			b.SelectExpr("COUNT(*)", "rows")
			b.TimeRange(time.Now().Add(-time.Hour), time.Now())
			err := b.ApplyFilters([]Filter{c.filter})
			if c.wantError {
				if err == nil {
					t.Fatalf("应报错但通过了: %+v", c.filter)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			sqlText, args, err := b.Build()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(sqlText, c.wantSQL) {
				t.Errorf("SQL 应包含 %q，实际:\n%s", c.wantSQL, sqlText)
			}
			// TimeRange 已占 2 个参数
			if got := len(args) - 2; got != c.wantArgs {
				t.Errorf("过滤参数数量应为 %d，实际 %d: %v", c.wantArgs, got, args)
			}
		})
	}
}

// LIKE 已含通配符时不应再包一层。
func TestApplyFiltersLikeKeepsWildcard(t *testing.T) {
	b := New("ocpx", impTable(t))
	b.SelectExpr("COUNT(*)", "rows")
	b.TimeRange(time.Now().Add(-time.Hour), time.Now())
	if err := b.ApplyFilters([]Filter{{Column: "product_channel", Operator: "like", Value: "abc%"}}); err != nil {
		t.Fatal(err)
	}
	_, args, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if got := args[len(args)-1]; got != "abc%" {
		t.Errorf("已含通配符的值不应被改写，实际 %v", got)
	}
}

func TestParseFilters(t *testing.T) {
	raw := []any{
		map[string]any{"column": "uid", "operator": "=", "value": float64(1)},
		map[string]any{"column": "os", "operator": "in", "values": []any{"android"}},
	}
	fs, err := ParseFilters(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 2 {
		t.Fatalf("应解析出 2 条过滤条件，实际 %d", len(fs))
	}
	if fs[0].Column != "uid" || fs[1].Operator != "in" {
		t.Errorf("解析结果不符: %+v", fs)
	}

	if _, err := ParseFilters("not-an-array"); err == nil {
		t.Error("非数组应报错")
	}
	if _, err := ParseFilters([]any{map[string]any{"operator": "="}}); err == nil {
		t.Error("缺少 column 应报错")
	}
}

func TestBucketExpr(t *testing.T) {
	if got := BucketExpr(GranHour, "req_time"); !strings.Contains(got, "%H:00:00") {
		t.Errorf("hour 粒度表达式不符: %s", got)
	}
	if got := BucketExpr(GranNone, "req_time"); got != "" {
		t.Errorf("none 粒度应返回空串，实际 %s", got)
	}
}

func TestSchemaCatalogIntegrity(t *testing.T) {
	names := schema.TableNames()
	if len(names) != 6 {
		t.Fatalf("应有 6 张表，实际 %d: %v", len(names), names)
	}
	for _, n := range names {
		tbl, err := schema.Get(n)
		if err != nil {
			t.Fatal(err)
		}
		if tbl.TimeColumn != "req_time" {
			t.Errorf("%s 的时间列应为 req_time，实际 %s", n, tbl.TimeColumn)
		}
		if _, ok := tbl.Column("req_time"); !ok {
			t.Errorf("%s 缺少 req_time 列", n)
		}
		if len(tbl.GroupableNames()) == 0 {
			t.Errorf("%s 没有可分组列", n)
		}
	}

	// 漏斗三环节都能取到表
	for _, line := range []schema.Line{schema.LineV1, schema.LineJD} {
		for _, st := range []schema.Stage{schema.StageImp, schema.StageClk, schema.StageConv} {
			if _, err := schema.StageTable(line, st); err != nil {
				t.Errorf("line=%s stage=%s 取表失败: %v", line, st, err)
			}
		}
	}

	// 列名查找应忽略大小写与空白
	tbl, _ := schema.Get("ocpx_v1_imp")
	if _, ok := tbl.Column("  Product_Channel "); !ok {
		t.Error("列名查找应忽略大小写与首尾空白")
	}
}

// 漏斗维度必须在三张表里都存在，CommonGroupable 是这个校验的依据。
func TestCommonGroupable(t *testing.T) {
	imp, _ := schema.Get("ocpx_v1_imp")
	clk, _ := schema.Get("ocpx_v1_clk")
	conv, _ := schema.Get("ocpx_v1_track")
	common := schema.CommonGroupable(imp, clk, conv)
	if len(common) == 0 {
		t.Fatal("三张表应有共同的可分组列")
	}
	for _, want := range []string{"product_channel", "advertiser_id", "campaign_id"} {
		found := false
		for _, c := range common {
			if c == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s 应属于三表共有维度，实际共有列: %v", want, common)
		}
	}
	// ua 只在曝光表里有，不应出现在共有列表
	for _, c := range common {
		if c == "ua" {
			t.Error("ua 是大字段且只在曝光表，不应出现在共有可分组列里")
		}
	}
}
