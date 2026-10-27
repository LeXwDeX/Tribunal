# 部署与运维（Docker / SQLite）

本文对应 ARCH14：内嵌控制台的服务镜像、镜像内 `git` 与管理 CLI、许可证文件、SQLite 单实例、非 root 用户和可写数据目录。各版本做过哪些验证见[验证状态](#10-验证状态)。

交付前准备、升级/回退边界与真实验收记录见 [交付就绪与验收记录](delivery-readiness.md)。

> 当前版本是 v1.1.0，验证状态见 [当前版本（v1.1.x）验证状态](delivery-readiness.md#当前版本v11x验证状态)。从 v1.0.0 升级前必须先读 [从 v1.0.0 升级到 v1.1.0](#从-v100-升级到-v110)。由 agent 代为部署时，使用 [Agent 部署指南](agent-deployment.md)。

## Compose 快速部署（推荐）

前提：Docker Engine 或 Docker Desktop，以及 Compose v2.20 或更新版本。宿主机无需 Go 或 Node。Docker daemon 所在机器需要能读取仓库配置文件；本示例不是远程 Docker daemon 的文件分发方案。首次构建需要访问基础镜像、Debian 软件源、Go 模块源和 npm registry。

### 准备配置

在仓库根目录执行一次，不覆盖已有 `.env`：

```bash
cp -n .env.example .env
chmod 600 .env
```

设置以下值：

| 变量 | 含义 |
|---|---|
| `GITLAB_URL` | 容器能访问的 GitLab 根地址，例如 `https://gitlab.example.internal`，不是 `/api/v4` 地址 |
| `LLM_PROTOCOL` | `openai-chat`、`openai-responses` 或 `anthropic` |
| `LLM_BASE_URL` | 与协议匹配的模型 API 地址；OpenAI 兼容接口通常包括 `/v1`，Anthropic 使用 API 根地址 |
| `LLM_MODEL` / `LLM_API_KEY` | 供应方实际支持的模型 ID 和访问密钥 |
| `REVIEW_MASTER_KEY` | `openssl rand -base64 32`，仅首次生成，必须连同数据备份 |
| `REVIEW_ADMIN_TOKEN` | `openssl rand -hex 32`，管理 API 的 Bearer token |
| `TRIBUNAL_SESSION_KEY` | 单独执行 `openssl rand -base64 32`，跨重启保持不变 |
| `TRIBUNAL_PUBLIC_URL` | 用户和 GitLab 可访问的服务地址；生产应为 HTTPS，默认仅供本机使用 |
| `TRIBUNAL_HTTP_PORT` | 宿主机端口，默认 8080；容器始终监听 8080。改变它时检查 `TRIBUNAL_PUBLIC_URL` 是否也需修改 |

旧环境变量名 `CRC_SESSION_KEY`、`CRC_PUBLIC_URL`、`CRC_HTTP_PORT` 仍兼容；新旧同时设置时新名优先。三个旧名只要配置了非空值，都会各记录一次弃用日志；日志只含变量名、不含值。日志表示旧名仍配置，不代表旧值一定生效或一定被忽略：Compose 会把仅设置的 `CRC_HTTP_PORT` 作为 `TRIBUNAL_HTTP_PORT` 的插值回退值，仅用于宿主机端口映射，并把仅设置的 `CRC_SESSION_KEY` 复制给容器的 `TRIBUNAL_SESSION_KEY`。`TRIBUNAL_HTTP_PORT` / `CRC_HTTP_PORT` 只影响宿主机映射，不是应用监听端口。`TRIBUNAL_SESSION_KEY` 未设置或恰好为空字符串时才回退旧名；非空但只含空白是非法值，启动失败，不回退旧名，也不生成临时 key。完整兼容规则见[环境变量](design/06-config.md#5-环境变量)。升级时必须复用原 session key 的同一个值，不要重新生成，否则现有浏览器会话失效。

`.env` 值包含 `$` 或 `#` 时使用单引号。不要把真实密钥写进 YAML、提交到仓库或放进 Docker build args。Compose 从 `.env` 做变量替换，再通过 `environment` 允许列表传入容器，不会把整个 `.env` 挂载或注入。宿主机已导出的同名变量优先于 `.env`；部署前检查是否有旧值，但不要把秘密输出到日志。环境变量仍对 Docker 管理员可见。

`config/server.docker.yaml` 只读挂载到 `/app/config/server.yaml`。它绑定一个名为 `primary` 的 provider，`onboarding` 档位用其初始化仓库上下文，`default` 档位用同一个 provider 执行 `code-review` 和 challenge 核验；plan 和 synthesize 沿用代码模式，不默认启用多模型对抗。数据库与工作区分别使用 `/app/data/review.db` 和 `/app/data/workspaces`。其他参数沿用应用内置默认值。不要移除 `profiles.onboarding.agent.provider` 或 `profiles.default.challenge.provider`：前者会导致工作区初始化失败，后者会导致评审图构建失败，即使健康检查和 `--check-config` 通过。

### 构建并启动

```bash
docker compose config --quiet
docker compose build
docker compose run --rm --no-deps review --config /app/config/server.yaml --check-config
docker compose up -d --wait --wait-timeout 120
docker compose ps
docker compose logs --tail=100 review
docker compose exec -T review /app/tribunalctl projects all
```

`config --quiet` 检查 Compose 结构和必填变量，避免普通 `config` 将展开后的秘密打印到终端。`--check-config` 检查应用配置，但不验证 session key、外部凭据、完整运行时或网络连接；session key 在实际启动时校验。`up --wait` 还会等待镜像中的 `/readyz` 健康检查；就绪只证明数据库与 worker 可用，不证明模型或 GitLab 可用。

默认访问 `http://localhost:8080`。服务只向宿主机 `127.0.0.1` 发布端口；GitLab webhook 和远程浏览器需要第 6 节的 HTTPS 反向代理。容器内 `localhost` 指容器本身，不是宿主机或其他 GitLab 容器。内部 CA 需由部署方配置可信证书，不要关闭 TLS 校验。

### 浏览器登录与项目注册

OAuth 默认关闭，访问 `/auth/login` 得到 503 是预期行为，Bearer API 和 CLI 仍可使用。启用控制台登录时：

1. 在 GitLab 创建 OAuth Application，scope 为 `read_user`，回调 URI 为 `<TRIBUNAL_PUBLIC_URL>/auth/callback`。
2. 在 `.env` 设置 `GITLAB_OAUTH_CLIENT_ID`、`GITLAB_OAUTH_CLIENT_SECRET` 和 `GITLAB_ADMIN_USERNAME`。管理员值填写 `id:<GitLab 用户 ID>`；GitLab 用户名写法仍兼容但已弃用（见[用户名写法弃用与迁移](#bootstrap_admins-用户名写法弃用与迁移)）；留空不会给任何正常账号平台管理员标签。该标签只用于平台管理面，不授予项目业务数据访问，项目授权见下文。
3. 在 `config/server.docker.yaml` 取消 `oauth` 及两个子字段、`bootstrap_admins` 的注释。设置正确的 HTTPS `TRIBUNAL_PUBLIC_URL`，配置反向代理，再执行 `docker compose up -d --force-recreate --wait --wait-timeout 120`。

模型密钥不是 GitLab 项目 token。注册项目需要单独的 `GITLAB_PROJECT_TOKEN`，由安全环境注入当前 shell，scope 为 `api` 和 `read_repository`。该变量不放进长期运行服务的 `.env`，仅传给这次 CLI 进程：

```bash
docker compose exec -T -e GITLAB_PROJECT_TOKEN review /app/tribunalctl projects register \
  --project group/backend-api \
  --token-env GITLAB_PROJECT_TOKEN \
  --profile default
docker compose exec -T review /app/tribunalctl projects all
```

注册会访问真实 GitLab 并创建后台 onboarding 任务，可能产生模型费用。默认需要手工创建 webhook，注册返回的 `webhook_secret` 只显示一次，请安全保管。要自动创建，增加 `--auto-webhook`，项目 token 还需 Maintainer 权限，`TRIBUNAL_PUBLIC_URL` 必须可由 GitLab 访问。完整权限与 webhook 事件见 [README 注册项目](../README.md#注册项目)。

### 项目授权、用户停用和凭据撤销

项目访问是 Tribunal 本地的 `project_grants`，不从 GitLab 组成员关系推导。`bootstrap_admins` 只决定平台管理标签；平台管理员读取或写入项目业务数据同样需要该项目授权。默认没有任何授权，`admin` 标签也不例外。无授权或项目不存在统一返回 404，只有 `read` 授权执行写操作返回 403。

**迁移已有部署并首次授权**

`0007_project_access.sql` 为现有 `users` 行补 `disabled=0`，并新增空的 `project_grants` 表；不会为任何用户创建 grant。升级后已有用户默认看不到任何项目，需按以下步骤逐个授权；新部署同样从零开始。

1. 让目标用户先通过 OAuth 登录一次。没有登录过的 GitLab 用户不在本地 `users` 表中，无法预先授权。
2. 用平台管理员 Bearer token 列出本地用户和全部已登记项目：

   ```bash
   docker compose exec -T review /app/tribunalctl users list
   docker compose exec -T review /app/tribunalctl projects all
   ```

   `projects list` 按 bearer 所属用户的项目授权过滤；bootstrap token 没有用户 ID 和 grant，通常返回空列表，不能用它确认“没有项目”。`projects all` 和 `users list` 是管理面列表，需要平台管理员凭据。
3. 授予只读或管理权限，并核对结果：

   ```bash
   docker compose exec -T review /app/tribunalctl projects grants list 42
   docker compose exec -T review /app/tribunalctl projects grants set 42 17 read
   docker compose exec -T review /app/tribunalctl projects grants set 42 17 manage
   docker compose exec -T review /app/tribunalctl projects grants delete 42 17
   ```

   参数依次是项目 ID、GitLab 用户 ID 和角色。`read` 可读取项目、评审、工作区、统计、SSE 和项目扩展；`manage` 还可修改项目设置、触发/取消评审、重新发布、人工标注、刷新工作区、轮换项目 Key 和重建 webhook。项目级 MCP/Skill/块绑定属于宿主执行配置，写入还需平台管理员标签，`manage` 本身不授予平台配置能力。
4. 让用户用自己的 API token 验证业务列表：

   ```bash
   docker compose exec -T -e USER_TOKEN review /app/tribunalctl \
     --api-token-env USER_TOKEN projects list
   ```

   用户 token 在控制台“系统 → API Token”中创建，默认 90 天、最长 365 天；历史无过期时间的 token 在撤销前继续有效。

**GitLab 退组、停用和撤销**

- 从 GitLab 组移除用户不会自动撤销 Tribunal 授权。Tribunal 不轮询组成员关系；本地 grant、会话和 API token 仍然有效，GitLab 账号未禁用时还可能重新 OAuth 登录。退组后必须显式处理：删除项目 grant（保留其他项目访问）或停用本地用户（撤销全部访问）。
- `users disable <id>` 在一个事务中标记账号 disabled 并删除该用户的全部会话和 API token；之后的 OAuth 登录、token 签发和已有凭据认证都会被拒绝，已打开的 SSE 在下一次轮询关闭。
- `users revoke <id>` 只撤销会话和 API token，不禁用账号；用户重新 OAuth 登录后可以继续使用已有项目 grant。需要阻止重新登录时使用 `users disable`。
- `users enable <id>` 恢复登录，但不会恢复已撤销的会话或 token。恢复后需重新登录并重新创建 token；项目 grant 不受停用影响。
- 普通用户可以在控制台撤销自己的 API token，不能停用账号、撤销他人凭据或管理 grant。跨用户停用、撤销和 grant 管理只允许平台管理员。

**bootstrap 凭据轮换**

`REVIEW_ADMIN_TOKEN` 由部署方生成、保存和轮换，Tribunal 不提供在线轮换 API。修改 `.env` 或 secret 注入后执行 `docker compose up -d --force-recreate --wait --wait-timeout 120`，重启后旧 token 立即失效；不要用数据库操作或用户 grant 替代这次轮换。`bootstrap_admins` 是 `id:<GitLab 用户 ID>` 或 GitLab 用户名的列表，修改后同样需要重启服务，不会创建 GitLab 账号，也不等于项目授权。

### 停止、修改和备份

```bash
docker compose stop review
docker compose start review
docker compose down
```

`down` 保留命名卷；**不要执行 `down -v`**，它会删除数据库和工作区。默认项目名仍为 `crc-review`，数据卷仍为 `crc-review_data`；这是刻意保留的兼容身份，改名会创建新卷并让现有数据不可见。若用 `-p` 更换项目名，也会使用另一数据卷。不要同时启动多个实例共享同一卷。

修改 `.env` 或配置后使用 `docker compose up -d --force-recreate --wait --wait-timeout 120`。`restart` 不会加载新的容器环境变量。升级程序需重新构建镜像，并按第 8 节先备份；不支持只回退镜像却保留升级后的数据库。

Compose 的停机一致性备份示例（先准备 `/srv/crc` 并限制备份访问；该目录是部署方自有的兼容路径，不随产品改名迁移）：

```bash
docker compose stop review
BACKUP_DIR="$(mktemp -d /srv/crc/backup-XXXXXXXX)"
docker compose cp review:/app/data/. "$BACKUP_DIR/"
docker compose start review
```

另行安全备份 `.env` 和实际配置。恢复时仍需使用新建空卷、匹配的镜像版本与主密钥，详见第 7 节；不要把旧 WAL 混进恢复数据。下文 `docker run` 示例使用独立容器名，不能与 Compose 实例同时占用同一端口或数据库。

## 1. 镜像内容与边界

镜像分四阶段构建：

1. `web`：执行 `npm ci` 和 `npm run build`，生成控制台 `web/dist`。
2. `build`：先 `go mod download`，再构建 `cmd/server` 和 `cmd/tribunalctl`；服务嵌入上一步的新 `web/dist`。
3. `codebase-memory`：按 `TARGETARCH` 下载 codebase-memory-mcp 0.12.1 的 `-portable` 发布包，校验固定的 SHA-256，核对许可证与仓库记录一致，只取出二进制和许可证文本。
4. `runtime`：包含 `/app/server`、`/app/tribunalctl`、示例配置、许可证、CA 证书、`/usr/local/bin/codebase-memory-mcp` 与隔离 wrapper，以及 `curl`、`git` 和创建非 root 用户所需的 `passwd` 工具。

镜像保留兼容符号链接 `/app/reviewctl`，指向 `/app/tribunalctl`；旧编排脚本可继续调用该旧路径。源码用户改用 `go run ./cmd/tribunalctl`。

运行特征：

- 以 UID/GID `10001:10001` 的非 root 用户 `tribunal` 运行，家目录 `/home/tribunal`，工作目录 `/app`。
- 持久化目录为 `/app/data`，声明为卷；SQLite 数据库、WAL/SHM 文件和项目工作区都在这里。
- 默认监听 `8080`，容器内为 HTTP；TLS 由反向代理终止。
- 镜像不包含 `.git`、`node_modules`、本地 `data`、真实 `config/server.yaml`、`.env`、证书或私钥。
- 许可证位于 `/licenses/LICENSE` 和 `/licenses/THIRD_PARTY_NOTICES.txt`；后者是单一声明文件，覆盖 Go 标准库、Linux/amd64 服务端的 40 个模块、9 个 production npm 包和镜像内置的 codebase-memory-mcp。当前 Dockerfile 仍把遗留的 `THIRD_PARTY_LICENSES` 一并复制到 `/licenses/`，但发布声明以 `THIRD_PARTY_NOTICES.txt` 为准。`git` 及 Debian 软件包自身的许可证在 `/usr/share/doc/`。
- 工作区只使用 http(s) Git remote，镜像不安装 SSH 客户端。镜像内置 codebase-memory-mcp（见下文）；其他 MCP 服务器由部署方安装或使用扩展镜像。
- 仅支持一个服务实例使用一个 SQLite 数据目录。不要用多个容器共享同一数据库文件或同一数据卷。
- 后台 run cleanup 在启动后立即执行、之后每小时执行。它先查询 `workspaces`、run 保护集合和 context 保护 SHA，再触碰项目文件；只处理有 `workspaces` 记录的项目。超过 `workspace.run_cleanup_after` 的 `runs/<id>` 需未被未终态 workflow、queued/running job、`publication_state=pending` 的 dossier 或本进程注册 run 引用；`context/<sha>` 需未被任何 DB revision、当前 workspace base/context、未终态或待发布 workflow base 引用。locked/非法 metadata 保留并 fail closed；项目列表查询失败时整轮保留，某项保护查询失败时跳过该项目。

基础镜像标签已在本机联网核对 Docker Hub（2026-10-22）：

| 用途 | 标签 |
|---|---|
| 前端构建 | `node:22.20.0-bookworm-slim` |
| Go 构建 | `golang:1.26.10-bookworm` |
| 运行时 | `debian:bookworm-slim` |

`Dockerfile` 使用这些标签，不使用 `latest` 或未核对的标签。生产发布建议进一步固定镜像 digest。

### 内置代码图谱服务 codebase-memory-mcp

镜像内置 [codebase-memory-mcp](https://github.com/DeusData/codebase-memory-mcp) 0.12.1（MIT，独立进程，不链接进 Go 二进制）：

| 路径 | 内容 |
|---|---|
| `/usr/local/bin/codebase-memory-mcp` | 上游静态链接二进制，SHA-256 在 Dockerfile 中固定 |
| `/usr/local/bin/tribunal-codebase-memory-mcp` | [隔离 wrapper](../scripts/codebase-memory-mcp.sh) |
| `/usr/local/bin/crc-codebase-memory-mcp` | 指向 wrapper 的旧名兼容链接 |
| `/licenses/codebase-memory-mcp/` | 上游 `LICENSE` 与 `THIRD_PARTY_NOTICES.md` |

镜像不运行上游 `install`，不写任何 agent 配置或 hooks。示例配置和 `config/server.docker.yaml` 的 `default_extensions` 在首次启动时登记平台级 stdio 服务器 `codebase-memory`（`--tool-profile=analysis`），并为 `code-review`、`review` 授权只读图谱工具、为 `verify` 授权 `get_code_snippet`。项目 onboarding 和刷新用 CLI 为固定的 `base/` 建索引。每个项目的 `HOME`、`CBM_CACHE_DIR`、`CBM_RUNTIME_DIR` 都在该项目工作区内，项目之间查不到彼此的索引；auto_index、watcher 和 UI 均关闭。规则详见 [默认扩展](design/06-config.md#默认扩展default_extensions) 和 [内置 codebase-memory-mcp](design/06-config.md#内置-codebase-memory-mcp)。

- **查看和测试**：控制台「扩展」页列出 `codebase-memory`；「测试并列出工具」返回的列表不含 `index_repository`、`delete_project`、`ingest_traces`、`manage_adr`。
- **内置标识**：它带「内置」标签，只能停用或启用。名称和配置随平台版本走，不能在控制台编辑；删除、改名、改配置或用导入覆盖都返回 409。
- **禁用**：在「扩展」页点「停用」，或 `PUT /api/v1/mcp-servers/{id}` 设置 `"enabled": false`，然后重启服务。重启和升级都保持停用。
- **故障表现**：二进制缺失或无法启动时评审照常完成，但没有图谱工具；评审结论里出现 `MCP tools unavailable` 告警，原本会 PASS 的评审显示为 INCOMPLETE。建索引失败时 onboarding 照常完成，工作区的 `last_error` 记录 `index: ...`。

## 2. 构建

在仓库根目录、且 `docker` 命令和容器运行时可用时执行：

```bash
docker build -t tribunal:local .
```

构建需要网络拉取基础镜像、Debian 包、Go 模块和 npm 依赖。`web/dist` 即使在本地已存在也会被 `.dockerignore` 排除；镜像内始终由 `web` 阶段重新构建，然后复制给 Go 阶段。

从 macOS 打包源码到 Linux 构建时，使用 `COPYFILE_DISABLE=1 tar --no-xattrs ...` 并核对归档文件清单，不能携带 AppleDouble 的 `._*` 文件。实跑中 `._*.sql` 被 `go:embed *.sql` 一并嵌入，导致迁移文件名非法、服务无法启动；配置检查不能发现此问题。应重新打包干净源码，不放宽迁移校验，也不删除数据库。

构建顺序固定为前端在前、Go 在后。若 `npm ci` 或 `npm run build` 失败，不会继续生成 Go 二进制。

## 3. 单独 docker run（不用 Compose 时）

推荐使用前文的 Compose 入口。以下 `docker run` 是自定义部署的最小骨架，不会读取 `.env`；它假定 `/srv/crc/server.yaml` 的 URL、模型等非秘密字段使用字面值。若挂载的是 `server.docker.yaml` 或其他带 `${NAME}` 的配置，必须把这些变量逐一传入容器。

创建命名卷并准备真实配置：

```bash
docker volume create crc-data
```

卷名 `crc-data` 是刻意保留的兼容名称；继续使用同名卷才能复用已有数据库。产品改名不迁移该卷。

秘密从部署环境注入，不要写进镜像、Compose 文件或命令历史：

```bash
export REVIEW_MASTER_KEY="$(openssl rand -base64 32)"
export REVIEW_ADMIN_TOKEN="$(openssl rand -hex 32)"
export TRIBUNAL_SESSION_KEY="$(openssl rand -base64 32)"
```

启动容器：

```bash
docker run -d --name tribunal \
  --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -e REVIEW_MASTER_KEY \
  -e REVIEW_ADMIN_TOKEN \
  -e TRIBUNAL_SESSION_KEY \
  -v crc-data:/app/data \
  -v /srv/crc/server.yaml:/app/config/server.yaml:ro \
  tribunal:local --config /app/config/server.yaml
```

检查：

```bash
docker logs tribunal
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz
docker inspect --format '{{.State.Health.Status}}' tribunal
```

注意：

- 必须覆盖默认 `--config`。镜像内的 `server.example.yaml` 只用于烟测，不是生产配置，也没有 `providers` 和 `propose`，不能执行真实评审。
- 启用模型 provider 时，还要传入配置中 `api_key_env` 指定的变量，例如 `-e LLM_API_KEY`。复用 `server.docker.yaml` 时还需 `-e TRIBUNAL_PUBLIC_URL -e GITLAB_URL -e LLM_PROTOCOL -e LLM_BASE_URL -e LLM_MODEL`；这些值都需先在宿主机环境中设置。不要仅传入三个平台秘密就把它当作完整评审部署。
- 必须挂载 `/app/data`。删除卷会丢失数据库，并且已保存的项目凭据无法恢复。
- 命名卷为空时会继承镜像内 `/app/data` 的属主 `10001:10001`。绑定挂载宿主机目录时，先修正属主：
  ```bash
  sudo install -d -o 10001 -g 10001 -m 0700 /srv/crc/data
  ```
  然后用 `-v /srv/crc/data:/app/data` 启动。
- 环境变量对 Docker 管理员可见。生产环境应由 secret manager 注入，并限制 Docker API 访问。应用只读取环境变量，不读取 `*_FILE` 形式，因此不能直接把 Docker secret 文件交给应用。
- 不要给容器挂载 Docker socket，也不要让多个服务实例共用同一个数据卷。

## 4. 必须持久化的秘密

| 变量 | 要求 | 丢失或轮换的影响 |
|---|---|---|
| `REVIEW_MASTER_KEY` | base64，解码后恰好 32 字节 | 用于解密已保存的项目令牌和 webhook secret。丢失后旧凭据无法解密，需要恢复旧 key 或重新登记项目。 |
| `REVIEW_ADMIN_TOKEN` | 至少 32 字节 | 平台管理面的 bootstrap Bearer 令牌，不授予项目业务数据访问。轮换由部署方更新环境变量并重启；重启后旧客户端立即失效。 |
| `TRIBUNAL_SESSION_KEY` | base64，解码后至少 32 字节 | 用于签名会话 cookie、OAuth state，并派生 CSRF token。浏览器会话持久化在 SQLite；固定该值时可跨重启继续使用未过期会话。未设置或恰好为空字符串时回退旧名；旧名也未配置时每进程随机生成并输出告警，重启后旧 cookie 失效。非空但只含空白（去除空白后为空）是非法值，启动失败，不回退旧名，也不生成临时 key。轮换该值会让所有现有浏览器会话失效。 |

生成示例：

```bash
openssl rand -base64 32   # REVIEW_MASTER_KEY / TRIBUNAL_SESSION_KEY
openssl rand -hex 32      # REVIEW_ADMIN_TOKEN
```

`secrets.master_key_env` 和 `auth.bootstrap_token_env` 可以改名；改名后实际环境变量也要对应修改。

`REVIEW_MASTER_KEY` 必须跨升级、重启和回滚保持不变。备份数据库时同时备份该 key；只有数据库而没有 key 无法解密项目凭据。`TRIBUNAL_SESSION_KEY` 也应持久化；固定 key 且复用同一 SQLite 数据卷时，重启后浏览器会话仍可继续使用。

## 5. OAuth（可选）

默认不启用 OAuth，`/auth/login` 返回 503，Bearer 认证仍可用。`config/server.example.yaml` 中提供了注释模板；启用时取消 `oauth` 映射的三行注释（`oauth:`、`client_id_env:`、`client_secret_env:`），并导出对应环境变量。

GitLab 侧：

1. 创建 OAuth Application。
2. Redirect URI 填 `https://<公开域名>/auth/callback`，例如 `https://review.example.internal/auth/callback`。
3. 勾选/使用 `read_user` scope。
4. 保存 client ID 和 client secret。

配置侧：

```yaml
server:
  public_url: "https://review.example.internal"

auth:
  oauth:
    client_id_env: GITLAB_OAUTH_CLIENT_ID
    client_secret_env: GITLAB_OAUTH_CLIENT_SECRET
  bootstrap_admins: ["id:42"]
```

容器环境：

```bash
export GITLAB_OAUTH_CLIENT_ID="..."
export GITLAB_OAUTH_CLIENT_SECRET="..."
```

宿主机导出的变量不会自动进入容器。启用 OAuth 时，`docker run` 必须增加 `-e GITLAB_OAUTH_CLIENT_ID -e GITLAB_OAUTH_CLIENT_SECRET`，否则容器看不到这两个变量，配置校验会失败。使用不带值的 `-e NAME` 时，Docker 从当前 shell 取值。

```bash
docker run -d --name tribunal \
  --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -e REVIEW_MASTER_KEY \
  -e REVIEW_ADMIN_TOKEN \
  -e TRIBUNAL_SESSION_KEY \
  -e GITLAB_OAUTH_CLIENT_ID \
  -e GITLAB_OAUTH_CLIENT_SECRET \
  -v crc-data:/app/data \
  -v /srv/crc/server.yaml:/app/config/server.yaml:ro \
  tribunal:local --config /app/config/server.yaml
```

说明：

- `server.public_url` 必须是 GitLab 能访问的外部地址，并且与 OAuth Application 的 Redirect URI 同源。应用固定使用 `public_url + /auth/callback`。
- `read_user` 是登录所需的最小 scope；应用调用 GitLab `/api/v4/user` 获取用户名。
- `bootstrap_admins` 推荐填 `id:<GitLab 用户 ID>`（如 `id:42`），只按 ID 匹配，GitLab 改名不影响。用户 ID 可在 GitLab 个人资料页或 `GET /api/v4/users?username=<name>` 查到。也兼容旧写法 GitLab **用户名**（不区分大小写，已弃用，见下文迁移步骤）：该用户名第一次以 admin 身份登录或访问时，Tribunal 把它绑定到当时的 GitLab 用户 ID 并写入 `admin_bindings`；之后其他账号即使改名为同一用户名，也只得到 `viewer`，日志记录 `bootstrap admin username is bound to another GitLab user`。匹配的用户获得平台 `admin` 标签；其他登录用户为 `viewer`。它不会创建 GitLab 账号，也不授予任何项目数据访问；项目权限仍需 `project_grants`。
- 用户名绑定是首次使用即信任：升级前若该用户名已被他人占用，第一个登录的账号会拿到绑定。升级后请确认绑定正确（`SELECT username, user_id FROM admin_bindings;`），或直接改用 `id:` 写法。管理员 GitLab 账号重建、需要重新绑定时，先停服务，执行 `DELETE FROM admin_bindings WHERE username = '<小写用户名>';`，再启动并让正确账号登录；改用 `id:` 写法则不需要这一步。
- 每个用户最多保留 10 个浏览器会话，再次登录会淘汰该用户自己最旧的会话，不影响其他用户；全局 4096 个会话只是存储兜底。
- OAuth 配置存在时，`client_id_env` 和 `client_secret_env` 必须同时设置，且两个环境变量都必须非空；`server.public_url` 必须是非空的绝对 HTTP(S) 地址，否则启动校验失败。
- `REVIEW_ADMIN_TOKEN` 与 OAuth 相互独立；没有浏览器会话时仍可用它调用用户、授权和系统配置等平台管理 API。它没有用户 ID 和项目 grant，不能读取项目业务数据；轮换由部署方更新环境变量并重启。
- 真实 OAuth 登录已在已验证快照的隔离测试实例验证；未单独复测其他生产 IdP 或所有设备场景。

### bootstrap_admins 用户名写法弃用与迁移

用户名写法已弃用，计划在下一个大版本（v2.0.0）移除；在此之前保持兼容。服务启动时，每个用户名条目记一条 `WARN`，消息为 `auth.bootstrap_admins username entry is deprecated ...`。日志只含条目在列表中的序号 `index`（从 0 开始），不含用户名：

- 该用户名已在 `admin_bindings` 绑定：日志带 `replacement=id:<N>`，把第 `index` 个条目直接替换成这个值即可，权限不变。
- 尚未绑定：日志带 `binding=none`。用 `GET <GITLAB_URL>/api/v4/users?username=<name>` 或 GitLab 个人资料页查到用户 ID 后写成 `id:<N>`；该用户登录过 Tribunal 的话，也可以用 `tribunalctl users list` 查 ID。
- 读取绑定失败：日志带 `binding=unavailable`，按上一条手动查 ID。

替换后重启服务，确认启动日志不再出现这条告警。`admin_bindings` 里的旧行不影响 `id:` 条目，可以保留。

## 6. 健康检查与 HTTPS 反向代理

应用端点：

- `GET /healthz`：进程存活检查，固定返回 `{"status":"ok"}`。
- `GET /readyz`：数据库可 ping 且 worker 已启动时返回 200；否则返回 503。容器 `HEALTHCHECK` 使用该端点。

反向代理必须终止 TLS，并且不要缓冲 SSE。SSE 端点为 `/api/v1/reviews/{id}/stream`，服务端已发送 `X-Accel-Buffering: no`，但代理侧仍应显式关闭缓冲。nginx 示例：

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header Connection "";
    proxy_buffering off;
    proxy_cache off;
    proxy_read_timeout 1h;
}
```

- `proxy_buffering off` 关闭缓冲；`proxy_cache off` 避免缓存事件流。
- `proxy_read_timeout` 要覆盖最长评审时间；SSE 每 15 秒发送心跳，但代理不应在评审结束前断开。
- 若使用 OAuth，`server.public_url` 必须是 HTTPS；应用据此设置安全 Cookie。
- 只将 `8080` 绑定到本机或内网，由反向代理暴露 HTTPS。不要把明文管理接口直接暴露到公网。
- `/healthz` 不检查数据库，适合存活探测；`/readyz` 才适合就绪探测和流量接入。
- 镜像的 `HEALTHCHECK` 固定请求 `127.0.0.1:8080`。推荐保持容器监听 8080，仅通过 `TRIBUNAL_HTTP_PORT` 修改宿主机映射。自定义容器内监听端口时，`docker run` 用 `--health-cmd` 覆盖；Compose 则需同步修改服务的 `healthcheck.test`、`ports` 和应用 `server.listen`，不能把 `--health-cmd` 直接传给 Compose。

## 7. SQLite 一致性备份与恢复

数据库启用 WAL。运行中只复制 `review.db` 会漏掉 `-wal`/`-shm`，不是一致备份。最稳妥的方式是停止服务后复制整个数据目录，同时带走工作区。

备份：

```bash
docker stop tribunal
BACKUP_DIR="$(mktemp -d /srv/crc/backup-XXXXXXXX)"
docker cp tribunal:/app/data/. "$BACKUP_DIR/"
docker start tribunal
```

目录中至少包含：

- `review.db`
- `review.db-wal`（若存在）
- `review.db-shm`（若存在）
- `workspaces/`

`workspaces/` 包含项目 checkout、运行目录和上下文文件，属于恢复所需数据；上下文文档的元数据同时记录在数据库中。停止容器后复制的数据库与工作区处于同一静止时间点。

恢复到新建的空卷，不把备份覆盖到旧数据目录。`docker cp` 和 `cp` 都会合并目录；旧 WAL 或工作区文件若残留，可能与备份不匹配。保留旧容器和旧卷，直到新容器验收通过。

```bash
docker stop tribunal
RESTORE_VOLUME="$(docker volume create)"
docker run --rm --user 0 --entrypoint sh \
  -v "$BACKUP_DIR:/backup:ro" \
  -v "$RESTORE_VOLUME:/restore" \
  tribunal:local -c 'cp -a /backup/. /restore/ && chown -R 10001:10001 /restore'
docker run -d --name tribunal-restored \
  --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -e REVIEW_MASTER_KEY -e REVIEW_ADMIN_TOKEN -e TRIBUNAL_SESSION_KEY \
  -v "$RESTORE_VOLUME:/app/data" \
  -v /srv/crc/server.yaml:/app/config/server.yaml:ro \
  tribunal:local --config /app/config/server.yaml
```

恢复后检查：

```bash
docker logs tribunal-restored
curl -fsS http://127.0.0.1:8080/readyz
```

恢复时将 `BACKUP_DIR` 设置为选定的备份目录，使用与该快照匹配的镜像和主密钥。启用 OAuth 或模型 provider 时，还要传入原容器使用的对应环境变量。一次性复制容器以 root 修正属主；服务容器仍以 `10001:10001` 运行。镜像内不含 SQLite 命令行，恢复库的 `PRAGMA integrity_check;` 需由宿主机或受控工具检查。

在线备份替代方案：使用宿主机或临时工具执行 SQLite `.backup` / `VACUUM INTO`，并对备份库运行 `PRAGMA integrity_check;`。这只能保证数据库快照一致；`workspaces/` 仍需单独备份，二者不保证同一事务时刻。不要把“在线数据库备份”描述成数据库与工作区的原子备份。

镜像不安装 `sqlite3` 命令行工具。需要在线备份时在宿主机或受控临时环境中执行，不要为了备份给运行时镜像增加长期依赖。

## 8. 升级与回滚

迁移文件编译在二进制内，服务启动时自动执行；只应用尚未执行的版本，没有 down migration。升级前必须备份。

升级步骤：

1. 记录当前镜像 tag/digest。
2. 停止旧容器，备份 `/app/data`（见上一节）。
3. 拉取并启动新镜像，继续使用同一数据卷、同一 `REVIEW_MASTER_KEY` 和真实配置。
4. 等待 `/readyz` 返回 200，检查日志和关键功能。

回滚步骤：

1. 停止新容器。
2. 恢复升级前的 `/app/data` 快照。
3. 在启动旧镜像之前隔离入站，并阻断 GitLab/OAuth/provider 出站访问；旧版也可能在启动时自动恢复任务和重试发布。先在隔离环境核对 pending 任务及已有 GitLab 副作用，核对完成并获授权前不解除出站限制。
4. 使用旧镜像、匹配配置及原主密钥启动恢复副本；不能因启动成功就对普通用户开放。完整安全边界见 [回退触发与操作计划](delivery-readiness.md#回退触发与操作计划)。

注意事项：

- 内置扩展随平台升级：新镜像附带的 codebase-memory-mcp 配置（版本、参数）在下次启动时写入已登记的内置服务器，启用状态保持不变；默认工具白名单在块绑定仍是默认机制写入的样子时同步更新，管理员改过的绑定和 YAML `blocks` 中声明的块不动。首次启动后才在 YAML `blocks` 中声明的块：数据库里该块若仍是默认机制写入的绑定，启动时删除并以 YAML 为准（日志 `file configuration replaces default block binding`）；管理员改过的仍以数据库为准。内置 Skill 刷新失败（例如修改了 `workspace.root`）只记 Warn 日志 `built-in skill refresh failed`，不阻断启动。已有部署若在数据库或 `mcp.config` 中自建了同名 `codebase-memory` 服务器，服务不会接管它，也不会绑定它的工具。
- 升级内置的 codebase-memory-mcp 版本时，按 [内置 codebase-memory-mcp](design/06-config.md#内置-codebase-memory-mcp) 同时更新 Dockerfile 版本与 SHA-256、`third_party/codebase-memory-mcp/` 和许可证清单，并重新跑 Docker smoke。
- 镜像 tag 改为 `tribunal:*`；旧镜像的仓库名是 `crc-review`，升级时重新构建或改 tag。Compose 项目名 `crc-review`、旧数据卷 `crc-review_data` 和 `crc-data` 刻意不改名；继续挂载原卷即可保留数据。
- 管理 CLI 改用 `tribunalctl`；旧镜像路径 `/app/reviewctl` 以兼容符号链接保留。源码用户改用 `go run ./cmd/tribunalctl`。
- 环境变量主名改为 `TRIBUNAL_SESSION_KEY`、`TRIBUNAL_PUBLIC_URL`、`TRIBUNAL_HTTP_PORT`；旧 `CRC_*` 名仍兼容，新旧同时设置时新名优先。三个旧名只要配置了非空值，都会各记录一次弃用日志；日志只含变量名、不含值。日志表示旧名仍配置，不代表旧值一定生效或一定被忽略。升级时复用原 session key 的同一个值，否则现有浏览器会话失效。
- 旧二进制直接读取已被新版本迁移过的 schema 不受支持。仅回退镜像而不恢复数据快照，可能因列/表变化失败或产生错误数据。
- 不要更换 `REVIEW_MASTER_KEY`，否则已保存的项目凭据无法解密。
- 生产环境使用不可变 tag 或 digest，保留旧镜像以便回滚。
- 升级窗口内不要同时运行两个容器指向同一个 SQLite 数据卷。

### 从 v1.0.0 升级到 v1.1.0

完整变更见 [CHANGELOG](../CHANGELOG.md#110---2026-10-26)。除了上面的通用步骤，这次升级还有以下几点：

1. **升级后旧版本读不了数据，回退只能靠备份。** 新版本启动时会：
   - 应用迁移 `0013_admin_bindings`、`0014_extension_defaults`、`0015_secret_migration`；
   - 在各服务初始化前运行 `MigrateProjectSecrets`，把已存的项目 token 和 webhook secret 改写成绑定项目与字段的新加密格式（前缀 `\x00tb1`）。

   v1.0.0 读不了新格式。所以必须先做第 7 节的一致性备份；回退时恢复这份备份，不能只换回 v1.0.0 镜像。
2. **继续使用原来的 `REVIEW_MASTER_KEY`。** 迁移要用它解开旧密文。迁移是幂等的，每个字段单独迁移，迁移期间被并发修改的行会跳过。解不开的字段只记一条告警（只含 `project_id` 和字段名），保持原样，不阻止启动；这类字段在升级前使用时同样会失败，需要在设置页用「更新访问 Key」或 Webhook「重建」重新写入。某次启动后不再有旧格式字段时，服务在 `secret_migration` 表记下迁移完成；此后启动不再解开任何旧格式密文，遇到时保持原样并告警（只含 `project_id` 和字段名），该字段同样要重新写入。新部署首次启动即记为完成。
3. **镜像和 CLI 改了名，数据卷没改。** 用新代码重新构建镜像（`docker compose build`）。Compose 项目名 `crc-review` 和数据卷 `crc-review_data` 保持不变，直接沿用。脚本里的 `/app/reviewctl` 可以继续用，建议改成 `/app/tribunalctl`。环境变量可以逐步改为 `TRIBUNAL_*` 新名，session key 要沿用原来的值。
4. **管理员配置。** `bootstrap_admins` 推荐改成 `id:<GitLab 用户 ID>`（用 `tribunalctl users list` 查 ID）。用户名写法暂时仍可用，但已弃用，启动时会记告警，迁移见 [用户名写法弃用与迁移](#bootstrap_admins-用户名写法弃用与迁移)：该用户名第一次以管理员身份出现时，会被绑定到当时的 GitLab 用户 ID 并写入 `admin_bindings`；之后其他账号即使改成这个用户名，也只有 viewer 权限。如果想换一个账号当管理员，改用 `id:` 写法。
5. **会话数量。** 每个用户最多保留 10 个浏览器会话，超出时淘汰该用户自己最旧的会话。升级时已有的会话保留，用户下次登录时才按上限淘汰。
6. **默认扩展。** 首次启动时会登记内置 Skill `code-review` 和内置 MCP 服务器 `codebase-memory`，并按 `default_extensions` 绑定到评审块。已有部署如果在数据库或 `mcp.config` 里自建了同名项，服务不会接管，也不会绑定它的工具。`config/server.docker.yaml` 已带默认配置；自定义配置文件要参照 [默认扩展配置](design/06-config.md#默认扩展default_extensions) 补上 `default_extensions`，不需要就写 `skills: []`。
7. **Webhook 的配置位置变了。** 控制台注册项目不再创建 Webhook，要在项目设置的「GitLab Webhook」面板里启用。已有项目的 Webhook 状态不受影响。CLI 的 `--auto-webhook` 仍然可用。

升级后检查：

```bash
docker compose logs --tail=200 review | grep -Ei 'migrat|secret|deprecat|error'
docker compose exec -T review /app/tribunalctl projects all
```

日志里不应出现 `stored project token cannot be decrypted` 或 `stored webhook secret cannot be decrypted` 告警，`projects all` 应列出全部已有项目。然后在控制台确认：内置 Skill 和 MCP 带「内置」标签；对一个已有项目执行「测试」MCP 或手动触发一次评审，确认能正常发布。

## 9. 无外部凭据的本地烟测

没有 Docker 时，可用本机 Go 和临时数据目录验证启动、readiness 和 OAuth 默认关闭行为。该烟测不接触 GitLab、模型 provider 或真实 OAuth。

先构建前端和二进制：

```bash
cd web
npm ci
npm run build
cd ..
go build -o /tmp/tribunal-server ./cmd/server
```

准备临时配置和数据：

```bash
TMP="$(mktemp -d)"
export REVIEW_MASTER_KEY="$(openssl rand -base64 32)"
export REVIEW_ADMIN_TOKEN="$(openssl rand -hex 32)"
export TRIBUNAL_SESSION_KEY="$(openssl rand -base64 32)"
cat > "$TMP/override.yaml" <<EOF
server:
  listen: "127.0.0.1:18080"
  public_url: "http://127.0.0.1:18080"
storage:
  dsn: "file:$TMP/data/review.db"
workspace:
  root: "$TMP/workspaces"
EOF
```

校验示例配置合并后的结果（OAuth 未启用）：

```bash
/tmp/tribunal-server --config config/server.example.yaml \
  --config "$TMP/override.yaml" --check-config
```

启动并检查：

```bash
/tmp/tribunal-server --config config/server.example.yaml \
  --config "$TMP/override.yaml" >"$TMP/server.log" 2>&1 &
PID=$!
for i in $(seq 1 50); do
  if curl -fsS http://127.0.0.1:18080/readyz >/dev/null 2>&1; then break; fi
  sleep 0.2
done
curl -fsS http://127.0.0.1:18080/healthz
curl -fsS http://127.0.0.1:18080/readyz
curl -fsS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18080/
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18080/auth/login
kill "$PID"
wait "$PID" || true
rm -rf "$TMP"
```

预期：

- `--check-config` 输出 `configuration OK`。
- `/healthz` 和 `/readyz` 返回 200。
- `/` 返回 200，返回嵌入的控制台页面。
- 带 Bearer 管理令牌请求 `/api/v1/projects` 返回 200。
- 未配置 OAuth 时 `/auth/login` 返回 503。

单独验证 OAuth 模板的配置解析（不进行真实登录）。以下命令沿用本节前面导出的 secrets；若在新 shell 执行，先重新 export `REVIEW_MASTER_KEY` 和 `REVIEW_ADMIN_TOKEN`：

```bash
TMP_OAUTH="$(mktemp -d)"
export GITLAB_OAUTH_CLIENT_ID="dummy-client-id"
export GITLAB_OAUTH_CLIENT_SECRET="dummy-client-secret"
cat > "$TMP_OAUTH/oauth.yaml" <<EOF
server:
  listen: "127.0.0.1:18081"
  public_url: "https://review.example.internal"
auth:
  oauth:
    client_id_env: GITLAB_OAUTH_CLIENT_ID
    client_secret_env: GITLAB_OAUTH_CLIENT_SECRET
  bootstrap_admins: ["id:42"]
storage:
  dsn: "file:$TMP_OAUTH/data/review.db"
workspace:
  root: "$TMP_OAUTH/workspaces"
EOF
/tmp/tribunal-server --config config/server.example.yaml \
  --config "$TMP_OAUTH/oauth.yaml" --check-config
unset GITLAB_OAUTH_CLIENT_ID GITLAB_OAUTH_CLIENT_SECRET
rm -rf "$TMP_OAUTH"
```

该检查只证明 OAuth 配置结构和环境变量引用合法，不证明 GitLab 回调、token 交换或用户角色映射已经通过真实集成验收。

### Docker 可用时的 Compose 烟测

仓库提供可重复的无外部凭据检查，CI 的 `docker` job 调用同一个脚本：

```bash
bash scripts/docker-smoke.sh
```

脚本使用独立临时 Compose 项目、数据卷、假环境变量和随机宿主机端口，不使用用户 `.env` 的真实凭据，也不登记真实项目。它验证 Compose 配置、镜像构建、非 root 身份、卷权限、Git、许可证、静态控制台、健康接口、Bearer CLI、默认 OAuth 关闭、内置 codebase-memory-mcp（版本、wrapper、兼容链接、许可证、CLI 在隔离 `HOME` 中建索引、默认服务器已登记且测试接口只列出只读工具），以及重建容器后数据仍存在。退出时只清理本次测试拥有的资源，不执行全局 prune。该脚本需要真实 Docker daemon；仅通过 shell 语法检查不算容器验证。

### Docker 可用时的单镜像烟测

先构建 `tribunal:local`，然后检查镜像内容：

```bash
docker run --rm --entrypoint git tribunal:local --version
docker run --rm --entrypoint id tribunal:local
docker run --rm --entrypoint stat tribunal:local -c '%u:%g %a' /app/data
docker run --rm \
  -e REVIEW_MASTER_KEY="$(openssl rand -base64 32)" \
  -e REVIEW_ADMIN_TOKEN="$(openssl rand -hex 32)" \
  tribunal:local --config /app/config/server.example.yaml --check-config
```

预期：`git version` 有输出，`id` 显示 uid 10001 和 gid 10001，`stat` 输出 `10001:10001 700`，`--check-config` 输出 `configuration OK`。

启动一个临时容器，只验证启动、readiness 和 Bearer 接口；使用示例配置，不连接外部 GitLab：

```bash
docker run -d --name tribunal-arch14-smoke \
  -p 127.0.0.1:18080:8080 \
  -e REVIEW_MASTER_KEY="$(openssl rand -base64 32)" \
  -e REVIEW_ADMIN_TOKEN="smoke-admin-token-0123456789abcdef" \
  -e TRIBUNAL_SESSION_KEY="$(openssl rand -base64 32)" \
  -v tribunal-arch14-smoke-data:/app/data \
  tribunal:local

for i in $(seq 1 60); do
  if curl -fsS http://127.0.0.1:18080/readyz >/dev/null 2>&1; then break; fi
  sleep 0.5
done

curl -fsS http://127.0.0.1:18080/healthz
curl -fsS http://127.0.0.1:18080/readyz
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18080/auth/login
curl -fsS -H 'Authorization: Bearer smoke-admin-token-0123456789abcdef' \
  http://127.0.0.1:18080/api/v1/projects

docker rm -f tribunal-arch14-smoke
docker volume rm tribunal-arch14-smoke-data
```

清理只删除本次创建的 `tribunal-arch14-smoke` 容器和 `tribunal-arch14-smoke-data` 卷。不要删除其他容器或卷。该烟测不验证真实 GitLab、OAuth、webhook 或评审执行，不能当作集成验收。

## 10. 验证状态

验证记录统一维护在 [交付就绪与验收记录](delivery-readiness.md)，本文不再重复：

- 当前版本：[当前版本（v1.1.x）验证状态](delivery-readiness.md#当前版本v11x验证状态)。
- v1.0.0（f143 快照）在隔离测试实例上的验收记录：[v1.0.0 历史验收记录](delivery-readiness.md#v100-历史验收记录f143仅供追溯)，仅供追溯，不覆盖 v1.1.x 的改动。

上面第 9 节的烟测不验证真实 GitLab、OAuth、webhook 或评审执行，不能当作集成验收。
