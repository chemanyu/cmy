package doris

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// interpolate 把 ? 占位符按顺序替换成转义后的字面量。
//
// 直连时占位符由 MySQL 驱动处理；HTTP 网关不支持占位符，只能在客户端把
// 参数拼进 SQL。这里对字符串做 MySQL 风格转义（单引号 + 反斜杠），把注入面
// 收敛到本函数——上层传进来的 args 都是 query 包校验过的过滤值/时间/数字。
//
// 只替换字符串字面量外的 ?。SQL 里若出现引号内的 ?（本项目不会），不动它。
func interpolate(query string, args []any) (string, error) {
	if len(args) == 0 {
		return query, nil
	}
	var b strings.Builder
	argi := 0
	inSingle := false
	inDouble := false
	for i := 0; i < len(query); i++ {
		ch := query[i]
		switch ch {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
			b.WriteByte(ch)
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
			b.WriteByte(ch)
		case '?':
			if inSingle || inDouble {
				b.WriteByte(ch)
				continue
			}
			if argi >= len(args) {
				return "", fmt.Errorf("占位符比参数多")
			}
			lit, err := literal(args[argi])
			if err != nil {
				return "", err
			}
			b.WriteString(lit)
			argi++
		default:
			b.WriteByte(ch)
		}
	}
	if argi != len(args) {
		return "", fmt.Errorf("参数比占位符多：给了 %d 个，用了 %d 个", len(args), argi)
	}
	return b.String(), nil
}

// literal 把一个 Go 值渲染成 SQL 字面量。
func literal(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "NULL", nil
	case string:
		return quoteString(x), nil
	case time.Time:
		return quoteString(x.Format("2006-01-02 15:04:05")), nil
	case bool:
		if x {
			return "1", nil
		}
		return "0", nil
	case int:
		return strconv.FormatInt(int64(x), 10), nil
	case int32:
		return strconv.FormatInt(int64(x), 10), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case uint:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint32:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint64:
		return strconv.FormatUint(x, 10), nil
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32), nil
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	default:
		// 兜底：当字符串处理，避免 %v 出意外格式。
		return quoteString(fmt.Sprintf("%v", x)), nil
	}
}

// quoteString 做 MySQL 单引号字符串转义。
func quoteString(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			b.WriteString("''") // 单引号双写
		case '\\':
			b.WriteString("\\\\")
		case 0:
			b.WriteString("\\0")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case 0x1a:
			b.WriteString("\\Z")
		default:
			b.WriteByte(s[i])
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// normalizeJSON 把网关 JSON 解出的值转成与直连一致的 Go 值。
//
// 网关把所有列都当字符串返回（"63" 而非 63），因此字符串走 normalize 判型，
// 与直连的 RawBytes 判型保持一致；json.Number/bool 直接映射。
func normalizeJSON(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return normalize([]byte(x))
	case float64:
		// encoding/json 默认把数字解成 float64；整数值还原成 int64。
		if x == float64(int64(x)) {
			return int64(x)
		}
		return x
	case bool:
		return x
	default:
		return x
	}
}

// sortStrings 就地排序，便于列序在无法解析 SELECT 时稳定。
func sortStrings(s []string) { sort.Strings(s) }
