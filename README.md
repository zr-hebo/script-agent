# Script-Agent

独立脚本执行组件：**Go 动态解释（Yaegi）、Bash、Python 3**。每个 Task 使用独立子进程和工作目录，参数统一为 JSON Map。

可作为二进制运行，也可直接作为 Go package 引入。**不依赖 Temporal，不修改批量平台，也不是安全沙箱。**

## 运行环境

- Linux / macOS，构建需要 Go 1.22+；本机验证工具链为 Go 1.26.1。
- Shell 需要 Bash；Python 需要 Python 3。只执行 Go 时不需要这两个解释器。
- Go 使用固定版本 Yaegi v0.16.1，无须为每份用户源码执行 `go build`；不承诺兼容全部 Go 语法、CGo、泛型或第三方依赖，当前以标准库脚本为支持范围。

## 快速开始

```bash
make build

# Go
./bin/script-agent run --source examples/custom/example.go --language go \
  --params '{"cluster_uuid":"cluster-002","dry_run":true}'

# Shell（只需 Bash，通过 PARAM_* 环境变量读取参数）
./bin/script-agent run --source examples/custom/example.sh --language shell \
  --params '{"cluster_uuid":"cluster-002","dry_run":true}'

# Python
./bin/script-agent run --source examples/custom/example.py --language python \
  --params '{"cluster_uuid":"cluster-002","dry_run":true}'
```

`run` 默认将日志实时输出到 stderr，stdout 仅输出最终 JSON。使用 `--stream-logs=false` 关闭实时日志。Go/Python 直接接收 Map 参数；仅 Shell 将 params 顶层字段映射为 `PARAM_*` 环境变量，例如 `cluster_uuid` → `PARAM_CLUSTER_UUID`。示例均不操作真实 cluster；Go/Python 返回参数，Shell 只 echo 参数值。

## 使用文档

详细用法统一维护在 [Usage 使用指南](docs/usage.md)：

| 内容 | 文档 |
|---|---|
| CLI、Map 参数、task_id、日志 | [通用调用](docs/usage.md#common) |
| Go 入口、TaskRunner、Outcome、错误处理 | [Go 用法](docs/usage.md#go) |
| Python 入口、返回值、三阶段钩子 | [Python 用法](docs/usage.md#python) |
| Shell 参数读取、结果文件、三阶段源码 | [Shell 用法](docs/usage.md#shell) |
| 阶段日志、完整 JSON、限制 | [生命周期](docs/usage.md#lifecycle) · [返回结果](docs/usage.md#result) · [限制](docs/usage.md#limits) |
| 嵌入调用、HTTP 服务、回调 | [平台接入](docs/usage.md#integration) |

## 构建、测试与 CI

| 命令 | 行为 |
|---|---|
| `make build` | 构建当前平台二进制，输出到 `bin/script-agent` |
| `make test` | 完整测试，启用竞态检测、禁用测试结果缓存，默认超时 90 秒 |
| `make cicd` | 依次执行格式检查、`make test`、`go vet`、`make build`；失败即停止 |

`make cicd` 不修改源码格式，不部署、不发布、不 push。测试需要本机 Go、Bash、Python 3，以及支持竞态检测的 CGO/C 工具链。

可以覆盖输出路径和测试超时；交叉编译仅使用 build，不运行目标平台测试：

```bash
make build BINARY=bin/script-agent-local
make test TEST_TIMEOUT=180s
GOOS=linux GOARCH=amd64 make build BINARY=bin/script-agent-linux-amd64
```

## 安全边界

子进程/Yaegi 不是安全沙箱。执行用户代码具有宿主用户的文件和网络权限，当前没有 CPU、内存、磁盘硬配额。生产环境执行不可信脚本时，应使用低权限容器/沙箱并限制网络、资源和凭证；不要向脚本暴露生产凭证或宿主目录。详见 [安全边界与验证](docs/usage.md#security)。
