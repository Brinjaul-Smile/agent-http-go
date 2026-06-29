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
- 持久化会话数据库：SQLite。
- SQLite Go 驱动：`modernc.org/sqlite`，纯 Go 实现，不需要单独部署数据库服务。

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
  maxBodySize: 1MiB
  logRoutes: false

runner:
  timeout: 10m

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

### 持久化会话

`session` 控制长对话持久化。默认开启，使用本地 SQLite 文件，不需要单独部署数据库服务。当前实现使用纯 Go SQLite 驱动 `modernc.org/sqlite`。

```yaml
session:
  enabled: true
  driver: sqlite
  maxTurns: 20
  maxHistorySize: 64KiB
  sqlite:
    path: ./data/agent-http.db
```

字段说明：

- `enabled`：是否启用 `/sessions/{sessionId}` 系列接口。
- `driver`：当前支持 `sqlite`；代码通过 `SessionStore` 抽象保留后续 RDS 扩展空间。
- `maxTurns`：拼接历史上下文时最多取最近多少轮成功对话。
- `maxHistorySize`：拼接历史上下文的最大大小，支持 `B`、`KiB`、`MiB`、`GiB`。
- `sqlite.path`：SQLite 数据库文件路径。首次启动会自动创建目录、数据库文件和表。

修改配置后需要重启服务生效。

## 接口

### `GET /health`

健康检查接口。

响应：

```json
{"ok":true}
```

### `GET /agents`

检查常见 agent CLI 是否存在于服务进程的 `PATH` 中，并返回当前服务是否支持通过 `/runs` 调用该 agent。

响应示例：

```json
{
  "ok": true,
  "agents": [
    {
      "name": "codex",
      "command": "codex",
      "available": true,
      "supported": true
    },
    {
      "name": "claude",
      "command": "claude",
      "available": false,
      "supported": true,
      "error": "claude CLI not found in PATH"
    }
  ]
}
```

### `POST /runs`

通用 agent 调用接口，通过 `agent` 字段选择后端。

请求示例：

```sh
curl -sS -X POST http://127.0.0.1:8787/runs \
  -H 'Content-Type: application/json' \
  -d '{"agent":"codex","prompt":"Reply with exactly: pong"}'
```

请求体：

```json
{
  "agent": "codex",
  "prompt": "Reply with exactly: pong",
  "cwd": "./optional-subdir"
}
```

字段说明：

- `agent`：必填，当前支持 `codex` 和 `claude`。
- `prompt`：必填，非空字符串。
- `cwd`：选填，必须解析到服务工作区内部。

成功响应：

```json
{
  "ok": true,
  "exitCode": 0,
  "output": "pong"
}
```

### `POST /codex`

兼容接口，等价于 `POST /runs` 且 `agent` 固定为 `codex`。

请求示例：

```sh
curl -sS -X POST http://127.0.0.1:8787/codex \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Reply with exactly: pong"}'
```

### `POST /sessions/{sessionId}/runs`

持久化长对话接口。`sessionId` 由调用方生成并稳定复用；同一个 `sessionId` 会复用历史，不同 `sessionId` 相互隔离。

请求示例：

```sh
curl -sS -X POST http://127.0.0.1:8787/sessions/chat-001/runs \
  -H 'Content-Type: application/json' \
  -d '{"agent":"codex","prompt":"我叫张三"}'

curl -sS -X POST http://127.0.0.1:8787/sessions/chat-001/runs \
  -H 'Content-Type: application/json' \
  -d '{"agent":"codex","prompt":"我叫什么？"}'
```

请求体和 `/runs` 相同：

```json
{
  "agent": "codex",
  "prompt": "我叫什么？",
  "cwd": "./optional-subdir"
}
```

规则：

- `sessionId` 允许字母、数字、`.`、`_`、`-`、`:`，最长 128 字节。
- 第一次调用会创建 session，并绑定当次的 `agent` 和解析后的 `cwd`。
- 后续同一 session 必须继续使用相同 `agent` 和 `cwd`，否则返回 `400`。
- 同一个 session 内请求串行执行，不同 session 可并发执行。
- 只有成功 turn 会参与后续上下文拼接；失败和超时会写入数据库用于审计，但不会污染后续上下文。

成功响应：

```json
{
  "ok": true,
  "sessionId": "chat-001",
  "exitCode": 0,
  "output": "你叫张三。"
}
```

### `GET /sessions/{sessionId}`

查询持久化会话和消息。

```sh
curl -sS http://127.0.0.1:8787/sessions/chat-001
```

响应示例：

```json
{
  "ok": true,
  "session": {
    "id": "chat-001",
    "agent": "codex",
    "cwd": "/path/to/workspace",
    "createdAt": "2026-06-29T12:00:00Z",
    "updatedAt": "2026-06-29T12:01:00Z"
  },
  "messages": [
    {
      "id": 1,
      "sessionId": "chat-001",
      "role": "user",
      "content": "我叫张三",
      "status": "ok",
      "createdAt": "2026-06-29T12:00:00Z"
    }
  ]
}
```

### `DELETE /sessions/{sessionId}`

删除持久化会话和关联消息。接口是幂等的，session 不存在也返回成功。

```sh
curl -sS -X DELETE http://127.0.0.1:8787/sessions/chat-001
```

响应：

```json
{"ok":true}
```

### 调试输出

`/runs`、`/codex` 和 `/sessions/{sessionId}/runs` 都支持 `debug=1`。开启后会额外返回原始 stdout 和 stderr。

```sh
curl -sS -X POST 'http://127.0.0.1:8787/codex?debug=1' \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Reply with exactly: pong"}'
```

响应示例：

```json
{
  "ok": true,
  "exitCode": 0,
  "output": "pong",
  "debug": {
    "stdout": "...",
    "stderr": "..."
  }
}
```

## 限制

- 请求体大小由 `server.maxBodySize` 控制，默认 `1MiB`。
- 单次 agent 执行超时时间由 `runner.timeout` 控制，默认 `10m`。
- 每个运行请求都会启动一个 CLI 子进程。
- 持久化会话使用 SQLite 本地文件；`session.enabled: false` 时不会注册 `/sessions/{sessionId}` 系列接口。
- 持久化会话只保留并查询本地数据库中的历史，不维护常驻 CLI 进程。
- 超时返回 HTTP `504`。
- 未知路由返回 HTTP `404`。
- 非 POST 方法调用 `/runs` 或 `/codex` 返回 HTTP `405`。

## 测试

```sh
go test ./...
```
