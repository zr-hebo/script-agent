# Script-Agent

独立脚本执行组件：**Go 动态解释（Yaegi）、Bash、Python 3**。每个 Task 使用独立子进程和工作目录，参数统一为 JSON Map。

可作为二进制运行，也可直接作为 Go package 引入。**不依赖 Temporal，不修改批量平台，也不是安全沙箱。**

## 运行环境

- Linux / macOS，构建需要 Go 1.22+；本机验证工具链为 Go 1.26.1。
- Shell 需要 Bash；Python 需要 Python 3。只执行 Go 时不需要这两个解释器。
- Go 使用固定版本 Yaegi v0.16.1，无须为每份用户源码执行 `go build`；不承诺兼容全部 Go 语法、CGo、泛型或第三方依赖，当前以标准库脚本为支持范围。

```bash
go build -o bin/script-agent ./cmd/script-agent
./bin/script-agent run --file examples/go-request.json
./bin/script-agent run --file examples/shell-request.json
./bin/script-agent run --file examples/python-request.json
```

`run` 输出一份完整结果 JSON。退出码：0 = 脚本成功且回调成功/跳过；1 = 执行或回调失败；2 = 命令行/输入格式错误。

## 三阶段生命周期

| 阶段 | 当前职责 |
|---|---|
| `prepare` | 校验语言、源码、Map 和回调地址；创建临时目录；写入脚本、参数及启动模板 |
| `run` | 启动独立子进程，执行用户函数/脚本，采集日志、结果和退出状态，处理超时/取消 |
| `post-run` | 向调用方回调执行结果；没有回调地址时记录 skipped |

这三阶段是 Agent 的固定生命周期，不是要求用户上传三份脚本。prepare 失败时跳过 run，但仍进入 post-run；无效/未授权的回调地址不会被访问。HTTP 输入在接收前校验，被 HTTP 400 拒绝的请求不创建 Task，也不发回调。

- Go/Python：正常退出且产生有效结果 JSON 才成功。`os.Exit(0)` 等绕过结果协议的行为不能算成功。
- Shell：退出码 0 表示成功，可选写入结果 JSON；若写入了无效结果，仍判失败。
- `Status` 保留脚本结果：`succeeded / failed / timed_out / cancelled`。回调失败单独记录在 `Callback` 和 post-run 阶段，不覆盖脚本结果，也不重新运行脚本。
- 取消先向进程组发 SIGTERM，默认宽限 1 秒后 SIGKILL。Go 入口会将 SIGTERM/SIGINT 转为 Context 取消；Python/Shell 可按需要处理信号。
- 阶段记录含开始/结束时间；取消尚未运行的任务时 run 为 skipped。日志、错误和结果只能尽力捕获，OOM、SIGKILL、节点故障不能保证有语言堆栈。

## 用户脚本规范

### Go：动态加载源码并调用 Handle

```go
package usercode

import "context"

func Handle(ctx context.Context, params map[string]any) (map[string]any, error) {
    if err := ctx.Err(); err != nil {
        return nil, err
    }
    return map[string]any{"cluster_uuid": params["cluster_uuid"]}, nil
}
```

必须是 `package usercode`，签名为 `Handle(context.Context, map[string]any) (map[string]any, error)`；`interface{}` 与 `any` 等价。每次执行创建独立解释器，不复用用户全局变量。标准输出用于日志，返回 Map 自动保存为结构化结果。错误返回与可捕获 panic 会保存诊断。用户创建的 goroutine 必须在函数返回前结束。

### Python：调用 handle(params)

```python
def handle(params):
    print("processing", params["cluster_uuid"])
    return {"cluster_uuid": params["cluster_uuid"], "dry_run": params["dry_run"]}
```

必须提供同步 `handle(params)`，返回 dict 或 None；异常保存 traceback。源码顶层会执行，`if __name__ == "__main__"` 不会执行。使用 `python3 -I -u`，不自动安装依赖，不继承 PYTHONPATH。

### Shell：读取参数文件

```bash
# 参数通过文件传入，不使用 eval 或源码插值。
python3 - <<'PY'
import json, os
with open(os.environ["SCRIPT_PARAMS_FILE"], encoding="utf-8") as f:
    params = json.load(f)
print("processing", params["cluster_uuid"])
with open(os.environ["SCRIPT_RESULT_FILE"], "w", encoding="utf-8") as f:
    json.dump({"cluster_uuid": params["cluster_uuid"]}, f)
PY
```

Agent 使用 Bash `-Eeuo pipefail` 和 ERR trap，记录失败行号及退出码。Bash 的条件语句等存在 errexit 例外，用户仍需检查业务是否成功，不能依靠日志中的 `error` 字样判断结果；用户不得关闭严格模式、吞掉错误或留下后台任务。

### 参数和结果

- `params` 为 JSON 对象，省略/null 时转换为空 Map，支持嵌套对象、数组、布尔、字符串等。
- Agent 不自行选择 Cluster。批量平台应按目标拆分 Task，并在调用前注入且保护 `params.cluster_uuid`。
- Go Map 中 JSON 数字按 `float64` 解码；大整数 ID 请用字符串，避免 JSON/浮点精度损失。
- 子进程环境提供 `SCRIPT_PARAMS_FILE`、兼容别名 `BATCH_PARAMS_FILE`、`SCRIPT_RESULT_FILE`。
- Go/Python 返回值、Shell 可选结果文件必须是 JSON 对象或 null；日志与结果分开。
- 源码最大 256 KiB，参数最大 64 KiB，结果最大 1 MiB；stdout/stderr 各保留前 64 KiB，并标记截断。
- `timeout_seconds` 默认 300，范围 1..3600，覆盖 Runner 的 prepare/run，不含队列等待和 post-run。post-run 有独立时限。

## 作为 Go package 使用

模块：`github.com/zr-hebo/script-agent`，package 名为 `scriptagent`。本地未发布时，在调用方项目使用 `go mod edit -replace github.com/zr-hebo/script-agent=/absolute/path/to/script-agent`，再添加该模块依赖。

当前批量平台声明 Go 1.19；直接引入本模块需要将调用方工具链和 go.mod 升至 Go 1.22+。本项目没有修改批量平台。若暂不升级，可先使用独立二进制/HTTP 方式接入。

```go
import agent "github.com/zr-hebo/script-agent"

executor, err := agent.New(agent.Config{
    GoExecutable: "/opt/script-agent", // Go 隔离 helper；不是 HTTP 服务
    Callback: agent.CallbackConfig{
        AllowedOrigins: []string{"https://batch.example.com"},
    },
})
if err != nil {
    return err
}

result := executor.Execute(ctx, agent.Request{
    TaskID: "task-001",
    Language: "go",
    Source: uploadedSource,
    Params: map[string]any{"cluster_uuid": "cluster-001", "dry_run": true},
    TimeoutSeconds: 60,
    CallbackURL: "https://batch.example.com/script/callback",
})
```

`Execute` 同步返回，包含 post-run 结果；调用方可用 goroutine 调度并通过 Context 取消。Executor 可并发复用，但 package 自身不限制并发，调用方必须限制；每次 Execute 都生成新的 execution_id，**不自动重试或去重脚本**。业务 task_id 可重复，仅用于关联。

不想单独部署 helper 二进制时，可以复用调用方自身的二进制：在 `main()` 启动业务服务之前调用 `HandleHelperCommand(ctx, os.Args[1:])`，命中后 `os.Exit(code)`；将 `os.Executable()` 的路径传入 `Config.GoExecutable`。参见 [完整嵌入示例](examples/embedded/main.go)。Go 的包级 `init()` 总会先执行，有启动副作用的宿主项目更适合使用独立 helper。

Shell/Python 的 package 调用不需要 Go helper。缺少对应执行程序时返回 process_error，不在 API 进程中降级执行用户代码。

## 作为 HTTP 服务运行

```bash
./bin/script-agent serve --listen 127.0.0.1:8080 --concurrency 4 --capacity 128
curl -sS -X POST http://127.0.0.1:8080/v1/executions \
  -H 'Content-Type: application/json' --data-binary @examples/python-request.json
```

| API | 用途 |
|---|---|
| `POST /v1/executions` | 提交 Request JSON，返回 HTTP 202 和 execution_id |
| `GET /v1/executions/{execution_id}` | 查询 queued/running/cancelling/终态、当前 phase、完整结果 |
| `DELETE /v1/executions/{execution_id}` | 请求取消；返回 202 不代表已停止，请查询终态 |
| `GET /healthz` | 存活检查 |

Request 示例见 examples。任务在 post-run 期间仍未完成；已结束 run/进入 post-run 后拒绝取消（409）。请求体最大 512 KiB；无效输入 400；未知执行 404；容量耗尽 429。

默认仅监听 loopback；非 loopback 必须设置 `SCRIPT_AGENT_TOKEN`，请求使用 `Authorization: Bearer <token>`（含健康检查）。跨主机部署须配置 TLS 反向代理、网络访问限制和鉴权。

HTTP 适配层使用有界内存存储，不是持久化任务队列；默认最多保留 128 条记录，新请求会淘汰最早创建的已完成记录，未完成记录不被淘汰。重启后记录丢失，重复 POST 会创建新执行。进程收到正常退出信号时取消任务并等待 post-run；SIGKILL/宕机没有可靠投递保证。

## post-run HTTP 回调

通过 `--callback-origins https://batch.example.com` 开放精确 origin；默认禁止全部回调。可通过 `SCRIPT_AGENT_CALLBACK_TOKEN` 设置回调 Bearer Token，凭证不会传给脚本。

- POST JSON，HTTP 2xx 为成功；不跟随重定向。
- 网络错误、408、429、5xx 最多尝试 3 次；总时限默认 10 秒；其他非 2xx 不重试。
- 每次重试使用相同 `Idempotency-Key: <execution_id>`，接收方应据此去重。
- 回调 body 包含 task_id、execution_id、脚本结果和日志，不包含源码、完整入参或回调 token；脚本主动打印的敏感信息仍需调用方治理。
- 允许的 origin 必须是管理员控制的服务；allowlist 不是完整的网络沙箱，部署时仍应限制出口。
- 当前是有界、尽力投递，不支持重启后的持久化补偿/可靠 outbox。

完整回调示例（无结构化结果的 Shell 成功）：

```json
{
  "event": "task.completed",
  "task_id": "task-001",
  "execution_id": "1bb0952303524df291317c0957140e28a",
  "phase": "post-run",
  "status": "succeeded",
  "outcome": {
    "status": "succeeded",
    "data": null,
    "error": null,
    "exit_code": 0,
    "stdout": "hello\n",
    "stderr": "",
    "stdout_truncated": false,
    "stderr_truncated": false,
    "duration_ms": 12
  }
}
```

## 安全边界与验证

子进程/Yaegi **不是安全沙箱**。当前只清理继承环境、隔离临时目录、限制日志/结果读取、处理进程组超时与取消；没有 CPU/内存/磁盘硬配额，不能阻止同 UID 文件访问、网络访问或恶意进程逃离进程组。程序仍使用宿主用户权限。默认不向公网开放；不可信代码必须放入独立低权限容器/沙箱，限制网络、凭证、资源和宿主挂载。不要向脚本暴露生产凭证或宿主目录，环境清理不能代替权限隔离。

```bash
go test -race ./...
go vet ./...
```

测试覆盖三语言、嵌套 Map、语法/签名错误、panic/traceback、Shell pipefail、日志截断、超时取消、回调重试与重定向拒绝、三阶段状态、HTTP 容量与正常关停。测试需要本机 Go、Bash 和 Python 3。
