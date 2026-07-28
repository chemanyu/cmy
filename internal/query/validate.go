package query

import (
	"fmt"
	"regexp"
	"strings"
)

// ValidateSelect 校验一条模型自撰的 SQL 是否可以安全执行，并在必要时追加 LIMIT。
//
// 设计取向：白名单 + 快速失败。这是给 ocpx_run_sql 兜底用的，业务工具走 Builder
// 完全不经过这里。校验步骤：
//  1. 剥离字符串字面量与注释后再做关键字检查——防止注释/字符串藏写操作
//  2. 必须以 SELECT 或 WITH 开头
//  3. 禁止多语句
//  4. 写操作与 DDL 关键字一律拒绝
//  5. 引用的表必须都在白名单内
//  6. 必须有 req_time 过滤
//  7. 无 LIMIT 时追加
//
// 返回可执行的 SQL。任何一步不通过都返回 sql_rejected 错误。
func ValidateSelect(raw string, allowedTables []string, limit int) (string, error) {
	sqlText := strings.TrimSpace(raw)
	sqlText = strings.TrimSuffix(sqlText, ";")
	sqlText = strings.TrimSpace(sqlText)

	if sqlText == "" {
		return "", fmt.Errorf("sql_rejected: sql 不能为空")
	}
	if len(sqlText) > 20000 {
		return "", fmt.Errorf("sql_rejected: SQL 长度 %d 超过 20000 字符上限", len(sqlText))
	}

	stripped, err := stripLiteralsAndComments(sqlText)
	if err != nil {
		return "", fmt.Errorf("sql_rejected: %w", err)
	}
	// 归一化：统一大写、压缩空白，便于关键字匹配。
	norm := strings.ToUpper(strings.Join(strings.Fields(stripped), " "))

	if strings.Contains(stripped, ";") {
		return "", fmt.Errorf("sql_rejected: 不允许多条语句（检测到分号）。一次只提交一条 SELECT")
	}
	if !strings.HasPrefix(norm, "SELECT ") && !strings.HasPrefix(norm, "SELECT\t") && !strings.HasPrefix(norm, "WITH ") {
		return "", fmt.Errorf("sql_rejected: 只允许 SELECT 或 WITH 开头的只读查询，当前语句开头是 %q", firstWord(norm))
	}

	// 写操作/DDL/权限/会话变更关键字。按词边界匹配，避免误伤 `update_time` 这类列名。
	forbidden := []string{
		"INSERT", "UPDATE", "DELETE", "DROP", "ALTER", "CREATE", "TRUNCATE", "REPLACE",
		"GRANT", "REVOKE", "SET", "USE", "LOAD", "MERGE", "CALL", "EXEC", "EXECUTE",
		"LOCK", "UNLOCK", "KILL", "ADMIN", "INSTALL", "UNINSTALL", "OUTFILE", "DUMPFILE",
		"INTO", "HANDLER", "PREPARE", "DEALLOCATE", "RESET", "FLUSH", "RECOVER", "REFRESH",
		"CANCEL", "PAUSE", "RESUME", "STOP", "BACKUP", "RESTORE",
	}
	for _, kw := range forbidden {
		if wordRe(kw).MatchString(norm) {
			return "", fmt.Errorf("sql_rejected: 检测到禁用关键字 %s。本工具只允许纯读取的 SELECT，"+
				"写操作、DDL、会话变更、文件导出一律不可用（即使写在子查询或 CTE 里）", kw)
		}
	}

	// 表引用白名单。抓取 FROM / JOIN 后面的第一个标识。
	refs := extractTableRefs(stripped)
	if len(refs) == 0 {
		return "", fmt.Errorf("sql_rejected: 没有解析到任何表引用。请显式 FROM 一张 OCPX 表：%s", strings.Join(allowedTables, ", "))
	}
	allowed := map[string]bool{}
	for _, t := range allowedTables {
		allowed[strings.ToLower(t)] = true
	}
	// CTE 名字也算合法引用。
	for _, name := range extractCTENames(stripped) {
		allowed[strings.ToLower(name)] = true
	}
	for _, ref := range refs {
		bare := bareTableName(ref)
		if !allowed[strings.ToLower(bare)] {
			return "", fmt.Errorf("sql_rejected: 不允许访问表 %q。本 MCP 只覆盖这 6 张 OCPX 表：%s",
				ref, strings.Join(allowedTables, ", "))
		}
	}

	// 必须有时间过滤——明细表无时间条件等于全表扫。
	if !wordRe("REQ_TIME").MatchString(norm) {
		return "", fmt.Errorf("missing_time_filter: SQL 里必须包含 req_time 的范围过滤（例如 " +
			"req_time >= '2026-07-27 00:00:00' AND req_time < '2026-07-28 00:00:00'）。" +
			"OCPX 明细表按 req_time 排序，缺少该过滤会全表扫描")
	}
	if !wordRe("WHERE").MatchString(norm) {
		return "", fmt.Errorf("missing_time_filter: SQL 缺少 WHERE 子句。请至少加上 req_time 范围过滤")
	}

	// 追加 LIMIT。已有 LIMIT 时不改写——驱动侧还有 MaxRows 兜底截断。
	if !wordRe("LIMIT").MatchString(norm) {
		sqlText = fmt.Sprintf("%s\nLIMIT %d", sqlText, limit)
	}
	return sqlText, nil
}

// wordRe 缓存按词边界匹配的正则。
var wordReCache = map[string]*regexp.Regexp{}

func wordRe(word string) *regexp.Regexp {
	if re, ok := wordReCache[word]; ok {
		return re
	}
	// 标识符字符集包含下划线，所以用 [^A-Z0-9_] 做边界而不是 \b。
	re := regexp.MustCompile(`(^|[^A-Z0-9_])` + regexp.QuoteMeta(word) + `($|[^A-Z0-9_])`)
	wordReCache[word] = re
	return re
}

// stripLiteralsAndComments 把字符串字面量与注释替换成占位符。
//
// 关键字检查必须在剥离之后做，否则 `WHERE err = 'drop table x'` 会被误杀，
// 而 `SELECT 1 /* */ ; DROP ...` 又会被漏过。
func stripLiteralsAndComments(s string) (string, error) {
	var out strings.Builder
	runes := []rune(s)
	n := len(runes)

	for i := 0; i < n; i++ {
		c := runes[i]
		switch {
		// 单/双引号字符串
		case c == '\'' || c == '"':
			quote := c
			out.WriteString(" '' ") // 占位，保留分词
			i++
			closed := false
			for i < n {
				if runes[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if runes[i] == quote {
					// SQL 里连续两个引号表示转义
					if i+1 < n && runes[i+1] == quote {
						i += 2
						continue
					}
					closed = true
					break
				}
				i++
			}
			if !closed {
				return "", fmt.Errorf("字符串字面量没有闭合引号 %c", quote)
			}

		// 反引号标识符：保留内容，它是表名/列名，白名单校验要用
		case c == '`':
			out.WriteRune('`')
			i++
			closed := false
			for i < n {
				if runes[i] == '`' {
					closed = true
					break
				}
				out.WriteRune(runes[i])
				i++
			}
			if !closed {
				return "", fmt.Errorf("反引号标识符没有闭合")
			}
			out.WriteRune('`')

		// -- 行注释
		case c == '-' && i+1 < n && runes[i+1] == '-':
			for i < n && runes[i] != '\n' {
				i++
			}
			out.WriteRune(' ')

		// # 行注释
		case c == '#':
			for i < n && runes[i] != '\n' {
				i++
			}
			out.WriteRune(' ')

		// /* */ 块注释
		case c == '/' && i+1 < n && runes[i+1] == '*':
			i += 2
			closed := false
			for i+1 < n {
				if runes[i] == '*' && runes[i+1] == '/' {
					i++
					closed = true
					break
				}
				i++
			}
			if !closed {
				return "", fmt.Errorf("块注释 /* 没有闭合")
			}
			out.WriteRune(' ')

		default:
			out.WriteRune(c)
		}
	}
	return out.String(), nil
}

// tableRefRe 匹配 FROM / JOIN 后紧跟的表标识（可能带库名前缀与反引号）。
var tableRefRe = regexp.MustCompile("(?i)(?:\\bFROM\\b|\\bJOIN\\b)\\s+([`\\w.]+)")

// extractTableRefs 提取全部表引用。子查询后的 `FROM (` 不会匹配到标识，自然跳过。
func extractTableRefs(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range tableRefRe.FindAllStringSubmatch(s, -1) {
		ref := strings.TrimSpace(m[1])
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out
}

// cteRe 匹配 WITH 子句里定义的名字。
var cteRe = regexp.MustCompile("(?i)(?:\\bWITH\\b|,)\\s*([`\\w]+)\\s+AS\\s*\\(")

func extractCTENames(s string) []string {
	var out []string
	for _, m := range cteRe.FindAllStringSubmatch(s, -1) {
		out = append(out, strings.Trim(strings.TrimSpace(m[1]), "`"))
	}
	return out
}

// bareTableName 去掉库名前缀与反引号，返回裸表名。
func bareTableName(ref string) string {
	ref = strings.ReplaceAll(ref, "`", "")
	if i := strings.LastIndex(ref, "."); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

func firstWord(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
