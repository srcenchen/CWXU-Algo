# GoAlgo 容器化与 CI/CD 设计

> 状态：已确认，作为容器化、GitHub Actions、生产部署与 `goalgo-ops` 的实施规格。

## 1. 目标与边界

- Debian 12 新机可通过一个入口完成空系统安装或备份恢复安装。
- 生产机不拉源码、不编译，只从阿里云 ACR 拉取不可变镜像。
- Docker Compose 管理前端、4 个后端服务和全部中间件。
- 后端 Docker、Compose、Nginx 和保留的部署配置位于后端仓库 `deploy/`。
- 前端仓库拥有自己的 Dockerfile 和 Actions，前后端镜像独立构建发布。
- 公网入口为纯 HTTP，默认 `0.0.0.0:8988`；HTTPS 由外部反向代理提供。
- 图片继续使用现有云存储，不恢复本地上传目录。
- PostgreSQL 全实例备份继续由 `core_data` 内置任务每日 02:00 执行。

## 2. 目录结构

```text
deploy/
├── compose.yaml
├── compose.bootstrap.yaml
├── env.example
├── release.env.example
├── docker/
│   ├── backend.Dockerfile
│   ├── frontend.Dockerfile
│   ├── nginx.conf
│   └── .dockerignore
├── config/
│   ├── gateway.yaml
│   ├── user.yaml
│   ├── core-data.yaml
│   └── agent.yaml
├── scripts/
│   ├── bootstrap.sh
│   ├── deploy.sh
│   ├── rollback.sh
│   ├── restore.sh
│   ├── start.sh
│   ├── stop.sh
│   ├── restart.sh
│   ├── status.sh
│   ├── logs.sh
│   └── doctor.sh
└── systemd/
    └── goalgo-runner.service.example
```

阶段 4 再根据真实命令补充最终运维文档，不在容器化前预写未经验证的操作步骤。

## 3. Compose 架构

### 3.1 服务

应用镜像各自独立：

- `frontend`
- `gateway`
- `user`
- `core-data`
- `agent`

基础设施：

- PostgreSQL 18 + pgvector
- Redis
- RabbitMQ
- Consul
- Nginx HTTP 入口

### 3.2 网络与入口

- Nginx 默认绑定 `${GOALGO_HTTP_BIND:-0.0.0.0}:${GOALGO_HTTP_PORT:-8988}`。
- `/api/*` 转发到 Gateway。
- SPA 静态资源由 frontend 镜像提供。
- SEO 分享路由保留现网语义并转发到 User。
- PostgreSQL、Redis、RabbitMQ、Consul和业务服务端口默认不暴露宿主机。
- 中间件管理端口仅通过显式 Compose profile 开启。
- Compose 不申请证书、不监听 443。

### 3.3 数据

宿主机固定目录：

```text
/opt/goalgo/data/postgres
/opt/goalgo/data/redis
/opt/goalgo/data/rabbitmq
/opt/goalgo/data/consul
```

PostgreSQL 是灾备权威数据。Redis、RabbitMQ 和 Consul 可重建，不进入核心数据库恢复包。

## 4. 镜像构建

### 4.1 后端

`deploy/docker/backend.Dockerfile` 使用多阶段构建并提供 4 个 target。每个运行镜像只包含对应服务二进制、CA 证书和时区。

`core-data` 额外包含 PostgreSQL 18 官方客户端和 zstd，供内置备份任务调用；镜像不启动额外 PostgreSQL 服务。

所有服务以非 root 用户运行。生产密钥、配置文件和数据库 DSN 只能在运行时挂载或注入，不进入镜像层。

### 4.2 前端

前端仓库的 GitHub Actions 使用当前提交。Node 构建阶段执行 `npm ci` 和 Vite 构建，运行阶段只包含静态产物和 Nginx。

正式前端 Dockerfile 位于前端仓库根目录；后端 `deploy/docker/frontend.Dockerfile` 仅作为保留的部署配置，不再被 Actions 使用。

## 5. 阿里云 ACR

实际资源：

```text
地域：华东1（杭州）
Registry：registry.cn-hangzhou.aliyuncs.com
命名空间：sanenchen
仓库：goalgo
可见性：公开
```

单仓库通过标签区分服务：

```text
frontend-sha-<frontend-commit>
gateway-sha-<backend-commit>
user-sha-<backend-commit>
core-data-sha-<backend-commit>
agent-sha-<backend-commit>
```

各 workflow 同时更新对应服务的 `*-latest` 标签。

仓库保持公开，因此镜像内严禁包含任何生产凭据、私钥、配置文件或证书。GitHub Actions 推送仍使用 Repository Secrets：

```text
ACR_USERNAME
ACR_PASSWORD
```

非敏感配置使用各仓库的工作流常量：

```text
ACR_REGISTRY=registry.cn-hangzhou.aliyuncs.com
ACR_IMAGE=registry.cn-hangzhou.aliyuncs.com/sanenchen/goalgo
```

## 6. GitHub Actions

后端正式仓库：`WXUProjects/CWXU-Algo`；前端正式仓库：`WXUProjects/AlgoAnalysisFront-new`。

### 6.1 前端镜像

前端 `main` push 直接运行 `front-image`，构建并推送 `frontend-sha-<frontend-commit>` 与 `frontend-latest`。流程不 checkout 后端仓库。

### 6.2 后端镜像

后端 `main` push 直接运行 `back-image`：用 matrix 拆成 gateway、user、core-data、agent 四个并行 job，各自构建并推送 commit 标签（`gateway-sha-*` 等），缩短整体构建时间。四个 job 全部成功后，`promote-latest` 统一提升 `*-latest` 标签，避免构建失败时发布一组混合版本。流程不 checkout 前端仓库，不再等待前置 CI，也不生成发布清单。

### 6.3 生产发布

自动生产部署已停用。原 workflow 保存在 `.github/workflows/production.yml.disabled`，不会被 GitHub Actions 加载；Compose、部署脚本和 `goalgo-ops` 仍保留供手工运维使用。

## 7. 新机初始化

服务器根目录：

```text
/opt/goalgo/
├── compose.yaml
├── .env
├── release.env
├── release.previous.env
├── config/
├── secrets/
├── restore/
├── logs/
├── state/
└── data/
```

生产密钥通过 `goalgo-production.tar.age` 导入。解密口令交互读取，不进入命令参数。`secrets/` 权限为 0700，密钥文件为 0600。

## 8. goalgo-ops

Go CLI 源码位于后端 `cmd/goalgo-ops/`。第二阶段 shell 脚本最终只作为薄入口。

### 8.1 安装模式

空系统：

```bash
goalgo-ops install
```

交互输入管理员用户名、密码、密码确认和邮箱；展示名默认用户名。初始化命令负责 bcrypt 密码、站点管理员标记、RBAC 系统角色、公共域成员和默认成员组。

无人值守：

```bash
goalgo-ops install --admin-config /root/goalgo-admin.env
```

文件必须 root 所有、权限 0600、非符号链接；创建成功后删除。密码不支持命令行参数。

远端最新备份安装：

```bash
goalgo-ops install --restore-latest
```

本地备份安装：

```bash
goalgo-ops install --restore-file ./backup.cwxubak
```

恢复安装不创建管理员，账号以备份为准。

### 8.2 本地备份导入

空实例：

```bash
goalgo-ops restore --file ./backup.cwxubak
```

覆盖已有系统：

```bash
goalgo-ops restore --file ./backup.cwxubak --replace --confirm RESTORE
```

不支持两套数据库合并。恢复前验证格式、SHA-256、HMAC、AES-GCM、zstd、tar、manifest 和 `pg_restore --list`。失败时不启动半恢复系统。

### 8.3 其他命令

```text
backup run/status/download/verify
deploy
rollback
start
stop
restart
status
logs
doctor
config import/export/validate
```

所有命令使用 `/run/lock/goalgo-ops.lock` 防并行。普通 stop 不删除数据卷。破坏性操作必须显式确认。

## 9. 迁移与回退

迁移顺序：

1. 旧机手动备份并等待成功。
2. 新机从最新备份恢复并以 `0.0.0.0:8988` 启动。
3. 完成登录、题库、博客、爬虫和备份状态 smoke test。
4. 外部反向代理切换到新机 8988。
5. 旧机停止应用但保留 24 至 72 小时。

切流失败时只将反向代理切回旧机，不把新机数据反向合并到旧机。再次迁移前从旧机生成新备份。

## 10. 实施阶段

1. 阶段 2A：镜像、Compose、Nginx、健康检查和本地全栈测试。
2. 阶段 2B：前后端独立 GitHub Actions 与 ACR 镜像推送。
3. 阶段 2C：旧机临时端口恢复演练，不切生产流量。
4. 阶段 3：实现 `goalgo-ops` 和首个管理员初始化命令。
5. 阶段 4：基于真实运行结果编写最终运维文档。

## 11. 验收标准

- 干净 Debian 12 可完成空系统安装并创建首个管理员。
- 可从又拍云最新备份或本地 `.cwxubak` 恢复。
- `0.0.0.0:8988` 提供完整 HTTP 服务。
- 所有 Compose 服务有健康检查。
- 数据卷在停止、重启和应用回滚后保持不变。
- PR 无法访问生产 Runner 和 ACR Secrets。
- 前端 main 自动发布前端镜像，后端 main 自动发布四个后端镜像。
- 前后端 Actions 不跨仓库 checkout，也无前置 CI 或自动生产部署。
- 生产发布失败可以恢复上一版镜像。
- 备份、恢复和覆盖保护均经过真实 PostgreSQL 18 演练。
