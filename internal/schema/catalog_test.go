package schema

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestCatalogMatchesDDL 用 schema.sql 反向校验内置目录。
//
// 目录是手写的，DDL 是事实来源。这个测试保证两者不会漂移——改了建表语句
// 但忘了同步目录时，这里会直接失败。
func TestCatalogMatchesDDL(t *testing.T) {
	raw, err := os.ReadFile("../../schema.sql")
	if err != nil {
		t.Skipf("找不到 schema.sql，跳过 DDL 一致性校验: %v", err)
	}

	ddl := parseDDL(string(raw))
	if len(ddl) == 0 {
		t.Fatal("没有从 schema.sql 解析出任何建表语句")
	}
	if len(ddl) != len(tableOrder) {
		t.Errorf("表数量不一致: schema.sql %d 张，目录 %d 张", len(ddl), len(tableOrder))
	}

	for name, want := range ddl {
		tbl, err := Get(name)
		if err != nil {
			t.Errorf("schema.sql 里的表 %s 不在目录中: %v", name, err)
			continue
		}

		// 列名与顺序
		var got []string
		for _, c := range tbl.Columns {
			got = append(got, c.Name)
		}
		if strings.Join(got, ",") != strings.Join(want.cols, ",") {
			t.Errorf("%s 列不一致:\n  DDL:  %v\n  目录: %v", name, want.cols, got)
		}

		// 列类型
		for _, c := range tbl.Columns {
			wantType, ok := want.types[c.Name]
			if !ok {
				continue
			}
			if !strings.EqualFold(c.Type, wantType) {
				t.Errorf("%s.%s 类型不一致: 目录 %q，DDL %q", name, c.Name, c.Type, wantType)
			}
		}

		// DUPLICATE KEY
		if strings.Join(tbl.DupKeys, ",") != strings.Join(want.dupKeys, ",") {
			t.Errorf("%s DUPLICATE KEY 不一致:\n  DDL:  %v\n  目录: %v", name, want.dupKeys, tbl.DupKeys)
		}
	}
}

type ddlTable struct {
	cols    []string
	types   map[string]string
	dupKeys []string
}

var (
	createRe = regexp.MustCompile("(?s)CREATE TABLE `(\\w+)` \\((.*?)\\n\\) DUPLICATE KEY\\((.*?)\\);")
	colRe    = regexp.MustCompile("^\\s+`(\\w+)`\\s+([a-zA-Z]+(?:\\(\\d+\\))?)")
	identRe  = regexp.MustCompile("`(\\w+)`")
)

func parseDDL(src string) map[string]ddlTable {
	out := map[string]ddlTable{}
	for _, m := range createRe.FindAllStringSubmatch(src, -1) {
		name, body, dup := m[1], m[2], m[3]
		tbl := ddlTable{types: map[string]string{}}
		for _, line := range strings.Split(body, "\n") {
			cm := colRe.FindStringSubmatch(line)
			if cm == nil {
				continue
			}
			tbl.cols = append(tbl.cols, cm[1])
			tbl.types[cm[1]] = strings.ToLower(cm[2])
		}
		for _, dm := range identRe.FindAllStringSubmatch(dup, -1) {
			tbl.dupKeys = append(tbl.dupKeys, dm[1])
		}
		out[name] = tbl
	}
	return out
}

// 每一列都必须归到一个已知 Kind，且 raw 类型不得出现在可分组列里。
func TestColumnKindsAreSane(t *testing.T) {
	valid := map[Kind]bool{
		KindTime: true, KindDim: true, KindDeviceID: true,
		KindMetric: true, KindDiag: true, KindRaw: true,
	}
	for _, tbl := range All() {
		for _, c := range tbl.Columns {
			if !valid[c.Kind] {
				t.Errorf("%s.%s 的 Kind %q 非法", tbl.Name, c.Name, c.Kind)
			}
			if c.Desc == "" {
				t.Errorf("%s.%s 缺少业务描述", tbl.Name, c.Name)
			}
		}
		for _, g := range tbl.GroupableNames() {
			col, _ := tbl.Column(g)
			if col.Kind == KindRaw {
				t.Errorf("%s.%s 是大字段，不应出现在可分组列里", tbl.Name, g)
			}
		}
	}
}
