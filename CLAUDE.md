# CLAUDE.md — ocpx_mcp

OCPX Doris MCP Server：把 Doris 上 6 张 OCPX 广告归因明细表提供表/字段元数据、查询指引和统一 SQL 查询工具，Streamable HTTP + Bearer token 对外提供。
使用说明看 `README.md`，本文件只记「改代码前必读」的约束与踩坑，避免重复犯。

## 架构速览（改哪找哪）

- `main.go` — HTTP 服务、Bearer 鉴权、优雅退出、按配置选后端
- `internal/config/` — 配置结构 + 校验；`default` 标签是各项默认值的唯一事实来源
- `internal/schema/catalog.go` — 6 张表的内置元数据目录，**列白名单的唯一事实来源**
- `internal/query/` — `builder.go` 参数化 SQL 构造 / `validate.go` run_sql 白名单校验 / `timewindow.go` 时间窗口
- `internal/doris/` — 两个后端实现同一个 `Querier` 接口：`client.go`(直连) / `http.go`(HTTP 网关)
- `internal/tools/` — 3 个对外 MCP 工具（list/describe/run_sql，旧业务实现未注册），全部经 `Deps.DB`(即 `Querier`)查询，不感知后端

## 两层安全模型（改查询逻辑前必读）

业务工具与 `ocpx_run_sql` 走**两条不同**的防护路径，别混淆：

1. **业务工具走 Builder**：表名/列名只能来自 `internal/schema` 内置目录，模型传入的字符串
   仅用于查表，查不到直接报 `column_not_found`，绝不拼接原文；字面量走 `?` 占位符。
   这层不需要 SQL 黑名单。
2. **`ocpx_run_sql` 走白名单校验**(`validate.go`)：先剥字符串字面量与注释再查关键字；
   必须 SELECT/WITH 开头；拒写操作/DDL/多语句；只能引用那 6 张表；必须带 `req_time` 过滤。

改任何一层，对应改它自己的防护，不要指望另一层兜底。

## 两种后端 + 关键约束（踩过的坑）

配了 `Doris.QueryURL` 走 HTTP 网关，否则 MySQL 直连。二者实现同一个 `doris.Querier` 接口，
工具层零感知。改后端相关代码前，这几条是实测踩出来的，别回退：

1. **HTTP 网关模式下表名不能带库前缀**
   - 那个网关只认裸表名(`ocpx_v1_clk`)，带库前缀(`monitor.ocpx_v1_clk`)会被它的表白名单
     拒掉——它把库名当成表名去匹配，报「表不在白名单内：monitor」。
   - 因此 `Config.TableQualifier()` 在 HTTP 模式返回空串，builder 生成裸表名；`Database`
     字段此时只用于日志展示，不进 SQL。直连模式才返回库名。
   - 位置：`internal/config/config.go` 的 `TableQualifier()`，工具层用它 new builder。

2. **HTTP 网关的响应：先看 code，再碰 data**
   - 成功 `{"code":0,"data":[{...}]}`，data 是**数组**；失败 `{"code":1,"data":"","errInfo":{...}}`，
     data 是**空字符串**。所以 `gatewayResp.Data` 声明成 `json.RawMessage`(不是 `[]`)，
     `code!=0` 直接读 errInfo，`code==0` 才把 data 解析成数组。若把 Data 声明成数组，
     错误响应会整体反序列化失败，把真正的报错吞成「不是预期 JSON」。
   - 位置：`internal/doris/http.go` 的 `do()`。

3. **HTTP 网关不支持 `?` 占位符**
   - 直连由 MySQL 驱动处理占位符；网关只能把参数插值回 SQL 再发。插值 + MySQL 转义在
     `internal/doris/interpolate.go`，注入面收敛在这个文件。改这里要保证转义正确。

4. **JSON 对象数组丢列序**
   - 网关返回 `[{列:值}]`，JSON key 无序。列顺序从 SQL 的 SELECT 子句解析还原；
     `SELECT *` 等解析不出时退回按列名排序(稳定)。位置：`http.go` 的 `resolveColumns()`。

## 配置

`etc/config.yaml`，go-zero `conf.MustLoad` 加载，`-f` 指定路径(默认 `etc/config.yaml`)。
默认值全在 `config.go` 的 `json:",default=..."` 标签里。直连模式必填 `Doris.Host`/`User`；
HTTP 模式必填 `Doris.QueryURL`(此时 Host/User 可空)。

**`etc/config.yaml` 含真实密码与 token**：`.gitignore` 里忽略它的那行默认是注释状态。
若不想让凭据入库，取消注释 `# etc/config.yaml`。部署脚本首次上传后不覆盖远端那份。

## 日志

全打到标准输出/错误，由 systemd 的 `append:` 落到文件（不自己管文件句柄）。两类内容用前缀分：
`[ocpx-mcp]` 应用日志、`[sql]` 每条实际 SQL（语句/参数/耗时/行数）。
`Doris.LogLevel` 控级别；慢查询阈值 2s（OLAP 聚合，几百毫秒正常，别调太低把每条都标慢）。
`append:` 无轮转，只增不减，量大时把 LogLevel 调 `warn`。

## 部署运维踩坑

- **`Restart=always` 会无限重启**：配置类错误(端口冲突、凭据错)会让服务崩溃→5 秒重启→再崩，
  日志每 5 秒刷一遍同样的块。看到这种循环，先查第一条 `Fatalf` 的真实原因，别只盯着重启。
  端口冲突多半是上一版进程没退干净——`lsof -iTCP:9990 -sTCP:LISTEN` 找占用者。
- **service 用文件名匹配**：早期若部署过 `ocpx-mcp`(连字符)版本，和现在的 `ocpx_mcp`(下划线)
  是两个不同 unit，会抢同一个端口。换名时先 `systemctl disable --now` 旧的。

## 开发

```bash
go test ./...      # 含 schema.sql 与内置目录一致性校验、HTTP 后端解析/插值/列序
go vet ./... && gofmt -l .
```

`internal/schema/catalog_test.go` 用 `schema.sql` 反向校验内置目录的列名/顺序/类型/DUPLICATE KEY。
**改了建表语句就同步改 `catalog.go`**，否则测试失败。
