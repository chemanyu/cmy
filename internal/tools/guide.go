package tools

import "github.com/chemanyu/mcp/internal/schema"

// JDOrderTotalSQL 复用于工具说明与字段示例，月份/日期仅替换边界。
const JDOrderTotalSQL = "SELECT " + conversionCounts + `
FROM ocpx_jd_callback
WHERE req_time >= 'START' AND req_time < 'END'
  AND (up_event_name = '4' OR type = 'scheduled_callback')`

// QuickQueryGuide 将高频问法直接映射到可执行模板，不要求先遍历元数据。
const QuickQueryGuide = `【高频查询直接套用】
“九月份，京东有多少订单转化”/“本月京东订单总量”/“今天京东收到多少订单”：
直接使用下面的已知表/字段模板，替换START/END后调用ocpx_run_sql一次。无需先list/describe、查字段取值、分组后再求和或关联点击。
` + JDOrderTotalSQL + `
时间替换：九月份→YYYY-09-01 00:00:00至YYYY-10-01 00:00:00；其他月份同理。本月→当月1日至下月1日；今天→当天零点至查询时刻。优先沿用上下文年份，无上下文年份时用当前年份，在回答中写明年月与Asia/Shanghai；不为常规月份查询反复确认年份。查询当前未结束月份时说明“截至查询时刻已收到”，不称为完整月结果。用户明确其他年份/时区时遵从用户。
模板含义：事件4 OR scheduled_callback低活订单，err非空仍计入收到总量；不自动加err/test_status/is_loss条件。无需选择具体账户；“京东”默认全账户。
结果回答：先报received_records（收到订单转化记录数），received_action_count单独标为action_pv行为数；可补充filtered_records（已收到但我方过滤不下传）。不能把unfiltered_records称作回传成功。
常见变体只改以下位置：
- “这个账户的京东订单”：追加 AND advertiser_id='ACCOUNT_ID'；“这个监测的京东订单”：追加 AND unikey='MONITOR_ID'，均为字符串并转义。
- 明确“事件4”：把括号内整个订单条件替换为up_event_name='4'；明确“低活订单”：替换为type='scheduled_callback'。泛指订单保留OR。
- “未过滤订单”：额外追加 AND (err IS NULL OR err='')；“我方过滤订单”：追加 AND err IS NOT NULL AND err!=''；“收到订单”保持原样。
- “按小时/每天变化”：增加DATE_FORMAT(req_time, '%Y-%m-%d %H:00:00')或DATE_FORMAT(req_time, '%Y-%m-%d')分组，保留相同订单条件与收到统计口径；无数据时间桶补0。
遇到未覆盖的字段或复杂关联，再读describe_ocpx_table的完整场景示例。
`

// QueryGuide 同时用于 initialize 指引和字段工具，避免调用策略与字段知识脱节。
const QueryGuide = QuickQueryGuide + `
业务词典：监测ID=unikey；账户=advertiser_id；上游转化=up_event_name。
三个字段均按字符串处理，保留原值。京东callback中：明确查询事件4只用 up_event_name='4'；低活订单转化用 type='scheduled_callback'，即使up_event_name为NULL或空串也纳入，不额外要求事件字段为空。泛指订单转化时用 (up_event_name='4' OR type='scheduled_callback')，可按type拆分低活订单；OR使同时满足两条件的记录只统计一次。不能把scheduled_callback改写成事件4，也不要用type='4'。
选表：track=ocpx_v1_track，callback=ocpx_jd_callback；对应点击为 ocpx_v1_clk / ocpx_jd_clk。
只说监测ID时沿用会话业务线；无上下文则分别查通用和京东并标注业务线，不混合汇总。问“有数据吗”且未指定环节时覆盖曝光、点击、转化。
时间：按用户业务时区把今天、八月份转换为明确的 [start,end)；未提供时区按 Asia/Shanghai 并说明。今天用当天零点到查询时刻；月份用该月1日至下月1日，年份按会话上下文并说明。不要硬编码示例日期，不依赖数据库 CURDATE() 的时区。
默认按各表 req_time 统计收到的数据；小时趋势也按 req_time 分桶。无数据小时补0，当前未结束小时标为不完整，不直接和完整上一小时比较。变化可给相邻完整小时差值/变化率，前值0时变化率为null。
“多少点击数据”回答记录条数。通用track与京东callback统一口径：err非空表示我们真实收到了转化，但被我方过滤，不向下游回传；不是虚假或未收到的转化。过滤记录用 err IS NOT NULL AND err!=''；err为空（NULL或空串）仅表示未被该字段标记过滤，不能据此断言回传成功。默认转化数量/订单数量/上游转化/收到趋势统计全部收到记录，包含err非空记录；同时可拆分过滤与未过滤数量。收到行为数对全部记录累加action_pv。只有明确查询未过滤转化时才加 (err IS NULL OR err='')；查询实际回传量需核实cb_action_pv/cb_depth_action_pv等回传指标口径，不能用err为空的行数替代。问“多少上游转化”按up_event_name分组并给合计，不默认只查事件4。
排查、收到的数据保留过滤记录，标注其已收到但被我方过滤、未向下游回传；按err分组时不能先排除err非空记录。test_status / is_loss遵循用户要求，不能替代err判定。上述err规则同样适用于低活订单，type是关键展示/分组字段，取值含义未知时只报告原值。err按完整内容分组；如后端不支持text分组，明确说明截断长度与可能合并错误的影响。
跨表：callback与clk按req_id关联，排除空req_id，并沿用账户/监测筛选。DUPLICATE KEY不保证唯一，先汇总点击侧匹配数量；多个不同点击时间单独标记歧义，不直接多对多JOIN计数，也不擅自认定最早/最近点击。
callback.log_time目录定义为Unix秒；关联前核实数据量级，不把毫秒当秒。与clk.req_time计算天数时，默认自然日差 DATEDIFF(FROM_UNIXTIME(log_time), clk.req_time)，要求数据库会话时区与业务时区一致。若需每满24小时，使用秒差除86400向下取整。负数、空时间、未匹配、歧义单独报告。
月份关联默认限定callback.req_time在目标月；点击回溯窗口可能跨月，不能擅自也限定同月。回溯范围不明确时询问；未匹配只能说在所选窗口内未匹配。双方都需req_time范围。
所有数据查询走ocpx_run_sql；裸表名兼容HTTP网关。示例里的占位文本必须替换，字符串值按SQL规则转义。结果truncated时说明结果不完整，不能用截断分组求总数。`

// conversionCounts 收到总量包含我方过滤的转化；未过滤量不代表回传成功量。
const conversionCounts = "COUNT(*) AS received_records, COALESCE(SUM(action_pv), 0) AS received_action_count, " +
	"COALESCE(SUM(CASE WHEN err IS NOT NULL AND err != '' THEN 1 ELSE 0 END), 0) AS filtered_records, " +
	"COALESCE(SUM(CASE WHEN err IS NULL OR err = '' THEN 1 ELSE 0 END), 0) AS unfiltered_records, " +
	"COALESCE(SUM(CASE WHEN err IS NULL OR err = '' THEN action_pv ELSE 0 END), 0) AS unfiltered_action_count"

type queryExample struct {
	Scenario string `json:"scenario"`
	Notes    string `json:"notes"`
	SQL      string `json:"sql"`
}

func keyFields(t *schema.Table) []string {
	var fields []string
	for _, name := range []string{"req_time", "unikey", "advertiser_id", "up_event_name", "type", "err", "req_id", "log_time", "action_pv"} {
		if _, ok := t.Column(name); ok {
			fields = append(fields, name)
		}
	}
	return fields
}

func examplesFor(table string) []queryExample {
	base := queryExample{
		Scenario: "监测ID在指定时间是否有数据/多少条数据",
		Notes:    "START/END替换成明确时间；MONITOR_ID替换成用户监测ID，例如82091968bc。点击和转化分别查各自表。",
		SQL:      "SELECT COUNT(*) AS record_count FROM " + table + " WHERE req_time >= 'START' AND req_time < 'END' AND unikey = 'MONITOR_ID'",
	}
	if table == "ocpx_v1_track" || table == "ocpx_jd_callback" {
		base.SQL = "SELECT " + conversionCounts + " FROM " + table + " WHERE req_time >= 'START' AND req_time < 'END' AND unikey = 'MONITOR_ID'"
		base.Notes += " 转化区分收到总数、err非空过滤数、未过滤记录数与未过滤行为数。"
	}
	examples := []queryExample{base}
	if table == "ocpx_jd_callback" {
		examples = append([]queryExample{{
			Scenario: "九月份，京东有多少订单转化 / 本月京东订单总量 / 今天京东收到多少订单",
			Notes:    "直接用本模板查询全账户总数。九月份将START/END替换为上下文年份（无则当前年份）的09-01零点和10-01零点；包含事件4、低活订单及err非空的已收到记录。先回答received_records，action_pv行为数另列；未结束月份说明截至查询时刻。账户/监测限制仅在用户指定时追加。",
			SQL:      JDOrderTotalSQL,
		}}, examples...)
	}
	if table == "ocpx_v1_track" || table == "ocpx_jd_callback" {
		examples = append(examples, queryExample{
			Scenario: "上游转化事件分布（今天track有多少上游转化）",
			Notes:    "查询全部上游事件并区分收到/过滤/未过滤数量；指定监测ID时增加unikey条件。合计需完整分组结果或独立总数查询。",
			SQL:      "SELECT up_event_name, " + conversionCounts + " FROM " + table + " WHERE req_time >= 'START' AND req_time < 'END' GROUP BY up_event_name ORDER BY received_records DESC",
		})
	}
	if table == "ocpx_jd_clk" {
		examples = append(examples, queryExample{
			Scenario: "京东账户点击数据，按type查看",
			Notes:    "ACCOUNT_ID为账户字符串，不是监测ID。需要明细时选择req_time、req_id、unikey、advertiser_id、type、err并限制行数。",
			SQL:      "SELECT type, COUNT(*) AS record_count FROM ocpx_jd_clk WHERE req_time >= 'START' AND req_time < 'END' AND advertiser_id = 'ACCOUNT_ID' GROUP BY type ORDER BY record_count DESC",
		})
	}
	if table == "ocpx_jd_callback" {
		examples = append(examples,
			queryExample{
				Scenario: "京东账户低活订单转化，按err分组",
				Notes:    "scheduled_callback表示低活订单，不要求up_event_name有值；ACCOUNT_ID替换为账户字符串。",
				SQL:      "SELECT type, err, " + conversionCounts + " FROM ocpx_jd_callback WHERE req_time >= 'START' AND req_time < 'END' AND advertiser_id = 'ACCOUNT_ID' AND type = 'scheduled_callback' GROUP BY type, err ORDER BY received_records DESC",
			},
			queryExample{
				Scenario: "账户今天的事件4转化数据，按err/type分组",
				Notes:    "ACCOUNT_ID例如1865615583177352。未要求分组时可查总数或明细；不要过滤掉报错和丢失记录。",
				SQL:      "SELECT up_event_name, type, err, " + conversionCounts + " FROM ocpx_jd_callback WHERE req_time >= 'START' AND req_time < 'END' AND advertiser_id = 'ACCOUNT_ID' AND up_event_name = '4' GROUP BY up_event_name, type, err ORDER BY received_records DESC",
			},
			queryExample{
				Scenario: "今天收到的京东订单转化（含低活），每小时变化",
				Notes:    "包含事件4和scheduled_callback低活订单，每小时分别返回收到/过滤/未过滤数量；订单/收到趋势默认用received_records，只有未过滤趋势用unfiltered_records。按收到时间req_time分桶，返回后补齐无数据小时；当前小时不完整。指定账户时补advertiser_id条件；用户明确只查事件4时改为仅up_event_name='4'。",
				SQL:      "SELECT DATE_FORMAT(req_time, '%Y-%m-%d %H:00:00') AS hour, " + conversionCounts + " FROM ocpx_jd_callback WHERE req_time >= 'START' AND req_time < 'END' AND (up_event_name = '4' OR type = 'scheduled_callback') GROUP BY hour ORDER BY hour",
			},
			queryExample{
				Scenario: "八月份事件4，关联点击后按转化日志时间与点击时间的自然日差分布",
				Notes:    "MONTH_START/MONTH_END为目标月份；CLICK_START/CLICK_END需确认回溯窗口。先核实log_time为Unix秒、数据库时区。点击侧聚合后每个req_id一行；多个不同点击时间标为ambiguous_click，不替用户选时间。重复同时间点击保留匹配数诊断。按天数分别统计收到、过滤、未过滤callback记录，不是JOIN配对数。若限定账户/监测，两边均补相同条件。",
				SQL: `WITH clicks AS (
 SELECT req_id, COUNT(*) AS click_rows, COUNT(DISTINCT req_time) AS click_times, MIN(req_time) AS click_time
 FROM ocpx_jd_clk
 WHERE req_time >= 'CLICK_START' AND req_time < 'CLICK_END' AND req_id IS NOT NULL AND req_id != ''
 GROUP BY req_id
), matched AS (
 SELECT CASE WHEN c.req_id IS NULL THEN 'unmatched'
             WHEN c.click_times != 1 THEN 'ambiguous_click'
             WHEN t.log_time IS NULL OR t.log_time <= 0 OR FROM_UNIXTIME(t.log_time) IS NULL THEN 'invalid_log_time'
             WHEN FROM_UNIXTIME(t.log_time) < c.click_time THEN 'negative_interval'
             ELSE 'matched' END AS match_status,
        CASE WHEN c.click_times = 1 AND t.log_time > 0
             THEN DATEDIFF(FROM_UNIXTIME(t.log_time), c.click_time) END AS days,
        c.click_rows, t.err, t.action_pv
 FROM ocpx_jd_callback t LEFT JOIN clicks c ON t.req_id = c.req_id
 WHERE t.req_time >= 'MONTH_START' AND t.req_time < 'MONTH_END' AND t.up_event_name = '4'
)
SELECT match_status, days, ` + conversionCounts + `,
       SUM(CASE WHEN click_rows > 1 THEN 1 ELSE 0 END) AS records_with_multiple_clicks
FROM matched GROUP BY match_status, days ORDER BY match_status, days`,
			},
		)
	}
	return examples
}
