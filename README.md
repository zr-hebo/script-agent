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

`run` 输出一份完整结果 JSON。退出码：0 = 主任务成功且用户清理/平台回调均成功或跳过；1 = 任一部分失败；2 = 命令行/输入格式错误。

## 三阶段生命周期

| 阶段 | Agent 职责 | 用户钩子 |
|---|---|---|
| `prepare` | 校验请求、准备隔离工作目录、加载代码 | 参数/业务检查、准备资源；默认空实现 |
| `run` | 执行业务、采集 Outcome、处理超时和取消 | 必需，返回明确的执行结果 |
| `post-run` | 尝试用户清理，随后发送平台回调 | 用户清理默认空实现，不能替代平台回调 |

- Prepare 失败或 panic：跳过 Run，仍尝试 PostRun；清理逻辑必须能处理只完成了一半的准备。
- Run 错误或可捕获 panic：构造失败 Outcome，再传给 PostRun。
- 用户 PostRun 使用独立超时，不复用已经取消的执行 Context。正常返回的 Run 结果在调用 PostRun 前保存，清理失败/超时不覆盖它。
- `status` / `outcome` 表示 Prepare/Run 的最终结果；`user_post_run` 单独保存用户清理结果，`callback` 保存平台回调结果。任一清理/回调失败会将 post-run 阶段标为 failed，但不会重跑脚本。
- 未定义的 Python/Shell 钩子及旧 Go Handle 的清理记录为 skipped；Go BaseTask 的空方法正常返回，记录为 succeeded。
- 已进入 post-run 后，不再接受 HTTP 取消；package 调用方的 Context 取消也不会取消已经开始的用户清理或系统回调。
- **用户清理不保证一定执行**：源码加载失败、进程被 SIGKILL/OOM 杀死、节点故障可能使 Go/Python 无法运行 PostRun。Agent 仍在运行时会尽力发送平台回调；不能把关键回调只放在用户代码里。
- HTTP 400 拒绝的请求不创建 Task，也不回调。已接收但 prepare 失败的 Task 仍进入平台 post-run；未授权回调地址不会被访问。

## 用户脚本规范

### 统一 TaskRunner / Outcome

Go 用户导入轻量 SDK：`github.com/zr-hebo/script-agent/sdk`。宿主 package 同时导出了这些类型的别名。

```go
type TaskRunner interface {
    Prepare(ctx context.Context, params map[string]any) error
    Run(ctx context.Context, params map[string]any) Outcome
    PostRun(ctx context.Context, params map[string]any, outcome Outcome) error
}
```

`Outcome` 的业务字段：

```go
type Outcome struct {
    Status Status         // succeeded / failed / timed_out / cancelled
    Data   map[string]any
    Error  error          // 普通 Go error，不使用 TaskError
    // 另有由 Agent 填写的日志、退出码、耗时和诊断堆栈。
}
```

- Run 不再同时返回第二个 error；业务错误统一放在 `Outcome.Error`。
- 成功必须显式为 `StatusSucceeded` 且 Error 为 nil；失败必须带非空错误信息；空状态、未知状态、矛盾结果都判失败。
- 自定义 JSON 编解码将 Error 转成字符串或 null，不会输出 `{}`。跨进程/HTTP 后只能保留错误消息，不能保证 `errors.Is/As` 原类型链。
- Agent 根据真实超时、取消和异常退出校正结果；不信任用户填写的日志、退出码和耗时。
- PostRun 接收保存前已校验的业务 Outcome；最终日志/进程退出码由进程外 Agent 在结束后补齐，因此用户钩子中不能依赖这些最终诊断字段。
- 诊断堆栈独立放在 `outcome.stack`，不自定义错误类型。

### Go：New() 返回 TaskRunner，只实现 Run 即可

```go
package usercode

import (
    "context"
    "fmt"

    "github.com/zr-hebo/script-agent/sdk"
)

type Task struct {
    sdk.BaseTask // Prepare、PostRun 默认空实现
}

func New() sdk.TaskRunner {
    return &Task{}
}

func (t *Task) Run(ctx context.Context, params map[string]any) sdk.Outcome {
    clusterUUID, ok := params["cluster_uuid"].(string)
    if !ok || clusterUUID == "" {
        return sdk.Outcome{
            Status: sdk.StatusFailed,
            Error:  fmt.Errorf("cluster_uuid is required"),
        }
    }
    return sdk.Outcome{
        Status: sdk.StatusSucceeded,
        Data:   map[string]any{"cluster_uuid": clusterUUID},
    }
}
```

需要自定义时覆盖 `Prepare`、`PostRun` 方法，参见 [完整三阶段 Go 示例](examples/scripts/task.go)。必须是 `package usercode`，工厂签名为 `New() sdk.TaskRunner`。

Agent 预注册 SDK 和 Yaegi 接口包装器，动态加载源码并创建任务实例；同一 Task 的三个方法共享同一对象和参数 Map，不同 Task 使用独立进程/解释器。不需要用户编译插件或部署 Worker。标准输出用于日志；用户创建的 goroutine 必须在所属阶段结束前退出。

### Python：run 必需，prepare/post_run 可省略

最小脚本：

```python
def run(params):
    return {
        "status": "succeeded",
        "data": {"cluster_uuid": params["cluster_uuid"]},
        "error": None,
    }
```

完整形式：

```python
def prepare(params):
    if not params.get("cluster_uuid"):
        raise ValueError("cluster_uuid is required")

def run(params):
    return {"status": "succeeded", "data": params, "error": None}

def post_run(params, outcome):
    print("cleanup after", outcome["status"])
```

- prepare/post_run 正常返回即成功，失败抛异常；默认不做任何操作。
- run 返回 Outcome dict；`error` 使用字符串或 None，`data` 使用 dict 或 None。异常自动转换成失败 Outcome 并保留 traceback。
- 在同一 Python 子进程中依次调用，参数 Map 的准备结果可传到 run；PostRun 收到序列化快照，修改不会覆盖已经保存的主结果。
- 使用 `python3 -I -u`，不自动安装依赖，不继承 PYTHONPATH。源码顶层会执行，`if __name__ == "__main__"` 不执行；不支持 async 入口。

### Shell：source 必需，其余阶段脚本可省略

Request 使用三个源码字段：

```json
{
  "language": "shell",
  "prepare_source": "echo ready > prepared.txt",
  "source": "test -f prepared.txt; echo running",
  "post_run_source": "test -f prepared.txt; echo cleanup",
  "params": {"cluster_uuid": "cluster-001"},
  "timeout_seconds": 60,
  "post_run_timeout_seconds": 10
}
```

省略 prepare_source/post_run_source 即空实现。这两个字段仅用于 Shell；Go/Python 通过源码内的方法/函数定义钩子。

- 每阶段为独立 Bash 进程，共享当前 Task 工作目录；文件可以跨阶段传递，shell 变量和 export 不会自动跨进程保留。
- 所有阶段通过 `SCRIPT_PARAMS_FILE` 读取 JSON Map（保留 `BATCH_PARAMS_FILE` 兼容别名），不通过 eval 或源码插值传参。
- Run 退出码 0 表示成功，非零表示失败；Agent 将其转换成统一 Outcome。
- Run 可将业务结果 Map/null 写到 `SCRIPT_RESULT_FILE`，这里仍是 **Data，不是整个 Outcome**。无结果文件时 Data 为 null；无效 JSON 判失败；Prepare 遗留的结果文件在 Run 前清除。
- PostRun 从 `SCRIPT_OUTCOME_FILE` 读取完整主结果，其中 error 为字符串或 null。post-run 非零退出仅表示清理失败。
- Bash 使用 `-Eeuo pipefail` 和 ERR trap 记录失败行号/退出码。条件语句等存在 errexit 例外，用户仍需检查业务结果，不得关闭严格模式或留下后台任务。

读取参数/写入业务结果的完整示例见 [Shell 请求](examples/shell-request.json)，该示例用 Python 3 解析 JSON；执行普通 Shell 本身仅需要 Bash。

### 参数、超时与兼容性

- params 必须为 JSON Map；省略/null 时为空 Map，支持嵌套对象、数组、布尔和字符串。
- 批量平台负责按 Cluster 拆分 Task、注入并保护 `params.cluster_uuid`；Agent 不自行选择目标。
- Go Map 中 JSON 数字按 float64 解码；大整数 ID 请使用字符串。
- 三个源码字段合计最大 256 KiB，参数最大 64 KiB；每份 Outcome JSON 最大 1 MiB。stdout/stderr 各保留前 64 KiB，含用户清理日志，并标记截断。
- timeout_seconds 默认 300，范围 1..3600，覆盖 Runner 准备和用户 Prepare/Run，不含排队、用户 PostRun、平台回调。
- post_run_timeout_seconds 默认 10，范围 1..60，限制用户 PostRun；平台回调另有独立超时，默认 10 秒。
- 超时/取消先向进程组发送 SIGTERM，默认宽限 1 秒，再 SIGKILL。Go 用户代码应检查 Context；协作退出后可进入独立时限的 PostRun。无法退出时会强杀，用户清理不保证执行。
- `outcome.duration_ms` 覆盖 Runner Prepare/Run/用户 PostRun，不含平台回调。每阶段还有独立开始/结束时间。
- 旧 Go `Handle(ctx, params) (map[string]any, error)` 和 Python `handle(params)` 仍可使用：返回 Map 被转换成 Outcome。优先选择新 Go New / Python run 入口。
- **响应协议变化**：以前 `outcome.error` 是含 type/message/stack 的对象，现在是字符串/null；stack 移到 outcome.stack。接收回调的批量平台需要相应适配；旧的 ExecutionError 类型已移除。

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
    PostRunTimeoutSeconds: 10,
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
- 回调 body 包含 task_id、execution_id、outcome、user_post_run 和日志。平台不会单独附加源码、完整入参或回调 token；用户主动返回/打印的敏感信息仍需调用方治理。
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
  },
  "user_post_run": {
    "status": "skipped",
    "data": null,
    "error": null,
    "exit_code": null,
    "stdout": "",
    "stderr": "",
    "stdout_truncated": false,
    "stderr_truncated": false,
    "duration_ms": 0
  }
}
```

## 安全边界与验证

子进程/Yaegi **不是安全沙箱**。当前只清理继承环境、隔离临时目录、限制日志/结果读取、处理进程组超时与取消；没有 CPU/内存/磁盘硬配额，不能阻止同 UID 文件访问、网络访问或恶意进程逃离进程组。程序仍使用宿主用户权限。默认不向公网开放；不可信代码必须放入独立低权限容器/沙箱，限制网络、凭证、资源和宿主挂载。不要向脚本暴露生产凭证或宿主目录，环境清理不能代替权限隔离。

```bash
go test -race ./...
go vet ./...
```

测试覆盖 TaskRunner 动态加载和默认空钩子、同实例状态、Outcome error 编解码、旧入口兼容、三语言阶段失败、独立清理超时、取消后回调、Map、日志、HTTP 容量及关停。测试需要本机 Go、Bash 和 Python 3。
