# Agent HTTP Go

这是本地 Agent HTTP 服务的 Go 版本实现。

服务会把本机已经安装并完成认证的 agent CLI 包装成同步 HTTP API。当前支持执行：

- `codex`：通过 `codex exec` 调用。
- `claude`：通过 `claude --bare -p --output-format json` 调用。

## 运行要求

- Go 1.25。
- macOS 或 Linux。
- 已安装 `codex` CLI，并且服务进程的 `PATH` 中可以找到它。
- 如需调用 Claude，需要安装 `claude` CLI，并且服务进程的 `PATH` 中可以找到它。

## 主要依赖

- Go 版本：`1.25.0`。
- 配置解析：`gopkg.in/yaml.v3`。
- 持久化会话数据库：SQLite 或 MySQL。
- ORM：`gorm.io/gorm`。
- GORM 数据库驱动：`gorm.io/driver/sqlite`、`gorm.io/driver/mysql`。
- MySQL DSN 解析：`github.com/go-sql-driver/mysql`。

## 启动服务

```sh
go run ./cmd/agent-http-go
```

默认监听地址是：

```text
http://127.0.0.1:8787
```

## 配置文件

服务默认读取当前目录下的 `config.yaml`。如果文件不存在，会使用默认配置继续启动。

示例：

```yaml
server:
  host: 127.0.0.1
  port: "8787"
  shutdownTimeout: 10s
  maxBodySize: 1MiB
  logRoutes: false
  swagger:
    enabled: false
  examples:
    enabled: false

runner:
  timeout: 10m
  codex:
    command: codex
    approvalPolicy: never
    sandbox: workspace-write
    ephemeral: true
  claude:
    command: claude

workspace:
  root: "."

log:
  level: info
  format: text

session:
  enabled: true
  driver: sqlite
  maxTurns: 20
  maxHistorySize: 64KiB
  sqlite:
    path: ./data/agent-http.db
  mysql:
    dsn: ""
```

如果需要加载其它配置文件，可以使用 `CONFIG_FILE`：

```sh
CONFIG_FILE=./local.yaml go run ./cmd/agent-http-go
```

本地个人配置建议命名为 `config.local.yaml`，该文件已加入 `.gitignore`，不会被提交：

```sh
CONFIG_FILE=./config.local.yaml go run ./cmd/agent-http-go
```

`HOST` 和 `PORT` 环境变量会覆盖 YAML 配置，适合部署时临时调整监听地址：

```sh
HOST=0.0.0.0 PORT=8080 go run ./cmd/agent-http-go
```

配置优先级：

1. 默认值：`127.0.0.1:8787`
2. `config.yaml`
3. 环境变量：`HOST` / `PORT`

### Agent 执行超时

`runner.timeout` 控制单次 agent CLI 子进程最多可以执行多久，默认是 `10m`。

配置值使用 Go duration 格式，例如：

- `30s`
- `10m`
- `1h`

### Agent CLI 配置

`runner.codex.command` 和 `runner.claude.command` 控制服务查找的 CLI 命令名，默认分别是 `codex` 和 `claude`。如果 CLI 不在 `PATH` 中，可以配置成绝对路径或 wrapper 脚本名。

`runner.codex.approvalPolicy`、`runner.codex.sandbox` 和 `runner.codex.ephemeral` 会传给 `codex app-server --stdio` 的 `thread/start` 请求，默认保持当前服务行为：

```yaml
runner:
  codex:
    approvalPolicy: never
    sandbox: workspace-write
    ephemeral: true
```

### 关闭超时

`server.shutdownTimeout` 控制收到中断信号后的优雅关闭等待时间，默认是 `10s`。

### HTTP 连接超时

`server.readHeaderTimeout` 控制读取请求头的最长时间，默认是 `5s`。`server.readTimeout` 控制读取完整请求体的最长时间，默认是 `30s`。`server.idleTimeout` 控制 keep-alive 空闲连接保留时间，默认是 `120s`。

服务不会默认设置 `WriteTimeout`，避免长时间 SSE 响应被固定写超时切断。

### 请求体大小

`server.maxBodySize` 控制 JSON 请求体最大大小，默认是 `1MiB`。

支持的单位：

- `B`
- `KiB`
- `MiB`
- `GiB`

也可以直接写字节数，例如 `1048576`。

### 工作区根目录

`workspace.root` 控制 agent 子进程允许使用的工作区边界，默认是当前目录 `.`。

请求体里的 `cwd` 必须解析到该目录内部，否则会返回 `400`：

```yaml
workspace:
  root: "."
```

### 日志格式和级别

`log.level` 控制日志级别，支持：

- `debug`
- `info`
- `warn`
- `error`

`log.format` 控制日志格式，支持：

- `text`
- `json`

### 路由注册日志

生产环境默认不输出路由注册日志。如果需要像 Gin 那样在启动时打印路由表，可以在 YAML 中打开：

```yaml
server:
  logRoutes: true
```

开启后会通过 `slog` 输出每个注册路由的 `method`、`path` 和 `handler`。

### 文档和调试页面

Swagger/OpenAPI 和 `examples` 调试页面默认不注册路由，需要时在 YAML 中显式打开：

```yaml
server:
  swagger:
    enabled: true
  examples:
    enabled: true
```

### 持久化会话

`session` 控制长对话持久化。默认开启，使用 GORM 写入本地 SQLite 文件，不需要单独部署数据库服务；也可以切换到 MySQL。

```yaml
session:
  enabled: true
  driver: sqlite
  maxTurns: 20
  maxHistorySize: 64KiB
  sqlite:
    path: ./data/agent-http.db
  mysql:
    dsn: ""
```

MySQL 示例：

```yaml
session:
  enabled: true
  driver: mysql
  mysql:
    dsn: user:pass@tcp(127.0.0.1:3306)/agent_http?charset=utf8mb4
```

字段说明：

- `enabled`：是否启用 `/sessions/{sessionId}` 系列接口。
- `driver`：当前支持 `sqlite`、`mysql`。
- `maxTurns`：拼接历史上下文时最多取最近多少轮成功对话。
- `maxHistorySize`：拼接历史上下文的最大大小，支持 `B`、`KiB`、`MiB`、`GiB`。
- `sqlite.path`：SQLite 数据库文件路径。首次启动会自动创建目录，并通过 GORM AutoMigrate 创建表。SQLite 连接默认启用 `foreign_keys`、`busy_timeout=5000` 和 WAL journal mode。
- `mysql.dsn`：MySQL 连接串，仅在 `driver: mysql` 时使用；服务会自动启用 `parseTime=true`。

修改配置后需要重启服务生效。

如果需要跑 MySQL 存储集成测试，可以提供测试库 DSN：

```sh
AGENT_HTTP_MYSQL_TEST_DSN='user:pass@tcp(127.0.0.1:3306)/agent_http?charset=utf8mb4' go test ./internal/agenthttp
```

## 接口文档

完整 HTTP API 契约维护在 [docs/openapi.yaml](docs/openapi.yaml)，可直接导入 Swagger UI、Redoc 或 OpenAPI 客户端生成工具。

启用 `server.swagger.enabled` 并启动服务后，可以直接在浏览器访问：

```text
http://127.0.0.1:8787/swagger
```

原始 OpenAPI YAML 地址：

```text
http://127.0.0.1:8787/openapi.yaml
```

当前接口覆盖：

- `GET /health`
- `GET /agents`
- `POST /runs`
- `POST /runs/stream`
- `POST /sessions/{sessionId}/runs`
- `POST /sessions/{sessionId}/runs/stream`
- `GET /sessions/{sessionId}`
- `DELETE /sessions/{sessionId}`
- Deprecated 兼容接口：`POST /codex`、`POST /codex/stream`

同时启用 `server.examples.enabled` 和 `session.enabled` 后，服务也会提供同源调试页面：

```text
http://127.0.0.1:8787/examples/session-stream
```

该页面用于本地验证持久化会话、SSE 流式输出、`debug=1` 事件、停止请求和 session 删除。

## 限制

- 请求体大小由 `server.maxBodySize` 控制，默认 `1MiB`。
- 单次 agent 执行超时时间由 `runner.timeout` 控制，默认 `10m`。
- 每个运行请求都会启动一个 CLI 子进程。
- SSE 接口默认推送 `start`、`delta`、`done`、`error` 事件；`delta` 只来自 agent CLI 实际输出的正文增量事件。
- `codex` 流式输出依赖 `codex app-server` 的 experimental 协议；如果当前 Codex CLI 版本未输出 `item/agentMessage/delta`，服务不会用最终结果做兜底假流式。
- SSE 接口只有在 `debug=1` 时才会额外推送 `result` 事件，避免客户端把最终 `output` 追加到已流式展示的正文里。
- 持久化会话可使用 SQLite 本地文件或 MySQL；`session.enabled: false` 时不会注册 `/sessions/{sessionId}` 系列接口。
- 持久化会话只保留并查询本地数据库中的历史，不维护常驻 CLI 进程。
- 超时返回 HTTP `504`。
- 未知路由返回 HTTP `404`。
- 非 POST 方法调用 `/runs`、`/runs/stream`、`/codex` 或 `/codex/stream` 返回 HTTP `405`。

## 测试

```sh
go test ./...
```
