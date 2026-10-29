# 部署与运维（Docker / SQLite）

本文对应 ARCH14：内嵌控制台的服务镜像、镜像内 `git` 与管理 CLI、许可证文件、SQLite 单实例、非 root 用户和可写数据目录。各版本做过哪些验证见[验证状态](#10-验证状态)。

交付前准备、升级/回退边界与真实验收记录见 [交付就绪与验收记录](delivery-readiness.md)。

> 当前已发布版本是 v1.1.0，验证状态见 [v1.1.x 验证状态](delivery-readiness.md#v110-验证状态)。v1.2.0 尚未发布，版本说明已合并，尚未打 tag；其验证状态见 [v1.2.0 验证状态](delivery-readiness.md#v120-验证状态)。从 v1.0.0 升级前必须先读 [从 v1.0.0 升级到 v1.1.0](#从-v100-升级到-v110)；从 v1.1.0 升级再读 [从 v1.1.0 升级到 v1.2.0](#从-v110-升级到-v120)。由 agent 代为部署时，使用 [Agent 部署指南](agent-deployment.md)。

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
| `REVIEW_MASTER_KEY` | base64，解码后恰好 32 字节；仅首次生成，必须连同数据备份 |
| `REVIEW_ADMIN_TOKEN` | 至少 32 字节，管理 API 的 Bearer token |
| `TRIBUNAL_SESSION_KEY` | base64，解码后至少 32 字节；独立生成，跨重启保持不变 |
| `TRIBUNAL_PUBLIC_URL` | 用户和 GitLab 可访问的服务地址；生产应为 HTTPS，默认仅供本机使用 |
| `TRIBUNAL_HTTP_PORT` | 宿主机端口，默认 8080；容器始终监听 8080。改变它时检查 `TRIBUNAL_PUBLIC_URL` 是否也需修改 |

首次生成使用[私有密钥生成步骤](agent-deployment.md#2-准备配置)，不要直接在终端打印密钥。旧环境变量名 `CRC_SESSION_KEY`、`CRC_PUBLIC_URL`、`CRC_HTTP_PORT` 仍兼容；新旧同时设置时新名优先。三个旧名只要配置了非空值，都会各记录一次弃用日志；日志只含变量名、不含值。日志表示旧名仍配置，不代表旧值一定生效或一定被忽略：Compose 会把仅设置的 `CRC_HTTP_PORT` 作为 `TRIBUNAL_HTTP_PORT` 的插值回退值，仅用于宿主机端口映射，并把仅设置的 `CRC_SESSION_KEY` 复制给容器的 `TRIBUNAL_SESSION_KEY`。`TRIBUNAL_HTTP_PORT` / `CRC_HTTP_PORT` 只影响宿主机映射，不是应用监听端口。`TRIBUNAL_SESSION_KEY` 未设置或恰好为空字符串时才回退旧名；非空但只含空白是非法值，启动失败，不回退旧名，也不生成临时 key。完整兼容规则见[环境变量](design/06-config.md#5-环境变量)。升级时必须复用原 session key 的同一个值，不要重新生成，否则现有浏览器会话失效。

`.env` 值包含 `$` 或 `#` 时使用单引号。不要把真实密钥写进 YAML、提交到仓库或放进 Docker build args。Compose 从 `.env` 做变量替换，再通过 `environment` 允许列表传入容器，不会把整个 `.env` 挂载或注入。宿主机已导出的同名变量优先于 `.env`；部署前检查是否有旧值，但不要把秘密输出到日志。环境变量仍对 Docker 管理员可见。

`config/server.docker.yaml` 只读挂载到 `/app/config/server.yaml`。它绑定一个名为 `primary` 的 provider，`onboarding` 档位用其初始化仓库上下文，`default` 档位用同一个 provider 执行 `code-review` 和 challenge 核验；plan 和 synthesize 沿用代码模式，不默认启用多模型对抗。该示例配置的数据库与工作区分别使用 `/app/data/review.db` 和 `/app/data/workspaces`；实际路径以生效配置和挂载为准。其他参数沿用应用内置默认值。不要移除 `profiles.onboarding.agent.provider` 或 `profiles.default.challenge.provider`：前者会导致工作区初始化失败，后者会导致评审图构建失败，即使健康检查和 `--check-config` 通过。

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
2. 在 `.env` 设置 `GITLAB_OAUTH_CLIENT_ID`、`GITLAB_OAUTH_CLIENT_SECRET` 和 `GITLAB_ADMIN_USERNAME`。管理员值填写 `id:<GitLab 用户 ID>`，必须先向用户确认实际数字 ID 和授权；GitLab 用户名写法仍兼容但已弃用（见[用户名写法弃用与迁移](#bootstrap_admins-用户名写法弃用与迁移)）；留空不会给任何正常账号平台管理员标签。该标签只用于平台管理面，不授予项目业务数据访问，项目授权见下文。
3. 在 `config/server.docker.yaml` 取消 `oauth` 及两个子字段、`bootstrap_admins` 的注释。设置正确的 HTTPS `TRIBUNAL_PUBLIC_URL`，配置反向代理，再执行 `docker compose up -d --force-recreate --wait --wait-timeout 120`。

注册会访问真实 GitLab 并创建后台 onboarding 任务，可能产生模型费用。执行前必须向用户确认目标 GitLab、项目、注册动作和可能费用，并获得明确授权；未授权时不要执行。

模型密钥不是 GitLab 项目 token。在 Bash 当前 shell 中先关闭 xtrace，由用户或安全密钥管理器将项目 token 注入 `GITLAB_PROJECT_TOKEN`，scope 为 `api` 和 `read_repository`。不要将本步骤使用的变量设为 readonly。不要把真实 token 写入命令、终端、日志、聊天或长期运行服务的 `.env`。将 `GITLAB_PROJECT` 设为本次已确认的项目，将 `REGISTRATION_DIR` 设为仓库外、不被同步或备份的现有私有目录的绝对路径；目录及其父路径必须受控，不得有不可信的同 UID 进程。自行确认 ACL 等没有额外授权，命令只核验目录属主和模式位并解析实际路径。以下步骤需要宿主机的 Python 3；确保终端录制和日志不会捕获秘密。token 和打开的响应文件描述符可被宿主 shell 的子进程继承，因此秘密处理完成前只运行可信程序；Docker 管理员仍可访问容器环境：

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
    elif ! docker compose exec -T -e GITLAB_PROJECT_TOKEN review /app/tribunalctl projects register \
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

命令块先检查 token、响应路径和确认变量能否清除，并清除旧项目 ID；检查失败时不注册。注册后尝试清除当前 shell 的项目 token，只有确认清除后才输出项目 ID；清除失败则返回失败并提示结束受影响的 shell。此时注册可能已经成功，不要直接重试；安全核对项目和 webhook 状态。项目 ID 只证明响应中的 `project.id` 是正整数，不能证明 onboarding 任务已完成。检查和实际清理并非原子操作，信号中断也可能跳过清理。清除当前变量不会清除已继承 token 的子进程或其他副本。命令拒绝相对目录，核验目录属于当前用户且没有组或其他用户权限，并解析符号链接得到绝对路径；`mktemp` 在该目录创建唯一的 0600 文件。命令在当前 shell 保持文件描述符 9 打开，以防文件删除重建后复用 inode；描述符 9 已被占用时不注册。注册响应直接进入该文件，标准输出只显示经过正整数校验的项目 ID；确认提示和不含秘密的错误信息写入标准错误。保持同一个 shell 中的 `REGISTRATION_FILE`、只读的本次文件标记和描述符 9，直到本次文件安全处理完成；每次新的注册使用新 shell。目录及父路径必须始终受控；这些检查不能原子防护不可信同 UID 进程或不受控目录的替换。手动模式中的 `webhook_secret` 只返回一次；不要在终端、日志、聊天或命令行参数中输出它，也不要用 `cat` 查看响应。自动创建时在同一个注册命令的 `--profile default` 后添加 `--auto-webhook`，仍须使用并清理私有响应文件；项目 token 还需 Maintainer 权限，`TRIBUNAL_PUBLIC_URL` 必须可由 GitLab 访问。完整权限与 webhook 事件见 [README 注册项目](../README.md#注册项目)。

手动配置时，在已验证 HTTPS 地址的 GitLab 项目 Webhooks 页面填写服务 URL、事件及 Secret token。仅在无录屏、无剪贴板历史或云同步、没有不可信进程的受控本地会话中，才可通过标准输入交给获批准的密钥管理器并自动填入 Secret token，不要将其发送到聊天或日志。例如在满足上述条件的 macOS 本地会话中，下列命令核对本次只读标记、路径和描述符 9，并从描述符读取该字段送入系统剪贴板，不输出到终端；若任何一步失败则停止：

```bash
(
  set +x
  set -o pipefail
  [ -n "${REGISTRATION_FILE:-}" ] && [ "$REGISTRATION_FILE" = "${REGISTRATION_CREATED_FILE:-}" ] &&
  [[ "$(builtin declare -p REGISTRATION_CREATED_FILE 2>/dev/null)" =~ ^declare[[:space:]]+-[^[:space:]]*r ]] &&
  [[ "$(builtin declare -p REGISTRATION_CREATED_ID 2>/dev/null)" =~ ^declare[[:space:]]+-[^[:space:]]*r ]] &&
  command -v pbcopy >/dev/null 2>&1 &&
  python3 -I -c 'import json, os, stat, sys
p, identity = sys.argv[1:]
d = os.stat(os.path.dirname(p))
s = os.lstat(p)
f = os.fstat(9)
if not os.path.isabs(p) or os.path.realpath(p) != p or not stat.S_ISDIR(d.st_mode) or d.st_uid != os.geteuid() or d.st_mode & 0o077 or not stat.S_ISREG(s.st_mode) or s.st_uid != os.geteuid() or s.st_mode & 0o777 != 0o600 or s.st_nlink != 1 or identity != str(s.st_dev) + ":" + str(s.st_ino) or (s.st_dev, s.st_ino) != (f.st_dev, f.st_ino): sys.exit(1)
os.lseek(9, 0, os.SEEK_SET)
with os.fdopen(9) as response: value = json.load(response)["webhook_secret"]
if type(value) is not str or not value: sys.exit(1)
sys.stdout.write(value)' "$REGISTRATION_FILE" "${REGISTRATION_CREATED_ID:-}" 2>/dev/null | pbcopy 2>/dev/null
)
```

立即粘贴到 GitLab 的 Secret token 字段并保存，然后用 `printf '' | pbcopy` 清空剪贴板；失败时也要清空。其他环境使用获批准、可从标准输入接收秘密的密钥管理器，不要使用会记录内容或同步剪贴板的工具。如果无法保证受控渠道，就停止并由有权限的用户安全完成配置。

完成手动配置或自动注册后，在同一个 shell 中删除本次文件并再次清除项目 token（不要使用通配符，不要重新启用 xtrace 直到秘密处理完毕）。命令使用本次注册留下的只读标记，核对绝对路径及文件身份；不能确认时先清除 token 并安全核对路径，不要删除其他文件：

```bash
set +x
if ! builtin unset -v GITLAB_PROJECT_TOKEN 2>/dev/null || [ "${GITLAB_PROJECT_TOKEN+x}" = x ]; then
  printf '无法清除项目 token；请结束受影响的 shell 并安全处理私有响应。\n' >&2
  false
elif ! (builtin unset -v REGISTRATION_FILE && [ "${REGISTRATION_FILE+x}" != x ]) 2>/dev/null; then
  printf '无法清除响应路径；已停止，请安全处理私有响应。\n' >&2
  false
elif [ -z "${REGISTRATION_FILE:-}" ] || [ "$REGISTRATION_FILE" != "${REGISTRATION_CREATED_FILE:-}" ] ||
     ! [[ "$(builtin declare -p REGISTRATION_CREATED_FILE 2>/dev/null)" =~ ^declare[[:space:]]+-[^[:space:]]*r ]] ||
     ! [[ "$(builtin declare -p REGISTRATION_CREATED_ID 2>/dev/null)" =~ ^declare[[:space:]]+-[^[:space:]]*r ]] ||
     ! python3 -I -c 'import os, re, stat, subprocess, sys
p, identity = sys.argv[1:]
parent = os.path.dirname(p)
d = os.stat(parent)
s = os.lstat(p)
f = os.fstat(9)
if not os.path.isabs(p) or os.path.realpath(p) != p or not re.fullmatch(r"tribunal-registration\.[A-Za-z0-9]{8}", os.path.basename(p)) or not stat.S_ISDIR(d.st_mode) or d.st_uid != os.geteuid() or d.st_mode & 0o077 or not stat.S_ISREG(s.st_mode) or s.st_uid != os.geteuid() or s.st_mode & 0o777 != 0o600 or s.st_nlink != 1 or identity != str(s.st_dev) + ":" + str(s.st_ino) or (s.st_dev, s.st_ino) != (f.st_dev, f.st_ino): sys.exit(1)
if subprocess.run(["rm", "--", p], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode: sys.exit(1)
after = os.stat(parent)
f = os.fstat(9)
if os.path.realpath(parent) != parent or (after.st_dev, after.st_ino) != (d.st_dev, d.st_ino) or not stat.S_ISDIR(after.st_mode) or after.st_uid != os.geteuid() or after.st_mode & 0o077 or identity != str(f.st_dev) + ":" + str(f.st_ino) or f.st_nlink != 0: sys.exit(1)
try: os.lstat(p)
except FileNotFoundError: pass
else: sys.exit(1)' "$REGISTRATION_FILE" "${REGISTRATION_CREATED_ID:-}" 2>/dev/null; then
  printf '未能确认本次响应文件已删除；请安全核对路径并处理，不要盲目重试。\n' >&2
  false
else
  if ! builtin unset -v REGISTRATION_FILE 2>/dev/null || [ "${REGISTRATION_FILE+x}" = x ]; then
    printf '已删除文件，但无法清除响应路径；请安全核对。\n' >&2
    false
  else
    exec 9<&-
  fi
fi
```

只有确认 token 已清除、响应路径可清除且路径和文件身份与本次只读标记和打开的描述符一致后才删除；删除后还须确认原目录未改变、描述符所指文件的链接数为 0 且原路径不存在（包括悬空符号链接），才清除 `REGISTRATION_FILE` 并关闭描述符 9。标记未建立、描述符关闭或改变、路径被修改或文件被替换时拒绝删除，保留路径供安全核对。删除失败、删除后核验失败或文件不存在时返回失败并保留原路径和描述符，不要盲目重试。路径缺失时返回失败且不执行删除，应先安全查明路径。只读标记留在当前 shell 中；安全处理完成后，下一次注册使用新 shell。路径预检、删除、事后核验和清除变量并非原子操作，不保护不可信同 UID 进程或不受控目录；若删除后变量清理意外失败，命令返回失败并保留路径，需安全核对文件状态。注册或解析失败时不继续、不直接重试：请求可能已经提交，响应也可能部分写入。保留私有文件，通过受控渠道核对项目和 webhook 状态，再处理该文件；不要覆盖或删除其他文件。如果命令被信号中断，先尝试在当前 shell 清除 token；readonly 或其他原因导致无法确认清除时，结束受影响的 shell，并安全处理保留的私有响应。如果项目已注册但未收到完整的一次性 secret，须经用户授权后使用服务支持的 webhook 重建方式轮换，不能重新注册来取回原 secret。删除文件不保证从 SSD、快照或备份中物理擦除，因此私有目录应使用受控加密临时存储且不被备份或同步；若秘密可能泄露，也应按实际状态重建 webhook 以轮换它。

### 项目删除后的残留核对

项目删除先安全检查本地目标、关闭项目 MCP 会话，再删除数据库登记和本地文件。数据库操作返回错误时，文件清理不会开始；该错误不证明数据库一定回滚。数据库提交后若文件清理失败，接口返回错误并说明登记已删除但本地清理未完成。GitLab webhook 删除是有界的尽力操作，不与数据库和文件系统原子提交。

服务提供只读检查和显式清理：

```bash
tribunalctl projects residues
tribunalctl projects residues clean --yes
```

检查命令列出可归属到未登记项目的 `<workspace>/<项目ID>` 和 `skills/project-<项目ID>`，不修改文件。只有存在项目 ID 匹配的受管标记 `.tribunal-managed` 的项目目录才标记 `removable`；标记只在服务首次创建项目根时写入，旧目录不会因普通访问自动获得标记。需要清理旧目录时，先人工确认归属，再手工创建内容为 `tribunal-workspace-v1 <项目ID>` 的普通文件。`skills/project-<id>` 容器、符号链接、非规范名称和无法归属的条目只报告、不删除。清理命令必须带 `--yes`，服务端还要求 `confirm: true`。`failed` 只含脱敏错误类别。清理前后仍应按本节完成备份和归属确认。

遇到删除错误或结果不确定时，不要直接重试登记或删除，也不要让服务自动接管残留。先停止服务，按实际配置确认数据库和 `workspace.root` 的位置、挂载与项目 ID，并做一致性备份。核对数据库中的登记状态、GitLab webhook 状态，以及该项目的工作区和 `skills/project-<项目ID>` 目标；空目录、普通文件和符号链接也可能阻止重新登记。由有权人员确认每个残留的归属和内容，再按组织的受控流程处理；不能确认归属时保留并升级处理。不要批量或盲目删除路径。确认相关目标均已安全处理后，才重新启动并按需重新登记。

状态、反应、候选评审和发布任务在项目锁内复核原任务或工作流；删除后同 ID 的新登记不会继承这些旧任务。锁保护只适用于共享同一工作区管理器的单个受管运行时，也不保证其他进程、宿主机上的外部修改或后台 MCP 进程遵守相同锁。

摘要评论创建的响应丢失后，重试前会回读本 bot 的摘要标记；身份或回读不可用时停止创建，稍后可重试发布。回读与创建不是原子操作，不同工作流并发或 GitLab 列表延迟仍可能导致重复评论。系统不会自动删除已有重复评论；需要人工核对。

### 项目授权、用户停用和凭据撤销

项目访问是 Tribunal 本地的 `project_grants`，不从 GitLab 组成员关系推导。`bootstrap_admins` 只决定平台管理标签；平台管理员读取或写入项目业务数据同样需要该项目授权。默认没有任何授权，`admin` 标签也不例外。入口检查中，无授权或项目不存在统一返回 404，只有 `read` 授权执行写操作返回 403。下述 dispatch 锁内复核不满足 manage 时统一返回 404，包括入口检查之后被降为 read 的请求。

手动触发评审、刷新工作区、取消及重发评审会在项目生命周期锁内重新查询当前 manage grant 和用户启用状态，再访问项目凭据或入队。项目删除与重新登记使用同一锁；旧请求在删除后继续执行时，没有新 grant 则拒绝。复核时已有新 grant 的请求仍可继续。grant 或用户状态的独立变更不与此锁串行，复核之后的撤销不能保证阻止正在进行的请求；会话和 API token 仍按入口认证，不在此处再次核验。其他项目写接口（包括项目配置、webhook、上下文、扩展和问题标注）没有此项锁内授权保证，不能将此修复视为所有在途请求的即时撤销。

**迁移已有部署并首次授权**

`0007_project_access.sql` 为现有 `users` 行补 `disabled=0`，并新增空的 `project_grants` 表；不会为任何用户创建 grant。升级后已有用户默认看不到任何项目，需按以下步骤逐个授权；新部署同样从零开始。

1. 让目标用户先通过 OAuth 登录一次。没有登录过的 GitLab 用户不在本地 `users` 表中，无法预先授权。
2. 用平台管理员 Bearer token 列出本地用户和全部已登记项目：

   ```bash
   docker compose exec -T review /app/tribunalctl users list
   docker compose exec -T review /app/tribunalctl projects all
   ```

   `projects list` 按 bearer 所属用户的项目授权过滤；bootstrap token 没有用户 ID 和 grant，通常返回空列表，不能用它确认“没有项目”。`projects all` 和 `users list` 是管理面列表，需要平台管理员凭据。
3. 先向用户明确确认实际项目 ID、目标 GitLab 用户 ID，以及本次唯一动作：授予 `read`、授予 `manage` 或删除授权。每次执行前，在当前 shell 将 `PROJECT_ID`、`GITLAB_USER_ID` 和 `GRANT_ACTION` 分别设置为这次确认的数字 ID 和动作（`read`、`manage` 或 `delete`）；不要沿用未重新确认的旧值。下列命令只执行一个动作，再核对结果；缺少输入、ID 不是无前导零的正整数或动作无效时不执行任何授权命令：

   ```bash
   if [ -z "${PROJECT_ID:-}" ] || [ -z "${GITLAB_USER_ID:-}" ] ||
      ! [[ "$PROJECT_ID" =~ ^[1-9][0-9]*$ && "$GITLAB_USER_ID" =~ ^[1-9][0-9]*$ ]]; then
     printf '缺少有效的项目 ID 或用户 ID，已停止。\n' >&2
     false
   else
     case "${GRANT_ACTION:-}" in
       read|manage)
         docker compose exec -T review /app/tribunalctl projects grants set "$PROJECT_ID" "$GITLAB_USER_ID" "$GRANT_ACTION" ;;
       delete)
         docker compose exec -T review /app/tribunalctl projects grants delete "$PROJECT_ID" "$GITLAB_USER_ID" ;;
       *) printf '动作无效，已停止。\n' >&2; false ;;
     esac &&
     docker compose exec -T review /app/tribunalctl projects grants list "$PROJECT_ID"
   fi
   ```

   `set` 参数依次是项目 ID、GitLab 用户 ID 和角色；`delete` 只接收两个 ID。`read` 可读取项目、评审、工作区、统计、SSE 和项目扩展；`manage` 还可修改项目设置、触发/取消评审、重新发布、人工标注、刷新工作区、轮换项目 Key 和重建 webhook。项目级 MCP/Skill/块绑定属于宿主执行配置，写入还需平台管理员标签，`manage` 本身不授予平台配置能力。
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

每次执行前，先向用户确认目标 GitLab 用户 ID 及本次唯一动作（`disable`、`enable` 或 `revoke`），并说明上面的影响。在当前 shell 将 `GITLAB_USER_ID` 和 `USER_ACTION` 设为这次确认的值，不要沿用旧值。下列命令只执行一个动作，缺少输入、ID 不是无前导零的正整数或动作无效时不执行：

```bash
if [ -z "${GITLAB_USER_ID:-}" ] || ! [[ "$GITLAB_USER_ID" =~ ^[1-9][0-9]*$ ]]; then
  printf '缺少有效的用户 ID，已停止。\n' >&2
  false
else
  case "${USER_ACTION:-}" in
    disable|enable|revoke)
      docker compose exec -T review /app/tribunalctl users "$USER_ACTION" "$GITLAB_USER_ID" ;;
    *) printf '动作无效，已停止。\n' >&2; false ;;
  esac
fi
```

**bootstrap 凭据轮换**

`REVIEW_ADMIN_TOKEN` 由部署方生成、保存和轮换，Tribunal 不提供在线轮换 API。修改 `.env` 或 secret 注入后执行 `docker compose up -d --force-recreate --wait --wait-timeout 120`，重启后旧 token 立即失效；不要用数据库操作或用户 grant 替代这次轮换。`bootstrap_admins` 是 `id:<GitLab 用户 ID>` 或 GitLab 用户名的列表，修改后同样需要重启服务，不会创建 GitLab 账号，也不等于项目授权。

### 停止、修改和备份

单独停服使用 `docker compose stop review`。`docker compose down` 会删除容器但保留命名卷，不适用于需要从旧容器复制数据的备份；**不要执行 `down -v`**，它会删除数据库和工作区。默认项目名仍为 `crc-review`，数据卷仍为 `crc-review_data`；这是刻意保留的兼容身份，改名会创建新卷并让现有数据不可见。若用 `-p` 更换项目名，也会使用另一数据卷。不要同时启动多个实例共享同一卷。

修改 `.env` 或配置后使用 `docker compose up -d --force-recreate --wait --wait-timeout 120`。`restart` 不会加载新的容器环境变量。升级程序需重新构建镜像，并按第 8 节先备份；不支持只回退镜像却保留升级后的数据库。

停机一致性备份前先核对实际路径和挂载，默认布局的命令见 [Compose 停机备份](#compose-停机备份)，自定义布局见第 7 节前提。备份命令不会重新启动旧服务；另行安全备份 `.env` 和实际配置。恢复时使用匹配布局的新空位置、匹配的镜像版本与主密钥，详见第 7 节；不要把旧 WAL 混进恢复数据。下文 `docker run` 示例使用独立容器名，不能与 Compose 实例同时占用同一端口或数据库。

## 1. 镜像内容与边界

镜像分四阶段构建：

1. `web`：执行 `npm ci` 和 `npm run build`，生成控制台 `web/dist`。
2. `build`：先 `go mod download`，再构建 `cmd/server` 和 `cmd/tribunalctl`；服务嵌入上一步的新 `web/dist`。
3. `codebase-memory`：按 `TARGETARCH` 下载 codebase-memory-mcp 0.12.1 的 `-portable` 发布包，校验固定的 SHA-256，核对许可证与仓库记录一致，只取出二进制和许可证文本。
4. `runtime`：包含 `/app/server`、`/app/tribunalctl`、示例配置、许可证、CA 证书、`/usr/local/bin/codebase-memory-mcp` 与隔离 wrapper，以及 `curl`、`git` 和创建非 root 用户所需的 `passwd` 工具。

镜像保留兼容符号链接 `/app/reviewctl`，指向 `/app/tribunalctl`；旧编排脚本可继续调用该旧路径。源码用户改用 `go run ./cmd/tribunalctl`。

运行特征：

- 以 UID/GID `10001:10001` 的非 root 用户 `tribunal` 运行，家目录 `/home/tribunal`；镜像默认工作目录为 `/app`，可由 `docker run -w` 或 Compose `working_dir` 覆盖。
- 镜像声明 `/app/data` 为卷；示例配置把 SQLite 数据库、WAL/SHM 文件和项目工作区放在这里，自定义配置可能使用其他路径和挂载。
- 默认监听 `8080`，容器内为 HTTP；TLS 由反向代理终止。
- 镜像不包含 `.git`、`node_modules`、本地 `data`、真实 `config/server.yaml`、`.env`、证书或私钥。
- 许可证位于 `/licenses/LICENSE` 和 `/licenses/THIRD_PARTY_NOTICES.txt`；后者是单一声明文件，覆盖 Go 标准库、Linux/amd64 服务端的 40 个模块、9 个 production npm 包和镜像内置的 codebase-memory-mcp。当前 Dockerfile 仍把遗留的 `THIRD_PARTY_LICENSES` 一并复制到 `/licenses/`，但发布声明以 `THIRD_PARTY_NOTICES.txt` 为准。`git` 及 Debian 软件包自身的许可证在 `/usr/share/doc/`。
- 工作区只使用 http(s) Git remote，镜像不安装 SSH 客户端。镜像内置 codebase-memory-mcp（见下文）；其他 MCP 服务器由部署方安装或使用扩展镜像。
- 仅支持一个服务实例使用一个 SQLite 数据目录。不要用多个容器共享同一数据库文件或同一数据卷。
- 两个实例不能共用一个数据卷。服务启动时在 SQLite 文件所在目录对 `tribunal.lock` 加独占锁（默认容器配置下为 `/app/data/tribunal.lock`，源码运行时默认为 `data/tribunal.lock`），在打开数据库之前完成。锁已被另一个进程持有时，服务记录 `data directory is locked by another Tribunal instance` 错误日志（带锁文件路径）并以退出码 1 退出，不会打开数据库。锁随进程退出由内核释放，包括崩溃或 `kill -9`；锁文件本身会留在目录里，不需要也不应该手工删除，它的存在不代表被锁。
  - 锁基于 `flock`，在 Linux（Docker 镜像）、macOS 和 BSD 上生效。Windows 等没有 `flock` 的平台只创建锁文件、不加锁，不在支持范围内。`flock` 只在同一内核内可靠：同一台宿主机上的多个容器共用一个卷可以互斥，跨主机共享的 NFS 等网络文件系统不受保护，不要把数据目录放在网络文件系统上。
  - `tribunalctl` 的命令都走 HTTP API，不直接打开数据库，不受锁影响。用 `sqlite3` 等外部工具直接打开 `review.db` 不会检查这个锁；按第 7 节先停止服务再操作。
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

镜像不运行上游 `install`，不写任何 agent 配置或 hooks。示例配置和 `config/server.docker.yaml` 的 `default_extensions` 在首次启动时登记平台级 stdio 服务器 `codebase-memory`（`--tool-profile=analysis`），并为 `code-review`、`review` 授权只读图谱工具、为 `verify` 授权 `get_code_snippet`。有效 ignore 为空时，项目 onboarding 和刷新可用 CLI 为固定的 `base/` 建索引；非空时按下节停用这些执行路径。每个项目的 `HOME`、`CBM_CACHE_DIR`、`CBM_RUNTIME_DIR` 都在该项目工作区内，项目之间查不到彼此的索引；auto_index、watcher 和 UI 均关闭。规则详见 [默认扩展](design/06-config.md#默认扩展default_extensions) 和 [内置 codebase-memory-mcp](design/06-config.md#内置-codebase-memory-mcp)。

### ignore 安全边界

新评审保存合并后的有效 ignore 策略。规则写法做了规范化：去掉前导 `/` 和 `./`；`dir/` 匹配任意层级的目录。`!` 否定和 `[...]` 字符类不支持：保存项目设置时返回 400，平台配置在配置检查时拒绝；仓库配置中出现这类规则会让评审永久失败。旧配置已经包含这类写法时，评审会以配置错误永久失败；底层匹配器也按“全部忽略”兜底，不会静默放行。被忽略的 diff、仓库读取、遍历和证据路径使用相同规则，目录祖先和符号链接目标也受检查。重命名的任一侧被忽略时，整个 diff 和对应新路径均被排除。允许读取未被忽略的非 diff 文件作为证据，但行内定位只使用 dossier 接收的 diff。明确指向 ignored 路径的模型结果会被拒绝，不会降为汇总评论发布。

有效 ignore 非空时还有以下变化：

- 不执行 MCP 工具或全量图谱索引，因为现有服务无法证明其读取范围符合 ignore。这是有意的安全降级，不是覆盖缺口：不记录 `tool_gaps`，不把 MCP 工具交给 agent，评审照常进行，结论不受影响（控制台工作区页显示 `index: blocked by ignore policy` 作为可见提示）。被忽略文件本身同样不算覆盖缺口。
- 旧索引和 `index_sha` 保留，但不会用于上述工具调用。刷新后索引状态为 `degraded`，固定原因是 `index: blocked by ignore policy`，即使旧 SHA 等于 base SHA 也不显示 ready。
- 不注入无来源证明的历史 context 文档，不删除、解锁或改写这些文档。初始化和刷新生成新上下文时，仓库读取同样受策略限制。
- `AGENTS.md`、`CLAUDE.md` 和 `.gitlab-review.yml` 按来源路径过滤。被忽略的仓库配置仍可在本地用于控制规则，但其原文、语言和自定义规则不作为模型输入。配置读取或解析失败会停止该次执行，而不是当成空规则。`.gitlab-review.yml` 解析失败或包含不支持的 ignore 写法时，评审永久失败、不重试，并在 MR 中留一条说明评论；修正文件后重新触发评审。启动恢复遇到这类错误会终结对应工作流；网络等临时读取错误仍保留为可恢复。
- git log 只返回提交 hash 和作者时间，不返回可包含任意文本的提交说明；git blame 不返回可能由 `.mailmap` 提供的作者元数据。独立于 checkout 的平台 Skill 仍可用；与 checkout 重叠、互为祖先或来源无法确认的 Skill 会阻止该次 agent 调用，不会先发送其描述或正文。

默认 ignore 非空，因此默认图谱会受到上述限制。项目和仓库规则只能追加到平台规则；不要为了恢复图谱而移除需要保护的规则。即使有效规则为空，MR 评审也仅注入来源 SHA 等于该 MR 目标分支 tip 的 context；锁定文档不会被修改或解锁。MCP 还要求工作区 ready、没有错误，且有效 index SHA 等于同一 tip。工作区锁保证并发，不证明材料来自目标分支。有效 ignore 为空而索引身份无法证明时，实际启用的绑定受阻会产生覆盖缺口；没有绑定不会产生缺口。

旧 dossier 缺少版本化策略时，程序仍可读取其历史记录，但不会复用旧节点输出继续调用模型或发布。启动恢复会取消旧的未完成工作流，待发布结果会跳过，手动重发也会拒绝；需要重新触发评审。已有发布或失败终态不因此改写；正常 job 重试可通过新 dossier 重建。旧数据库、索引、备份及已经外发的内容不会自动清除。

同样，缺少版本化工作区来源证明或来源矛盾的旧 dossier 不会续跑或发布，需重新触发评审。续跑原本允许 MCP 的快照前，会在工作区锁内重新检查索引；索引不再安全时保留旧快照并取消续跑。原本禁用 MCP 的快照不会在续跑时自动获得权限。已完成且来源证明有效的快照不因之后共享索引变化而被拒绝发布。

升级前自动生成的 context 即使 SHA 匹配，也可能使用过其他分支的索引。缺少安全生成工作流证明的文档仍保留并在控制台显示，但不再注入 MR 评审；锁定状态不会自动改变。管理员可解锁后重新生成，或检查内容并显式手工保存以确认。手工保存的 context 仍须匹配 MR 目标分支 tip。

此保护针对已知仓库路径和输入通道，不是任意文本内容识别。若被忽略的内容已被复制到允许读取的文件、MR 描述或其他自由文本，路径规则不能自动识别它。checkout 运行期间须由服务独占，不能把此检查当作抵抗不可信并发文件修改的隔离机制。

策略在新执行开始时形成快照，恢复和发布入口再次检查。修改配置不是对已在执行或发送中的调用进行即时撤销；需要立即停下当前评审时应同时使用取消操作。已经发出的内容无法由新规则撤回。

### 图谱管理与排查

- **查看和测试**：控制台「扩展」页列出 `codebase-memory`；「测试并列出工具」返回的列表不含 `index_repository`、`delete_project`、`ingest_traces`、`manage_adr`。
- **内置标识**：它带「内置」标签，只能停用或启用。名称和配置随平台版本走，不能在控制台编辑；删除、改名、改配置或用导入覆盖都返回 409。
- **禁用**：在「扩展」页点「停用」，或 `PUT /api/v1/mcp-servers/{id}` 设置 `"enabled": false`，然后重启服务。重启和升级都保持停用。
- **故障表现**：二进制缺失或无法启动时评审照常完成，但没有图谱工具；评审结论里出现 `MCP tools unavailable` 告警，原本会 PASS 的评审显示为 INCOMPLETE。非取消的建索引失败不阻断 onboarding 或刷新；其他必要步骤及状态保存成功时工作区最终为 `ready`，`last_error` 为固定消息 `index: code graph indexing failed`，刷新保留旧 `index_sha`。ignore 阻断索引时 `index_status` 为 `degraded`；其他情况下比较已存的 SHA，不代表当前索引健康。
- **排查信息**：索引失败告警 `code graph index failed; continuing without it` 和一般工作区失败告警 `workspace operation failed` 含 `project_id` 与 `error_class`；类别限于 `timeout`、`canceled`、`panic`、`internal`，其他类别归为 `internal`。 节点失败事件优先使用执行器提供的固定类别（如 `auth`、`rate_limited`、`sandbox_denied`、`panic`），没有分类时仍是 `executor_error`、`timeout` 或 `expand_failed`；`panic` 不记录 panic 值。这些日志和工作区 API 不提供原始原因；按项目 ID 在受控环境中检查索引配置、可执行文件和运行环境。旧数据库和备份可能仍含错误原文，读取接口不会清除它们；应限制数据库和备份访问，回退旧版可能重新暴露原文。

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

秘密从部署环境注入，不要写进镜像、Compose 文件或命令历史。以下示例仅用于首次生成；任一步失败时停止，不使用部分结果：

```bash
set +x
REVIEW_MASTER_KEY="$(openssl rand -base64 32)" &&
REVIEW_ADMIN_TOKEN="$(openssl rand -hex 32)" &&
TRIBUNAL_SESSION_KEY="$(openssl rand -base64 32)" &&
export REVIEW_MASTER_KEY REVIEW_ADMIN_TOKEN TRIBUNAL_SESSION_KEY
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
- 此示例按默认数据路径挂载 `/app/data`；自定义 `storage.dsn` 或 `workspace.root` 时还须持久化相应路径和挂载。删除数据卷会丢失其中的数据库或工作区；没有数据库备份时已保存的项目凭据无法恢复。
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

新部署生成密钥时，使用[私有临时文件步骤](agent-deployment.md#2-准备配置)，不要将生成的秘密打印到终端或日志。已有部署必须保留原值，不能在恢复时重新生成。

`secrets.master_key_env` 和 `auth.bootstrap_token_env` 可以改名；改名后实际环境变量也要对应修改。

`REVIEW_MASTER_KEY` 必须跨升级、重启和回滚保持不变。备份数据库时同时备份该 key；只有数据库而没有 key 无法解密项目凭据或已迁移的 MCP 配置。`TRIBUNAL_SESSION_KEY` 也应持久化；固定 key 且复用同一 SQLite 数据卷时，重启后浏览器会话仍可继续使用。

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
  bootstrap_admins: []
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
- `bootstrap_admins: []` 不授予任何账号平台管理员标签。需要授予时，先向用户确认实际 GitLab 数字用户 ID 和授权，再填写 `id:<GitLab 用户 ID>`；该写法只按 ID 匹配，GitLab 改名不影响。用户 ID 可在 GitLab 个人资料页或 `GET /api/v4/users?username=<name>` 查到。也兼容旧写法 GitLab **用户名**（不区分大小写，已弃用，见下文迁移步骤）：该用户名第一次以 admin 身份登录或访问时，Tribunal 把它绑定到当时的 GitLab 用户 ID 并写入 `admin_bindings`；之后其他账号即使改名为同一用户名，也只得到 `viewer`，日志记录 `bootstrap admin username is bound to another GitLab user`。匹配的用户获得平台 `admin` 标签；其他登录用户为 `viewer`。它不会创建 GitLab 账号，也不授予任何项目数据访问；项目权限仍需 `project_grants`。
- 用户名绑定是首次使用即信任：升级前若该用户名已被他人占用，第一个登录的账号会拿到绑定。升级后请确认绑定正确（`SELECT username, user_id FROM admin_bindings;`），或直接改用 `id:` 写法。管理员 GitLab 账号重建、需要重新绑定时，先停服务，执行 `DELETE FROM admin_bindings WHERE username = '<小写用户名>';`，再启动并让正确账号登录；改用 `id:` 写法则不需要这一步。
- 每个用户最多保留 10 个浏览器会话，再次登录会淘汰该用户自己最旧的会话，不影响其他用户；全局 4096 个会话只是存储兜底，触发时先淘汰非管理员的最旧会话，只有剩下的会话全部属于管理员时才淘汰管理员的会话。角色按当前 `bootstrap_admins` 判断，启动时和会话被使用时都会刷新；用户名写法的管理员要等第一次使用、完成绑定之后才算管理员。
- OAuth 配置存在时，`client_id_env` 和 `client_secret_env` 必须同时设置，且两个环境变量都必须非空；`server.public_url` 必须是非空的绝对 HTTP(S) 地址，否则启动校验失败。
- `REVIEW_ADMIN_TOKEN` 与 OAuth 相互独立；没有浏览器会话时仍可用它调用用户、授权和系统配置等平台管理 API。它没有用户 ID 和项目 grant，不能读取项目业务数据；轮换由部署方更新环境变量并重启。
- 真实 OAuth 登录已在已验证快照的隔离测试实例验证；未单独复测其他生产 IdP 或所有设备场景。

### bootstrap_admins 用户名写法弃用与迁移

用户名写法已弃用，计划在下一个大版本（v2.0.0）移除；在此之前保持兼容。服务启动时，每个用户名条目记一条 `WARN`，消息为 `auth.bootstrap_admins username entry is deprecated ...`。日志含条目在列表中的序号 `index`（从 0 开始）以及绑定状态或替代值，不含用户名：

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

数据库启用 WAL。运行中只复制数据库文件会漏掉 `-wal`/`-shm`，不是一致备份。先核对生效的 `storage.dsn` 和 `workspace.root`，包括配置合并、环境变量展开及相对路径解析，并核对容器的全部实际挂载及其来源、目标和覆盖关系；不要只看示例 YAML 或 Compose 文件。所有布局的备份位置都不得位于任何数据挂载来源内，需检查实际路径、符号链接和挂载别名。检查时不要输出环境变量或秘密。

`workspace.root` 的相对路径，以及 `storage.dsn` 中 SQLite 普通文件名或相对 `file:` URI 的路径，均按服务器进程的实际工作目录解析，不是配置文件所在目录。镜像默认工作目录为 `/app`，但 `docker run -w` 或 Compose `working_dir` 可以覆盖；必须核对运行时实际工作目录、解析后的文件和工作区位置及全部挂载。例如工作目录为 `/srv/tribunal` 时，`file:data/review.db` 和 `data/workspaces` 分别指向 `/srv/tribunal/data/review.db` 和 `/srv/tribunal/data/workspaces`。

以下 `docker compose cp`、`docker cp` 和单卷恢复示例**仅适用**这些条件全部成立时：生效数据库路径是 `/app/data/review.db`，工作区根目录是 `/app/data/workspaces`，两者及各工作区目录解析后的实际位置都位于同一个 `/app/data` 持久化数据挂载中，且没有其他数据挂载，也没有任何遮盖该目录的重叠挂载（包括嵌套的配置或秘密挂载）。`/app/data` 外的配置和秘密非数据挂载可以保留。

自定义路径、分离卷或额外/重叠数据挂载的部署不要执行这些默认命令。按实际布局操作：

1. 确认数据库、工作区及嵌套挂载的实际来源，并记录备份项与挂载目标的对应关系；无法确定时停止并排查。
2. 停止服务及所有其他写入进程，在整个备份期间保持停止。分别备份 `storage.dsn` 所指数据库、存在时的同名 `-wal`/`-shm`、`workspace.root` 及其中每个工作区目录，包括嵌套挂载实际提供的内容；核对各挂载来源，不能把缺失的目录当成空目录。另行安全备份实际配置和密钥。
3. 恢复前确认备份完整且已校验，并先确定全部恢复目标及挂载覆盖顺序。为各数据部分新建匹配自定义布局的空卷或空目录，分别恢复到正确位置并核对，不能覆盖旧数据或混入其他 WAL。按实际服务进程的 UID/GID 设置每个数据挂载及嵌套挂载的所有权和目录权限；默认镜像为 `10001:10001`，自定义 `user` 时以实际身份为准。启用 rootless 或用户命名空间时，还需按容器与宿主机的 UID/GID 映射设置宿主机属主。全部恢复和校验完成后才可按下文隔离要求启动恢复副本；不要启动旧服务。

默认布局示例先准备 `/srv/crc` 并限制备份访问；该目录是部署方自有的兼容路径，不随产品改名迁移。确认该目录及将创建的 `BACKUP_DIR` 不在任何数据挂载来源内；检查符号链接、实际路径和挂载别名，若不能确认则停止，另选独立位置并相应修改下面的 `mktemp` 路径，避免复制到源数据内。先停止使用相同数据的其他写入进程，并在整个备份期间保持停止；无法保证一致时停止并排查。按部署类型只选下面一种命令。每步均需成功：停服、创建目录或复制失败时立即停止并排查，不得启动服务。复制成功不等于备份已验证；还需核对数据完整性、数据库和工作区，并安全备份实际配置和秘密。

### Compose 停机备份

仅在符合上面的默认配置与挂载前提时，在原 Compose 项目的仓库根目录执行；如果部署时指定了 `-p` 或自定义 Compose 文件，所有命令必须使用相同的项目名和文件。保留原容器，不能先执行 `docker compose down`：

```bash
docker compose stop review &&
  BACKUP_DIR="$(mktemp -d /srv/crc/backup-XXXXXXXX)" &&
  docker compose cp review:/app/data/. "$BACKUP_DIR/"
```

### 单独 docker run 停机备份

此命令只适用于符合上面默认配置与挂载前提、第 3 节名为 `tribunal` 的独立容器，不适用于 Compose 的 `review` 服务：

```bash
docker stop tribunal &&
BACKUP_DIR="$(mktemp -d /srv/crc/backup-XXXXXXXX)" &&
docker cp tribunal:/app/data/. "$BACKUP_DIR/"
```

两种备份命令均保持旧服务停止。备份、恢复和升级期间不要重新启动旧服务。

目录中至少包含：

- `review.db`
- `review.db-wal`（若存在）
- `review.db-shm`（若存在）
- `workspaces/`（原数据中存在时）

`workspaces/` 包含项目 checkout、运行目录和上下文文件，属于恢复所需数据；上下文文档的元数据同时记录在数据库中。停止容器后复制的数据库与工作区处于同一静止时间点。备份中没有 `workspaces/` 时，先确认原数据确实没有该目录，不能把复制不完整当成空工作区。

### 校验备份

所有布局都须核对备份中的数据库、工作区和全部文件是否完整。将备份数据库及同一快照中存在的同名 `-wal`/`-shm` 一起复制到隔离的受控副本，在宿主机或受控临时工具中对该副本执行 `PRAGMA integrity_check;`，结果必须只有 `ok`；仅有 `ok` 不能证明快照完整。副本及所在目录需允许写入，以便 SQLite 处理 WAL；不要使用 `immutable=1`，它可能忽略 WAL 并在缺少已提交数据时仍返回 `ok`。不得只检查主数据库而忽略 WAL；不要在原始备份上运行检查，SQLite 打开文件可能修改数据库或 WAL。检查失败、结果不是 `ok` 或无法确认完整性时，停止并排查，不得继续升级或恢复。运行镜像不含 SQLite 命令行，不需要为此安装。

### 恢复

确认选定的备份完整且已校验，将 `BACKUP_DIR` 设为该目录，并将 `RESTORE_IMAGE` 设为与快照匹配的镜像 tag 或 digest。按原部署类型停止旧服务：Compose 使用 `docker compose stop review`，独立容器使用 `docker stop tribunal`。确认旧服务已停止并保持停止，保留旧容器和旧卷，直到新容器验收通过。

先在独立于恢复目标的全新空卷或目录中复制该快照，按实际布局、配置和 UID/GID 准备一次性验证副本。使用与快照匹配的镜像和主密钥，在完全断网且无入口的环境中启动该副本（独立 Docker 容器可使用 `--network none`，不要发布端口），在容器内按实际监听地址检查 `/readyz`，核对工作区及待处理任务，并按[回退计划](delivery-readiness.md#回退触发与操作计划)核对既有外部副作用。启动可能修改数据库、运行任务或执行迁移；验证副本不能作为恢复目标或覆盖原始备份。验证失败或无法隔离时停止并排查。验证通过后，仍须从原始备份重新复制到新的恢复目标并完成下述校验，不能复制验证副本中已改变的数据。

启动恢复目标前隔离入口并阻断 GitLab、OAuth 和模型 provider 出站；核对完成并获得授权前不要解除限制。下面的目标启动命令不会自行设置这些隔离规则。

所有布局在复制到恢复目标后、启动恢复副本前，还须核对恢复目标与备份中的数据库、工作区和全部文件是否一致，并将恢复目标中的数据库及同一快照中存在的 `-wal`/`-shm` 一起复制到新的受控副本，按上一步再次执行 `PRAGMA integrity_check;`。不要在实际恢复目标上运行检查。校验失败或无法确认完整性时停止并排查，不得启动。

以下单卷示例仅适用于上述默认配置和挂载前提，且恢复时的生效配置和挂载也必须符合该前提，服务使用默认 UID/GID `10001:10001`；自定义布局或运行身份按上面的步骤分别恢复并调整所有权，不要先执行此示例。恢复到新建的空卷，不把备份覆盖到旧数据目录。`docker cp` 和 `cp` 都会合并目录；旧 WAL 或工作区文件若残留，可能与备份不匹配。先复制数据：

```bash
[ -n "${BACKUP_DIR:-}" ] && [ -f "$BACKUP_DIR/review.db" ] &&
  [ -n "${RESTORE_IMAGE:-}" ] &&
RESTORE_VOLUME="$(docker volume create)" &&
docker run --rm --user 0 --entrypoint sh \
  -v "$BACKUP_DIR:/backup:ro" \
  -v "$RESTORE_VOLUME:/restore" \
  "$RESTORE_IMAGE" -c 'cp -a /backup/. /restore/ && chown -R 10001:10001 /restore'
```

复制失败时停止并排查，不得继续。按上文完成断网快照副本验证、恢复目标的全部校验且隔离规则已生效后，才可执行下面的启动命令。该启动和后续健康检查示例还要求生效配置的 `auth.bootstrap_token_env` 为 `REVIEW_ADMIN_TOKEN`、`secrets.master_key_env` 为 `REVIEW_MASTER_KEY`、`server.listen` 为 `:8080`，并在当前 shell 安全导出与快照匹配的主密钥、bootstrap token 和 session key。若配置改了变量名或容器监听地址/端口，必须按实际配置更改容器的 `-e`、端口映射和健康检查；不能直接套用下面的启动及检查命令。示例用独立容器恢复，无论旧服务是否使用 Compose；确认宿主机端口 18080 未被旧服务或其他程序占用，必要时改用另一个空闲端口，不要和旧服务共用端口或数据卷。保持同一 shell 中的 `RESTORE_VOLUME` 和 `RESTORE_IMAGE`：

```bash
[ -n "${RESTORE_VOLUME:-}" ] && [ -n "${RESTORE_IMAGE:-}" ] &&
[ -n "${REVIEW_MASTER_KEY:-}" ] && [ -n "${REVIEW_ADMIN_TOKEN:-}" ] &&
[ -n "${TRIBUNAL_SESSION_KEY:-}" ] &&
docker run -d --name tribunal-restored \
  -p 127.0.0.1:18080:8080 \
  -e REVIEW_MASTER_KEY -e REVIEW_ADMIN_TOKEN -e TRIBUNAL_SESSION_KEY \
  -v "$RESTORE_VOLUME:/app/data" \
  -v /srv/crc/server.yaml:/app/config/server.yaml:ro \
  "$RESTORE_IMAGE" --config /app/config/server.yaml
```

任一步失败都不得继续启动恢复副本；检查并保留失败的卷供排查，不要覆盖旧卷。`/srv/crc/server.yaml` 是示例路径，需提前准备与快照匹配的实际配置；Compose 使用的配置和环境变量不会自动传给 `docker run`。若配置使用变量模板，必须按实际配置传入所需变量；复用 `server.docker.yaml` 所需的 GitLab、公开 URL 和模型变量见[单独 docker run](#3-单独-docker-run不用-compose-时)。启用 OAuth 时还要传入相应的客户端变量，以及配置引用的 `GITLAB_ADMIN_USERNAME`。

恢复后检查：

```bash
docker logs tribunal-restored
curl -fsS http://127.0.0.1:18080/readyz
```

恢复时使用与快照匹配的主密钥。启用 OAuth 或模型 provider 时，还要传入原容器使用的对应环境变量。一次性复制容器以 root 修正属主；服务容器仍以 `10001:10001` 运行。

在线备份替代方案：使用宿主机或临时工具执行 SQLite `.backup` / `VACUUM INTO`，并对备份库运行 `PRAGMA integrity_check;`。这只能保证数据库快照一致；生效的 `workspace.root` 及各挂载中的工作区仍需单独备份，二者不保证同一事务时刻。不要把“在线数据库备份”描述成数据库与工作区的原子备份。

镜像不安装 `sqlite3` 命令行工具。需要在线备份时在宿主机或受控临时环境中执行，不要为了备份给运行时镜像增加长期依赖。

## 8. 升级与回滚

迁移文件编译在二进制内，服务启动时自动执行；只应用尚未执行的版本，没有 down migration。升级前必须备份。

### dossier 归属迁移（v1.2.0）

v1.2.0 共四个迁移：`0016_dossier_mr_title`、`0017_mcp_config_seal`、`0018_dossier_project`、`0019_job_defer_window`，都没有 down migration。`0018_dossier_project` 为 dossier 增加内部项目归属。旧记录仅根据实际关联的 workflow 回填；没有关联 workflow 的旧孤儿保持未知归属，不从快照猜测。迁移不改写压缩快照内容。项目删除时清理本项目已知归属的孤儿；未知归属记录保留。

运行时启动和定期工作区清理会在项目锁内，将无 workflow、归属已知且任务终结或已被保留策略移除的 reservation 标记为 skipped，保留快照供审计。排队或运行中的任务继续保留 reservation 以供重试。未知归属或没有任务 ID 的记录不会按已知归属规则收敛。运行清理会在超过 `storage.job_retention`、没有活跃 workflow/job 且没有其他 pending dossier 引用时，把 `project_id=0` 的记录结算为 skipped，保留审计行；未到保留期或仍有引用的记录保留。

升级前按第 7 节做一致性备份。此迁移没有 down migration；回退须恢复升级前完整快照及匹配旧程序，不能只替换镜像。

### MCP 配置加密升级（v1.2.0）

1. 按[第 7 节](#7-sqlite-一致性备份与恢复)停止服务并做一致性备份。保存原主密钥、配置和镜像版本，不重新生成 `REVIEW_MASTER_KEY`。
2. 新程序追加迁移 `0017_mcp_config_seal`。随后在启动 runtime、默认扩展和 HTTP 服务前，把平台、项目、停用及内置 MCP 配置一起加密到 `config_enc`，将旧 `config` 置为 `{}`，并写入加密的密钥校验标记。这些数据更新在同一事务中完成；失败则回滚，保留原配置，拒绝启动。结构迁移可能已经完成，不代表数据回填完成。
3. 首次回填用现有项目密文验证主密钥。至少一个允许格式的非空字段认证成功即可确认密钥，其余损坏的项目字段保持原样，仍由原有项目迁移逻辑隔离；存在项目密文但没有任何字段能认证时拒绝启动。没有历史密文的新库或仅有旧 MCP 明文的库，无法验证操作者首次提供的 key 是否正确，首次成功配置的 key 将成为后续校验依据。已有 MCP 密文或校验标记必须能认证。
   首次密封时如果没有可验证的项目或 MCP 密文，启动日志会记录 `MCP master key has no verification evidence`（只含环境变量名，不含密钥值）。看到该告警时应立即核对并保存部署密钥；首次成功密封后，该密钥成为后续校验依据。
4. 启动失败时，先在受控环境核对原密钥和备份，不删除数据库、密文或迁移标记来强行启动。同 key 重启可重复验证；完成后发现明文、缺失密文或损坏配置会拒绝启动，运行中的存储读取也不会回退到明文。
5. 升级后 API 不再提供可读回的 MCP 配置，响应统一为 `config: {}`。仅改名或启停时省略 `config`；替换配置使用原始完整模板。管理员更新时，`****` 占位符在存储层事务内与当前配置合并，并发更新不会把对方刚写入的密钥回滚成占位符。默认扩展刷新仍保留停用状态和管理员改过的绑定。

回退只能用升级前的一致性备份和匹配的旧程序。旧程序可能把 `{}` 当作配置，或重新写入明文；不能仅替换镜像后继续使用已迁移数据库。即使后续启动步骤失败，已提交的 MCP 加密事务也不会自动撤销。

迁移只清空当前逻辑列，不保证历史 WAL、SQLite 空闲页、文件系统快照或备份中的明文已擦除。继续限制这些文件的访问；本升级不自动删除或重写历史副本。

### 通用升级与回滚步骤

升级步骤：

1. 记录当前镜像 tag/digest。
2. 按第 7 节核对生效路径与全部挂载，停止旧服务并备份、核验数据库和所有工作区，另存配置和秘密。只有符合默认布局前提时才能使用 [Compose](#compose-停机备份)或[独立容器](#单独-docker-run-停机备份)示例；在启动新镜像前保持旧服务停止。
3. 隔离验证并获得部署授权后，拉取并启动新镜像，继续使用原来的全部数据挂载、同一 `REVIEW_MASTER_KEY` 和真实配置。
4. 等待 `/readyz` 返回 200，检查日志和关键功能。

回滚步骤：

1. 停止新容器。
2. 按第 7 节将升级前数据库和全部工作区快照恢复到匹配原布局的新空卷或空目录，不覆盖旧数据。
3. 在启动旧镜像之前隔离入站，并阻断 GitLab/OAuth/provider 出站访问；旧版也可能在启动时自动恢复任务和重试发布。先在隔离环境核对 pending 任务及已有 GitLab 副作用，核对完成并获授权前不解除出站限制。
4. 使用旧镜像、匹配配置及原主密钥启动恢复副本；不能因启动成功就对普通用户开放。完整安全边界见 [回退触发与操作计划](delivery-readiness.md#回退触发与操作计划)。

注意事项：

- 内置扩展随平台升级：新镜像附带的 codebase-memory-mcp 配置（版本、参数）在下次启动时写入已登记的内置服务器，启用状态保持不变；默认工具白名单在块绑定仍是默认机制写入的样子时同步更新，管理员改过的绑定和 YAML `blocks` 中声明的块不动。首次启动后才在 YAML `blocks` 中声明的块：数据库里该块若仍是默认机制写入的绑定，启动时删除并以 YAML 为准（日志 `file configuration replaces default block binding`）；管理员改过的仍以数据库为准。内置 Skill 刷新失败（例如修改了 `workspace.root`）只记 Warn 日志 `built-in skill refresh failed`，不阻断启动。已有部署若在数据库或 `mcp.config` 中自建了同名 `codebase-memory` 服务器，服务不会接管它，也不会绑定它的工具。
- 升级内置的 codebase-memory-mcp 版本时，按 [内置 codebase-memory-mcp](design/06-config.md#内置-codebase-memory-mcp) 同时更新 Dockerfile 版本与 SHA-256、`third_party/codebase-memory-mcp/` 和许可证清单，并重新跑 Docker smoke。
- 镜像 tag 改为 `tribunal:*`；旧镜像的仓库名是 `crc-review`，升级时重新构建或改 tag。Compose 项目名 `crc-review`、旧数据卷 `crc-review_data` 和 `crc-data` 刻意不改名；默认布局可继续挂载原卷，自定义布局还须保留全部实际数据挂载。
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

   v1.0.0 读不了新格式。所以必须先按[第 7 节](#7-sqlite-一致性备份与恢复)的实际布局做一致性备份；回退时恢复这份备份，不能只换回 v1.0.0 镜像。
2. **继续使用原来的 `REVIEW_MASTER_KEY`。** 迁移要用它解开旧密文。迁移是幂等的，每个字段单独迁移，迁移期间被并发修改的行会跳过。解不开的字段只记一条告警（只含 `project_id` 和字段名），保持原样，不阻止启动；这类字段在升级前使用时同样会失败，需要在设置页用「更新访问 Key」或 Webhook「重建」重新写入。某次启动后不再有旧格式字段时，服务在 `secret_migration` 表记下迁移完成；此后启动不再解开任何旧格式密文，遇到时保持原样并告警（只含 `project_id` 和字段名），该字段同样要重新写入。新部署首次启动即记为完成。
3. **镜像和 CLI 改了名，数据卷没改。** 用新代码重新构建镜像（`docker compose build`）。默认 Compose 项目名 `crc-review` 和数据卷 `crc-review_data` 保持不变，可直接沿用；自定义布局还须保留全部实际数据挂载。脚本里的 `/app/reviewctl` 可以继续用，建议改成 `/app/tribunalctl`。环境变量可以逐步改为 `TRIBUNAL_*` 新名，session key 要沿用原来的值。
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

### 从 v1.1.0 升级到 v1.2.0

**v1.2.0 尚未发布：版本说明已合并，尚未打 tag，也未发布 Release。** 以下步骤以 `main` `96e8081` 的代码为准；打 tag 后需要重新核对镜像 digest。完整变更见 [CHANGELOG](../CHANGELOG.md) 的 `[1.2.0] - 2026-10-29` 段，验证状态见 [v1.2.0 验证状态](delivery-readiness.md#v120-验证状态)。真实实例验收（[#108](https://github.com/LeXwDeX/Tribunal/issues/108)）尚未完成，发布前必须补齐。

先按第 8 节的通用步骤操作：记录当前镜像 tag/digest，核对生效路径和全部挂载，停止旧容器并备份数据库和所有工作区。本次升级有这些额外注意点：

1. **先做一致性备份，四个迁移都不可回退。**
   - 本次新增 `0016_dossier_mr_title`、`0017_mcp_config_seal`、`0018_dossier_project`、`0019_job_defer_window`，都没有 down migration。回退只能恢复升级前的全量快照，不能只换回 v1.1.0 镜像而保留已迁移数据库；旧版本读取新 schema 不受支持。
   - 升级前按[第 7 节](#7-sqlite-一致性备份与恢复)停止服务，备份生效数据库、可能存在的 `-wal`/`-shm` 和所有工作区；只有符合默认配置与挂载前提时才能复制整个 `/app/data`。运行中只复制数据库文件会漏掉 WAL，不是一致备份。
   - 另存实际配置、镜像 digest 和 `REVIEW_MASTER_KEY`。恢复使用匹配布局的新空卷或空目录，不把快照覆盖到旧数据，也不混入旧 WAL。
2. **迁移 `0016_dossier_mr_title` 只追加列，但仍然要先备份。**
   - 迁移对 `workflow_dossiers` 执行 `ALTER TABLE ... ADD COLUMN mr_title TEXT NOT NULL DEFAULT ''`，不改写已有行，不回填旧评审，也不改变既有索引。
   - 升级后已有评审的标题为空：列表接口省略 `mr_title`，控制台继续显示 `!iid`。这是预期行为，不需要回填，也无法从旧数据恢复标题。
3. **迁移 `0017_mcp_config_seal` 会加密已存 MCP 配置。**
   - 启动时使用原 `REVIEW_MASTER_KEY`，在事务中把平台、项目、停用及内置 MCP 配置加密到 `config_enc`，清空旧明文列并写入密钥校验标记；错误密钥、损坏的 MCP 配置或写入失败会阻止启动。结构迁移完成不代表数据回填完成。
   - 不能更换主密钥。首次回填的密钥校验、启动失败处理和回退边界见 [MCP 配置加密升级](#mcp-配置加密升级v120)。
   - 升级后 API 不再提供可读回的 MCP 配置，响应统一为 `config: {}`；仅改名或启停时省略 `config`。旧程序可能把 `{}` 当作配置或重新写入明文，不能只替换镜像回退。
   - 迁移只清空当前逻辑列，不保证历史 WAL、SQLite 空闲页、文件系统快照或备份中的明文已擦除。
4. **迁移 `0018_dossier_project` 回填 dossier 项目归属。**
   - 旧记录仅根据实际关联的 workflow 回填；没有关联的旧孤儿保持未知归属，不从快照猜测，也不改压缩快照内容。
   - 升级前按第 7 节做一致性备份；此迁移没有 down migration，回退须恢复升级前完整快照及匹配旧程序。运行时的孤儿清理行为见 [dossier 归属迁移](#dossier-归属迁移v120)。

本次还新增 `0019_job_defer_window`，只给 `jobs` 追加 `defer_since TEXT`。任务连续因执行权争用被延后超过固定的 15 分钟窗口时按失败结算，`last_error` 为 `execution deferral window exceeded`，不再无限延长；旧行保持 NULL 兼容。该窗口不是配置项。该迁移同样没有 down migration，回退须恢复升级前快照。
5. **ignore 保护范围扩大。**
   - 有效 ignore 非空时，仓库读取和证据检查遵守同一规则；无法证明符合规则的 MCP 工具及全量图谱索引不再执行，无来源证明的历史上下文不再注入。
   - 默认 ignore 非空，因此默认图谱能力会降级。这是安全降级：不记 `tool_gaps`，不会把 PASS 改为 INCOMPLETE。仅有索引身份无法证明且 ignore 为空时，实际启用并绑定的工具仍记为覆盖缺口。
   - 管理员绑定、历史上下文和旧索引保留。缺少版本化策略的旧工作流不续跑或重发，需要重新触发评审。详见 [ignore 安全边界](#ignore-安全边界)。
6. **日志字段改名，检索和告警要同步调整。**
   - 生产日志不再输出原始错误文本，原 `error` 字段改为 `error_class`（脱敏类别，例如 `timeout`、`canceled`、`internal`、`mr_closed`）。
   - 任务结算日志 `review job settled without retry`、`workspace job settled without retry` 的 `reason` 字段也改名为 `error_class`，值为 `mr_closed`、`review_superseded`、`stale_publication`、`permanent_job` 等。
   - 评审筛选、发布失败、扩展绑定跳过等业务 `reason` 字段仍然存在。不要把所有 `reason` 检索都直接替换成 `error_class`。
   - `scripts/logcheck` 随 `go test ./...` 运行；本地修改代码后先跑它，避免在 logger 调用里记录 `err` 或 `err.Error()`。
7. **启动锁：两个实例不能共用数据目录。**
   - 新版本在打开 SQLite 前对数据库所在目录的 `tribunal.lock` 加 `flock` 独占锁（默认容器配置下为 `/app/data/tribunal.lock`）。锁被占用时，服务记录带锁文件路径的错误日志并以退出码 1 退出，不会打开数据库。
   - 同一数据目录只能有一个进程；同一宿主机上多个容器共用同一卷可以互斥，但跨主机的 NFS 等网络文件系统不受保护。
   - 锁随进程退出由内核释放，崩溃后无需清理；`tribunal.lock` 文件残留是正常的，不要手工删除。锁的限制见[镜像内容与边界](#1-镜像内容与边界)。
8. **`bootstrap_admins` 用户名写法已弃用。**
   - 用户名条目仍可用，但启动时每个条目记一条 `WARN`，含 `index` 及绑定状态或替代值，不含用户名；已绑定时给出 `replacement=id:<N>`。
   - 计划和注意点见 [用户名写法弃用与迁移](#bootstrap_admins-用户名写法弃用与迁移)。计划在 v2.0.0 移除；在此之前改用 `id:<GitLab 用户 ID>`。
   - `id:` 写法不受影响，也不需要在升级时重新绑定。
9. **Webhook 存储失败改返回 503。** 查项目、写投递记录或入队失败时，`POST /webhooks/gitlab` 返回 503，不再返回 200 加 `ignored: storage_error`。GitLab 会把这次投递记为失败；连续失败可能自动停用 hook，升级后要检查 GitLab 端的 hook 状态。已停用的 hook 需要重新启用，失败投递可以用 Resend request 重发。
10. **全局会话上限优先保留管理员。** 全局 4096 个会话的兜底上限触发时，先淘汰非管理员的最旧会话。角色按当前 `bootstrap_admins` 刷新，改配置并重启后不需要用户重新登录。每用户 10 个会话的上限不变，无需迁移。
11. **升级后检查。**
    - `docker compose logs` 中不应出现迁移失败、`data directory is locked by another Tribunal instance`、MCP 密钥校验失败或 `bootstrap_admins` 未预期的弃用告警。
    - `/readyz` 返回 200；已有项目可正常读取，已有评审列表可打开，旧评审显示 `!iid` 而不是报错。
    - MCP 服务器列表和详情返回 `config: {}`；内置 `codebase-memory` 仍在，默认 ignore 下评审不再因 MCP 工具记为覆盖缺口。
    - 触发一次新评审，确认列表能显示 `mr_title`，日志检索使用 `error_class` 仍能命中；被 ignore 的路径不出现在评论中。
    - v1.2.0 尚未完成真实实例验收；在 #108 完成前不要把本次升级当作发布验收。

## 9. 无外部凭据的本地烟测

没有 Docker 时，可用本机 Go 和临时数据目录验证启动、readiness 和 OAuth 默认关闭行为。该烟测不接触 GitLab、模型 provider 或真实 OAuth。

在受控的隔离环境执行以下完整代码块，不要拆分。先确认 `127.0.0.1:18080` 空闲且测试期间不会被其他进程使用；健康接口本身不能证明进程身份。它不启动 Docker；会构建前端，修改本检出的 `web` 构建产物，并在私有临时目录启动本次二进制。不要在不可信同 UID 进程或不受控目录的环境中运行。

```bash
(
  set +x
  set -eu
  set -o pipefail
  unset REVIEW_MASTER_KEY REVIEW_ADMIN_TOKEN TRIBUNAL_SESSION_KEY
  TMP="$(mktemp -d)"
  PID=
  cleanup_smoke() {
    for child in $(jobs -pr); do
      if [ "$child" = "$PID" ]; then kill "$PID" 2>/dev/null || true; fi
    done
    if [ -n "$PID" ]; then wait "$PID" 2>/dev/null || true; fi
    rm -rf -- "$TMP"
  }
  trap cleanup_smoke EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  mkdir -p "$TMP/npm-home"
  (
    cd web
    /usr/bin/env -i PATH="$PATH" HOME="$TMP/npm-home" npm ci
    /usr/bin/env -i PATH="$PATH" HOME="$TMP/npm-home" npm run build
  )
  go build -o "$TMP/tribunal-server" ./cmd/server

  REVIEW_MASTER_KEY="$(openssl rand -base64 32)"
  REVIEW_ADMIN_TOKEN="$(openssl rand -hex 32)"
  TRIBUNAL_SESSION_KEY="$(openssl rand -base64 32)"
  export REVIEW_MASTER_KEY REVIEW_ADMIN_TOKEN TRIBUNAL_SESSION_KEY
  cat > "$TMP/override.yaml" <<EOF
server:
  listen: "127.0.0.1:18080"
  public_url: "http://127.0.0.1:18080"
storage:
  dsn: "file:$TMP/data/review.db"
workspace:
  root: "$TMP/workspaces"
EOF
  "$TMP/tribunal-server" --config config/server.example.yaml \
    --config "$TMP/override.yaml" --check-config
  "$TMP/tribunal-server" --config config/server.example.yaml \
    --config "$TMP/override.yaml" >"$TMP/server.log" 2>&1 &
  PID=$!
  for ((i=0; i<50; i++)); do
    kill -0 "$PID"
    if [ "$(curl -fsS --max-time 2 -o /dev/null -w '%{http_code}' http://127.0.0.1:18080/readyz 2>/dev/null)" = 200 ]; then break; fi
    sleep 0.2
  done
  [ "$(curl -fsS --max-time 2 -o /dev/null -w '%{http_code}' http://127.0.0.1:18080/healthz)" = 200 ]
  [ "$(curl -fsS --max-time 2 -o /dev/null -w '%{http_code}' http://127.0.0.1:18080/readyz)" = 200 ]
  [ "$(curl -sS --max-time 2 -o /dev/null -w '%{http_code}' http://127.0.0.1:18080/)" = 200 ]
  [ "$(curl -sS --max-time 2 -o /dev/null -w '%{http_code}' http://127.0.0.1:18080/auth/login)" = 503 ]
  [ "$(printf 'Authorization: Bearer %s\n' "$REVIEW_ADMIN_TOKEN" |
    curl -fsS --max-time 2 -o /dev/null -w '%{http_code}' -H @- http://127.0.0.1:18080/api/v1/projects)" = 200 ]

  export GITLAB_OAUTH_CLIENT_ID="dummy-client-id"
  export GITLAB_OAUTH_CLIENT_SECRET="dummy-client-secret"
  cat > "$TMP/oauth.yaml" <<EOF
server:
  listen: "127.0.0.1:18081"
  public_url: "https://review.example.internal"
auth:
  oauth:
    client_id_env: GITLAB_OAUTH_CLIENT_ID
    client_secret_env: GITLAB_OAUTH_CLIENT_SECRET
  bootstrap_admins: ["id:42"]
storage:
  dsn: "file:$TMP/oauth-data/review.db"
workspace:
  root: "$TMP/oauth-workspaces"
EOF
  "$TMP/tribunal-server" --config config/server.example.yaml \
    --config "$TMP/oauth.yaml" --check-config
)
```

三个烟测密钥只存在于子 shell 和其子进程中，成功、失败或中断后不改变调用者的值或导出状态；`npm ci` 和构建只接收继承的 PATH 与临时 HOME，不继承其他环境变量。临时管理员 token 只经标准输入传给 curl，不作为命令行参数。子 shell 在退出时尝试停止其运行中的后台任务并删除本次临时目录；任务检查和发信号不是原子操作，不能在有不可信进程或共享端口的环境保证停机所有权。SIGKILL、机器故障等也可能跳过清理，应在受控环境核对残留，不要按 PID 盲目停止其他进程。

预期两个 `--check-config` 输出 `configuration OK`；`/healthz`、`/readyz`、`/` 和带临时 Bearer 管理令牌的 `/api/v1/projects` 返回 200；未配置 OAuth 时 `/auth/login` 返回 503。任一步失败即停止。OAuth 检查只验证配置结构和环境变量引用，不证明 GitLab 回调、token 交换或用户角色映射已经通过真实集成验收。

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
(
  set +x
  REVIEW_MASTER_KEY="$(openssl rand -base64 32)" &&
  REVIEW_ADMIN_TOKEN="$(openssl rand -hex 32)" &&
  export REVIEW_MASTER_KEY REVIEW_ADMIN_TOKEN &&
  docker run --rm -e REVIEW_MASTER_KEY -e REVIEW_ADMIN_TOKEN \
    tribunal:local --config /app/config/server.example.yaml --check-config
)
```

预期：`git version` 有输出，`id` 显示 uid 10001 和 gid 10001，`stat` 输出 `10001:10001 700`，`--check-config` 输出 `configuration OK`。

启动、readiness 和 Bearer 接口验证使用上面的[隔离 Compose 烟测](#docker-可用时的-compose-烟测)。该脚本需要 Docker daemon 和 Docker Compose v2，使用独立的临时项目和数据卷；仅检查脚本语法不算 Docker 实测。该烟测不验证真实 GitLab、OAuth、webhook 或评审执行，不能当作集成验收。

## 10. 验证状态

验证记录统一维护在 [交付就绪与验收记录](delivery-readiness.md)，本文不再重复：

- 已发布版本：[v1.1.x 验证状态](delivery-readiness.md#v110-验证状态)。
- 待发布版本：[v1.2.0 验证状态](delivery-readiness.md#v120-验证状态)。
- v1.0.0（f143 快照）在隔离测试实例上的验收记录：[v1.0.0 历史验收记录](delivery-readiness.md#v100-历史验收记录f143仅供追溯)，仅供追溯，不覆盖 v1.1.x 和 v1.2.0 的改动。

上面第 9 节的烟测不验证真实 GitLab、OAuth、webhook 或评审执行，不能当作集成验收。
