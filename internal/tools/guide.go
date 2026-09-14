package tools

import "github.com/chemanyu/mcp/internal/schema"

// QueryGuide 同时用于 initialize 指引和字段工具，避免调用策略与字段知识脱节。
const QueryGuide = `业务词典：监测ID=unikey；账户=advertiser_id；上游转化=up_event_name。
三个字段均按字符串处理，保留原值。京东callback中：明确查询事件4只用 up_event_name='4'；低活订单转化用 type='scheduled_callback'，即使up_event_name为NULL或空串也纳入，不额外要求事件字段为空。泛指订单转化时用 (up_event_name='4' OR type='scheduled_callback')，可按type拆分低活订单；OR使同时满足两条件的记录只统计一次。不能把scheduled_callback改写成事件4，也不要用type='4'。
选表：track=ocpx_v1_track，callback=ocpx_jd_callback；对应点击为 ocpx_v1_clk / ocpx_jd_clk。
只说监测ID时沿用会话业务线；无上下文则分别查通用和京东并标注业务线，不混合汇总。问“有数据吗”且未指定环节时覆盖曝光、点击、转化。
时间：按用户业务时区把今天、八月份转换为明确的 [start,end)；未提供时区按 Asia/Shanghai 并说明。今天用当天零点到查询时刻；月份用该月1日至下月1日，年份按会话上下文并说明。不要硬编码示例日期，不依赖数据库 CURDATE() 的时区。
默认按各表 req_time 统计收到的数据；小时趋势也按 req_time 分桶。无数据小时补0，当前未结束小时标为不完整，不直接和完整上一小时比较。变化可给相邻完整小时差值/变化率，前值0时变化率为null。
“多少点击/转化数据”优先回答记录条数 COUNT(*)；转化同时列出 SUM(action_pv) 行为数并标注。问“多少上游转化”按 up_event_name 分组并给合计，不默认只查事件4。
排查、收到的数据默认不额外剔除 test_status / is_loss；用户要求正式有效统计时才明确过滤口径。type是关键展示/分组字段，取值含义未知时只报告原值。err按完整内容分组；如后端不支持text分组，明确说明截断长度与可能合并错误的影响。
跨表：callback与clk按req_id关联，排除空req_id，并沿用账户/监测筛选。DUPLICATE KEY不保证唯一，先汇总点击侧匹配数量；多个不同点击时间单独标记歧义，不直接多对多JOIN计数，也不擅自认定最早/最近点击。
callback.log_time目录定义为Unix秒；关联前核实数据量级，不把毫秒当秒。与clk.req_time计算天数时，默认自然日差 DATEDIFF(FROM_UNIXTIME(log_time), clk.req_time)，要求数据库会话时区与业务时区一致。若需每满24小时，使用秒差除86400向下取整。负数、空时间、未匹配、歧义单独报告。
月份关联默认限定callback.req_time在目标月；点击回溯窗口可能跨月，不能擅自也限定同月。回溯范围不明确时询问；未匹配只能说在所选窗口内未匹配。双方都需req_time范围。
所有数据查询走ocpx_run_sql；裸表名兼容HTTP网关。示例里的占位文本必须替换，字符串值按SQL规则转义。结果truncated时说明结果不完整，不能用截断分组求总数。`

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
	examples := []queryExample{base}
	if table == "ocpx_v1_track" || table == "ocpx_jd_callback" {
		examples = append(examples, queryExample{
			Scenario: "上游转化事件分布（今天track有多少上游转化）",
			Notes:    "查询全部上游事件；指定监测ID时增加unikey条件。合计需完整分组结果或独立总数查询。",
			SQL:      "SELECT up_event_name, COUNT(*) AS record_count, COALESCE(SUM(action_pv), 0) AS action_count FROM " + table + " WHERE req_time >= 'START' AND req_time < 'END' GROUP BY up_event_name ORDER BY record_count DESC",
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
				SQL:      "SELECT type, err, COUNT(*) AS record_count, COALESCE(SUM(action_pv), 0) AS action_count FROM ocpx_jd_callback WHERE req_time >= 'START' AND req_time < 'END' AND advertiser_id = 'ACCOUNT_ID' AND type = 'scheduled_callback' GROUP BY type, err ORDER BY record_count DESC",
			},
			queryExample{
				Scenario: "账户今天的事件4转化数据，按err/type分组",
				Notes:    "ACCOUNT_ID例如1865615583177352。未要求分组时可查总数或明细；不要过滤掉报错和丢失记录。",
				SQL:      "SELECT up_event_name, type, err, COUNT(*) AS record_count, COALESCE(SUM(action_pv), 0) AS action_count FROM ocpx_jd_callback WHERE req_time >= 'START' AND req_time < 'END' AND advertiser_id = 'ACCOUNT_ID' AND up_event_name = '4' GROUP BY up_event_name, type, err ORDER BY record_count DESC",
			},
			queryExample{
				Scenario: "今天收到的京东订单转化（含低活），每小时变化",
				Notes:    "包含事件4和scheduled_callback低活订单，按收到时间req_time分桶，返回后补齐无数据小时；当前小时不完整。指定账户时补advertiser_id条件；用户明确只查事件4时改为仅up_event_name='4'。",
				SQL:      "SELECT DATE_FORMAT(req_time, '%Y-%m-%d %H:00:00') AS hour, COUNT(*) AS record_count, COALESCE(SUM(action_pv), 0) AS action_count FROM ocpx_jd_callback WHERE req_time >= 'START' AND req_time < 'END' AND (up_event_name = '4' OR type = 'scheduled_callback') GROUP BY hour ORDER BY hour",
			},
			queryExample{
				Scenario: "八月份事件4，关联点击后按转化日志时间与点击时间的自然日差分布",
				Notes:    "MONTH_START/MONTH_END为目标月份；CLICK_START/CLICK_END需确认回溯窗口。先核实log_time为Unix秒、数据库时区。点击侧聚合后每个req_id一行；多个不同点击时间标为ambiguous_click，不替用户选时间。重复同时间点击保留匹配数诊断。总数统计callback记录，不是JOIN配对数。若限定账户/监测，两边均补相同条件。",
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
        c.click_rows
 FROM ocpx_jd_callback t LEFT JOIN clicks c ON t.req_id = c.req_id
 WHERE t.req_time >= 'MONTH_START' AND t.req_time < 'MONTH_END' AND t.up_event_name = '4'
)
SELECT match_status, days, COUNT(*) AS callback_records,
       SUM(CASE WHEN click_rows > 1 THEN 1 ELSE 0 END) AS records_with_multiple_clicks
FROM matched GROUP BY match_status, days ORDER BY match_status, days`,
			},
		)
	}
	return examples
}
