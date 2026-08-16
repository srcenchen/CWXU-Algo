# 规格：goalgo-ops 交互式简化

> 日期：2026-08-16
> 状态：已确认
> 范围：`cwxu-algo/cmd/goalgo-ops` + `internal/` 相关包 + `deploy/tests` 契约测试

## 1. 背景与目标

当前 `goalgo-ops` 各命令缺参数时直接报错退出，`install` 全程静默、无步骤提示；首个管理员需额外执行 `admin-init`。目标：

1. 新增 `init`：把需要填写的 `.env` 信息暴露出来，用户填写后直接 `install`。
2. 简化 `install`：管理员创建并入，逐步输出进度提示。
3. 全部命令「缺参自动交互」：终端下缺必填项时交互式提示，非终端（CI/脚本）保留报错。

## 2. 命令结构

### 2.1 新增 `init [--root]`

- 生成/更新 `.env`（来自 `deploy/env.example`，首行 `GOALGO_ROOT=<root>`）。
- 双形态：
  - **交互式**（TTY）：逐字段提问，每项带默认值（回车即默认）；已存在 `.env` 时询问「保留 / 重新填写」。
  - **模板**（非 TTY 或 `--print`）：把带注释说明的模板打印到 stdout，用户另存编辑。
- 只写 `.env`（0600），不触碰 secrets/release.env；幂等。
- 可填字段（均带默认值）：
  - `COMPOSE_PROJECT_NAME`（默认 `goalgo`）
  - `GOALGO_HTTP_BIND`（默认 `0.0.0.0`）
  - `GOALGO_HTTP_PORT`（默认 `8988`）
  - `GOALGO_ROOT`（默认 `/opt/goalgo`）
  - `TZ`（默认 `Asia/Shanghai`）
  - `POSTGRES_USER`（默认 `goalgo`）
  - `RABBITMQ_USER`（默认 `goalgo`）

### 2.2 `install` 简化

`opsinstall.Install`（目录结构 + 随机密钥 + JWT 密钥对 + `secrets/app.env` + ownership + marker）保持不变，但在外层命令中输出分步进度。随后顺序：

1. `.env` 已由 `init` 填写；若缺失关键字段则交互补全。
2. release 来源：`--release-file` 指定；否则默认 `ResolveLatest`（解析 `<svc>-latest`，上轮已实现），交互可改选。
3. 首个管理员：`state/install.json` 未标记 `adminCreated` 时交互创建（复用 `promptAdminConfig` 逻辑，下沉到 `internal/opsadmin`）；创建成功后写回 `install.json` 标记。`--admin-config` 保留非交互路径。
4. 启动服务（docker compose up）+ 健康检查 + 冒烟（现有逻辑不变）。

### 2.3 `admin-init` 保留

作为非交互/脚本入口，与 install 内嵌逻辑共享 `internal/opsadmin`。

## 3. 交互式基础设施 `internal/opsprompt`

- `String(提示, 默认) string`
- `Password(提示) string`（掩码 + 两次确认）
- `Confirm(提示) bool`
- `Choice(提示, 选项...) int`
- 非 TTY 时返回 `ErrNonInteractive`：调用方转为明确报错「缺少必填项，请在终端运行或提供参数」。
- 迁移 `cmd/goalgo-ops/admin.go` 现有 `promptAdminConfig` 到 `internal/opsadmin`；`admin-init` 与该包共享。
- `internal/opsinstall` 的 `state/install.json` 增加 `adminCreated` 字段，由 install/admin-init 共同维护。

## 4. 缺参自动交互矩阵

仅「缺必填项且 TTY」时触发；有参数/环境变量时行为不变。

| 命令 | 交互补全 |
|---|---|
| `init` | 逐字段填写 .env |
| `install` | release 来源选择 + 管理员 |
| `deploy` | 未给候选文件时选择（默认当前 release.env） |
| `restore` | 归档来源（本地 `*.cwxubak` 列表 / `--latest`）、密钥文件路径；覆盖仍须输入 `RESTORE` |
| `backup verify` | 归档与密钥文件路径 |
| `config export` | 输出路径（默认 `./goalgo-config.tar`） |
| `config import` | 导入 tar 路径 |
| 其余（validate/start/stop/restart/status/logs/doctor） | 无新增 |

## 5. 步骤/进度提示 `internal/opsprogress`

- `Step(current, total, message)`：`[2/5] 生成密钥与 JWT 密钥对`，输出到 stderr。
- `Done(message)`：成功摘要到 stdout。
- `Fail(phase, err)`：统一失败（复用 `fail` 语义，`goalgo-ops: <阶段>: ...`）。
- `install` 使用固定 5 步：初始化目录 / 生成密钥与 JWT / 解析发布镜像 / 创建管理员（如无）/ 启动并冒烟。
- **镜像拉取与容器创建也必须有进度提示**：第 5 步内拆出可见子步骤——
  - `[5.x] 拉取发布镜像`（`compose pull` 前输出，注明 digest/标签来源）
  - `[5.x] 创建并启动容器`（`compose up` 前输出）
  - `[5.x] 等待健康并冒烟`（`up --wait`、`Health`、`Smoke` 前输出）
- 对 `install`、`deploy`、`restart`、`rollback` 等会执行 `Pull`/`Up` 的命令，同样在 `compose.Pull` 与 `compose.Up` 前输出对应子步骤提示；避免长时间静默。
- `init`、`restore` 在关键阶段同样输出阶段行；`restore` 现有 `printf` 步骤提示保留并统一格式。

## 6. 错误与安全

- 交互输入非空校验；密码掩码输入（`golang.org/x/term`，已依赖）。
- 破坏性操作（restore 覆盖）仍须显式确认 token，不因交互放宽。
- `.env` 0600；模板注释标明字段用途；不写入任何密钥。
- 非 TTY 缺参 → 明确报错，不阻塞等待输入。

## 7. 测试

- `internal/opsprompt`：默认值、非 TTY `ErrNonInteractive` 单测。
- `internal/opsprogress`：输出格式单测。
- `internal/opsinstall`：`adminCreated` 标记读写单测。
- `internal/opscompose`：`Pull`/`Up` 进度输出（若在 compose 层实现）或调用点提示的单测。
- 命令层：注入 fake runner（沿用 `opsexec` 模式）测「缺参 → 交互提示 / 报错」分支。
- `deploy/tests/test_operations.py`：断言 usage 含 `init`；install 帮助含 `--release-file`；非交互提示存在。

## 8. 非目标（YAGNI）

- 不新增图形化向导 / TUI。
- 不扩展 `.env` 字段（仅现有字段，密钥仍自动生成）。
- 不改变 release 不可变（digest）部署模型。
- 不把交互做成强制（脚本保持非交互兼容）。
