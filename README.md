# Tribunal

GitLab CE MR 多 agent 自动评审服务

基于 Go 的 GitLab CE MR 自动评审服务，使用 MIT 协议。

当前 M0–M6 功能已有本地实现，覆盖项目注册与 webhook、三种 LLM 协议、agent 工具循环、DAG 与持久化、主 agent 编排/对抗核验/裁决、GitLab 发布、工作区、MCP/Skill、OAuth 持久会话、项目 `read`/`manage` 授权与 CSRF、管理 API、React 控制台、API token、统计、反馈采集、人工标注与许可证清单。

当前版本是 **v1.1.0**，变更和升级必读见 [CHANGELOG](CHANGELOG.md)。v1.1.0 通过了 CI，包括 Compose 冒烟测试，但没有在真实 GitLab 实例上验收；具体做过哪些验证、还缺哪些，见 [当前版本（v1.1.x）验证状态](docs/delivery-readiness.md#当前版本v11x验证状态)。

部署时：人工部署看 [部署与运维](docs/operations.md)，由 agent 代为部署和接入时用 [Agent 部署指南](docs/agent-deployment.md)。

v1.0.0（[PR #30](https://github.com/LeXwDeX/Tribunal/pull/30)、[Release](https://github.com/LeXwDeX/Tribunal/releases/tag/v1.0.0)）曾在隔离测试实例上用 f143 快照做过真实验收，包括实际镜像、离线恢复/在线升级、真实 GitLab 发布和两次 deep 运行。该记录不覆盖 v1.1.x 的改动，只用于追溯，见 [v1.0.0 历史验收记录](docs/delivery-readiness.md#v100-历史验收记录f143仅供追溯)。

总体目标见 [ARCH.md](ARCH.md)，实现层面的设计与已知偏差见 [详细设计](docs/design/README.md)，部署、备份与运行边界见 [部署与运维](docs/operations.md)。

## Docker Compose 部署

需要 Docker Engine 或 Docker Desktop，以及 Compose v2.20 或更新版本。宿主机不需要安装 Go 或 Node。镜像包含控制台、服务和 `tribunalctl`，SQLite 与工作区放在同一个持久卷中。

首次部署，在仓库根目录准备 `.env`：

```bash
cp -n .env.example .env
chmod 600 .env
```

填写真实 `GITLAB_URL`、`LLM_MODEL`、`LLM_API_KEY`，检查 `LLM_PROTOCOL` 和 `LLM_BASE_URL` 是否匹配模型接口。分别生成并持久保存 `REVIEW_MASTER_KEY`、`REVIEW_ADMIN_TOKEN`、`TRIBUNAL_SESSION_KEY`，生成方法见[私有密钥生成步骤](docs/agent-deployment.md#2-准备配置)。`.env` 已被 Git 和 Docker 构建上下文忽略；Compose 只传入显式列出的环境变量。

本文环境变量示例使用新主名 `TRIBUNAL_SESSION_KEY`、`TRIBUNAL_PUBLIC_URL`、`TRIBUNAL_HTTP_PORT`。旧名 `CRC_SESSION_KEY`、`CRC_PUBLIC_URL`、`CRC_HTTP_PORT` 仍兼容；新旧同时设置时新名优先。`TRIBUNAL_SESSION_KEY` 未设置或恰好为空字符串时才回退旧名；非空但只含空白是非法值，启动失败，不回退旧名，也不生成临时 key。三个旧名只要配置了非空值，都会各记录一次弃用日志；日志只含变量名、不含值。日志表示旧名仍配置，不代表旧值一定生效或一定被忽略：Compose 会把仅设置的 `CRC_HTTP_PORT` 作为 `TRIBUNAL_HTTP_PORT` 的插值回退值，仅用于宿主机端口映射，并把仅设置的 `CRC_SESSION_KEY` 复制给容器的 `TRIBUNAL_SESSION_KEY`。`TRIBUNAL_HTTP_PORT` / `CRC_HTTP_PORT` 只影响 Compose 宿主机映射，不是应用监听端口。若部署的 `.env` 仍列出旧名，照常可用。升级时复用原 session key 的同一个值，不要重新生成，否则现有浏览器会话失效。

```bash
docker compose config --quiet
docker compose build
docker compose run --rm --no-deps review --config /app/config/server.yaml --check-config
docker compose up -d --wait --wait-timeout 120
docker compose exec -T review /app/tribunalctl projects all
```

`/app/tribunalctl` 是当前镜像内的 CLI。旧镜像的 CLI 路径是 `/app/reviewctl`；新镜像保留指向 `/app/tribunalctl` 的 `/app/reviewctl` 兼容符号链接，旧编排脚本可继续调用。源码用户改用 `go run ./cmd/tribunalctl`。

默认地址为 `http://localhost:8080`，端口仅绑定本机。默认支持 API/Bearer 管理，**浏览器登录需要另外启用 GitLab OAuth**：填写 `.env` 的 OAuth 变量及管理员 `id:<GitLab 用户 ID>`（写入 `GITLAB_ADMIN_USERNAME`），取消 `config/server.docker.yaml` 中对应配置的注释，并配置 HTTPS 反向代理与 `TRIBUNAL_PUBLIC_URL`。修改 `.env` 后用 `docker compose up -d --force-recreate`，单纯 `restart` 不会更新容器环境变量。

停止使用 `docker compose down`，不要加 `-v`，否则会删除数据库和工作区。完整启动、项目注册、OAuth、备份及验证步骤见 [部署与运维](docs/operations.md)。

## 本地启动

需要 Go 1.26 或更新版本。SQLite 使用纯 Go 驱动，不需要 C 编译器。控制台产物通过 `//go:embed all:dist`（`web/embed.go`）打进二进制，因此**编译或运行前要先构建前端**。

在当前 Bash shell 中先执行 `set +x`，再由用户或安全密钥管理器将已持久保存的 `REVIEW_MASTER_KEY`、`REVIEW_ADMIN_TOKEN` 和 `TRIBUNAL_SESSION_KEY` 安全注入当前环境，不要把值写入命令、终端或日志。全新部署可先按[私有密钥生成步骤](docs/agent-deployment.md#2-准备配置)生成并保存；已有部署必须复用原值。若原会话密钥使用旧名 `CRC_SESSION_KEY`，本启动示例需要将同一个值安全注入 `TRIBUNAL_SESSION_KEY`，不要重新生成。

以下启动、单独构建及验证命令在子 shell 中清除 `REVIEW_MASTER_KEY`、`REVIEW_ADMIN_TOKEN`、`TRIBUNAL_SESSION_KEY`、`CRC_SESSION_KEY`、`LLM_API_KEY`、`GITLAB_PROJECT_TOKEN` 和 `GITLAB_OAUTH_CLIENT_SECRET`，确认它们均未设置后才运行 npm。其他环境变量仍会被继承，请先移除其中的秘密。无法清除或确认清除时命令会失败并停止；只读变量无法清除时请改用没有这些只读变量的 shell。检查假定 Bash、变量展开和执行的命令可信，不能防御恶意 shell、函数、别名或被篡改的 npm。当前 shell 和服务使用的值不变；启动块会将三个新名标记为 export。保持 xtrace 关闭，然后运行：

```bash
set +x
if [ -n "${REVIEW_MASTER_KEY:-}" ] && [ -n "${REVIEW_ADMIN_TOKEN:-}" ] &&
   [ -n "${TRIBUNAL_SESSION_KEY:-}" ]; then
  export REVIEW_MASTER_KEY REVIEW_ADMIN_TOKEN TRIBUNAL_SESSION_KEY &&
  (cd web && builtin unset -v REVIEW_MASTER_KEY REVIEW_ADMIN_TOKEN TRIBUNAL_SESSION_KEY CRC_SESSION_KEY \
    LLM_API_KEY GITLAB_PROJECT_TOKEN GITLAB_OAUTH_CLIENT_SECRET &&
    [ -z "${REVIEW_MASTER_KEY+x}${REVIEW_ADMIN_TOKEN+x}${TRIBUNAL_SESSION_KEY+x}${CRC_SESSION_KEY+x}" ] &&
    [ -z "${LLM_API_KEY+x}${GITLAB_PROJECT_TOKEN+x}${GITLAB_OAUTH_CLIENT_SECRET+x}" ] &&
    npm ci && npm run build) &&
  go run ./cmd/server --config config/server.example.yaml --check-config &&
  go run ./cmd/server --config config/server.example.yaml
else
  printf '缺少已保存的密钥或管理员 token；已停止。\n' >&2
  false
fi
```

全新检出没有 `web/dist` 时，`go build ./...` / `go run ./cmd/server` 会因 embed 找不到目录而失败。先执行 `(cd web && builtin unset -v REVIEW_MASTER_KEY REVIEW_ADMIN_TOKEN TRIBUNAL_SESSION_KEY CRC_SESSION_KEY LLM_API_KEY GITLAB_PROJECT_TOKEN GITLAB_OAUTH_CLIENT_SECRET && [ -z "${REVIEW_MASTER_KEY+x}${REVIEW_ADMIN_TOKEN+x}${TRIBUNAL_SESSION_KEY+x}${CRC_SESSION_KEY+x}" ] && [ -z "${LLM_API_KEY+x}${GITLAB_PROJECT_TOKEN+x}${GITLAB_OAUTH_CLIENT_SECRET+x}" ] && npm ci && npm run build)`。

示例配置可以启动服务，并提供 Bearer 认证。浏览器控制台登录另外需要 GitLab OAuth application，并在配置中设置 `auth.oauth.client_id_env` / `client_secret_env` 和绝对 HTTP(S) 地址 `server.public_url`；启用 OAuth 但缺少该地址时启动校验失败。示例配置未启用 `auth.oauth`，`/auth/login` 会返回 503：

```bash
export GITLAB_OAUTH_CLIENT_ID="..."
export GITLAB_OAUTH_CLIENT_SECRET="..."
```

`REVIEW_MASTER_KEY` 必须持久保存，重启时使用同一个值。丢失或更换它将无法解密已注册项目的凭据。不要在每次启动时重新生成。`REVIEW_ADMIN_TOKEN` 是管理员 bootstrap Bearer token，应由部署环境保管，不写入 YAML 或命令行参数。浏览器会话持久化在 SQLite；固定 `TRIBUNAL_SESSION_KEY`（旧名 `CRC_SESSION_KEY` 仍兼容，见上文兼容说明）时，未过期会话在重启后仍有效；每次启动重新生成 key 会让旧 cookie 失效并只记录告警。

未配置 OAuth 时，浏览器端无法登录；`REVIEW_ADMIN_TOKEN` 仍可作为 Bearer 使用，但它只是平台管理面 token：可调用用户、授权和系统配置接口，没有本地用户 ID，也不获得任何项目业务数据访问。已通过控制台创建的用户 API token（明文只在创建时显示一次）也继续可用于 Bearer，但创建 token 本身需要浏览器会话。浏览器会话和 API token 的 `admin`/`viewer` 标签在每次请求时按当前 `bootstrap_admins` 计算；条目推荐写 `id:<GitLab 用户 ID>`，旧的用户名写法在首次使用时绑定到当时的 GitLab 用户 ID，之后改名占用同一用户名的其他账号只得到 `viewer`（详见 [docs/operations.md](docs/operations.md)）；项目业务数据另由本地 `project_grants` 的 `read`/`manage` 决定。平台管理员不会因为 `admin` 标签而自动获得项目权限，必须显式授予 grant；无 grant 或项目不存在统一返回 404，只有 `read` 时执行写操作返回 403。`users.role` 只记录签发时的标签，不作为已有 token 的项目权限来源。`REVIEW_ADMIN_TOKEN` 由配置固定，轮换由部署环境更新变量并重启完成，不受本地用户停用影响；从 `bootstrap_admins` 移除条目并重启后，其平台管理标签失效。用户新签发的 API token 默认 90 天、最长 365 天；历史无过期时间的 token 在撤销前继续有效。

注册真实项目之前，需要把 `gitlab.url` 改为实际 GitLab 实例地址。可以用第二个 `--config` 指定覆盖文件；后面的映射递归合并，数组替换。

默认数据库为 `data/review.db`。服务启动时创建目录并执行迁移。

```bash
curl --fail http://localhost:8080/healthz
curl --fail http://localhost:8080/readyz
go run ./cmd/tribunalctl --url http://localhost:8080 projects all
```

`/healthz` 检查进程，`/readyz` 检查数据库和 worker 生命周期。非本机部署应使用 HTTPS 反向代理和访问控制，不能把明文 HTTP 管理接口暴露到公网。

## 注册项目

注册会访问真实 GitLab 并创建 onboarding 任务，可能产生模型费用。执行前必须向用户确认目标 GitLab、项目、注册动作和可能费用，并获得明确授权；未授权时不要执行。项目 token 需要 `api` 和 `read_repository` scope，自动创建 webhook 还需要 Maintainer 权限。

在 Bash 当前 shell 中先关闭 xtrace，由用户或安全密钥管理器将 token 注入 `GITLAB_PROJECT_TOKEN`，不要把真实 token 写入命令、终端、日志或聊天。不要将本步骤使用的变量设为 readonly。将 `GITLAB_PROJECT` 设为本次已确认的项目，将 `REGISTRATION_DIR` 设为仓库外、不被同步或备份的现有私有目录的绝对路径；目录及其父路径必须受控，不得有不可信的同 UID 进程。自行确认 ACL 等没有额外授权，命令只核验目录属主和模式位并解析实际路径。以下步骤需要宿主机的 Python 3；确保终端录制和日志不会捕获秘密。token 和打开的响应文件描述符可被宿主 shell 的子进程继承，因此秘密处理完成前只运行可信程序。

```bash
set +x
if {
  if ! (builtin unset -v GITLAB_PROJECT_TOKEN && [ "${GITLAB_PROJECT_TOKEN+x}" != x ]) 2>/dev/null; then
    printf '无法确认项目 token 可以清除；已停止，请结束受影响的 shell。\n' >&2
    false
  elif [ "${REGISTRATION_CREATED_FILE+x}" = x ] || [ "${REGISTRATION_CREATED_ID+x}" = x ]; then
    printf '本 shell 已有注册标记；请先安全处理响应，再使用新 shell。\n' >&2
    false
  elif ( : <&9 ) 2>/dev/null; then
    printf '文件描述符 9 已被占用；请使用新 shell。\n' >&2
    false
  elif ! (builtin unset -v REGISTRATION_FILE && [ "${REGISTRATION_FILE+x}" != x ]) 2>/dev/null ||
       ! (builtin unset -v registration_confirm && [ "${registration_confirm+x}" != x ]) 2>/dev/null ||
       ! (builtin unset -v registration_dir && [ "${registration_dir+x}" != x ]) 2>/dev/null ||
       ! (builtin unset -v registration_identity && [ "${registration_identity+x}" != x ]) 2>/dev/null; then
    printf '响应路径或确认变量无法清除；已停止。\n' >&2
    false
  elif ! builtin unset -v PROJECT_ID 2>/dev/null || [ "${PROJECT_ID+x}" = x ]; then
    printf '无法清除项目 ID；已停止。\n' >&2
    false
  elif [ -n "${REGISTRATION_FILE:-}" ]; then
    printf '先安全处理上次的私有响应文件；已停止。\n' >&2
    false
  elif [ -z "${GITLAB_PROJECT_TOKEN:-}" ] || [ -z "${GITLAB_PROJECT:-}" ] ||
     [ -z "${REGISTRATION_DIR:-}" ] || [[ "$REGISTRATION_DIR" != /* ]] || [ ! -d "$REGISTRATION_DIR" ] ||
     ! command -v python3 >/dev/null 2>&1; then
      printf '缺少项目 token、已确认的项目、私有目录绝对路径或 Python 3；已停止。\n' >&2
    false
  else
    export GITLAB_PROJECT_TOKEN
    printf '已获用户授权注册已确认的项目并承担可能费用？输入 yes 继续：' >&2
    if ! IFS= read -r registration_confirm || [ "$registration_confirm" != yes ]; then
      printf '未确认；已停止。\n' >&2
      false
    elif ! registration_dir="$(python3 -I -c 'import os, stat, sys
p = os.path.realpath(sys.argv[1])
s = os.stat(p)
if not os.path.isabs(p) or "\n" in p or not stat.S_ISDIR(s.st_mode) or s.st_uid != os.geteuid() or s.st_mode & 0o077: sys.exit(1)
sys.stdout.write(p)' "$REGISTRATION_DIR" 2>/dev/null)"; then
      printf '无法核验私有目录；已停止。\n' >&2
      false
    elif ! REGISTRATION_FILE="$(umask 077; mktemp "$registration_dir/tribunal-registration.XXXXXXXX" 2>/dev/null)"; then
      printf '无法创建私有响应文件；已停止。\n' >&2
      false
    elif ! { exec 9< "$REGISTRATION_FILE"; } 2>/dev/null; then
      printf '无法保持本次响应文件；已停止并保留路径供安全核对。\n' >&2
      false
    elif ! registration_identity="$(python3 -I -c 'import os, re, stat, sys
p, d = sys.argv[1:]
s = os.lstat(p)
f = os.fstat(9)
if os.path.dirname(p) != d or not re.fullmatch(r"tribunal-registration\.[A-Za-z0-9]{8}", os.path.basename(p)) or not stat.S_ISREG(s.st_mode) or s.st_uid != os.geteuid() or s.st_mode & 0o777 != 0o600 or s.st_nlink != 1 or (s.st_dev, s.st_ino) != (f.st_dev, f.st_ino): sys.exit(1)
sys.stdout.write(str(s.st_dev) + ":" + str(s.st_ino))' "$REGISTRATION_FILE" "$registration_dir" 2>/dev/null)"; then
      exec 9<&-
      printf '无法核验本次响应文件；已停止并保留路径供安全核对。\n' >&2
      false
    elif ! builtin readonly REGISTRATION_CREATED_FILE="$REGISTRATION_FILE" REGISTRATION_CREATED_ID="$registration_identity"; then
      exec 9<&-
      printf '无法记录本次响应文件；已停止并保留路径供安全核对。\n' >&2
      false
    elif ! go run ./cmd/tribunalctl --url http://localhost:8080 projects register \
      --project "$GITLAB_PROJECT" --token-env GITLAB_PROJECT_TOKEN \
      --profile default > "$REGISTRATION_FILE" 2>/dev/null; then
      printf '注册失败或结果不确定；停止并核对状态，不要直接重试。\n' >&2
      false
    elif ! PROJECT_ID="$(python3 -I -c 'import json, sys; value = json.load(sys.stdin)["project"]["id"]; print(value) if type(value) is int and 0 < value <= 9223372036854775807 else sys.exit(1)' < "$REGISTRATION_FILE" 2>/dev/null)"; then
      printf '无法读取项目 ID；保留私有响应并核对注册状态，不要直接重试。\n' >&2
      false
    fi
  fi
}; then
  if builtin unset -v GITLAB_PROJECT_TOKEN 2>/dev/null && [ "${GITLAB_PROJECT_TOKEN+x}" != x ]; then
    printf '%s\n' "$PROJECT_ID"
  else
    printf '无法清除项目 token；注册可能已成功，不要直接重试。请结束受影响的 shell 并安全处理私有响应。\n' >&2
    false
  fi
else
  if ! builtin unset -v GITLAB_PROJECT_TOKEN 2>/dev/null || [ "${GITLAB_PROJECT_TOKEN+x}" = x ]; then
    printf '无法清除项目 token；请结束受影响的 shell 并安全处理私有响应。\n' >&2
  fi
  false
fi
```

`--project` 也可以是 GitLab 项目 ID 或配置的 GitLab 根地址下的完整 HTTP(S) 仓库 URL。命令块会尝试清除当前 shell 的项目 token；无法确认或清除时返回失败，保留响应路径并提示结束受影响的 shell。即使最后清除 token 失败，注册也可能已经成功；不要直接重试，先安全核对项目和 webhook 状态。信号中断可能跳过清理；中断后先尝试在当前 shell 清除 token，若不能确认清除则结束该 shell。清除当前变量也不会清除已继承 token 的子进程或其他副本。响应先写入新建的 0600 私有文件，标准输出只显示项目 ID；该正整数校验不能证明 onboarding 已完成，须另行检查任务状态。确认提示和不含秘密的错误信息写入标准错误。不要输出、复制到日志或直接打开响应文件。默认手工创建 webhook，响应中的 `webhook_secret` 只返回一次。如何通过受控渠道填写 GitLab 的 Secret token、清理私有文件，以及失败时如何处理，见[部署与运维的项目注册步骤](docs/operations.md#浏览器登录与项目注册)。URL 为服务地址加 `/webhooks/gitlab`，启用 Merge request、Comments 和 Push 事件。

控制台先「校验连接」，再「保存并初始化」。管理员也可调用 `POST /api/v1/projects/validate`，提交 `{project, token}`，只读验证身份、至少 Reporter 权限、scope、有效期和 Git HEAD；不会保存凭据、调用模型或创建 webhook。校验成功不证明完整 clone 或评论发布可用，详见 [项目 API](docs/design/03-http-api.md#41-项目)。设置页的「更新访问 Key」只更新本平台凭据，不在 GitLab 创建或撤销 token。

自动创建时在同一个注册命令的 `--profile default` 后添加 `--auto-webhook`，仍须将响应写入私有文件并安全清理；把 `server.public_url` 配为 GitLab 能访问的服务地址。重复注册返回冲突，不覆盖已有凭据。自动创建 hook 成功但入库失败时会尝试删除刚创建的 hook；网络中断等异常仍可能需要管理员核对 GitLab 上的 hook。

CLI 默认读取 `REVIEW_ADMIN_TOKEN`，可以用全局参数 `--api-token-env NAME` 改变环境变量名。注册响应和普通查询都不会返回项目 access token 或数据库密文。项目业务列表按当前 bearer 所属用户的项目授权过滤；静态 bootstrap token 没有用户 ID 和 grant，因此 `tribunalctl projects list` 返回空列表是正常的。管理面列出全部已登记项目用 `tribunalctl projects all`。用户和授权管理命令：

```bash
tribunalctl users list
```

查询用户需要平台管理员凭据；用户自己的 API token 只用于其已有项目 grant 的业务访问。首次授权前，目标用户需要先通过 OAuth 登录一次，才能在 `users list` 中得到 GitLab 用户 ID。`users disable` 会同时撤销该用户的全部会话和 API token；`users revoke` 只撤销凭据，不禁用账号。已有部署升级后所有用户默认没有 project grant，包括平台管理员；先确认具体用户、项目和动作，再按[部署与运维](docs/operations.md#项目授权用户停用和凭据撤销)中的单次授权或用户停用步骤执行，不要默认授予任何用户。

## 入口与队列

| 入口 | 当前行为 |
|---|---|
| MR open / reopen / update（带 oldrev） | 入队 `review.run`；Draft/标签/目标分支在 hook 或 worker 合并仓库配置后过滤 |
| MR close / merge | 入队 `review.cancel` |
| MR 评论 `/review`、`/review full`、`/review <profile>`、`/review cancel` | 先入队 `review.command`，worker 检查机器人身份、MR 状态和 Developer 以上权限，再入队目标任务 |
| 目标分支 push | 入队 `workspace.refresh`；分支删除不入队 |
| 项目注册 | 原子写入项目、初始工作区和 `workspace.onboard` 任务 |
| API 重新发布 | 入队 `review.publish` |

Webhook 不调用 GitLab 或模型。认证失败返回 401；数据库失败返回 503 和 `ignored: storage_error`，不写去重记录、不假报已接收；其他情况返回 200，并通过 `accepted` 或 `ignored` 区分结果。缺少或非法的 `X-Gitlab-Event-UUID` 会被忽略。GitLab 不会自动重试失败的投递：503 会让这次投递在 GitLab 的 Webhook「Recent events」里显示为失败，可以用 Resend request 重发，同一个事件 UUID 会被正常受理。GitLab 连续多次收到失败响应时可能自动停用这个 hook，需要关注 GitLab 端的 hook 状态。

启用仓库配置（`allow_repo_config=true`）时，Draft 的最终过滤在 worker 读取目标分支配置后完成，webhook 保持无网络调用；标签只允许仓库追加排除，项目级早期过滤仍安全；目标分支由 worker 按实际 MR 重新确认。自动评审、裸 `/review` 和未带 profile 的手动 API 优先采用仓库 `profile`；显式命令或显式 API profile 优先。

`GET /api/v1/projects` 按当前用户的项目授权过滤；静态 bootstrap bearer 或没有 grant 的账号会得到空列表。管理面的 `/api/v1/admin/projects`、`tribunalctl projects all` 返回全部登记项目，用于选择授权对象。`projects`、`users`、`grants`、`api-tokens`、`mcp-servers`、`skills`、`block-bindings` 列表使用游标分页，默认 50、最大 200。`tribunalctl projects list`、`tribunalctl projects all` 和 `tribunalctl users list` 会自动遍历所有页；只调用一次并假定返回全量列表的旧客户端现在只会得到第一页（最多 50 条），必须循环 `next_cursor`。

UUID 在项目内去重，去重记录和入队在同一事务中提交。同一 key 最多保留一个 running 和一个 queued 任务；新 webhook 或通过权限检查的评审命令替换待执行任务的内容，不覆盖正在执行的内容。同一 key 的任务不会并行领取。完成、失败及续租均校验 worker、attempt 和租约，过期 worker 无法覆盖新执行结果。替换 payload 不重置 attempt，以免旧执行编号重新生效。

服务关闭时取消正在执行的 handler；尚未结算的任务保留 running，重启后待租约到期恢复，默认最多等待约 5 分钟。

`review.run`、`review.cancel`、`review.command`、`review.publish`、`workspace.onboard` 和 `workspace.refresh` 均已注册处理器。`/review help` 和权限不足由 command worker 回复一条说明；每条回复带该源 note ID 的隐藏 marker，重试时更新同一条回复，不重复创建。受理 run/cancel 时，入队事务记录源 note 的 eyes，终态由状态 worker 协调；help/拒绝不添加表情。webhook 仍不调用 GitLab，也不添加 emoji。目标分支模式采用 Go `path.Match`，不提供递归 `**` 语义。

## 默认扩展

新部署首次启动时会自动登记内置的平台级 Skill `code-review`，并挂到 `code-review` 和 `review` 评审块。它改编自 [mattpocock/skills 的 code-review](https://www.skills.sh/mattpocock/skills/code-review)（MIT，出处见 `internal/skill/builtin/code-review/NOTICE.md`），提供 Standards / Spec 双轴评审方法和代码坏味道基线，改成了无需交互的版本。

- 控制台「扩展 → Skill」中，内置项带「内置」标签，可以禁用或重新启用，但不能删除（API 返回 409）。禁用后不再加载，仍引用它的块会跳过它；重启和升级都保留禁用状态。
- 平台升级后，下次启动会把已登记副本的文件和描述更新为新版本附带的内容，启用状态不变。
- 在「块绑定」中把它从某个块移除后，重启不会恢复；升级也不会改写管理员改过的绑定。
- 配置 `default_extensions.skills: []` 可以让新部署不登记默认 Skill。详见 [默认扩展配置](docs/design/06-config.md#默认扩展default_extensions)。

Docker 镜像还内置校验过的 [codebase-memory-mcp](https://github.com/DeusData/codebase-memory-mcp) 0.12.1（MIT）代码图谱服务和隔离 wrapper `/usr/local/bin/tribunal-codebase-memory-mcp`。示例配置和 `config/server.docker.yaml` 会登记内置平台 MCP 服务器 `codebase-memory`（只读 analysis 工具面），并为 `code-review`、`review`、`verify` 块授权只读图谱工具；注册项目后 onboarding 用 CLI 为 `base/` 建索引，每个项目的索引只在自己的工作区内可见。

- 控制台「扩展 → MCP 服务器」中它带「内置」标签，可以测试、停用和重新启用，不能编辑或删除（API 返回 409）。重启和升级都保留停用状态。
- 平台升级后，下次启动会把它的配置和默认工具白名单更新为新版本附带的内容；启用状态和管理员改过的块绑定不变。
- 二进制缺失或启动失败时评审照常完成，带 `MCP tools unavailable` 告警，原本的 PASS 显示为 INCOMPLETE。
- 停用方式和版本升级步骤见 [内置 codebase-memory-mcp](docs/design/06-config.md#内置-codebase-memory-mcp)。

## 验证

```bash
(cd web && builtin unset -v REVIEW_MASTER_KEY REVIEW_ADMIN_TOKEN TRIBUNAL_SESSION_KEY CRC_SESSION_KEY \
  LLM_API_KEY GITLAB_PROJECT_TOKEN GITLAB_OAUTH_CLIENT_SECRET &&
  [ -z "${REVIEW_MASTER_KEY+x}${REVIEW_ADMIN_TOKEN+x}${TRIBUNAL_SESSION_KEY+x}${CRC_SESSION_KEY+x}" ] &&
  [ -z "${LLM_API_KEY+x}${GITLAB_PROJECT_TOKEN+x}${GITLAB_OAUTH_CLIENT_SECRET+x}" ] &&
  npm test && npm run build) &&
go build ./... &&
go vet ./... &&
go test ./... &&
go test -race ./... &&
go mod verify
```

`go build` 依赖 `web/dist`，因此在全新检出上必须先执行前端构建。完整 Go 测试需要 Bash 和真实的 Python 3（注册文档测试会执行 Python 3）；`go test -race` 还需要启用 cgo 和可用的 C 编译器。自托管 runner 必须在运行前确认这些前提。生产二进制和部署运行不依赖 Python 3；上面的人工注册文档示例单独需要宿主机 Python 3。测试使用临时 SQLite 数据库和本地 HTTP 假 GitLab，覆盖注册、加密、认证、事件过滤、并发去重、租约恢复、旧 attempt 拒绝、CLI、工作流、发布、工作区、扩展和控制台静态资源。

各版本做过哪些验证见 [交付就绪与验收记录](docs/delivery-readiness.md)：当前版本看 [当前版本（v1.1.x）验证状态](docs/delivery-readiness.md#当前版本v11x验证状态)，v1.0.0 的 f143 隔离实例验收看 [v1.0.0 历史验收记录](docs/delivery-readiness.md#v100-历史验收记录f143仅供追溯)。

## 主体已实现 / 范围外 / 已知限制

**已实现（本地）**

- 项目注册、token 校验/加密、webhook 去重与持久化任务队列。
- OpenAI Chat Completions、OpenAI Responses、Anthropic Messages 三种协议；agent 工具循环。
- DAG 引擎、评审块、主 agent 编排、verify/裁决、结果发布与 republish。
- 工作区 onboarding/刷新、上下文文档、MCP/Skill 与项目级绑定。
- OAuth 持久会话、CSRF、平台 admin/viewer 标签、项目 `read`/`manage` 授权、用户停用与凭据撤销、API token（默认 90 天、最长 365 天）、管理 API、React 控制台、反馈收集。
- 总体/项目/provider/块的运行次数、成本、平均/P95 耗时、样本数及 UTC 日趋势。项目统计工作流，provider/块统计已启动节点，界面可展开每日明细；不把节点重试或多轮调用当作额外运行。筛选与分桶均以 workflow 创建时间为准，详见 [统计 API 口径](docs/design/03-http-api.md#45-系统)。
- 项目/评审摘要、运行趋势，以及按 provider 的提出方准确率和挑战方误杀率；原始反馈计数单独展示，不作为人工结论。
- finding 人工标注：项目 `manage` 权限可保存、更正或清除有效/无效/未确定结论，`read` 只读；保存操作者与时间，同一 finding ID 的重新发布保留标注。`quality_by_provider` 展示提出方准确率、被挑战推翻比例、挑战方误杀率及样本数，未标注/未确定不进入人工结论分母。
- DAG 并发提交在 revision 前进时继续重试，同 revision 连续冲突有界失败；完成写入的持久化错误显式返回。finalize 遇到并发新增节点会回到调度循环。deep 完整执行、失败评审摘要、optional verify 失败转 uncertain、MR 描述的不可信边界与 P1 禁止 PASS 均有本地集成测试。模型替身只证明调用链和确定性约束，不证明真实模型抗提示注入。

**范围外 / 不构成发布门槛**

生产规模、负载和计费、物理设备/完整 WCAG、Postgres、其他协议/provider、每个 MCP HTTP backend 的具体实现不在当前范围，不作为发布条件。

**已知限制**

- SSE 通过每 2 秒轮询数据库产生事件，不是推送。
- 统计按筛选范围读取 workflow 和节点，并在内存中保存耗时样本计算 P95；`/providers` 的近 24 小时统计也复用该完整聚合。未做生产规模压测，大数据量下的响应时间与内存占用仍待测量，不能据本地测试声称满足容量目标。
- 数据库中的平台/项目 MCP、Skill 和绑定变更按进程内缓存生效，通常需要重启服务。
- 数据库 MCP 配置在保存时保留 `${NAME}` 模板，在启动、项目解析和测试时展开；缺失或禁止的变量、未知字段会拒绝执行。MCP 测试始终使用当前记录的配置和作用域，不按同名平台实例复用。相对工作区路径会规范化；项目删除会清理项目工作区和受管技能目录，并拒绝通过根或父级符号链接删除外部目录。
- 数据保留清理已实现（`internal/app/retention_service.go`，默认每 6 小时）：`llm_calls`、`webhook_deliveries`、终态 `jobs` 按配置保留期删除，`0005_retention_indexes.sql` 为这些扫描增加索引；`context_docs` 对每个 `(project, kind)` 保留最近 `storage.context_versions` 个非 active 且未锁定版本，active 版本和所有 locked 版本不删除。
- 上下文文档保存采用追加版本：每个 `(project, kind)` 只有一个 active 版本。人工保存会追加 active 版本；生成任务在当前 active 版本 locked 时写入新版本但不切换；同一 base 和 workflow 的重试复用已有版本。数据库是 source of truth，人工保存和锁定会同步工作区上下文文件与 metadata；`workspaces.context_sha` 只跟踪 agents。
- 仓库级 `.gitlab-review.yml` 在评审 worker 中按目标分支合并。`allow_repo_config=true` 时 Draft 由 worker 最终判断；标签只追加排除；目标分支不能被仓库配置覆盖。手动命令不受自动 Draft/标签/分支过滤限制。
- 后台 run cleanup 在启动后立即执行、之后每小时运行。它先读取项目列表、保护集合和上下文 SHA 集合，再进入项目文件：删除超过 `workspace.run_cleanup_after` 且未被未终态 workflow、queued/running job、`publication_state=pending` 的 dossier 或本进程注册 run 引用的 `runs/<id>`；同时删除超过同一阈值、未被任何 `context_docs` 记录或当前 workspace/未终态/待发布 workflow base 保护的 `context/<sha>`。metadata 标记 locked 或 metadata 缺失/非法时保留并 fail closed。项目列表查询失败时整轮不删除；某个项目的保护查询失败时跳过该项目。缺失 mirror 时只有通过全部检查的 run 才删除；其他 Git 失败保留目录。
- 浏览器会话持久化在 SQLite，cookie 保存随机 token 及其 HMAC 签名；固定 `TRIBUNAL_SESSION_KEY`（旧名 `CRC_SESSION_KEY` 仍兼容）时重启后未过期会话仍有效。直接运行应用且新旧名都未配置时，每进程随机生成 key 并告警，重启后旧会话失效；本仓库 Compose 要求配置其中一个名字。每个用户最多保留 10 个浏览器会话，超出时只淘汰该用户自己最旧的会话。浏览器会话和 API token 的 `admin`/`viewer` 标签每请求按 `bootstrap_admins` 计算（用户名条目绑定首个 GitLab 用户 ID），重启并移除管理员配置后平台管理权限同步降级；项目业务数据由 `project_grants` 独立控制，默认无权限，平台管理员也不例外。静态 `REVIEW_ADMIN_TOKEN` 保持平台管理权限，但没有用户 ID 和项目授权。
- 项目列表返回 `workspace` 和 `last_review`，项目详情返回 `workspace` 和最多 20 条 `recent_reviews`，并保留原有扁平项目字段；列表当前按项目逐项补充摘要（每个项目 2 次读取），分页默认 50、最大 200。评审详情的 `mr_title` 来自已存储 dossier；`publications.commit_status` 只从 `status:<sha>:<state>` 发布键解析 `success`/`failed`，旧键没有状态时视为未知。
- 反馈采集写入 `thumbsup` / `thumbsdown` 与 `resolved_*` signal。讨论只采集发布者首条 note 的 award，不把 thread reply 的表情当作对 finding 的反馈；汇总评论的 award 保持未关联 finding。`stats.feedback` 返回原始计数 `{total, by_signal, by_provider}`，按项目和 `observed_at` 的 `[from,to)` 过滤；归因只统计能按持久化 `feedback.finding_id` 精确匹配 finding 且 publication 一致的 discussion。summary、unlinked 和关联不一致的信号仍计入原始总数；同一 signal 可同时计入提出方和挑战方，两个角色的计数不能相加当作总数。它们不自动修改人工标注。顶层 `overturned_ratio` 是规则裁决中 `drop` 的占比，不是挑战推翻率或误杀率。
- 人工标注仅保留最新结论，采用最后一次成功写入，不提供历史审计或并发编辑冲突提示；清除时一并清空操作者和时间。质量指标按 workflow 创建时间筛选，但使用当前标注，因此旧时间窗的结果可随更正变化。去重后的 finding 每行只归给保存的提出方，挑战方按同一 workflow 的 `challenge_node` 关联；缺失 provider 的角色不计入该角色统计。样本可能存在人工选择偏差，不代表模型总体效果；标注不改变发布结果或 verdict。
- 覆盖缺口是硬完成条件：`DiffSummary.files` 只表示进入材料快照的文件数，不是已评审数；`omitted` 是容量截断或 GitLab 未提供 diff 等原因，`ignored` 是用户规则排除，两者不同。存在 omitted、评审节点实际遇到的提示词截断、完成 plan 后未分配的非 generated included 文件或上游证据不足时，最终结论为 `INCOMPLETE`；全部 omitted 不建 workflow，只发布 `INCOMPLETE` 摘要，列出 omitted 总数、原因计数和最多 4 条路径，其余只计数；正常 dossier 保留完整的 omitted 列表。全部被 ignore 时发布「无需评审」。
- workflow token 预算是共享上限，按 `input_tokens + output_tokens` 计算；cache read/write 和 reasoning 在 usage 中分别表示，但不额外相加，避免与输入/输出重复计数。每次模型请求前检查已提交用量加当前 attempt 已知 partial usage；并发或已发出的请求仍可能在检查后完成并产生有界超支，不能当精确账单。失败重试的已知 usage 计入下一次尝试的预算和最终节点用量；预算不足保留已有结果，按 required/optional 和确定性兜底处理，不无提示 PASS。
- `synthesize` 的预算/传输失败直接走确定性规则兜底，不重试；其他 agent 节点仍对 transport 错误重试。
- 共识投票和 challenger 选择按配置的 endpoint + model 身份归一，同端点同模型的 provider 别名不增加独立票数。无法解析身份的多个 provider 共用同一个 unknown bucket：known 与 unknown 可各计一票，unknown 之间不增加独立票；metadata 缺失会限制挑战者独立性，不能据此声称跨模型确认。没有独立 challenger 时使用已声明的 fallback 并显式提示。`CodeRef` 反证在固定 head/base 快照中检查路径和行号，缺失快照或读取失败降为 `UNCERTAIN`。
- 评审执行状态和 GitLab 发布状态分开保存：`publication_state` 为 `pending`、`published`、`skipped` 或 `failed`，失败原因有界脱敏保存。pending 在重启后恢复；stale、取消和关闭 failure note 记为 skipped；永久发布失败需手动 republish。`INCOMPLETE` 和 `BLOCKING` 都不会写成 commit status success。
- Web 依赖为 `react-router-dom 7.18.4`、`vite 7.3.7`、`vitest 4.1.12` / `@vitest/coverage-v8 4.1.12`。Vitest 4 改用 AST 重映射 V8 覆盖率，阈值现为语句 74%、分支 72%、函数 71%、行 78%；升级前旧记录中的 41.86% 与当前数字不可直接比较。
- 统计页项目筛选使用游标分页，每次加载 100 项，可继续加载或重试；使用 `usePaginated` 的分页列表的“重试加载更多”沿用失败请求的同一 cursor，首屏重试或“刷新”才从第一页重新加载。统计日期筛选把结束日映射到次日本地午夜，服务端按 `[from,to)` 计数。
- 统计页、总览和项目详情均把 `stats.runs` 标为“运行次数”，并说明包含评审和 onboarding。`blocking_ratio` 的分母仍是全部 workflow，因此 onboarding 会稀释该比例，不应把它解释为仅评审的阻断比例。
- 节点 provider/model 表示启动时选择的配置，不等于模型调用次数或响应实际模型标识；reasoner 自跳过和 agent synthesize 回退仍可能带归属。修复前历史节点的空归属不回填，因此旧时间窗的按 provider 节点统计仍不完整；逐次调用信息可查 `llm_calls`。
- 许可证发布清单是单一文件 `licenses/THIRD_PARTY_NOTICES.txt`，由 `go run ./scripts/licensebundle` 生成，CI 用 `-check` 验证。内容覆盖 Go 标准库、Linux/amd64 服务端的 40 个模块、9 个 production npm 包、内置 Skill 和镜像内置的 codebase-memory-mcp；主机模块清单是 42 个，与运行时清单不同。当前 Dockerfile 仍复制遗留的 `THIRD_PARTY_LICENSES`，但发布声明以新单文件为准。
- MCP `tools/call` 已在两家 provider 的实际 deep 中验证（成功/调用 10/10、10/11；第二次一次模型工具错误已纠正），`delete_project` 不可用，项目只看到自己的索引；真实 stdio 已测，并非每个 HTTP backend 都已验证。详见 [ARCH §8.4](ARCH.md#84-codebase-memory-mcp-接入要点)。
- OpenAI Responses adapter 在每次 Generate 时发送请求级随机不透明 `X-Session-Id`；仅 Responses 协议，无新配置、依赖或 400 重试。该 header 是网关兼容措施，不是 OpenAI 标准、会话认证或幂等保证；已部署网关的内部实现未证实，可能增加缓存/握手成本。
- Postgres 未实现，配置 `storage.driver: postgres` 会在启动时报错。
- 评审命令受理时在入队事务中记录源 note 的 eyes，终态由状态 worker 协调为完成/失败标记或清除；`/review help` 和权限拒绝只回复说明、不添加表情。Webhook 自身仍无网络调用，也不添加 emoji。

**用户确认的验收口径（2026-10-23、2026-10-25）**

- Issue #10「沿用现状」：不增加空评审的 skipped workflow。所有变更文件都被 ignore 时发布「无需评审」，不建 workflow；所有文件都因采集缺口 omitted 时也不建 workflow，只发布 `INCOMPLETE` 摘要：列出 omitted 总数、原因计数和最多 4 条路径，其余只计数；正常 dossier 保留完整的 omitted 列表。最终代次检查拒绝检查时已过期的结果，但检查与 GitLab 写入并非原子操作；发布窗口限制见[关键流程](docs/design/05-flows.md)。
- Issue #11「维持保护策略」：每个 `(project, kind)` 保留 5 个未锁定且非 active 的历史版本；active 版本和全部 locked 版本额外保护，总数可以超过 5。不扩大删除范围。
- Issue #4「独立实现验收」：接受现有独立 Go DAG 实现，不要求移植上游代码，不虚构上游 NOTICE、版权人或来源。该结论只验收本地实现，不是新的上游授权或法律审计。
- Issue #3（2026-10-25 确认）：按既定发布键边界验收。相同事件 UUID、相同 `publish_key` 和同一评审重复发布不新增重复评论；显式新评审可创建新代次，模型换标题可能再次发布同一问题。本版不增加跨次问题语义去重。真实相同 UUID 并发重放三次和重复发布两次均未增加工作流、评论或讨论。
- 上述口径只覆盖所列验收点，不代表整个 MVP 已验收；该确认不构成新的上游授权或法律审计。
