# OCPX Doris MCP Server

提供 **表/字段元数据、常用场景 SQL 示例和统一 SQL 查询入口**，让模型根据用户描述生成 SQL 并查询 Doris。

基于 [mark3labs/mcp-go](https://github.com/mark3labs/mcp-go) v0.48.0，Streamable HTTP + Bearer token 传输，协议版本 `2025-06-18`。

> 依赖版本说明：mcp-go **v0.53.0 起把 go 指令提到了 1.25.5**，会强制本地 Go 自动切换工具链。本项目固定在 v0.48.0（只要求 go 1.23），`go.mod` 的 go 指令对齐本地的 1.25.0，编译不触发工具链切换。升级 mcp-go 前先确认本地 Go 版本够。

## 覆盖的表

两条业务线，各三个漏斗环节，共 6 张 DUPLICATE KEY 明细表（定义见 `schema.sql`）：

| 业务线 | 曝光 | 点击 | 转化 |
| --- | --- | --- | --- |
| `v1` 通用 OCPX | `ocpx_v1_imp` | `ocpx_v1_clk` | `ocpx_v1_track` |
| `jd` 京东专用 | `ocpx_jd_imp` | `ocpx_jd_clk` | `ocpx_jd_callback` |

所有表的时间列都是 `req_time`；`unikey` 是监测 ID，`advertiser_id` 是账户，`up_event_name` 是上游转化事件。监测 ID 不代表单条记录唯一。

## 工具

仅注册三个工具，保留原名称和请求参数：

| 工具 | 用途 |
| --- | --- |
| `list_ocpx_tables` | 六张表及业务线、环节；元数据会话内复用 |
| `describe_ocpx_table` | 字段定义、关键字段、查询口径和按表提供的 SQL 示例 |
| `ocpx_run_sql` | 统一执行自定义 SELECT / WITH，返回真实查询结果 |

已知表可直接 describe，再调用 run_sql；已有字段上下文可直接 run_sql。
原 funnel / breakdown / trend / device_lookup / loss_analysis / sample_rows 不再对外注册；源码暂留但不出现在工具列表中。
更新服务后客户端需刷新工具列表或重新连接。

### 常用场景

| 用户说法 | 查询指引 |
| --- | --- |
| 82091968bc 今天有数据吗 / 多少点击和转化 | unikey 字符串过滤，各环节分别统计；无业务线上下文时分开查通用和京东 |
| 今天 track 有多少上游转化 | ocpx_v1_track 按 up_event_name 分组，返回记录数和 action_pv 行为数 |
| 1865615583177352 今天 callback 事件4 | advertiser_id 字符串过滤，up_event_name='4'；可按 err/type 分组 |
| 今天京东订单每小时变化 | callback 的 (up_event_name='4' OR type='scheduled_callback')，按 req_time 小时分桶，补0；当前小时注明未结束 |
| 八月份 callback 事件4关联 clk 看天数差 | req_id 关联，callback.log_time（Unix秒）对 clk.req_time；点击侧先聚合避免数量膨胀，歧义/未匹配单独展示 |

完整指引与示例统一维护在 `internal/tools/guide.go`，同时提供给模型的服务说明和字段工具。
日期按业务时区转换为明确起止时间；默认 Asia/Shanghai 并说明。月份关联需明确点击回溯窗口，不擅自限定点击也在同月。
天数默认自然日差，数据库会话时区须与业务时区一致；与每满24小时的口径区分。
排查查询默认不剔除测试、丢失和错误记录；正式有效统计由用户需求确定 SQL 条件。
京东 callback 中 `type='scheduled_callback'` 表示低活订单转化，即使 `up_event_name` 为 NULL 或空串也纳入。查询低活订单只按该 type 过滤；泛指订单用 `(up_event_name='4' OR type='scheduled_callback')`，避免遗漏低活订单，也避免重复计数。用户明确查询事件4时仍只按 `up_event_name='4'` 过滤。其他 type 取值含义未配置，不猜测。

## 查询校验与限制

`ocpx_run_sql` 使用 `internal/query/validate.go` 的校验：只允许 SELECT/WITH 和六张表，拒绝写操作、多语句；检查 req_time 与 WHERE，缺 LIMIT 时追加。
当前校验是文本级检查，不能证明每张关联表都有正确时间范围，也不强制 SQL 时间跨度上限。模型应显式给每张明细表加起止条件。
执行层统一限制返回行数与超时；truncated 表示返回不完整。SQL 工具不自动补测试/丢失过滤或转化统计口径。

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
  Database: "monitor"     # HTTP模式仅用于元数据展示，查询用裸表名
  LogLevel: "info"
```

网关接口约定：`GET <QueryURL>?sql=<SQL>&key=<Key>`，返回
`{"code":0,"data":[{"列":"值",...}],"errInfo":[]}`。`ocpx_run_sql` 通过统一查询接口执行，不感知具体后端。

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
internal/tools/guide.go      业务词典、查询口径、常用场景 SQL 示例
internal/tools/analytics.go  未注册的旧业务工具实现
internal/tools/diagnostics.go run_sql 与未注册的旧诊断工具实现
deploy.sh / deploy_test.sh   线上 / 内网部署
ocpx_mcp.service             systemd 单元
```

## 错误码

工具返回的错误都带机器可读前缀，便于模型自我纠正：

| 错误码 | 含义与恢复动作 |
| --- | --- |
| `column_not_found` | 列名不存在 → 调 `describe_ocpx_table` 核对 |
| `table_not_found` | 表名不存在 → 调 `list_ocpx_tables` |
| `sql_rejected` | SQL 未通过校验 → 读报错原因改写，不要试图绕过 |
| `missing_time_filter` | 缺 `req_time` 过滤 → 补上 |
| `timeout` | 查询超时 → 收窄窗口或加过滤 |
| `connection_failed` | 连不上 Doris → 检查 host/port |
| `permission_denied` | Doris 账号无权限 |
