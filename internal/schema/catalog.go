// Package schema 内置 OCPX 六张 Doris 明细表的元数据目录。
//
// 这里是所有工具的"唯一事实来源"：列名白名单、列的业务含义、
// 维度/指标/设备 ID 分类，都从本目录读取。任何拼进 SQL 的标识符
// 都必须先经过本包校验，绝不允许把模型传入的字符串直接当列名用。
package schema

import (
	"fmt"
	"sort"
	"strings"
)

// Kind 是列的用途分类，决定它能出现在 SQL 的哪个位置。
type Kind string

const (
	KindTime     Kind = "time"       // 时间列，可做分桶与范围过滤
	KindDim      Kind = "dimension"  // 维度列，可 GROUP BY / 过滤
	KindDeviceID Kind = "device_id"  // 设备/用户标识列，可用于链路反查
	KindMetric   Kind = "metric"     // 数值列，可聚合
	KindDiag     Kind = "diagnostic" // 诊断列（err/is_loss 等）
	KindRaw      Kind = "raw"        // 大字段（text/超长 varchar），只能明细展示，禁止 GROUP BY
)

// Stage 是归因漏斗中的环节。
type Stage string

const (
	StageImp  Stage = "imp"  // 曝光
	StageClk  Stage = "clk"  // 点击
	StageConv Stage = "conv" // 转化/回传
)

// Line 是业务线：通用 OCPX 与京东专用链路。
type Line string

const (
	LineV1 Line = "v1"
	LineJD Line = "jd"
)

// Column 描述一列。
type Column struct {
	Name string
	Type string
	Kind Kind
	Desc string
}

// Table 描述一张表。
type Table struct {
	Name       string
	Line       Line
	Stage      Stage
	Title      string
	Desc       string
	TimeColumn string
	DupKeys    []string
	Columns    []Column

	byName map[string]Column
}

// Column 按列名查找，第二个返回值表示是否存在。
func (t *Table) Column(name string) (Column, bool) {
	c, ok := t.byName[strings.ToLower(strings.TrimSpace(name))]
	return c, ok
}

// ColumnsOfKind 返回指定分类下的所有列。
func (t *Table) ColumnsOfKind(kinds ...Kind) []Column {
	want := map[Kind]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	var out []Column
	for _, c := range t.Columns {
		if want[c.Kind] {
			out = append(out, c)
		}
	}
	return out
}

// GroupableNames 返回允许 GROUP BY 的列名（排除大字段与时间列）。
func (t *Table) GroupableNames() []string {
	var out []string
	for _, c := range t.Columns {
		if c.Kind == KindDim || c.Kind == KindDeviceID || c.Kind == KindDiag {
			out = append(out, c.Name)
		}
	}
	return out
}

// -----------------------------------------------------------------------------
// 列元数据注册表
// -----------------------------------------------------------------------------

// colMeta 是跨表共享的列定义。六张表的公共列占绝大多数，集中定义一份，
// 各表只声明自己的列顺序，个别类型差异走 typeOverrides。
var colMeta = map[string]Column{
	"req_time":        {Type: "datetime", Kind: KindTime, Desc: "请求时间，分区/排序主键，所有查询都必须带此列的范围过滤"},
	"uid":             {Type: "int", Kind: KindDim, Desc: "内部媒体/账号 uid"},
	"support_id":      {Type: "int", Kind: KindDim, Desc: "支撑方 ID"},
	"agent_id":        {Type: "int", Kind: KindDim, Desc: "代理商 ID"},
	"product_id":      {Type: "int", Kind: KindDim, Desc: "产品 ID"},
	"channel_id":      {Type: "int", Kind: KindDim, Desc: "渠道 ID"},
	"product_channel": {Type: "varchar(128)", Kind: KindDim, Desc: "产品渠道标识（产品+渠道拼接串），最常用的下钻维度"},
	"place_id":        {Type: "varchar(128)", Kind: KindDim, Desc: "广告位 ID"},
	"dhh_task_id":     {Type: "varchar(128)", Kind: KindDim, Desc: "ДHH 任务 ID（投放任务）"},
	"unikey":          {Type: "varchar(16)", Kind: KindDim, Desc: "归因唯一键：同一次曝光→点击→转化在三张表里共用，漏斗串联的首选 join key"},
	"advertiser_id":   {Type: "varchar(128)", Kind: KindDim, Desc: "广告主 ID"},
	"campaign_id":     {Type: "varchar(128)", Kind: KindDim, Desc: "广告计划 ID"},
	"ad_id":           {Type: "varchar(128)", Kind: KindDim, Desc: "广告/创意 ID"},
	"cid":             {Type: "varchar(128)", Kind: KindDim, Desc: "外部渠道点击 ID，媒体侧回传对齐用"},
	"p1":              {Type: "varchar(32)", Kind: KindDim, Desc: "京东链路专用位置/场景参数"},
	"user_id":         {Type: "varchar(20)", Kind: KindDeviceID, Desc: "用户 ID"},
	"test_status":     {Type: "int", Kind: KindDim, Desc: "测试标记：0=正常流量，非 0=测试流量。统计口径默认应剔除非 0"},
	"os":              {Type: "varchar(32)", Kind: KindDim, Desc: "操作系统（android / ios 等）"},
	"type":            {Type: "varchar(32)", Kind: KindDim, Desc: "记录类型/上报类型标识"},
	"servername":      {Type: "varchar(128)", Kind: KindDim, Desc: "处理该请求的服务实例 hostname:port，排查单机异常用"},
	"ip":              {Type: "varchar(64)", Kind: KindDeviceID, Desc: "客户端 IP"},
	"ua":              {Type: "text", Kind: KindRaw, Desc: "原始 User-Agent（大字段，禁止 GROUP BY）"},
	"ua_md5":          {Type: "varchar(128)", Kind: KindDim, Desc: "User-Agent 的 MD5，UA 聚合用"},
	"is_loss":         {Type: "int", Kind: KindDiag, Desc: "丢失标记：1=该条被丢弃/未生效，0=正常"},
	"err":             {Type: "text", Kind: KindDiag, Desc: "错误信息，空串表示无错误"},
	"imei":            {Type: "varchar(128)", Kind: KindDeviceID, Desc: "IMEI 明文"},
	"imei_sum":        {Type: "varchar(128)", Kind: KindDeviceID, Desc: "IMEI 的 MD5"},
	"idfa":            {Type: "varchar(128)", Kind: KindDeviceID, Desc: "IDFA 明文（iOS）"},
	"idfa_sum":        {Type: "varchar(128)", Kind: KindDeviceID, Desc: "IDFA 的 MD5"},
	"oaid":            {Type: "varchar(512)", Kind: KindDeviceID, Desc: "OAID 明文（Android）"},
	"oaid_sum":        {Type: "varchar(128)", Kind: KindDeviceID, Desc: "OAID 的 MD5"},
	"caid":            {Type: "varchar(256)", Kind: KindDeviceID, Desc: "CAID，格式为 version_md5（iOS 中广协 ID）"},
	"caidjson":        {Type: "text", Kind: KindRaw, Desc: "CAID 原始 JSON（大字段，禁止 GROUP BY）"},
	"callback_param":  {Type: "varchar(10240)", Kind: KindRaw, Desc: "媒体回调参数原文（大字段，禁止 GROUP BY）"},
	"req_id":          {Type: "varchar(256)", Kind: KindDim, Desc: "请求 ID，单次请求全链路排查用"},
	"log_time":        {Type: "bigint", Kind: KindMetric, Desc: "日志时间戳（秒级 Unix 时间）"},
	"ts":              {Type: "int", Kind: KindMetric, Desc: "事件时间戳（秒级 Unix 时间）"},

	"event_name":         {Type: "varchar(128)", Kind: KindDim, Desc: "转化事件名（本系统口径）"},
	"up_event_name":      {Type: "varchar(128)", Kind: KindDim, Desc: "上游（媒体侧）事件名"},
	"down_event_name":    {Type: "varchar(128)", Kind: KindDim, Desc: "下游（广告主侧）事件名"},
	"action_pv":          {Type: "tinyint", Kind: KindMetric, Desc: "浅层转化行为数，按行累加即为转化量"},
	"depth_action_pv":    {Type: "tinyint", Kind: KindMetric, Desc: "深层转化行为数"},
	"cb_action_pv":       {Type: "tinyint", Kind: KindMetric, Desc: "已回传的浅层转化数"},
	"cb_depth_action_pv": {Type: "tinyint", Kind: KindMetric, Desc: "已回传的深层转化数"},
}

// typeOverrides 记录与 colMeta 默认类型不一致的个别列（多为 varchar 长度差异）。
var typeOverrides = map[string]map[string]string{
	"ocpx_v1_clk": {
		"place_id":      "varchar(65533)",
		"dhh_task_id":   "varchar(65533)",
		"unikey":        "varchar(65533)",
		"advertiser_id": "varchar(65533)",
		"campaign_id":   "varchar(65533)",
		"ad_id":         "varchar(65533)",
		"type":          "varchar(65533)",
		"req_id":        "varchar(128)",
	},
	"ocpx_v1_track":    {"req_id": "varchar(128)"},
	"ocpx_jd_imp":      {"req_id": "varchar(128)"},
	"ocpx_jd_clk":      {"req_id": "varchar(128)"},
	"ocpx_jd_callback": {"req_id": "varchar(128)", "caid": "varchar(1000)", "p1": "varchar(16)"},
}

// -----------------------------------------------------------------------------
// 表定义
// -----------------------------------------------------------------------------

var commonDupKeys = []string{"req_time", "uid", "support_id", "agent_id", "product_id", "channel_id", "product_channel", "place_id", "dhh_task_id"}
var jdDupKeys = []string{"req_time", "uid", "product_channel", "p1", "cid", "advertiser_id"}

// tableDefs 按建表顺序声明每张表的列名。
var tableDefs = []struct {
	name    string
	line    Line
	stage   Stage
	title   string
	desc    string
	dupKeys []string
	cols    []string
}{
	{
		name: "ocpx_v1_imp", line: LineV1, stage: StageImp,
		title:   "通用曝光明细表",
		desc:    "OCPX 通用链路的曝光（展示）明细，一行一次曝光。漏斗第一环，CTR 的分母。",
		dupKeys: commonDupKeys,
		cols: []string{
			"req_time", "uid", "support_id", "agent_id", "product_id", "channel_id", "product_channel",
			"place_id", "dhh_task_id", "unikey", "advertiser_id", "campaign_id", "ad_id", "cid",
			"user_id", "test_status", "os", "ua", "ua_md5", "ip", "type", "is_loss", "err",
			"idfa_sum", "oaid_sum", "callback_param", "imei", "idfa", "oaid", "imei_sum",
			"log_time", "caid", "req_id", "caidjson", "servername",
		},
	},
	{
		name: "ocpx_v1_clk", line: LineV1, stage: StageClk,
		title:   "通用点击明细表",
		desc:    "OCPX 通用链路的点击明细，一行一次点击。漏斗第二环，CTR 的分子、CVR 的分母。相比曝光表少了原始 ua 列。",
		dupKeys: commonDupKeys,
		cols: []string{
			"req_time", "uid", "support_id", "agent_id", "product_id", "channel_id", "product_channel",
			"place_id", "dhh_task_id", "unikey", "advertiser_id", "campaign_id", "ad_id", "cid",
			"user_id", "test_status", "os", "ua_md5", "ip",
			"idfa_sum", "oaid_sum", "callback_param", "imei", "idfa", "oaid", "imei_sum",
			"log_time", "type", "is_loss", "err", "caid", "req_id", "caidjson", "servername",
		},
	},
	{
		name: "ocpx_v1_track", line: LineV1, stage: StageConv,
		title:   "通用转化明细表",
		desc:    "OCPX 通用链路的转化明细，一行一次转化事件。漏斗第三环，CVR 的分子。转化量应对 action_pv 求和而不是 count(*)。",
		dupKeys: commonDupKeys,
		cols: []string{
			"req_time", "uid", "support_id", "agent_id", "product_id", "channel_id", "product_channel",
			"place_id", "dhh_task_id", "unikey", "advertiser_id", "campaign_id", "ad_id", "cid",
			"event_name", "up_event_name", "down_event_name",
			"action_pv", "depth_action_pv", "cb_action_pv", "cb_depth_action_pv",
			"user_id", "test_status", "ip", "type", "is_loss", "err",
			"idfa_sum", "oaid_sum", "callback_param", "ua_md5", "imei", "idfa", "oaid", "imei_sum",
			"log_time", "ts", "caid", "req_id", "caidjson", "servername",
		},
	},
	{
		name: "ocpx_jd_imp", line: LineJD, stage: StageImp,
		title:   "京东曝光明细表",
		desc:    "京东专用链路的曝光明细。相比通用曝光表多了 p1 与转化事件列，DUPLICATE KEY 也不同（以 product_channel/p1/cid/advertiser_id 为主）。",
		dupKeys: jdDupKeys,
		cols: []string{
			"req_time", "uid", "product_channel", "p1", "cid", "advertiser_id",
			"support_id", "agent_id", "product_id", "channel_id", "place_id", "dhh_task_id", "unikey",
			"campaign_id", "ad_id", "event_name", "up_event_name", "down_event_name",
			"action_pv", "depth_action_pv", "cb_action_pv", "cb_depth_action_pv",
			"user_id", "test_status", "ip", "type", "is_loss", "err",
			"idfa_sum", "oaid_sum", "callback_param", "ua_md5", "imei", "idfa", "oaid", "imei_sum",
			"log_time", "ts", "caid", "req_id", "caidjson", "servername",
		},
	},
	{
		name: "ocpx_jd_clk", line: LineJD, stage: StageClk,
		title:   "京东点击明细表",
		desc:    "京东专用链路的点击明细，列结构与 ocpx_jd_imp 完全一致。",
		dupKeys: jdDupKeys,
		cols: []string{
			"req_time", "uid", "product_channel", "p1", "cid", "advertiser_id",
			"support_id", "agent_id", "product_id", "channel_id", "place_id", "dhh_task_id", "unikey",
			"campaign_id", "ad_id", "event_name", "up_event_name", "down_event_name",
			"action_pv", "depth_action_pv", "cb_action_pv", "cb_depth_action_pv",
			"user_id", "test_status", "ip", "type", "is_loss", "err",
			"idfa_sum", "oaid_sum", "callback_param", "ua_md5", "imei", "idfa", "oaid", "imei_sum",
			"log_time", "ts", "caid", "req_id", "caidjson", "servername",
		},
	},
	{
		name: "ocpx_jd_callback", line: LineJD, stage: StageConv,
		title:   "京东转化回传表",
		desc:    "京东专用链路的转化回传明细。DUPLICATE KEY 额外包含 up_event_name，即同一次点击的不同上游事件各占一行。",
		dupKeys: append(append([]string{}, jdDupKeys...), "up_event_name"),
		cols: []string{
			"req_time", "uid", "product_channel", "p1", "cid", "advertiser_id", "up_event_name", "err",
			"support_id", "agent_id", "product_id", "channel_id", "place_id", "dhh_task_id", "unikey",
			"campaign_id", "ad_id", "event_name", "down_event_name",
			"action_pv", "depth_action_pv", "cb_action_pv", "cb_depth_action_pv",
			"user_id", "test_status", "ip", "type", "is_loss",
			"idfa_sum", "oaid_sum", "callback_param", "ua_md5", "imei", "idfa", "oaid", "imei_sum",
			"log_time", "ts", "caid", "req_id", "caidjson", "servername",
		},
	},
}

var (
	tables      = map[string]*Table{}
	tableOrder  []string
	stageByLine = map[Line]map[Stage]*Table{}
)

func init() {
	for _, def := range tableDefs {
		t := &Table{
			Name:       def.name,
			Line:       def.line,
			Stage:      def.stage,
			Title:      def.title,
			Desc:       def.desc,
			TimeColumn: "req_time",
			DupKeys:    def.dupKeys,
			byName:     map[string]Column{},
		}
		for _, name := range def.cols {
			meta, ok := colMeta[name]
			if !ok {
				panic(fmt.Sprintf("schema: 表 %s 引用了未注册的列 %q", def.name, name))
			}
			col := Column{Name: name, Type: meta.Type, Kind: meta.Kind, Desc: meta.Desc}
			if ov, ok := typeOverrides[def.name][name]; ok {
				col.Type = ov
			}
			t.Columns = append(t.Columns, col)
			t.byName[name] = col
		}
		tables[t.Name] = t
		tableOrder = append(tableOrder, t.Name)
		if stageByLine[t.Line] == nil {
			stageByLine[t.Line] = map[Stage]*Table{}
		}
		stageByLine[t.Line][t.Stage] = t
	}
}

// Get 按表名取表定义。
func Get(name string) (*Table, error) {
	t, ok := tables[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return nil, fmt.Errorf("未知表 %q，可用表：%s（请先调用 list_ocpx_tables）", name, strings.Join(TableNames(), ", "))
	}
	return t, nil
}

// TableNames 返回全部表名。
func TableNames() []string {
	out := append([]string{}, tableOrder...)
	return out
}

// All 返回全部表定义，顺序与 TableNames 一致。
func All() []*Table {
	out := make([]*Table, 0, len(tableOrder))
	for _, n := range tableOrder {
		out = append(out, tables[n])
	}
	return out
}

// StageTable 按业务线 + 漏斗环节取表。
func StageTable(line Line, stage Stage) (*Table, error) {
	m, ok := stageByLine[line]
	if !ok {
		return nil, fmt.Errorf("未知业务线 %q，只支持 v1（通用）与 jd（京东）", line)
	}
	t, ok := m[stage]
	if !ok {
		return nil, fmt.Errorf("业务线 %q 没有 %q 环节的表", line, stage)
	}
	return t, nil
}

// ParseLine 校验业务线取值。
func ParseLine(s string) (Line, error) {
	switch Line(strings.ToLower(strings.TrimSpace(s))) {
	case LineV1:
		return LineV1, nil
	case LineJD:
		return LineJD, nil
	default:
		return "", fmt.Errorf("未知业务线 %q，只支持 v1（通用 OCPX）与 jd（京东）", s)
	}
}

// ParseStage 校验漏斗环节取值。
func ParseStage(s string) (Stage, error) {
	switch Stage(strings.ToLower(strings.TrimSpace(s))) {
	case StageImp:
		return StageImp, nil
	case StageClk:
		return StageClk, nil
	case StageConv:
		return StageConv, nil
	default:
		return "", fmt.Errorf("未知环节 %q，只支持 imp（曝光）/ clk（点击）/ conv（转化）", s)
	}
}

// StageLabel 返回环节的中文名。
func StageLabel(s Stage) string {
	switch s {
	case StageImp:
		return "曝光"
	case StageClk:
		return "点击"
	case StageConv:
		return "转化"
	}
	return string(s)
}

// CommonGroupable 返回在给定若干表中都存在、且可 GROUP BY 的列名（已排序）。
func CommonGroupable(ts ...*Table) []string {
	if len(ts) == 0 {
		return nil
	}
	count := map[string]int{}
	for _, t := range ts {
		for _, n := range t.GroupableNames() {
			count[n]++
		}
	}
	var out []string
	for n, c := range count {
		if c == len(ts) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
