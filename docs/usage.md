# Script-Agent 使用指南

返回 [README](../README.md)。本指南描述当前实现；所有示例命令从项目根目录执行，均不依赖 jq。

## 目录

- [通用调用、参数、日志](#common)
- [Go](#go)
- [Python](#python)
- [Shell](#shell)
- [生命周期与阶段日志](#lifecycle)
- [完整返回结果](#result)
- [限制与兼容性](#limits)
- [Go package、HTTP 与回调](#integration)
- [安全边界](#security)

<a id="common"></a>
## 通用调用方式

所有命令均在项目根目录执行；先运行 `make build`。脚本使用独立临时工作目录，不能假定当前目录是仓库目录。修改脚本无需重建 Agent。

| 模式 | 用途 |
|---|---|
| `run --source PATH --language go\|shell\|python --params JSON` | 直接读取源码文件，适合本地调试 |
| `run --file request.json` | 读取完整请求，可指定业务 ID、超时、回调及 Shell 三阶段源码 |
| `run` 或 `run --file -` | 从 stdin 读取一份完整请求 JSON |

`--source` 不能与 `--file` 同时使用；`--language`、`--params` 仅与 `--source` 配合使用。`--language` 默认 `go`，`--params` 默认 `{}`，必须是 JSON 对象，不能为数组或 null。

### Map 参数与任务标识

- Go 入口直接接收 `map[string]any`，Python 入口直接接收 `dict`，不生成 `PARAM_*` 环境变量。仅 Shell 将 params 映射为 `PARAM_*` 环境变量，也可通过 `SCRIPT_PARAMS_FILE` 读取完整 JSON。
- `cluster_uuid`、`dry_run` 是示例业务参数，不是 Agent 的固定字段。Go/Python custom 示例要求非空字符串 `cluster_uuid`，可选布尔值 `dry_run` 默认 true；Shell custom 示例只打印这两个环境变量，不校验业务类型，缺失时分别显示空字符串、true。所有示例均不实际操作 cluster。
- 布尔值使用 `true` / `false`，不要传字符串 `"false"`。数字进入 Go Map 后是 float64，大整数 ID 请用字符串。
- 每个 cluster 的执行由批量平台拆分、提交，Agent 不会自动遍历参数中的 cluster 列表。
- `task_id` 是调用方提供的业务关联 ID，最多 256 字节，可为空或重复；`execution_id` 是每次执行生成的 ID，用于 HTTP 查询、取消及回调去重。重复 task_id 不会阻止再次执行。
- `--source` 模式当前没有 `--task-id` 或超时/回调 URL 参数；需要这些字段时使用完整请求。把 task_id 放入 params 不会设置结果顶层的 task_id。

### Shell params 环境变量映射

仅对 Shell 启用，注入当前 Task 的脚本子进程，不修改 Agent 全局环境。Go/Python 使用原 Map，不应用下面的环境变量名称、重名、NUL 和映射大小校验；仍遵守 JSON 可序列化及 params JSON 最大 64 KiB 的通用限制。

| params | 子进程环境变量 |
|---|---|
| `"cluster_uuid": "cluster-002"` | `PARAM_CLUSTER_UUID=cluster-002` |
| `"dry_run": false` | `PARAM_DRY_RUN=false` |
| `"count": 3` | `PARAM_COUNT=3` |
| `"options": {"region":"sg"}` | `PARAM_OPTIONS={"region":"sg"}` |
| `"items": [1,2]` | `PARAM_ITEMS=[1,2]` |
| `"optional": null` | `PARAM_OPTIONAL=null` |

- 仅映射顶层 key：必须符合 `[A-Za-z_][A-Za-z0-9_]*`，转大写并加 `PARAM_`。不自动替换连字符/空格，不展开嵌套字段；嵌套对象内部 key 不受环境变量名称规则限制。
- 非法 key、转换后重名（如 `name` 与 `NAME`）会被拒绝。字符串原样传递；布尔、数字、数组、对象、null 使用 JSON 文本，因此所有环境变量都是字符串。缺失 key 不设置变量，空字符串与 null 不等价。
- 环境变量不能含 NUL：顶层字符串含 NUL 时拒绝；对象/数组中的 NUL 会由 JSON 转义。映射后的 `NAME=value`（含结束符）总大小最多 64 KiB，独立于原 params JSON 的 64 KiB 上限。
- 前缀保证不会覆盖 `PATH`、`HOME`、`SCRIPT_*` 等 Agent 变量；例如 params 中的 `PATH` 只会生成 `PARAM_PATH`。参数不会作为 Shell 代码执行，脚本引用变量时仍需加双引号。
- Shell 的 prepare/run/post-run 均注入同一份初始参数快照。每阶段是独立进程，阶段内 export 不会传到下一阶段；需要跨阶段传递新数据时使用任务工作目录中的文件。
- Go 使用 `params["cluster_uuid"]`，Python 使用 `params["cluster_uuid"]`，复杂对象/数组直接从 Map 读取。Go/Python 的三个钩子共享参数 Map，Prepare 对它的修改可供 Run 使用；参数文件仍是初始快照。
- 环境变量会被脚本启动的子进程继承；不要将敏感值打印到日志。

Shell 的这项校验对 CLI、HTTP 和 Go package 一致：HTTP 无效请求返回 400、不创建执行；CLI 执行请求校验失败时返回失败结果，退出码为 1。Go/Python 的 Map 可保留 `name` 与 `NAME`、连字符/中文 key 等 JSON 字段，无需满足 Shell 变量命名规则。

### 完整请求字段

| 字段 | 说明 |
|---|---|
| `language` | 必填：go、shell、python |
| `source` | 必填：源码内容，不是路径 |
| `params` | JSON Map；完整请求中省略/null 表示 `{}` |
| `task_id` | 可选，业务关联 ID |
| `timeout_seconds` | 默认 300，范围 1–3600，覆盖准备和运行 |
| `post_run_timeout_seconds` | 默认 10，范围 1–60，仅用于用户清理 |
| `prepare_source` / `post_run_source` | 可选，仅 Shell 使用，内容是源码 |
| `callback_url` | 可选，必须匹配管理员配置的允许 origin |

现有请求可直接执行：

```bash
./bin/script-agent run --file examples/go-request.json
./bin/script-agent run --file examples/python-request.json
./bin/script-agent run --file examples/shell-request.json
```

要把本地 Go 文件和 task_id/超时封装为完整请求，可以用 Python 标准库生成 JSON 后传给 stdin，无需手工转义源码：

```bash
python3 - <<'PY' | ./bin/script-agent run
import json
from pathlib import Path

print(json.dumps({
    "task_id": "batch-1001/cluster-002",
    "language": "go",
    "source": Path("examples/custom/example.go").read_text(encoding="utf-8"),
    "params": {"cluster_uuid": "cluster-002", "dry_run": True},
    "timeout_seconds": 60,
    "post_run_timeout_seconds": 10,
}))
PY
```

### 实时日志和 CLI 退出码

`run` 输出一份完整结果 JSON。退出码：0 = 主任务成功且用户清理/平台回调均成功或跳过；1 = 任一部分失败；2 = 命令行/输入格式错误。

`run` **默认开启 `--stream-logs`**：Go、Shell、Python 的 prepare/run/post-run 用户日志实时转发到 Agent 的 **stderr**，stdout 只输出最终 JSON。无需修改脚本，不需要显式添加该参数。

```bash
# 终端实时显示日志，最终 JSON 保存到文件
./bin/script-agent run --file examples/go-request.json > result.json

# 关闭实时日志（最终 JSON 中仍保留日志）
./bin/script-agent run --file examples/go-request.json --stream-logs=false
```

实时转发不受结果中 stdout/stderr 各 64 KiB 的保留上限影响。转发队列最多 1 MiB；输出端过慢时丢弃新日志，并在输出端可写时尽力提示丢弃字节数。任务结束后最多等待 1 秒排空队列，输出端失败或持续阻塞时不保证日志送达，不影响任务结果。stdout/stderr 合并显示，不保证两个流之间的严格顺序；最终 JSON 仍分别保存。请避免 `2>&1` 将日志混入结果 JSON。

此默认行为仅适用于 CLI `run`；`serve` 不启用日志转发，也没有 HTTP 实时日志接口。Go package 默认保持静默，可选配置 `Config.LogWriter`（必须并发安全且快速返回）。

## Go

### 运行命令

在项目根目录执行：

```bash
make build
./bin/script-agent run \
  --source examples/custom/example.go \
  --language go \
  --params '{"cluster_uuid":"cluster-001","dry_run":true}'
```

- 修改 [自定义脚本示例](../examples/custom/example.go) 的 `Run` 方法即可编写业务逻辑；`Prepare`、`PostRun` 继承 `sdk.BaseTask` 的空实现。
- `--source` 读取源码文件，`--language` 默认为 `go`，也支持 `shell`、`python`；`--params` 必须是 JSON Map，默认 `{}`，不是将参数拼接到源码中。
- 示例要求 `cluster_uuid` 为非空字符串，`dry_run` 为可选布尔值（默认 `true`）。示例只打印日志并返回参数，不实际操作 cluster。
- 输出为完整结果 JSON：成功时 `status` 为 `succeeded`，业务数据位于 `outcome.data`，日志位于 `outcome.stdout`；缺少 `cluster_uuid` 会返回失败结果，退出码为 1。
- 这是 `package usercode` 脚本，由 Agent 动态加载，不能直接使用 `go run examples/custom/example.go`。修改脚本后重新执行命令即可，无须重新构建 Agent。
- `--source` 与 `--file` 不能同时使用；`--file` 仍用于完整请求 JSON。需要指定超时、回调或 Shell 三阶段源码时，使用完整请求方式。

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

需要自定义时覆盖 `Prepare`、`PostRun` 方法，参见 [完整三阶段 Go 示例](../examples/scripts/task.go)。必须是 `package usercode`，工厂签名为 `New() sdk.TaskRunner`。

Agent 预注册 SDK 和 Yaegi 接口包装器，动态加载源码并创建任务实例；同一 Task 的三个方法共享同一对象和参数 Map，不同 Task 使用独立进程/解释器。不需要用户编译插件或部署 Worker。标准输出用于日志；用户创建的 goroutine 必须在所属阶段结束前退出。

### 校验、失败与取消

运行中的业务错误放入 `Outcome.Error`，不要再返回第二个 error：

```go
return sdk.Outcome{
    Status: sdk.StatusFailed,
    Error:  fmt.Errorf("cluster operation failed"),
}
```

长循环和外部请求应检查或传递 `ctx`；可捕获 panic 会转为失败并保留 stack。不要调用 `os.Exit`，它会跳过清理。返回 succeeded 不会自动检查外部系统是否真的完成操作，需要脚本自行验证。

Go 使用 Yaegi v0.16.1 解释运行，Agent 内置 SDK；当前以标准库脚本为支持范围，不自动下载第三方包，不承诺兼容全部 Go 语法、CGo 或泛型。无需生成 `.so` 或对每个脚本执行 `go build`。

完整三阶段源码可以直接运行：

```bash
./bin/script-agent run \
  --source examples/scripts/task.go \
  --language go \
  --params '{"cluster_uuid":"cluster-002"}'
```

## Python

### 运行命令

示例文件：[example.py](../examples/custom/example.py)。只需实现 `run(params)`，省略的 prepare/post_run 默认为空实现。

```bash
./bin/script-agent run \
  --source examples/custom/example.py \
  --language python \
  --params '{"cluster_uuid":"cluster-002","dry_run":true}'
```

需要 Python 3。Agent 动态加载源码并调用入口；直接 `python3 examples/custom/example.py` 只定义函数，不会运行 Task。
示例返回参数并打印三条日志，业务数据位于 `outcome.data`，分条日志位于 run 阶段的 `logs`。

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

### 校验和失败

参数缺失或业务失败可直接 `raise ValueError("原因")`；也可以显式返回以下 Outcome：

```python
return {"status": "failed", "data": None, "error": "cluster operation failed"}
```

不要返回普通业务 dict 或只返回 True/False；新入口必须返回 Outcome。日志使用 `print`，错误日志可用 `print("原因", file=sys.stderr)`（先 import sys）；不要把完整 Outcome 打印到 stdout 代替 return。

需要依赖时由部署环境预先安装，Agent 不负责 pip install。使用 `--python /absolute/path/to/python3` 可选择解释器（Shell 脚本自行调用的 python3 不受此参数控制）。不支持 async def 入口；用户线程和子进程须在阶段结束前停止。

## Shell

### 运行命令

在项目根目录执行；这个最小示例只需要 Bash，不依赖 Python 或 jq：

```bash
make build
./bin/script-agent run \
  --source examples/custom/example.sh \
  --language shell \
  --params '{"cluster_uuid":"cluster-002","dry_run":true}'
```

完整脚本 [example.sh](../examples/custom/example.sh)：

```bash
#!/usr/bin/env bash
set -Eeuo pipefail

echo "cluster_uuid=${PARAM_CLUSTER_UUID:-}"
echo "dry_run=${PARAM_DRY_RUN:-true}"
```

- 直接使用 Agent 注入的环境变量，不需要解析 JSON、不使用 eval；`--params '{}'` 也可以运行。示例中的 `:-` 为缺失或空字符串提供默认显示值，不会把 false 当作空值。
- 终端会显示 `cluster_uuid=cluster-002`、`dry_run=true`，两条日志分别保存在 run 阶段的 `logs`，汇总保存在 `outcome.stdout`。
- 本示例只验证传参和日志功能，不写 `SCRIPT_RESULT_FILE`，因此 `outcome.data` 为 null；不执行 cluster 操作。
- 使用 Agent 运行，不需要可执行权限。Prepare、PostRun 默认空实现；需要三阶段和结果文件时再参考 `examples/shell-request.json`（该进阶示例需要 Python 3）。

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

Shell 源码支持 LF、CRLF 和混合换行。Agent 在写入各阶段脚本文件前，将 `prepare_source`、`source`、`post_run_source` 中的 CRLF（`\r\n`）统一转换为 LF（`\n`），无需手动转换 Windows 格式文件。单独的 `\r`、参数值及 Go/Python 源码保持不变；CLI、HTTP 和 Go package 使用同一处理逻辑。

- 每阶段为独立 Bash 进程，共享当前 Task 工作目录；文件可以跨阶段传递，shell 变量和 export 不会自动跨进程保留。
- 所有阶段均可通过 `PARAM_*` 环境变量读取参数，也可通过 `SCRIPT_PARAMS_FILE` 读取 JSON Map（保留 `BATCH_PARAMS_FILE` 兼容别名），不通过 eval 或源码插值传参。
- Run 退出码 0 表示成功，非零表示失败；Agent 将其转换成统一 Outcome。
- Run 可将业务结果 Map/null 写到 `SCRIPT_RESULT_FILE`，这里仍是 **Data，不是整个 Outcome**。无结果文件时 Data 为 null；无效 JSON 判失败；Prepare 遗留的结果文件在 Run 前清除。
- PostRun 从 `SCRIPT_OUTCOME_FILE` 读取完整主结果，其中 error 为字符串或 null。post-run 非零退出仅表示清理失败。
- Bash 使用 `-Eeuo pipefail` 和 ERR trap 记录失败行号/退出码。条件语句等存在 errexit 例外，用户仍需检查业务结果，不得关闭严格模式或留下后台任务。

读取并打印参数见 [最小 Shell 脚本](../examples/custom/example.sh)，只需 Bash。需要解析 JSON 字段、写入业务结果或执行三阶段时，可参考 [进阶 Shell 请求](../examples/shell-request.json)，该进阶示例用 Python 3 解析 JSON。

### 业务代码与失败处理

最小示例通过 `PARAM_CLUSTER_UUID` 直接读取顶层参数；不会自动生成不带前缀的 `cluster_uuid` 变量。嵌套对象/数组的环境变量值仍是 JSON，实际业务若需提取其内部字段，应使用可靠的 JSON 解析工具；不要使用 eval 或用正则模拟通用 JSON 解析，传给命令的变量始终使用双引号。

显式业务检查示例（不是最小示例的一部分）：

```bash
if [[ -z "${PARAM_CLUSTER_UUID:-}" ]]; then
  printf 'cluster_uuid is required\n' >&2
  exit 1
fi
```

退出 0 不代表外部操作必然成功；需要检查业务结果。不要用 `|| true` 掩盖失败，也不要关闭严格模式。业务结果文件必须包含有效 JSON Map/null，不是日志或整份 Outcome。
stdout/stderr 只用于日志，即使打印 JSON 也不会被当作业务返回值；未写结果文件时 `outcome.data` 为 null。

只传 `--source example.sh` 时，脚本整体就是 run；在文件中定义 `prepare()`、`post_run()` 不会被 Agent 自动调用。需要三阶段时使用 `prepare_source`、`source`、`post_run_source`，参考现有完整请求。

可通过 `--bash /absolute/path/to/bash` 选择 Bash；不需要 chmod +x，也不要使用 `sh example.sh` 代替 Agent 的生命周期执行。

<a id="lifecycle"></a>
## 三阶段生命周期

| 阶段 | Agent 职责 | 用户钩子 |
|---|---|---|
| `prepare` | 校验请求、准备隔离工作目录、加载代码 | 参数/业务检查、准备资源；默认空实现 |
| `run` | 执行业务、采集 Outcome、处理超时和取消 | 必需，返回明确的执行结果 |
| `post-run` | 尝试用户清理，随后发送平台回调 | 用户清理默认空实现，不能替代平台回调 |

- Prepare 失败或 panic：跳过 Run，仍尝试 PostRun；清理逻辑必须能处理只完成了一半的准备。
- Run 错误或可捕获 panic：构造失败 Outcome，再传给 PostRun。
- 用户 PostRun 使用独立超时，不复用已经取消的执行 Context。正常返回的 Run 结果在调用 PostRun 前保存，清理失败/超时不覆盖它。
- `status` / `outcome` 表示 Prepare/Run 的最终结果；不再返回 `user_post_run`。用户清理仍会执行，错误信息和堆栈保存在 post-run 阶段的 `error`、`stack`；`callback` 保存平台回调结果。任一清理/回调失败会将 post-run 阶段标为 failed，但不会重跑脚本。
- 未定义的 Python/Shell 钩子及旧 Go Handle 的清理跳过；Go BaseTask 的空方法正常返回。清理、回调均成功或跳过时，post-run 阶段为 succeeded。
- 已进入 post-run 后，不再接受 HTTP 取消；package 调用方的 Context 取消也不会取消已经开始的用户清理或系统回调。
- **用户清理不保证一定执行**：源码加载失败、进程被 SIGKILL/OOM 杀死、节点故障可能使 Go/Python 无法运行 PostRun。Agent 仍在运行时会尽力发送平台回调；不能把关键回调只放在用户代码里。
- HTTP 400 拒绝的请求不创建 Task，也不回调。已接收但 prepare 失败的 Task 仍进入平台 post-run；未授权回调地址不会被访问。

### 阶段日志

`phases` 中每个阶段包含 `logs` 数组（无日志时为 `[]`），支持多条记录；例如一个 run 阶段：

```json
{
  "name": "run",
  "status": "succeeded",
  "started_at": "2026-09-10T06:49:41.764838Z",
  "finished_at": "2026-09-10T06:49:41.765089Z",
  "logs": [
    {"stream": "stdout", "message": "start cluster-002"},
    {"stream": "stdout", "message": "processing cluster-002"},
    {"stream": "stdout", "message": "finished cluster-002"}
  ],
  "logs_truncated": false
}
```

- `stream` 为 stdout/stderr，`message` 为日志文本，不包含行末换行符。按行收集，未换行的尾部在阶段结束时保存，超过 4 KiB 的长行拆成多条记录。
- 每阶段 stdout/stderr 合计最多保留 64 KiB、1024 条记录，超限设置 `logs_truncated: true`；阶段间分别计数，不影响实时转发。
- 日志按所属阶段归档；两个输出流之间不保证严格顺序。脚本自行缓冲的内容应在阶段结束前 flush，后台 goroutine/子进程应在所属阶段结束前停止。
- 保留 `outcome.stdout` / `outcome.stderr` 的汇总字段。阶段失败时还可查看该阶段的 `error`、`stack`（有值时返回）。CLI 最终结果和 HTTP 查询的完成结果包含阶段日志；HTTP 暂不提供运行中的日志订阅。

<a id="result"></a>
## 完整返回结果

以下是 custom Go 示例成功执行的完整 JSON（执行 ID、时间和耗时每次不同）。`phases[].logs` 按阶段分条记录，`outcome.data` 是业务返回值，不再输出 `user_post_run`。

```json
{
  "task_id": "",
  "execution_id": "e972c78d1968996a6c2acd9bf4ffce22",
  "status": "succeeded",
  "phase": "post-run",
  "phases": [
    {
      "name": "prepare",
      "status": "succeeded",
      "started_at": "2026-09-10T07:39:44.075784Z",
      "finished_at": "2026-09-10T07:39:44.094535Z",
      "logs": [],
      "logs_truncated": false
    },
    {
      "name": "run",
      "status": "succeeded",
      "started_at": "2026-09-10T07:39:44.094535Z",
      "finished_at": "2026-09-10T07:39:44.094787Z",
      "logs": [
        {
          "stream": "stdout",
          "message": "start cluster_uuid=cluster-002"
        },
        {
          "stream": "stdout",
          "message": "processing cluster_uuid=cluster-002 dry_run=true"
        },
        {
          "stream": "stdout",
          "message": "finished cluster_uuid=cluster-002"
        }
      ],
      "logs_truncated": false
    },
    {
      "name": "post-run",
      "status": "succeeded",
      "started_at": "2026-09-10T07:39:44.094787Z",
      "finished_at": "2026-09-10T07:39:44.097013Z",
      "logs": [],
      "logs_truncated": false
    }
  ],
  "outcome": {
    "status": "succeeded",
    "data": {
      "cluster_uuid": "cluster-002",
      "dry_run": true
    },
    "exit_code": 0,
    "stdout": "start cluster_uuid=cluster-002\nprocessing cluster_uuid=cluster-002 dry_run=true\nfinished cluster_uuid=cluster-002\n",
    "stderr": "",
    "stdout_truncated": false,
    "stderr_truncated": false,
    "duration_ms": 20,
    "error": null
  },
  "callback": {
    "status": "skipped",
    "attempts": 0,
    "http_status": 0
  }
}
```

`status` / `outcome` 代表主任务结果；主任务成功但 post-run 清理或回调失败时，顶层 status 仍可能是 succeeded，但 post-run 阶段为 failed、CLI 退出码为 1。主阶段错误位于 outcome.error 及对应阶段 error；清理错误只在 post-run 阶段，不覆盖主结果。

<a id="limits"></a>
## 参数、超时与兼容性

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

<a id="integration"></a>
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

不想单独部署 helper 二进制时，可以复用调用方自身的二进制：在 `main()` 启动业务服务之前调用 `HandleHelperCommand(ctx, os.Args[1:])`，命中后 `os.Exit(code)`；将 `os.Executable()` 的路径传入 `Config.GoExecutable`。参见 [完整嵌入示例](../examples/embedded/main.go)。Go 的包级 `init()` 总会先执行，有启动副作用的宿主项目更适合使用独立 helper。

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
- 回调 body 包含 task_id、execution_id、outcome、phases（含用户清理错误和阶段日志），不再包含 user_post_run。phases 是发起回调前的快照：post-run 尚未结束，不包含本次回调自身的最终结果。平台不会单独附加源码、完整入参或回调 token；用户主动返回/打印的敏感信息仍需调用方治理。
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
  "phases": [
    {
      "name": "prepare",
      "status": "succeeded",
      "started_at": "2026-09-10T06:49:41.750000Z",
      "finished_at": "2026-09-10T06:49:41.752000Z",
      "logs": [],
      "logs_truncated": false
    },
    {
      "name": "run",
      "status": "succeeded",
      "started_at": "2026-09-10T06:49:41.752000Z",
      "finished_at": "2026-09-10T06:49:41.762000Z",
      "logs": [{"stream": "stdout", "message": "hello"}],
      "logs_truncated": false
    },
    {
      "name": "post-run",
      "status": "running",
      "started_at": "2026-09-10T06:49:41.762000Z",
      "finished_at": null,
      "logs": [],
      "logs_truncated": false
    }
  ]
}
```

<a id="security"></a>
## 安全边界与验证

子进程/Yaegi **不是安全沙箱**。当前只清理继承环境、隔离临时目录、限制日志/结果读取、处理进程组超时与取消；没有 CPU/内存/磁盘硬配额，不能阻止同 UID 文件访问、网络访问或恶意进程逃离进程组。程序仍使用宿主用户权限。默认不向公网开放；不可信代码必须放入独立低权限容器/沙箱，限制网络、凭证、资源和宿主挂载。不要向脚本暴露生产凭证或宿主目录，环境清理不能代替权限隔离。

```bash
make test
make cicd
```

测试覆盖 TaskRunner 动态加载和默认空钩子、同实例状态、Outcome error 编解码、旧入口兼容、三语言阶段失败、独立清理超时、取消后回调、Map、日志、HTTP 容量及关停。测试需要本机 Go、Bash 和 Python 3。
