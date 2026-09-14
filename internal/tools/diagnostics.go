package tools

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/chemanyu/mcp/internal/query"
	"github.com/chemanyu/mcp/internal/schema"
)

// -----------------------------------------------------------------------------
// ocpx_device_lookup
// -----------------------------------------------------------------------------

const deviceLookupDesc = `
按设备标识反查该设备在【曝光/点击/转化】三张表里的全部记录，用于单点排查：
"这个 IMEI 到底有没有转化""这个用户的点击为什么没回传""这台设备走了哪条链路"。

输入:
  - id_type (string, 必填): imei / imei_sum / idfa / idfa_sum / oaid / oaid_sum / caid / user_id / ip / unikey / req_id / cid
  - id_value (string, 必填): 标识值。带 _sum 后缀的是 MD5，不带的是明文，注意别混
  - line (string, 可选): v1 / jd。留空则两条链路都查
  - start / end: 时间窗口，留空为最近 7 天
  - limit: 每张表返回的明细上限，默认 20

返回: {id_type, id_value, window, stages:[{table, stage, matched, rows:[...]}], total_matched}

【链路串联提示】
  - unikey 是曝光→点击→转化的共用归因键，用 id_type=unikey 能拿到最干净的单次链路
  - 只有设备号时，先用设备号查出 unikey，再用 unikey 反查，可避免同设备多次投放混在一起
  - 记录里 is_loss=1 或 err 非空说明该环节出了问题，接着用 ocpx_loss_analysis 看整体是否普遍

【本工具不剔除测试流量与丢失记录】——排查场景恰恰需要看到这些异常记录。`

// 允许反查的标识列（必须是 device_id 类或高基数维度）。
var lookupIDTypes = []string{
	"imei", "imei_sum", "idfa", "idfa_sum", "oaid", "oaid_sum", "caid",
	"user_id", "ip", "unikey", "req_id", "cid",
}

func registerDeviceLookup(s *server.MCPServer, d *Deps) {
	opts := []mcp.ToolOption{
		mcp.WithString("id_type", mcp.Required(),
			mcp.Description("标识类型。带 _sum 的是 MD5 值，不带的是明文"),
			mcp.Enum(lookupIDTypes...)),
		mcp.WithString("id_value", mcp.Required(), mcp.Description("标识值")),
		mcp.WithString("line", mcp.Description("限定业务线 v1 / jd，留空则两条链路都查"), mcp.Enum("v1", "jd")),
		mcp.WithNumber("limit", mcp.Description("每张表的明细行数上限，默认 20"), mcp.DefaultNumber(20), mcp.Min(1)),
	}
	opts = append(opts, withTimeWindow("最近 7 天")...)

	s.AddTool(readOnlyTool("ocpx_device_lookup", deviceLookupDesc, opts...), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		idType, err := req.RequireString("id_type")
		if err != nil {
			return errf("missing_argument: %v", err), nil
		}
		idType = strings.ToLower(strings.TrimSpace(idType))
		if !contains(lookupIDTypes, idType) {
			return errf("invalid_id_type: 不支持按 %q 反查。可用：%s", idType, strings.Join(lookupIDTypes, ", ")), nil
		}
		idValue, err := req.RequireString("id_value")
		if err != nil {
			return errf("missing_argument: %v", err), nil
		}
		if strings.TrimSpace(idValue) == "" {
			return errf("missing_argument: id_value 不能为空"), nil
		}
		win, err := windowFromRequest(req, d, 24*7)
		if err != nil {
			return errf("%v", err), nil
		}
		limit := limitFromRequest(req, d, 20)

		// 确定要查的表集合。
		var targets []*schema.Table
		if ls := strings.TrimSpace(req.GetString("line", "")); ls != "" {
			line, err := schema.ParseLine(ls)
			if err != nil {
				return errf("%v", err), nil
			}
			for _, st := range []schema.Stage{schema.StageImp, schema.StageClk, schema.StageConv} {
				t, err := schema.StageTable(line, st)
				if err != nil {
					return errf("%v", err), nil
				}
				targets = append(targets, t)
			}
		} else {
			targets = schema.All()
		}

		type stageResult struct {
			Table   string           `json:"table"`
			Line    string           `json:"line"`
			Stage   string           `json:"stage"`
			Matched int              `json:"matched"`
			Rows    []map[string]any `json:"rows"`
			Note    string           `json:"note,omitempty"`
		}
		var results []stageResult
		total := 0

		for _, t := range targets {
			if _, ok := t.Column(idType); !ok {
				results = append(results, stageResult{
					Table: t.Name, Line: string(t.Line), Stage: string(t.Stage),
					Matched: 0, Rows: []map[string]any{},
					Note: fmt.Sprintf("该表没有 %s 列，已跳过", idType),
				})
				continue
			}

			b := query.New(d.Cfg.TableQualifier(), t)
			for _, c := range lookupSelectColumns(t) {
				if _, err := b.SelectColumn(c); err != nil {
					return errf("internal_error: %v", err), nil
				}
			}
			b.TimeRange(win.Start, win.End)
			if err := b.ApplyFilters([]query.Filter{{Column: idType, Operator: "=", Value: idValue}}); err != nil {
				return errf("%v", err), nil
			}
			b.OrderBy("`"+t.TimeColumn+"`", false).Limit(limit)

			sqlText, args, err := b.Build()
			if err != nil {
				return errf("internal_error: %v", err), nil
			}
			res, err := d.DB.Query(ctx, sqlText, args...)
			if err != nil {
				return errf("查询 %s 失败: %v", t.Name, err), nil
			}
			total += res.RowCount
			results = append(results, stageResult{
				Table: t.Name, Line: string(t.Line), Stage: string(t.Stage),
				Matched: res.RowCount, Rows: res.RowMaps(),
			})
		}

		payload := map[string]any{
			"id_type":       idType,
			"id_value":      idValue,
			"window":        win.String(),
			"stages":        results,
			"total_matched": total,
		}

		var b strings.Builder
		fmt.Fprintf(&b, "%s = %s，窗口 %s\n\n", idType, idValue, win.String())
		if total == 0 {
			b.WriteString("三张表都没有命中记录。可能原因：时间窗口太窄、标识类型选错（明文 vs MD5）、或该设备确实无数据。\n")
		}
		for _, r := range results {
			fmt.Fprintf(&b, "- %s（%s/%s）: %d 行", r.Table, r.Line, schema.StageLabel(schema.Stage(r.Stage)), r.Matched)
			if r.Note != "" {
				fmt.Fprintf(&b, " — %s", r.Note)
			}
			b.WriteString("\n")
		}
		return resultJSON(payload, b.String()), nil
	})
}

// lookupSelectColumns 决定反查时返回哪些列：跳过大字段，保留归因与诊断关键列。
func lookupSelectColumns(t *schema.Table) []string {
	var out []string
	for _, c := range t.Columns {
		if c.Kind == schema.KindRaw {
			continue // ua / caidjson / callback_param 太大，明细里不返回
		}
		out = append(out, c.Name)
	}
	return out
}

// -----------------------------------------------------------------------------
// ocpx_loss_analysis
// -----------------------------------------------------------------------------

const lossAnalysisDesc = `
统计丢失（is_loss=1）与报错（err 非空）的规模与原因分布，用于回答"为什么量对不上"
"丢了多少""报的什么错""是哪个服务实例在报错"。

输入:
  - table (string, 必填)
  - start / end: 时间窗口，留空为最近 24 小时
  - group_by (string[], 可选): 按维度下钻丢失情况，最多 2 个（如 product_channel / servername）
  - top_errors (number, 可选): 返回的错误原因 Top N，默认 20；设 0 则跳过错误明细
  - filters (可选)

返回:
  {
    table, window,
    overall: {total, loss, error_rows, loss_rate, error_rate},
    by_dimension: [{维度..., total, loss, error_rows, loss_rate}],
    top_errors: [{err, cnt}]
  }

  - loss_rate = loss/total，error_rate = error_rows/total
  - top_errors 里的 err 是原始错误文本，已按出现次数降序

【读法】
  - loss_rate 高但 error_rate 低 → 大概率是主动丢弃（限频/黑名单/预算），不是程序错误
  - error_rate 高且集中在少数 servername → 单机故障，查那台机器
  - 错误文本分散无规律 → 上游数据质量问题，看 top_errors 的具体内容`

func registerLossAnalysis(s *server.MCPServer, d *Deps) {
	opts := []mcp.ToolOption{
		mcp.WithString("table", mcp.Required(), mcp.Description("目标表名"), mcp.Enum(schema.TableNames()...)),
		mcp.WithArray("group_by", mcp.Description("下钻维度，最多 2 个。排查单机问题常用 servername"),
			mcp.WithStringItems(), mcp.MaxItems(2)),
		mcp.WithNumber("top_errors", mcp.Description("错误原因 Top N，默认 20，设 0 跳过"), mcp.DefaultNumber(20), mcp.Min(0)),
		filtersToolOption(),
	}
	opts = append(opts, withTimeWindow("最近 24 小时")...)

	s.AddTool(readOnlyTool("ocpx_loss_analysis", lossAnalysisDesc, opts...), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tableName, err := req.RequireString("table")
		if err != nil {
			return errf("missing_argument: %v", err), nil
		}
		t, err := schema.Get(tableName)
		if err != nil {
			return errf("%v", err), nil
		}
		if _, ok := t.Column("is_loss"); !ok {
			return errf("unsupported_table: 表 %s 没有 is_loss 列，无法做丢失分析", t.Name), nil
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
		if len(groupBy) > 2 {
			return errf("too_many_dimensions: loss 分析的 group_by 最多 2 个，当前 %d 个", len(groupBy)), nil
		}
		topErrors := req.GetInt("top_errors", 20)

		hasErr := false
		if _, ok := t.Column("err"); ok {
			hasErr = true
		}

		// 指标表达式：总数、丢失数、报错数。
		lossExprs := func(b *query.Builder) {
			b.SelectExpr("COUNT(*)", "total")
			b.SelectExpr("SUM(CASE WHEN `is_loss` = 1 THEN 1 ELSE 0 END)", "loss")
			if hasErr {
				b.SelectExpr("SUM(CASE WHEN `err` IS NOT NULL AND `err` != '' THEN 1 ELSE 0 END)", "error_rows")
			}
		}

		// 1) 整体
		ob := query.New(d.Cfg.TableQualifier(), t)
		lossExprs(ob)
		ob.TimeRange(win.Start, win.End)
		if err := ob.ApplyFilters(filters); err != nil {
			return errf("%v", err), nil
		}
		overallSQL, overallArgs, err := ob.Build()
		if err != nil {
			return errf("internal_error: %v", err), nil
		}
		overall, err := d.DB.QueryOneRow(ctx, overallSQL, overallArgs...)
		if err != nil {
			return errf("%v", err), nil
		}
		if overall == nil {
			overall = map[string]any{"total": 0, "loss": 0, "error_rows": 0}
		}
		addRates(overall)

		// 2) 按维度下钻
		byDim := []map[string]any{}
		var dimSQL string
		if len(groupBy) > 0 {
			gb := query.New(d.Cfg.TableQualifier(), t)
			for _, g := range groupBy {
				if err := gb.GroupByColumn(g); err != nil {
					return errf("%v", err), nil
				}
			}
			lossExprs(gb)
			gb.TimeRange(win.Start, win.End)
			if err := gb.ApplyFilters(filters); err != nil {
				return errf("%v", err), nil
			}
			gb.OrderByAlias("loss", true).Limit(limitFromRequest(req, d, 50))
			sqlText, args, err := gb.Build()
			if err != nil {
				return errf("internal_error: %v", err), nil
			}
			dimSQL = sqlText
			res, err := d.DB.Query(ctx, sqlText, args...)
			if err != nil {
				return errf("%v", err), nil
			}
			byDim = res.RowMaps()
			for _, r := range byDim {
				addRates(r)
			}
		}

		// 3) 错误原因 Top N
		topErrList := []map[string]any{}
		var errSQL string
		if hasErr && topErrors > 0 {
			eb := query.New(d.Cfg.TableQualifier(), t)
			// err 是 text，不能直接 GROUP BY；截断到前 200 字符做聚合。
			errExpr := "SUBSTR(`err`, 1, 200)"
			eb.GroupByExpr(errExpr, "err")
			eb.SelectExpr("COUNT(*)", "cnt")
			eb.TimeRange(win.Start, win.End)
			eb.Where("`err` IS NOT NULL AND `err` != ''")
			if err := eb.ApplyFilters(filters); err != nil {
				return errf("%v", err), nil
			}
			eb.OrderByAlias("cnt", true).Limit(min(topErrors, d.Cfg.Query.MaxRows))
			sqlText, args, err := eb.Build()
			if err != nil {
				return errf("internal_error: %v", err), nil
			}
			errSQL = sqlText
			res, err := d.DB.Query(ctx, sqlText, args...)
			if err != nil {
				return errf("%v", err), nil
			}
			topErrList = res.RowMaps()
		}

		payload := map[string]any{
			"table":        t.Name,
			"window":       win.String(),
			"overall":      overall,
			"by_dimension": byDim,
			"top_errors":   topErrList,
			"sql": map[string]string{
				"overall":      overallSQL,
				"by_dimension": dimSQL,
				"top_errors":   errSQL,
			},
		}

		var b strings.Builder
		fmt.Fprintf(&b, "表 %s，窗口 %s\n\n## 整体\n\n总行数 %s，丢失 %s（%s），报错 %s（%s）\n",
			t.Name, win.String(),
			fmtVal(overall["total"]), fmtVal(overall["loss"]), fmtVal(overall["loss_rate"]),
			fmtVal(overall["error_rows"]), fmtVal(overall["error_rate"]))
		if len(byDim) > 0 {
			fmt.Fprintf(&b, "\n## 按 %s 下钻（%d 组）\n", strings.Join(groupBy, ", "), len(byDim))
		}
		if len(topErrList) > 0 {
			fmt.Fprintf(&b, "\n## 错误原因 Top %d\n", len(topErrList))
			for _, e := range topErrList {
				fmt.Fprintf(&b, "- [%s] %s\n", fmtVal(e["cnt"]), truncate(fmtVal(e["err"]), 160))
			}
		}
		return resultJSON(payload, b.String()), nil
	})
}

// addRates 依据 total/loss/error_rows 补上比率字段。
func addRates(m map[string]any) {
	total := toFloat(m["total"])
	m["loss_rate"] = ratio(toFloat(m["loss"]), total)
	if _, ok := m["error_rows"]; ok {
		m["error_rate"] = ratio(toFloat(m["error_rows"]), total)
	}
}

// -----------------------------------------------------------------------------
// ocpx_sample_rows
// -----------------------------------------------------------------------------

const sampleRowsDesc = `
取少量明细行看数据长什么样。写复杂过滤条件之前，用它确认某个字段的真实取值格式
（例如 product_channel 到底是什么形态、caid 的 version_md5 长什么样）。

输入:
  - table (string, 必填)
  - start / end: 时间窗口，留空为最近 1 小时
  - columns (string[], 可选): 只返回指定列。留空返回除大字段外的全部列
  - filters (可选)
  - include_raw (bool, 默认 false): 是否包含 ua / caidjson / callback_param 这类大字段
  - limit: 默认 10，上限受服务配置约束

返回: {table, window, columns, rows, row_count, sql}

【不要用本工具做统计】它只取样本、不保证代表性。计数用 ocpx_breakdown，趋势用 ocpx_trend。`

func registerSampleRows(s *server.MCPServer, d *Deps) {
	opts := []mcp.ToolOption{
		mcp.WithString("table", mcp.Required(), mcp.Description("目标表名"), mcp.Enum(schema.TableNames()...)),
		mcp.WithArray("columns", mcp.Description("只返回这些列，留空返回除大字段外全部列"),
			mcp.WithStringItems()),
		filtersToolOption(),
		mcp.WithBoolean("include_raw", mcp.Description("是否包含 ua / caidjson / callback_param 等大字段，默认 false"), mcp.DefaultBool(false)),
		mcp.WithNumber("limit", mcp.Description("返回行数，默认 10"), mcp.DefaultNumber(10), mcp.Min(1)),
	}
	opts = append(opts, withTimeWindow("最近 1 小时")...)

	s.AddTool(readOnlyTool("ocpx_sample_rows", sampleRowsDesc, opts...), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tableName, err := req.RequireString("table")
		if err != nil {
			return errf("missing_argument: %v", err), nil
		}
		t, err := schema.Get(tableName)
		if err != nil {
			return errf("%v", err), nil
		}
		win, err := windowFromRequest(req, d, 1)
		if err != nil {
			return errf("%v", err), nil
		}
		filters, err := filtersFromRequest(req)
		if err != nil {
			return errf("%v", err), nil
		}
		includeRaw := req.GetBool("include_raw", false)
		limit := limitFromRequest(req, d, 10)

		cols := req.GetStringSlice("columns", nil)
		if len(cols) == 0 {
			for _, c := range t.Columns {
				if !includeRaw && c.Kind == schema.KindRaw {
					continue
				}
				cols = append(cols, c.Name)
			}
		}

		b := query.New(d.Cfg.TableQualifier(), t)
		for _, c := range cols {
			if _, err := b.SelectColumn(c); err != nil {
				return errf("%v", err), nil
			}
		}
		b.TimeRange(win.Start, win.End)
		if err := b.ApplyFilters(filters); err != nil {
			return errf("%v", err), nil
		}
		b.OrderBy("`"+t.TimeColumn+"`", true).Limit(limit)

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
			"columns":      res.Columns,
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
// ocpx_run_sql —— 兜底逃生舱
// -----------------------------------------------------------------------------

const runSQLDesc = `统一执行自定义只读 SELECT / WITH，直接返回数据，无需经过其他业务工具。
输入保持不变：sql（必填），limit（可选）。返回 columns、rows、row_count、truncated、execution_ms、sql。
表名/字段不确定时读取 list_ocpx_tables / describe_ocpx_table；describe 同时提供常用场景 SQL 示例。
监测ID=unikey，账户=advertiser_id，上游转化=up_event_name，京东事件4为 up_event_name='4'；低活订单为 type='scheduled_callback'（可无上游事件值）；泛指订单时用两者OR条件。
所有查询显式限制 req_time 起止范围；时间、测试/丢失过滤及转化统计口径由 SQL 决定，本工具不自动补充。
只允许六张 OCPX 表，拒绝写操作和多语句；缺少 LIMIT 时追加，结果受服务行数上限与超时限制。
HTTP 网关使用裸表名。SQL 示例需替换为实际日期和ID，SQL中的字符串值必须转义。
错误恢复：column_not_found 查看字段；sql_rejected 按错误修改；missing_time_filter 补时间范围；timeout 缩小范围。`

func registerRunSQL(s *server.MCPServer, d *Deps) {
	tool := readOnlyTool("ocpx_run_sql", runSQLDesc,
		mcp.WithString("sql", mcp.Required(), mcp.Description("单条 SELECT 语句，必须带 req_time 过滤条件")),
		mcp.WithNumber("limit", mcp.Description("追加的行数上限"), mcp.Min(1)),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		raw, err := req.RequireString("sql")
		if err != nil {
			return errf("missing_argument: %v", err), nil
		}
		limit := limitFromRequest(req, d, d.Cfg.Query.MaxRows)

		safe, err := query.ValidateSelect(raw, schema.TableNames(), limit)
		if err != nil {
			return errf("%v", err), nil
		}
		res, err := d.DB.Query(ctx, safe)
		if err != nil {
			return errf("%v", err), nil
		}

		payload := map[string]any{
			"columns":      res.Columns,
			"rows":         res.RowMaps(),
			"row_count":    res.RowCount,
			"truncated":    res.Truncated,
			"execution_ms": res.ExecutionMS,
			"sql":          safe,
		}
		return resultJSON(payload, res.Markdown()), nil
	})
}

// -----------------------------------------------------------------------------
// 小工具
// -----------------------------------------------------------------------------

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func metricNames(t *schema.Table) string {
	var out []string
	for _, c := range t.ColumnsOfKind(schema.KindMetric) {
		out = append(out, c.Name)
	}
	return strings.Join(out, ", ")
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// toFloat 把结果集里的数值转成 float64，无法转换时返回 0。
func toFloat(v any) float64 {
	switch n := v.(type) {
	case nil:
		return 0
	case int64:
		return float64(n)
	case int:
		return float64(n)
	case float64:
		return n
	case string:
		f, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return 0
		}
		return f
	}
	return 0
}

// ratio 返回 a/b，分母为 0 时返回 nil（而不是 0——避免把"无数据"误读成"转化率 0%"）。
func ratio(a, b float64) any {
	if b == 0 {
		return nil
	}
	// 保留 6 位小数，避免 JSON 里出现长尾浮点噪声。
	r := a / b
	return float64(int64(r*1e6+0.5)) / 1e6
}

func fmtVal(v any) string {
	if v == nil {
		return "-"
	}
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return fmt.Sprintf("%v", v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// indent 把子查询缩进，纯粹为了让返回的 sql 字段可读。
func indent(s string) string {
	return "\n  " + strings.ReplaceAll(s, "\n", "\n  ") + "\n"
}
