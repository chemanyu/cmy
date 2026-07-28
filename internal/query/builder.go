// Package query 负责把工具参数安全地翻译成 Doris SQL。
//
// 安全模型（重要）：
//   - 标识符（表名、列名）永远来自 internal/schema 目录，模型传入的字符串
//     只用于"查表"，查不到就报错，绝不拼接原文。
//   - 字面量（过滤值、时间、limit）永远走 ? 占位符，交给驱动转义。
//   - 因此本包不需要、也不做任何 SQL 字符串黑名单过滤。
package query

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/chemanyu/mcp/internal/schema"
)

// Builder 增量拼装一条 SELECT。
type Builder struct {
	db    string
	table *schema.Table

	selects []string

	wheres    []string
	whereArgs []any

	groups  []string
	havings []string
	orders  []string
	limit   int
}

// New 针对指定库表创建 Builder。
func New(db string, t *schema.Table) *Builder {
	return &Builder{db: db, table: t}
}

// Table 返回 Builder 绑定的表。
func (b *Builder) Table() *schema.Table { return b.table }

// Qualified 返回带库名的表标识，如 `ocpx`.`ocpx_v1_imp`。
func (b *Builder) Qualified() string { return QualifyTable(b.db, b.table.Name) }

// QualifyTable 生成 `db`.`table` 形式的标识。db/table 均来自受控来源。
func QualifyTable(db, table string) string {
	if db == "" {
		return quoteIdent(table)
	}
	return quoteIdent(db) + "." + quoteIdent(table)
}

// quoteIdent 给标识符加反引号。只接受已经过白名单校验的名字，
// 这里仍对反引号做转义作为第二道防线。
func quoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// SelectExpr 追加一个 select 表达式（表达式由本包内部生成，不接受外部原文）。
func (b *Builder) SelectExpr(expr, alias string) *Builder {
	if alias != "" {
		b.selects = append(b.selects, expr+" AS "+quoteIdent(alias))
	} else {
		b.selects = append(b.selects, expr)
	}
	return b
}

// SelectColumn 追加一个已校验的列。
func (b *Builder) SelectColumn(name string) (*Builder, error) {
	col, err := b.ResolveColumn(name)
	if err != nil {
		return b, err
	}
	b.selects = append(b.selects, quoteIdent(col.Name))
	return b, nil
}

// ResolveColumn 校验列名并返回列定义。这是所有列名进入 SQL 的唯一入口。
func (b *Builder) ResolveColumn(name string) (schema.Column, error) {
	col, ok := b.table.Column(name)
	if !ok {
		return schema.Column{}, fmt.Errorf("column_not_found: 表 %s 没有列 %q。可用列请调用 describe_ocpx_table（表名 %s）核对",
			b.table.Name, name, b.table.Name)
	}
	return col, nil
}

// ResolveGroupColumn 在 ResolveColumn 之上额外拒绝大字段。
func (b *Builder) ResolveGroupColumn(name string) (schema.Column, error) {
	col, err := b.ResolveColumn(name)
	if err != nil {
		return col, err
	}
	if col.Kind == schema.KindRaw {
		return col, fmt.Errorf("invalid_dimension: 列 %q 是大字段（%s），不能作为聚合维度。可用维度：%s",
			col.Name, col.Type, strings.Join(b.table.GroupableNames(), ", "))
	}
	return col, nil
}

// GroupByColumn 把列同时加入 SELECT 与 GROUP BY。
func (b *Builder) GroupByColumn(name string) error {
	col, err := b.ResolveGroupColumn(name)
	if err != nil {
		return err
	}
	b.selects = append(b.selects, quoteIdent(col.Name))
	b.groups = append(b.groups, quoteIdent(col.Name))
	return nil
}

// GroupByExpr 加入一个内部生成的分组表达式（如时间分桶）。
func (b *Builder) GroupByExpr(expr, alias string) *Builder {
	b.SelectExpr(expr, alias)
	b.groups = append(b.groups, expr)
	return b
}

// Where 追加一个已参数化的条件。
func (b *Builder) Where(cond string, args ...any) *Builder {
	b.wheres = append(b.wheres, cond)
	b.whereArgs = append(b.whereArgs, args...)
	return b
}

// Having 追加一个 HAVING 条件。
func (b *Builder) Having(cond string, args ...any) *Builder {
	b.havings = append(b.havings, cond)
	b.whereArgs = append(b.whereArgs, args...)
	return b
}

// TimeRange 追加时间范围过滤。所有工具都必须调用它——Doris 明细表没有
// 时间过滤等于全表扫。
func (b *Builder) TimeRange(start, end time.Time) *Builder {
	tc := quoteIdent(b.table.TimeColumn)
	return b.Where(fmt.Sprintf("%s >= ? AND %s < ?", tc, tc),
		start.Format(tsLayout), end.Format(tsLayout))
}

// ExcludeTestTraffic 剔除测试流量（test_status != 0）。
func (b *Builder) ExcludeTestTraffic() *Builder {
	if _, ok := b.table.Column("test_status"); ok {
		b.Where("(`test_status` = 0 OR `test_status` IS NULL)")
	}
	return b
}

// ExcludeLoss 剔除丢失记录（is_loss = 1）。
func (b *Builder) ExcludeLoss() *Builder {
	if _, ok := b.table.Column("is_loss"); ok {
		b.Where("(`is_loss` = 0 OR `is_loss` IS NULL)")
	}
	return b
}

// OnlyLoss 只保留丢失记录。
func (b *Builder) OnlyLoss() *Builder {
	if _, ok := b.table.Column("is_loss"); ok {
		b.Where("`is_loss` = 1")
	}
	return b
}

// OrderBy 追加排序。expr 必须是内部生成或已 quote 的标识。
func (b *Builder) OrderBy(expr string, desc bool) *Builder {
	dir := "ASC"
	if desc {
		dir = "DESC"
	}
	b.orders = append(b.orders, expr+" "+dir)
	return b
}

// OrderByAlias 按 select 别名排序。
func (b *Builder) OrderByAlias(alias string, desc bool) *Builder {
	return b.OrderBy(quoteIdent(alias), desc)
}

// Limit 设置行数上限。
func (b *Builder) Limit(n int) *Builder {
	b.limit = n
	return b
}

// Build 生成最终 SQL 与参数。
func (b *Builder) Build() (string, []any, error) {
	if len(b.selects) == 0 {
		return "", nil, fmt.Errorf("internal: SELECT 列表为空")
	}
	if len(b.wheres) == 0 {
		return "", nil, fmt.Errorf("internal: 拒绝生成无 WHERE 条件的查询（会全表扫描）")
	}

	var sb strings.Builder
	sb.WriteString("SELECT ")
	sb.WriteString(strings.Join(b.selects, ", "))
	sb.WriteString("\nFROM ")
	sb.WriteString(b.Qualified())
	sb.WriteString("\nWHERE ")
	sb.WriteString(strings.Join(b.wheres, "\n  AND "))
	if len(b.groups) > 0 {
		sb.WriteString("\nGROUP BY ")
		sb.WriteString(strings.Join(b.groups, ", "))
	}
	if len(b.havings) > 0 {
		sb.WriteString("\nHAVING ")
		sb.WriteString(strings.Join(b.havings, "\n  AND "))
	}
	if len(b.orders) > 0 {
		sb.WriteString("\nORDER BY ")
		sb.WriteString(strings.Join(b.orders, ", "))
	}
	if b.limit > 0 {
		sb.WriteString(fmt.Sprintf("\nLIMIT %d", b.limit))
	}

	return sb.String(), append([]any{}, b.whereArgs...), nil
}

// -----------------------------------------------------------------------------
// 过滤条件
// -----------------------------------------------------------------------------

// Filter 是一条来自模型的过滤条件。
type Filter struct {
	Column   string `json:"column"`
	Operator string `json:"operator"`
	Value    any    `json:"value"`
	Values   []any  `json:"values"`
}

// 允许的操作符白名单。key 是模型可写的形式，value 是 SQL 片段。
var operators = map[string]string{
	"=":           "=",
	"!=":          "!=",
	"<>":          "!=",
	">":           ">",
	">=":          ">=",
	"<":           "<",
	"<=":          "<=",
	"eq":          "=",
	"ne":          "!=",
	"gt":          ">",
	"gte":         ">=",
	"lt":          "<",
	"lte":         "<=",
	"like":        "LIKE",
	"not_like":    "NOT LIKE",
	"in":          "IN",
	"not_in":      "NOT IN",
	"is_null":     "IS NULL",
	"is_not_null": "IS NOT NULL",
}

// OperatorNames 返回支持的操作符列表，用于工具描述与报错。
func OperatorNames() []string {
	seen := map[string]bool{}
	var out []string
	for k := range operators {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// ApplyFilters 把一组过滤条件安全地加到 Builder 上。
func (b *Builder) ApplyFilters(filters []Filter) error {
	for i, f := range filters {
		if err := b.applyFilter(f); err != nil {
			return fmt.Errorf("filters[%d]: %w", i, err)
		}
	}
	return nil
}

func (b *Builder) applyFilter(f Filter) error {
	col, err := b.ResolveColumn(f.Column)
	if err != nil {
		return err
	}
	opKey := strings.ToLower(strings.TrimSpace(f.Operator))
	if opKey == "" {
		opKey = "="
	}
	op, ok := operators[opKey]
	if !ok {
		return fmt.Errorf("invalid_operator: 不支持操作符 %q。支持：%s", f.Operator, strings.Join(OperatorNames(), ", "))
	}
	ident := quoteIdent(col.Name)

	switch op {
	case "IS NULL", "IS NOT NULL":
		b.Where(ident + " " + op)
		return nil

	case "IN", "NOT IN":
		vals := f.Values
		if len(vals) == 0 && f.Value != nil {
			// 允许 value 传数组或逗号分隔串
			switch v := f.Value.(type) {
			case []any:
				vals = v
			case string:
				for _, part := range strings.Split(v, ",") {
					if p := strings.TrimSpace(part); p != "" {
						vals = append(vals, p)
					}
				}
			default:
				vals = []any{v}
			}
		}
		if len(vals) == 0 {
			return fmt.Errorf("missing_value: 操作符 %s 需要通过 values 提供非空取值列表", opKey)
		}
		if len(vals) > 1000 {
			return fmt.Errorf("too_many_values: %s 的取值不能超过 1000 个，当前 %d 个", opKey, len(vals))
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(vals)), ", ")
		b.Where(fmt.Sprintf("%s %s (%s)", ident, op, placeholders), vals...)
		return nil

	default:
		if f.Value == nil {
			return fmt.Errorf("missing_value: 操作符 %s 需要通过 value 提供取值", opKey)
		}
		if op == "LIKE" || op == "NOT LIKE" {
			s, ok := f.Value.(string)
			if !ok {
				return fmt.Errorf("invalid_value: %s 的取值必须是字符串", opKey)
			}
			if !strings.ContainsAny(s, "%_") {
				s = "%" + s + "%" // 未写通配符时按包含匹配
			}
			b.Where(fmt.Sprintf("%s %s ?", ident, op), s)
			return nil
		}
		b.Where(fmt.Sprintf("%s %s ?", ident, op), f.Value)
		return nil
	}
}

// ParseFilters 把工具入参里的 []any 解析成 []Filter。
func ParseFilters(raw any) ([]Filter, error) {
	if raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("invalid_filters: filters 必须是对象数组，形如 [{\"column\":\"product_channel\",\"operator\":\"=\",\"value\":\"abc\"}]")
	}
	var out []Filter
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid_filters: filters[%d] 必须是对象", i)
		}
		f := Filter{}
		if v, ok := m["column"].(string); ok {
			f.Column = v
		} else {
			return nil, fmt.Errorf("invalid_filters: filters[%d] 缺少 column", i)
		}
		if v, ok := m["operator"].(string); ok {
			f.Operator = v
		}
		f.Value = m["value"]
		if vs, ok := m["values"].([]any); ok {
			f.Values = vs
		}
		out = append(out, f)
	}
	return out, nil
}
