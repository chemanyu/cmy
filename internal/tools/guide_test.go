package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/chemanyu/mcp/internal/config"
	"github.com/chemanyu/mcp/internal/doris"
	"github.com/chemanyu/mcp/internal/query"
	"github.com/chemanyu/mcp/internal/schema"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type recordingDB struct {
	doris.Querier
	sql string
}

func (d *recordingDB) Query(_ context.Context, sql string, _ ...any) (*doris.Result, error) {
	d.sql = sql
	return &doris.Result{Columns: []string{"record_count"}, Rows: [][]any{{int64(7)}}, RowCount: 1}, nil
}

func TestPublicToolsAndSQLExecution(t *testing.T) {
	db := &recordingDB{}
	cfg := &config.Config{}
	cfg.Query.MaxRows = 100
	s := server.NewMCPServer("test", "1")
	Register(s, &Deps{DB: db, Cfg: cfg})
	if len(s.ListTools()) != 3 {
		t.Fatalf("public tools: %v", s.ListTools())
	}
	for _, name := range []string{"list_ocpx_tables", "describe_ocpx_table", "ocpx_run_sql"} {
		if s.GetTool(name) == nil {
			t.Fatalf("missing %s", name)
		}
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = "ocpx_run_sql"
	req.Params.Arguments = map[string]any{"sql": "SELECT COUNT(*) AS record_count FROM ocpx_v1_clk WHERE req_time >= '2026-09-14 00:00:00' AND req_time < '2026-09-15 00:00:00' AND unikey = '82091968bc'", "limit": 20}
	res, err := s.GetTool("ocpx_run_sql").Handler(context.Background(), req)
	if err != nil || res.IsError || !strings.HasSuffix(db.sql, "LIMIT 20") {
		t.Fatalf("execution: %v, %v, %s", res, err, db.sql)
	}
	// Metadata remains usable without a database and carries both structured and text guidance.
	db.sql = ""
	req.Params.Name = "describe_ocpx_table"
	req.Params.Arguments = map[string]any{"table": "ocpx_jd_callback"}
	res, err = s.GetTool("describe_ocpx_table").Handler(context.Background(), req)
	if err != nil || res.IsError || db.sql != "" {
		t.Fatalf("metadata: %v, %v", res, err)
	}
	payload, ok := res.StructuredContent.(map[string]any)
	if !ok || len(payload["query_examples"].([]queryExample)) < 4 {
		t.Fatalf("missing examples: %v", res)
	}
}

func TestScenarioSQLPassesValidator(t *testing.T) {
	for _, name := range schema.TableNames() {
		for _, ex := range examplesFor(name) {
			t.Run(name+"/"+ex.Scenario, func(t *testing.T) {
				if _, err := query.ValidateSelect(ex.SQL, schema.TableNames(), 100); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
