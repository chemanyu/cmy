// Package tools 注册 OCPX MCP 的全部工具。
package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/chemanyu/mcp/internal/config"
	"github.com/chemanyu/mcp/internal/doris"
	"github.com/chemanyu/mcp/internal/query"
	"github.com/chemanyu/mcp/internal/schema"
)

// Deps 是工具处理函数共享的依赖。
type Deps struct {
	DB  doris.Querier
	Cfg *config.Config
}

// Register 把全部工具挂到 MCP server 上。
func Register(s *server.MCPServer, d *Deps) {
	registerListTables(s, d)
	registerDescribeTable(s, d)
	registerFunnel(s, d)
	registerBreakdown(s, d)
	registerTrend(s, d)
	registerDeviceLookup(s, d)
	registerLossAnalysis(s, d)
	registerSampleRows(s, d)
	registerRunSQL(s, d)
}

// readOnlyTool 构造一个只读工具的公共注解。所有工具都不写数据。
func readOnlyTool(name, description string, opts ...mcp.ToolOption) mcp.Tool {
	base := []mcp.ToolOption{
		mcp.WithDescription(description),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
	}
	return mcp.NewTool(name, append(base, opts...)...)
}

// 时间窗口参数是绝大多数工具的公共入参，集中定义保证描述一致。
func withTimeWindow(defaultDesc string) []mcp.ToolOption {
	return []mcp.ToolOption{
		mcp.WithString("start",
			mcp.Description("窗口起点（含）。支持 \"2026-07-28 10:00:00\"、\"2026-07-28\"、相对写法 \"-2d\" / \"-6h\"。留空则为 "+defaultDesc)),
		mcp.WithString("end",
			mcp.Description("窗口终点（不含）。格式同 start，留空为当前时间")),
	}
}

// resultJSON 把业务结果序列化进 structuredContent，同时给出 markdown fallback。
func resultJSON(payload any, fallback string) *mcp.CallToolResult {
	return mcp.NewToolResultStructured(payload, fallback)
}

func errf(format string, a ...any) *mcp.CallToolResult {
	return mcp.NewToolResultError(fmt.Sprintf(format, a...))
}

// -----------------------------------------------------------------------------
// list_ocpx_tables
// -----------------------------------------------------------------------------

const listTablesDesc = `
列出本 MCP 覆盖的全部 OCPX Doris 表，以及每张表在归因漏斗中的位置。

【什么时候调用】用户问任何 OCPX / 广告归因问题时的第一步。你不知道有哪些表、
哪张表属于哪条业务线，先调本工具拿到地图。

返回: {database, tables:[{name, line, stage, title, description, time_column, column_count}]}

两条业务线（不要混用）:
  - line=v1 通用 OCPX 链路: ocpx_v1_imp(曝光) → ocpx_v1_clk(点击) → ocpx_v1_track(转化)
  - line=jd  京东专用链路:   ocpx_jd_imp(曝光) → ocpx_jd_clk(点击) → ocpx_jd_callback(转化)

【会话内幂等】本工具返回的是编译进服务的静态目录，单次会话内不会变化。
调用过一次就复用结果，不要重复调用。

下一步: 用 describe_ocpx_table 看具体表的列，或直接用 ocpx_funnel / ocpx_breakdown
等业务工具——那些工具不需要你手写 SQL。`

func registerListTables(s *server.MCPServer, d *Deps) {
	tool := readOnlyTool("list_ocpx_tables", listTablesDesc)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		type tableInfo struct {
			Name        string `json:"name"`
			Line        string `json:"line"`
			Stage       string `json:"stage"`
			StageLabel  string `json:"stage_label"`
			Title       string `json:"title"`
			Description string `json:"description"`
			TimeColumn  string `json:"time_column"`
			ColumnCount int    `json:"column_count"`
		}
		var infos []tableInfo
		for _, t := range schema.All() {
			infos = append(infos, tableInfo{
				Name:        t.Name,
				Line:        string(t.Line),
				Stage:       string(t.Stage),
				StageLabel:  schema.StageLabel(t.Stage),
				Title:       t.Title,
				Description: t.Desc,
				TimeColumn:  t.TimeColumn,
				ColumnCount: len(t.Columns),
			})
		}
		payload := map[string]any{
			"database": d.Cfg.Doris.Database,
			"tables":   infos,
		}

		var b strings.Builder
		fmt.Fprintf(&b, "库: %s\n\n", d.Cfg.Doris.Database)
		b.WriteString("| 表名 | 业务线 | 环节 | 说明 | 列数 |\n| --- | --- | --- | --- | --- |\n")
		for _, i := range infos {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %d |\n", i.Name, i.Line, i.StageLabel, i.Title, i.ColumnCount)
		}
		return resultJSON(payload, b.String()), nil
	})
}

// -----------------------------------------------------------------------------
// describe_ocpx_table
// -----------------------------------------------------------------------------

const describeTableDesc = `
返回一张 OCPX 表的完整列清单，含类型、业务含义与用途分类。写任何过滤条件
或选择聚合维度之前，都应先读本工具的输出，不要凭列名猜测。

输入: table (string, 必填，来自 list_ocpx_tables)
返回: {table, line, stage, time_column, duplicate_key, columns:[{name,type,kind,description}],
       groupable_columns, device_id_columns, metric_columns}

列的 kind 决定它能用在哪:
  - time       时间列，只有 req_time；所有查询必须按它过滤
  - dimension  可 GROUP BY、可过滤
  - device_id  设备/用户标识，可用 ocpx_device_lookup 反查全链路
  - metric     数值列，可求和/求平均
  - diagnostic 诊断列（is_loss / err），用 ocpx_loss_analysis 分析
  - raw        大字段（text、超长 varchar），只能在明细里看，禁止 GROUP BY

【重要口径提醒】
  - 转化量要对 action_pv 求和，不是 count(*)——一行可能代表多次行为
  - 正式统计应剔除 test_status != 0（测试流量）与 is_loss = 1（丢失记录），
    业务工具默认已按此口径处理
  - DUPLICATE KEY 表意味着可能存在重复行，去重需按业务键（通常是 unikey）

【会话内幂等】同一张表不要重复调用本工具。`

func registerDescribeTable(s *server.MCPServer, d *Deps) {
	tool := readOnlyTool("describe_ocpx_table", describeTableDesc,
		mcp.WithString("table",
			mcp.Required(),
			mcp.Description("表名，取值来自 list_ocpx_tables"),
			mcp.Enum(schema.TableNames()...),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name, err := req.RequireString("table")
		if err != nil {
			return errf("missing_argument: %v", err), nil
		}
		t, err := schema.Get(name)
		if err != nil {
			return errf("%v", err), nil
		}

		type colInfo struct {
			Name        string `json:"name"`
			Type        string `json:"type"`
			Kind        string `json:"kind"`
			Description string `json:"description"`
		}
		var cols []colInfo
		for _, c := range t.Columns {
			cols = append(cols, colInfo{c.Name, c.Type, string(c.Kind), c.Desc})
		}
		var deviceCols, metricCols []string
		for _, c := range t.ColumnsOfKind(schema.KindDeviceID) {
			deviceCols = append(deviceCols, c.Name)
		}
		for _, c := range t.ColumnsOfKind(schema.KindMetric) {
			metricCols = append(metricCols, c.Name)
		}

		payload := map[string]any{
			"table":             t.Name,
			"database":          d.Cfg.Doris.Database,
			"line":              string(t.Line),
			"stage":             string(t.Stage),
			"stage_label":       schema.StageLabel(t.Stage),
			"title":             t.Title,
			"description":       t.Desc,
			"time_column":       t.TimeColumn,
			"duplicate_key":     t.DupKeys,
			"columns":           cols,
			"groupable_columns": t.GroupableNames(),
			"device_id_columns": deviceCols,
			"metric_columns":    metricCols,
		}

		var b strings.Builder
		fmt.Fprintf(&b, "# %s — %s\n\n%s\n\n", t.Name, t.Title, t.Desc)
		fmt.Fprintf(&b, "- 库: `%s`\n- 业务线: %s\n- 漏斗环节: %s\n- 时间列: `%s`（所有查询必须按它过滤）\n- DUPLICATE KEY: %s\n\n",
			d.Cfg.Doris.Database, t.Line, schema.StageLabel(t.Stage), t.TimeColumn, strings.Join(t.DupKeys, ", "))
		b.WriteString("## 列\n\n| 列名 | 类型 | 分类 | 含义 |\n| --- | --- | --- | --- |\n")
		for _, c := range cols {
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", c.Name, c.Type, c.Kind, c.Description)
		}
		return resultJSON(payload, b.String()), nil
	})
}

// -----------------------------------------------------------------------------
// 共享的参数解析辅助
// -----------------------------------------------------------------------------

// windowFromRequest 解析并校验时间窗口。
func windowFromRequest(req mcp.CallToolRequest, d *Deps, defaultHours int) (query.TimeWindow, error) {
	return query.ParseWindow(
		req.GetString("start", ""),
		req.GetString("end", ""),
		defaultHours,
		d.Cfg.Query.MaxWindowDays,
	)
}

// filtersFromRequest 解析 filters 参数。
func filtersFromRequest(req mcp.CallToolRequest) ([]query.Filter, error) {
	raw, ok := req.GetArguments()["filters"]
	if !ok || raw == nil {
		return nil, nil
	}
	return query.ParseFilters(raw)
}

// limitFromRequest 取 limit 并按服务上限封顶。
func limitFromRequest(req mcp.CallToolRequest, d *Deps, def int) int {
	n := req.GetInt("limit", def)
	if n <= 0 {
		n = def
	}
	if n > d.Cfg.Query.MaxRows {
		n = d.Cfg.Query.MaxRows
	}
	return n
}

// filtersToolOption 是各业务工具复用的 filters 入参定义。
func filtersToolOption() mcp.ToolOption {
	return mcp.WithArray("filters",
		mcp.Description("附加过滤条件，数组元素形如 {\"column\":\"product_channel\",\"operator\":\"=\",\"value\":\"xxx\"}。"+
			"operator 支持 "+strings.Join(query.OperatorNames(), " / ")+"；"+
			"in / not_in 用 values 传数组；is_null / is_not_null 不需要取值；"+
			"like 未写 %% 时按包含匹配。column 必须是目标表真实存在的列（见 describe_ocpx_table）"),
		mcp.Items(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"column":   map[string]any{"type": "string", "description": "列名"},
				"operator": map[string]any{"type": "string", "description": "操作符，默认 ="},
				"value":    map[string]any{"description": "单值操作符的取值"},
				"values":   map[string]any{"type": "array", "description": "in / not_in 的取值列表"},
			},
			"required": []any{"column"},
		}),
	)
}
