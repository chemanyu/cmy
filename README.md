# OCPX Doris MCP Server

把 Doris 上的 OCPX 广告归因明细表包装成 **MCP 业务语义工具**，让模型不写 SQL 就能做漏斗、下钻、趋势、设备排查与丢失分析。

基于 [mark3labs/mcp-go](https://github.com/mark3labs/mcp-go) v0.48.0，Streamable HTTP + Bearer token 传输，协议版本 `2025-06-18`。

> 依赖版本说明：mcp-go **v0.53.0 起把 go 指令提到了 1.25.5**，会强制本地 Go 自动切换工具链。本项目固定在 v0.48.0（只要求 go 1.23），`go.mod` 的 go 指令对齐本地的 1.25.0，编译不触发工具链切换。升级 mcp-go 前先确认本地 Go 版本够。

## 覆盖的表

两条业务线，各三个漏斗环节，共 6 张 DUPLICATE KEY 明细表（定义见 `schema.sql`）：

| 业务线 | 曝光 | 点击 | 转化 |
| --- | --- | --- | --- |
| `v1` 通用 OCPX | `ocpx_v1_imp` | `ocpx_v1_clk` | `ocpx_v1_track` |
| `jd` 京东专用 | `ocpx_jd_imp` | `ocpx_jd_clk` | `ocpx_jd_callback` |

所有表的时间列都是 `req_time`；`unikey` 是曝光→点击→转化的共用归因键。

## 工具

| 工具 | 用途 |
| --- | --- |
| `list_ocpx_tables` | 拿表地图，第一步 |
| `describe_ocpx_table` | 某张表的完整列定义、类型、业务含义、用途分类 |
| `ocpx_funnel` | 一次算出曝光→点击→转化漏斗，含 CTR / CVR / 曝光转化率 |
| `ocpx_breakdown` | 单表按任意维度聚合 Top N |
| `ocpx_trend` | 按 minute / hour / day 输出时间序列 |
| `ocpx_device_lookup` | 按设备号 / unikey / IP 反查三张表的全链路记录 |
| `ocpx_loss_analysis` | 丢失率、报错率、错误原因 Top N、按 servername 定位单机故障 |
| `ocpx_sample_rows` | 取少量明细看数据长什么样 |
| `ocpx_run_sql` | 兜底：执行自定义 SELECT（受严格校验） |

### 内置统一口径

业务工具默认已按对外汇报口径处理，模型无需自己拼条件：

- 剔除 `test_status != 0`（测试流量）与 `is_loss = 1`（丢失记录），可用 `include_test` / `include_loss` 打开
- 转化量对 `action_pv` 求和，而不是 `count(*)`——一行可能代表多次行为
- 比率的分母为 0 时返回 `null` 而不是 `0`，避免把"无数据"误读成"转化率 0%"

## 安全模型

分两层，互不依赖：

**业务工具走 Builder（`internal/query/builder.go`）**
- 标识符（表名、列名）只能来自 `internal/schema` 的内置目录：模型传入的字符串仅用于查表，查不到直接报 `column_not_found`，绝不拼接原文
- 字面量（过滤值、时间）全部走 `?` 占位符交给驱动转义
- 因此这一层不需要任何 SQL 黑名单

**`ocpx_run_sql` 走白名单校验（`internal/query/validate.go`）**
- 先剥离字符串字面量与注释，再做关键字检查——所以 `WHERE err = 'drop table x'` 不会被误杀，而 `SELECT 1 /* */ ; DROP ...` 也不会被漏过
- 必须 `SELECT` / `WITH` 开头；写操作、DDL、权限、会话变更、文件导出关键字一律拒绝（含子查询与 CTE 内部）
- 拒绝多语句；只能引用这 6 张表（CTE 名字除外）
- 必须包含 `req_time` 过滤，否则拒绝执行
- 无 `LIMIT` 时自动追加

**共同的性能闸门**：时间窗口跨度上限（默认 31 天）、时间桶数量上限（5000）、返回行数上限（默认 2000）、单查询超时（默认 120s）。

## 配置

配置文件 `etc/config.yaml`，用 `-f` 指定路径（和 adt-go 下其他项目一致，go-zero `conf.MustLoad` 加载）。

```yaml
Name: ocpx-mcp
Host: 0.0.0.0
Port: 9990

MCP:
  EndpointPath: "/mcp"
  AuthToken: ""       # 留空则不鉴权，生产必须填

Doris:
  Host: "172.16.3.23"
  Port: 9030          # FE 的 MySQL 协议端口，不是 8030
  User: "root"
  Password: ""
  Database: "ocpx"
  Charset: "utf8mb4"
  MaxIdleConns: 4
  MaxOpenConns: 16
  ConnMaxLifetime: 3600
  LogLevel: "info"    # SQL 日志级别: silent / error / warn / info

Query:
  Timeout: 120        # 单条查询超时（秒）
  MaxRows: 2000       # 单次返回行数上限
  MaxWindowDays: 31   # 时间窗口跨度上限（天）
```

直连模式必填 `Doris.Host` 与 `Doris.User`，其余都有默认值（见 `internal/config/config.go` 的 `default` 标签）。
建议 Doris 账号只给 SELECT 权限。

### 两种后端

Doris 有两种访问方式，二选一，由配置自动切换：

| | 直连（默认） | HTTP 网关 |
| --- | --- | --- |
| 触发条件 | 不配 `QueryURL` | 配了 `Doris.QueryURL` |
| 通道 | MySQL 协议连 FE:9030 | GET 转发 SELECT 的 HTTP 接口 |
| 必填 | `Host`/`User`/`Password` | `QueryURL`（+ `QueryKey`） |
| 用途 | 有 Doris 账号时的常规方式 | 拿不到直连凭据、只有一个查询接口时的过渡方案 |

HTTP 网关模式的配置：

```yaml
Doris:
  QueryURL: "https://rta.zhltech.net/index.php?r=tool/ocpx-query/query"
  QueryKey: "xxxxxxxx"
  Database: "monitor"     # 仍用于 SQL 里的表名限定
  LogLevel: "info"
```

网关接口约定：`GET <QueryURL>?sql=<SQL>&key=<Key>`，返回
`{"code":0,"data":[{"列":"值",...}],"errInfo":[]}`。上层 6 个业务工具与
`ocpx_run_sql` 完全不感知走的是哪种后端。

两处与直连的固有差异，代码已补偿（见 `internal/doris/http.go`）：

- **网关不支持 `?` 占位符**：客户端先把参数插值回 SQL（字符串做 MySQL 转义防注入）再发。
- **JSON 对象数组丢列序**：从 SQL 的 SELECT 子句解析列顺序还原；`SELECT *` 等解析不出时退回按列名排序（稳定）。

**表名不带库前缀**：这个网关只认裸表名（`ocpx_v1_clk`），带库前缀（`monitor.ocpx_v1_clk`）
会被它的表白名单拒掉——它会把库名当成表名去匹配。所以网关模式下 `Config.TableQualifier()`
返回空，builder 生成裸表名；`Database` 字段此时只用于日志展示，不进 SQL。直连模式不受影响。

局限：网关把所有值当字符串返回，客户端按内容判型（纯数字→整数/浮点，其余留字符串），
与直连的行为一致；但超时、慢查询判定只能基于 HTTP 往返，不是 Doris 侧真实执行时间。

## 日志

只有一个去处：全部打到标准输出/错误，由 systemd 的 `append:` 落到

```
/data/log/go/adt-go/ocpx_mcp/ocpx_mcp.output.log
/data/log/go/adt-go/ocpx_mcp/ocpx_mcp.error.log
```

里面混着两类内容，用前缀区分：

| 前缀 | 内容 |
| --- | --- |
| `[ocpx-mcp]` | 启动信息、Doris 探活、鉴权警告、每个 HTTP 请求的耗时 |
| `[sql]` | 每条实际执行的 SQL、占位符参数、耗时、返回行数 |

服务本身不写日志文件，也就没有目录不存在、权限不足、句柄泄漏这些问题。

SQL 日志格式（多行 SQL 会压成一行，便于 grep）：

```
[sql] 2026/07/28 17:07:49 [OK] 13ms | rows=0 | SELECT `req_time`, ... FROM `monitor`.`ocpx_v1_imp` WHERE `req_time` >= ? AND `req_time` < ? ORDER BY `req_time` DESC LIMIT 1 | args=["2026-06-28 17:07:49", "2026-07-28 17:07:49"]
[sql] 2026/07/28 17:07:50 [SLOW>=2s] 3.4s | rows=120 | SELECT ... | args=[...]
[sql] 2026/07/28 17:07:51 [ERROR] 1ms | rows=0 | SELECT ... | args=[...] | err=...
```

只看 SQL：`grep '\[sql\]' ocpx_mcp.output.log`；只看慢查询与报错：`grep -E 'SLOW|ERROR'`。

`Doris.LogLevel` 控制记录范围：`info` 全记 / `warn` 只记慢查询与失败 / `error` 只记失败 / `silent` 不记。
慢查询阈值 2s——这是 OLAP 聚合查询，几百毫秒属正常，用 alsc 那边的 200ms 会把每条都标成慢查询。

**注意**：`append:` 没有轮转，output.log 只增不减（其他项目也一样，`media_monitor/logs/sql.log`
已经 60MB+）。量大时把 `LogLevel` 调成 `warn` 只留慢查询与失败。

## 运行

```bash
go run . -f etc/config.yaml     # -f 默认就是 etc/config.yaml，可省略
```

启动后：
- MCP 端点 `http://<host>:9990/mcp`
- 健康检查 `http://<host>:9990/healthz`（会真实 ping Doris）

Doris 连不上时服务仍会启动（Doris 可能晚于本服务就绪），真正的错误在工具调用时以 `connection_failed` 返回。

## 接入客户端

Claude Code：

```bash
claude mcp add --transport http ocpx http://<host>:9990/mcp \
  --header "Authorization: Bearer openid.xxxxx"
```

`.mcp.json` 写法：

```json
{
  "mcpServers": {
    "ocpx": {
      "type": "http",
      "url": "http://<host>:9990/mcp",
      "headers": { "Authorization": "Bearer openid.xxxxx" }
    }
  }
}
```

## 开发

```bash
go test ./...      # 含 schema.sql 与内置目录的一致性校验
go vet ./...
gofmt -l .
```

`internal/schema/catalog_test.go` 会用 `schema.sql` 反向校验内置目录的列名、顺序、类型与 DUPLICATE KEY。**改了建表语句就同步改 `catalog.go`**，否则这个测试会失败。

## 部署

和 adt-go 下其他项目同一套路：交叉编译 → scp → systemctl 重启。

```bash
./deploy.sh         # 线上 root@adx-s12.ms:/usr/local/adt-go/ocpx_mcp
./deploy_test.sh    # 内网 root@172.16.3.34:/data/ocpx-mcp
```

配置文件是 `etc/config.yaml`。首次部署会上传一份「密码留空」的模板后停下——
凭据不经过部署脚本的命令行与日志：

```bash
ssh root@adx-s12.ms vi /usr/local/adt-go/ocpx_mcp/etc/config.yaml   # 填 Doris.Password 与 MCP.AuthToken
ssh root@adx-s12.ms systemctl restart ocpx_mcp
ssh root@adx-s12.ms 'curl -s localhost:9990/healthz'                # 返回 ok 表示 Doris 也通了
```

之后每次上线就只是 `./deploy.sh`，**远端 `etc/config.yaml` 不会被覆盖**。

日志按公司约定落 `/data/log/go/adt-go/ocpx_mcp/`（脚本会建目录，`append:` 的目录不存在服务起不来）：

```bash
ssh root@adx-s12.ms tail -f /data/log/go/adt-go/ocpx_mcp/ocpx_mcp.output.log          # 应用日志 + SQL
ssh root@adx-s12.ms "grep '\[sql\]' /data/log/go/adt-go/ocpx_mcp/ocpx_mcp.output.log" # 只看 SQL
ssh root@adx-s12.ms systemctl status ocpx_mcp
```

## 目录结构

```
main.go                      HTTP 服务、Bearer 鉴权、优雅退出
etc/config.yaml              配置文件（-f 指定）
internal/config/             配置结构与校验
internal/schema/catalog.go   6 张表的内置元数据目录（列白名单的唯一事实来源）
internal/doris/client.go     直连后端（MySQL 协议）、结果归一化、错误码翻译
internal/doris/http.go       HTTP 网关后端、列序还原
internal/doris/interpolate.go 占位符插值（HTTP 模式用）与值转义
internal/doris/sqllog.go     SQL 日志（语句/参数/耗时/行数，打到标准输出）
internal/query/builder.go    参数化 SQL 构造器
internal/query/timewindow.go 时间窗口解析与分桶
internal/query/validate.go   run_sql 的白名单校验
internal/tools/tools.go      工具注册、元数据工具、共享入参
internal/tools/analytics.go  funnel / breakdown / trend
internal/tools/diagnostics.go device_lookup / loss_analysis / sample_rows / run_sql
deploy.sh / deploy_test.sh   线上 / 内网部署
ocpx_mcp.service             systemd 单元
```

## 错误码

工具返回的错误都带机器可读前缀，便于模型自我纠正：

| 错误码 | 含义与恢复动作 |
| --- | --- |
| `column_not_found` | 列名不存在 → 调 `describe_ocpx_table` 核对 |
| `table_not_found` | 表名不存在 → 调 `list_ocpx_tables` |
| `invalid_dimension` | 用大字段做维度 → 换 `groupable_columns` 里的列 |
| `invalid_metric` | 用非数值列求和 → 换 `metric_columns` 里的列 |
| `invalid_time` | 时间格式错 → 用 `2006-01-02 15:04:05` / `-2d` |
| `window_too_large` | 窗口超上限 → 收窄 start/end |
| `too_many_buckets` | 分桶过多 → 换更粗粒度 |
| `sql_rejected` | SQL 未通过校验 → 读报错原因改写，不要试图绕过 |
| `missing_time_filter` | 缺 `req_time` 过滤 → 补上 |
| `timeout` | 查询超时 → 收窄窗口或加过滤 |
| `connection_failed` | 连不上 Doris → 检查 host/port |
| `permission_denied` | Doris 账号无权限 |
