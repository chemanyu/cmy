package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/chemanyu/mcp/internal/query"
	"github.com/chemanyu/mcp/internal/schema"
)

// -----------------------------------------------------------------------------
// ocpx_funnel
// -----------------------------------------------------------------------------

const funnelDesc = `
一次调用算出【曝光 → 点击 → 转化】完整漏斗，含 CTR / CVR / 曝光转化率。
问"转化率多少""哪个渠道效果好""量级怎么样"时用这个，不要自己拼 SQL。

输入:
  - line (string, 必填): v1=通用 OCPX 链路, jd=京东链路
  - start / end: 时间窗口，留空为最近 24 小时
  - group_by (string[], 可选): 下钻维度，最多 3 个。留空则返回整体漏斗
  - filters (可选): 附加过滤条件
  - include_test / include_loss (bool, 默认 false): 是否纳入测试流量 / 丢失记录
  - limit: 分组数上限，默认 50

返回: {line, window, group_by, rows:[{维度..., imp, clk, conv, conv_pv, ctr, cvr, imp_cvr}], row_count}
  - imp/clk 是行数；conv 是转化表行数；conv_pv 是 action_pv 求和（真实转化次数，口径上更准）
  - ctr = clk/imp, cvr = conv_pv/clk, imp_cvr = conv_pv/imp；分母为 0 时该比率为 null

【口径默认值】默认剔除 test_status != 0 与 is_loss = 1，这是对外汇报口径。
只有用户明确要"含测试流量"或"含丢失"时才把对应开关打开。

【性能】三张明细表各扫一遍并按维度 FULL JOIN。窗口越大越慢，跨度上限见服务配置。
若报 timeout，收窄窗口或减少 group_by 维度。

错误恢复:
  - column_not_found  → 调 describe_ocpx_table 核对列名
  - window_too_large  → 收窄 start/end
  - invalid_dimension → 该列是大字段，换 groupable_columns 里的列`

func registerFunnel(s *server.MCPServer, d *Deps) {
	opts := []mcp.ToolOption{
		mcp.WithString("line",
			mcp.Required(),
			mcp.Description("业务线：v1=通用 OCPX 链路，jd=京东链路"),
			mcp.Enum("v1", "jd"),
		),
		mcp.WithArray("group_by",
			mcp.Description("下钻维度列名，最多 3 个。留空返回整体漏斗。常用：product_channel / advertiser_id / campaign_id / ad_id / place_id / os"),
			mcp.WithStringItems(),
			mcp.MaxItems(3),
		),
		filtersToolOption(),
		mcp.WithBoolean("include_test", mcp.Description("是否纳入测试流量（test_status != 0），默认 false"), mcp.DefaultBool(false)),
		mcp.WithBoolean("include_loss", mcp.Description("是否纳入丢失记录（is_loss = 1），默认 false"), mcp.DefaultBool(false)),
		mcp.WithNumber("limit", mcp.Description("返回的分组数上限，默认 50"), mcp.DefaultNumber(50), mcp.Min(1)),
	}
	opts = append(opts, withTimeWindow("最近 24 小时")...)

	s.AddTool(readOnlyTool("ocpx_funnel", funnelDesc, opts...), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		lineStr, err := req.RequireString("line")
		if err != nil {
			return errf("missing_argument: %v", err), nil
		}
		line, err := schema.ParseLine(lineStr)
		if err != nil {
			return errf("%v", err), nil
		}
		win, err := windowFromRequest(req, d, 24)
		if err != nil {
			return errf("%v", err), nil
		}
		filters, err := filtersFromRequest(req)
		if err != nil {
			return errf("%v", err), nil
		}
		groupBy := req.GetStringSlice("group_by", nil)
		if len(groupBy) > 3 {
			return errf("too_many_dimensions: group_by 最多 3 个维度，当前 %d 个", len(groupBy)), nil
		}
		includeTest := req.GetBool("include_test", false)
		includeLoss := req.GetBool("include_loss", false)
		limit := limitFromRequest(req, d, 50)

		impT, err := schema.StageTable(line, schema.StageImp)
		if err != nil {
			return errf("%v", err), nil
		}
		clkT, _ := schema.StageTable(line, schema.StageClk)
		convT, _ := schema.StageTable(line, schema.StageConv)

		// 校验维度在三张表里都存在且可分组。
		for _, g := range groupBy {
			for _, t := range []*schema.Table{impT, clkT, convT} {
				b := query.New(d.Cfg.TableQualifier(), t)
				if _, err := b.ResolveGroupColumn(g); err != nil {
					return errf("%v（提示：漏斗维度必须在曝光/点击/转化三张表里都存在，三表共有可分组列：%s）",
						err, strings.Join(schema.CommonGroupable(impT, clkT, convT), ", ")), nil
				}
			}
		}

		// 每个环节各构造一条聚合子查询。
		build := func(t *schema.Table, metricExprs [][2]string) (string, []any, error) {
			b := query.New(d.Cfg.TableQualifier(), t)
			for _, g := range groupBy {
				if err := b.GroupByColumn(g); err != nil {
					return "", nil, err
				}
			}
			for _, m := range metricExprs {
				b.SelectExpr(m[0], m[1])
			}
			b.TimeRange(win.Start, win.End)
			if !includeTest {
				b.ExcludeTestTraffic()
			}
			if !includeLoss {
				b.ExcludeLoss()
			}
			if err := b.ApplyFilters(filters); err != nil {
				return "", nil, err
			}
			return b.Build()
		}

		impSQL, impArgs, err := build(impT, [][2]string{{"COUNT(*)", "imp"}})
		if err != nil {
			return errf("%v", err), nil
		}
		clkSQL, clkArgs, err := build(clkT, [][2]string{{"COUNT(*)", "clk"}})
		if err != nil {
			return errf("%v", err), nil
		}
		convSQL, convArgs, err := build(convT, [][2]string{
			{"COUNT(*)", "conv"},
			{"COALESCE(SUM(`action_pv`), 0)", "conv_pv"},
			{"COALESCE(SUM(`depth_action_pv`), 0)", "deep_conv_pv"},
		})
		if err != nil {
			return errf("%v", err), nil
		}

		sqlText, args := assembleFunnel(groupBy, impSQL, impArgs, clkSQL, clkArgs, convSQL, convArgs, limit)

		res, err := d.DB.Query(ctx, sqlText, args...)
		if err != nil {
			return errf("%v", err), nil
		}

		rows := res.RowMaps()
		for _, r := range rows {
			imp := toFloat(r["imp"])
			clk := toFloat(r["clk"])
			convPV := toFloat(r["conv_pv"])
			r["ctr"] = ratio(clk, imp)
			r["cvr"] = ratio(convPV, clk)
			r["imp_cvr"] = ratio(convPV, imp)
		}

		payload := map[string]any{
			"line":         string(line),
			"window":       win.String(),
			"group_by":     groupBy,
			"tables":       []string{impT.Name, clkT.Name, convT.Name},
			"include_test": includeTest,
			"include_loss": includeLoss,
			"rows":         rows,
			"row_count":    len(rows),
			"truncated":    res.Truncated,
			"execution_ms": res.ExecutionMS,
			"sql":          sqlText,
		}
		return resultJSON(payload, funnelMarkdown(win, groupBy, rows, res.Truncated)), nil
	})
}

// assembleFunnel 把三条环节聚合拼成一条查询。
//
// 无维度时三条子查询各返回一行，直接 CROSS JOIN；
// 有维度时以曝光为基准做 LEFT JOIN——曝光是漏斗入口，没有曝光的点击属于数据异常，
// 用 ocpx_loss_analysis 单独排查更合适。
func assembleFunnel(groupBy []string, impSQL string, impArgs []any, clkSQL string, clkArgs []any, convSQL string, convArgs []any, limit int) (string, []any) {
	args := append(append(append([]any{}, impArgs...), clkArgs...), convArgs...)

	if len(groupBy) == 0 {
		sqlText := fmt.Sprintf(
			"SELECT i.imp, COALESCE(c.clk, 0) AS clk, COALESCE(t.conv, 0) AS conv, "+
				"COALESCE(t.conv_pv, 0) AS conv_pv, COALESCE(t.deep_conv_pv, 0) AS deep_conv_pv\n"+
				"FROM (%s) i\nCROSS JOIN (%s) c\nCROSS JOIN (%s) t",
			indent(impSQL), indent(clkSQL), indent(convSQL))
		return sqlText, args
	}

	var selectCols, joinClk, joinConv []string
	for _, g := range groupBy {
		q := "`" + strings.ReplaceAll(g, "`", "``") + "`"
		selectCols = append(selectCols, "i."+q+" AS "+q)
		joinClk = append(joinClk, fmt.Sprintf("i.%s <=> c.%s", q, q))
		joinConv = append(joinConv, fmt.Sprintf("i.%s <=> t.%s", q, q))
	}
	sqlText := fmt.Sprintf(
		"SELECT %s, i.imp, COALESCE(c.clk, 0) AS clk, COALESCE(t.conv, 0) AS conv, "+
			"COALESCE(t.conv_pv, 0) AS conv_pv, COALESCE(t.deep_conv_pv, 0) AS deep_conv_pv\n"+
			"FROM (%s) i\nLEFT JOIN (%s) c ON %s\nLEFT JOIN (%s) t ON %s\n"+
			"ORDER BY i.imp DESC\nLIMIT %d",
		strings.Join(selectCols, ", "),
		indent(impSQL), indent(clkSQL), strings.Join(joinClk, " AND "),
		indent(convSQL), strings.Join(joinConv, " AND "), limit)
	return sqlText, args
}

func funnelMarkdown(win query.TimeWindow, groupBy []string, rows []map[string]any, truncated bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "窗口: %s\n", win.String())
	if len(groupBy) > 0 {
		fmt.Fprintf(&b, "维度: %s\n", strings.Join(groupBy, ", "))
	}
	b.WriteString("\n")
	cols := append(append([]string{}, groupBy...), "imp", "clk", "conv_pv", "ctr", "cvr", "imp_cvr")
	b.WriteString("| " + strings.Join(cols, " | ") + " |\n|" + strings.Repeat(" --- |", len(cols)) + "\n")
	for _, r := range rows {
		cells := make([]string, 0, len(cols))
		for _, c := range cols {
			cells = append(cells, fmtVal(r[c]))
		}
		b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
	}
	if truncated {
		b.WriteString("\n> 结果已截断，不是全部分组。\n")
	}
	return b.String()
}

// -----------------------------------------------------------------------------
// ocpx_breakdown
// -----------------------------------------------------------------------------

const breakdownDesc = `
对单张 OCPX 表按任意维度聚合，返回 Top N。问"哪个渠道量最大""按操作系统分布
怎样""哪个广告主曝光最多"时用这个。

输入:
  - table (string, 必填): 目标表，来自 list_ocpx_tables
  - group_by (string[], 必填): 聚合维度，最多 4 个
  - start / end: 时间窗口，留空为最近 24 小时
  - metrics (string[], 可选): 额外聚合的数值列（对该列求 SUM）。转化表常用 action_pv / depth_action_pv /
    cb_action_pv / cb_depth_action_pv。留空只返回行数
  - filters / include_test / include_loss / limit: 同 ocpx_funnel，limit 默认 50
  - order_by (string, 可选): 排序字段，可填 "rows" 或 metrics 里的列名，默认按 rows 降序

返回: {table, window, group_by, rows:[{维度..., rows, sum_xxx...}], row_count, sql}
  - rows 是该分组的记录行数
  - sum_xxx 是对应 metric 列的求和

【只想看单表分布用本工具；要跨环节算转化率用 ocpx_funnel。】

错误恢复:
  - invalid_dimension → 该列是 text/超长 varchar，不能分组；换 describe_ocpx_table
    返回的 groupable_columns
  - invalid_metric    → 该列不是数值列，换 metric_columns 里的列`

func registerBreakdown(s *server.MCPServer, d *Deps) {
	opts := []mcp.ToolOption{
		mcp.WithString("table", mcp.Required(),
			mcp.Description("目标表名"), mcp.Enum(schema.TableNames()...)),
		mcp.WithArray("group_by", mcp.Required(),
			mcp.Description("聚合维度列名，1~4 个"),
			mcp.WithStringItems(), mcp.MinItems(1), mcp.MaxItems(4)),
		mcp.WithArray("metrics",
			mcp.Description("额外求和的数值列，如 action_pv。留空只统计行数"),
			mcp.WithStringItems(), mcp.MaxItems(6)),
		mcp.WithString("order_by", mcp.Description("排序字段：\"rows\" 或 metrics 中的列名，默认 rows")),
		filtersToolOption(),
		mcp.WithBoolean("include_test", mcp.Description("是否纳入测试流量，默认 false"), mcp.DefaultBool(false)),
		mcp.WithBoolean("include_loss", mcp.Description("是否纳入丢失记录，默认 false"), mcp.DefaultBool(false)),
		mcp.WithNumber("limit", mcp.Description("返回分组数上限，默认 50"), mcp.DefaultNumber(50), mcp.Min(1)),
	}
	opts = append(opts, withTimeWindow("最近 24 小时")...)

	s.AddTool(readOnlyTool("ocpx_breakdown", breakdownDesc, opts...), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tableName, err := req.RequireString("table")
		if err != nil {
			return errf("missing_argument: %v", err), nil
		}
		t, err := schema.Get(tableName)
		if err != nil {
			return errf("%v", err), nil
		}
		groupBy := req.GetStringSlice("group_by", nil)
		if len(groupBy) == 0 {
			return errf("missing_argument: group_by 至少需要 1 个维度。该表可分组列：%s",
				strings.Join(t.GroupableNames(), ", ")), nil
		}
		if len(groupBy) > 4 {
			return errf("too_many_dimensions: group_by 最多 4 个，当前 %d 个", len(groupBy)), nil
		}
		win, err := windowFromRequest(req, d, 24)
		if err != nil {
			return errf("%v", err), nil
		}
		filters, err := filtersFromRequest(req)
		if err != nil {
			return errf("%v", err), nil
		}
		metrics := req.GetStringSlice("metrics", nil)
		limit := limitFromRequest(req, d, 50)

		b := query.New(d.Cfg.TableQualifier(), t)
		for _, g := range groupBy {
			if err := b.GroupByColumn(g); err != nil {
				return errf("%v", err), nil
			}
		}
		b.SelectExpr("COUNT(*)", "rows")

		metricAliases := map[string]bool{"rows": true}
		for _, m := range metrics {
			col, err := b.ResolveColumn(m)
			if err != nil {
				return errf("%v", err), nil
			}
			if col.Kind != schema.KindMetric {
				return errf("invalid_metric: 列 %q 的分类是 %s，不是数值指标列。该表可求和的列：%s",
					col.Name, col.Kind, metricNames(t)), nil
			}
			alias := "sum_" + col.Name
			b.SelectExpr(fmt.Sprintf("COALESCE(SUM(`%s`), 0)", col.Name), alias)
			metricAliases[alias] = true
		}

		b.TimeRange(win.Start, win.End)
		if !req.GetBool("include_test", false) {
			b.ExcludeTestTraffic()
		}
		if !req.GetBool("include_loss", false) {
			b.ExcludeLoss()
		}
		if err := b.ApplyFilters(filters); err != nil {
			return errf("%v", err), nil
		}

		orderBy := strings.TrimSpace(req.GetString("order_by", "rows"))
		if orderBy == "" {
			orderBy = "rows"
		}
		if orderBy != "rows" && !strings.HasPrefix(orderBy, "sum_") {
			orderBy = "sum_" + orderBy
		}
		if !metricAliases[orderBy] {
			return errf("invalid_order_by: order_by=%q 不在可排序字段内。可用：%s",
				req.GetString("order_by", ""), strings.Join(sortedKeys(metricAliases), " / ")), nil
		}
		b.OrderByAlias(orderBy, true).Limit(limit)

		sqlText, args, err := b.Build()
		if err != nil {
			return errf("internal_error: %v", err), nil
		}
		res, err := d.DB.Query(ctx, sqlText, args...)
		if err != nil {
			return errf("%v", err), nil
		}

		payload := map[string]any{
			"table":        t.Name,
			"window":       win.String(),
			"group_by":     groupBy,
			"metrics":      metrics,
			"order_by":     orderBy,
			"rows":         res.RowMaps(),
			"row_count":    res.RowCount,
			"truncated":    res.Truncated,
			"execution_ms": res.ExecutionMS,
			"sql":          sqlText,
		}
		return resultJSON(payload, fmt.Sprintf("表 %s，窗口 %s\n\n%s", t.Name, win.String(), res.Markdown())), nil
	})
}

// -----------------------------------------------------------------------------
// ocpx_trend
// -----------------------------------------------------------------------------

const trendDesc = `
按时间粒度输出趋势序列，用于看波动、找异常时间点、对比同比环比。问"最近几天
趋势""几点开始掉量""昨天什么时候有毛刺"时用这个。

输入:
  - table (string, 必填)
  - granularity (string, 必填): minute / hour / day
  - start / end: 时间窗口，留空为最近 7 天
  - metrics (string[], 可选): 额外求和的数值列
  - filters / include_test / include_loss: 同 ocpx_breakdown
  - group_by (string[], 可选): 最多 1 个维度，用于多序列对比（如按 product_channel 分线）

返回: {table, granularity, window, rows:[{bucket, [维度], rows, sum_xxx...}], row_count, sql}
  rows 按时间正序排列，可直接用于画折线。

【分桶上限】时间桶数量超过 5000 会直接报 too_many_buckets——minute 粒度只适合
几小时的窗口，看多天请用 hour，看多周请用 day。

【空桶不补齐】某个时间桶没有数据时不会出现在结果里，判断"掉零"要看桶是否缺失。`

func registerTrend(s *server.MCPServer, d *Deps) {
	opts := []mcp.ToolOption{
		mcp.WithString("table", mcp.Required(), mcp.Description("目标表名"), mcp.Enum(schema.TableNames()...)),
		mcp.WithString("granularity", mcp.Required(),
			mcp.Description("时间粒度：minute（只适合几小时窗口）/ hour / day"),
			mcp.Enum("minute", "hour", "day")),
		mcp.WithArray("metrics", mcp.Description("额外求和的数值列，如 action_pv"),
			mcp.WithStringItems(), mcp.MaxItems(6)),
		mcp.WithArray("group_by", mcp.Description("可选的对比维度，最多 1 个（多序列对比用）"),
			mcp.WithStringItems(), mcp.MaxItems(1)),
		filtersToolOption(),
		mcp.WithBoolean("include_test", mcp.Description("是否纳入测试流量，默认 false"), mcp.DefaultBool(false)),
		mcp.WithBoolean("include_loss", mcp.Description("是否纳入丢失记录，默认 false"), mcp.DefaultBool(false)),
	}
	opts = append(opts, withTimeWindow("最近 7 天")...)

	s.AddTool(readOnlyTool("ocpx_trend", trendDesc, opts...), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tableName, err := req.RequireString("table")
		if err != nil {
			return errf("missing_argument: %v", err), nil
		}
		t, err := schema.Get(tableName)
		if err != nil {
			return errf("%v", err), nil
		}
		granStr, err := req.RequireString("granularity")
		if err != nil {
			return errf("missing_argument: %v", err), nil
		}
		gran, err := query.ParseGranularity(granStr)
		if err != nil {
			return errf("%v", err), nil
		}
		if gran == query.GranNone {
			return errf("invalid_granularity: granularity 必须是 minute / hour / day 之一"), nil
		}
		win, err := windowFromRequest(req, d, 24*7)
		if err != nil {
			return errf("%v", err), nil
		}
		if err := query.CheckBucketCount(win, gran); err != nil {
			return errf("%v", err), nil
		}
		filters, err := filtersFromRequest(req)
		if err != nil {
			return errf("%v", err), nil
		}
		groupBy := req.GetStringSlice("group_by", nil)
		if len(groupBy) > 1 {
			return errf("too_many_dimensions: ocpx_trend 的 group_by 最多 1 个维度，当前 %d 个。要多维下钻请用 ocpx_breakdown", len(groupBy)), nil
		}

		b := query.New(d.Cfg.TableQualifier(), t)
		bucket := query.BucketExpr(gran, t.TimeColumn)
		b.GroupByExpr(bucket, "bucket")
		for _, g := range groupBy {
			if err := b.GroupByColumn(g); err != nil {
				return errf("%v", err), nil
			}
		}
		b.SelectExpr("COUNT(*)", "rows")
		for _, m := range req.GetStringSlice("metrics", nil) {
			col, err := b.ResolveColumn(m)
			if err != nil {
				return errf("%v", err), nil
			}
			if col.Kind != schema.KindMetric {
				return errf("invalid_metric: 列 %q 不是数值指标列。该表可求和的列：%s", col.Name, metricNames(t)), nil
			}
			b.SelectExpr(fmt.Sprintf("COALESCE(SUM(`%s`), 0)", col.Name), "sum_"+col.Name)
		}

		b.TimeRange(win.Start, win.End)
		if !req.GetBool("include_test", false) {
			b.ExcludeTestTraffic()
		}
		if !req.GetBool("include_loss", false) {
			b.ExcludeLoss()
		}
		if err := b.ApplyFilters(filters); err != nil {
			return errf("%v", err), nil
		}
		b.OrderByAlias("bucket", false)
		if len(groupBy) == 1 {
			b.OrderByAlias(groupBy[0], false)
		}
		b.Limit(d.Cfg.Query.MaxRows)

		sqlText, args, err := b.Build()
		if err != nil {
			return errf("internal_error: %v", err), nil
		}
		res, err := d.DB.Query(ctx, sqlText, args...)
		if err != nil {
			return errf("%v", err), nil
		}

		payload := map[string]any{
			"table":        t.Name,
			"granularity":  string(gran),
			"window":       win.String(),
			"group_by":     groupBy,
			"rows":         res.RowMaps(),
			"row_count":    res.RowCount,
			"truncated":    res.Truncated,
			"execution_ms": res.ExecutionMS,
			"sql":          sqlText,
		}
		fallback := fmt.Sprintf("表 %s，粒度 %s，窗口 %s\n\n%s", t.Name, gran, win.String(), res.Markdown())
		return resultJSON(payload, fallback), nil
	})
}
