# Agent 部署指南

这份指南写给代替用户部署和接入 Tribunal 的 agent，按顺序执行即可。每一步都写明做什么、怎么验证、失败时查哪里。概念说明和完整参数见 [部署与运维](operations.md) 与 [配置设计](design/06-config.md)，本文不重复。

适用版本：v1.2.0（尚未发布，打 tag 前以 `main` 为准），Docker Compose 单实例加 SQLite。从旧版本升级时，先按 [升级流程](#8-从-v100-升级) 执行，不要直接走全新部署的步骤。

## 0. 安全约束（每一步都要遵守）

- **不输出秘密**：不把主密钥、token、client secret、webhook secret 打印到终端、日志、PR、Issue 或聊天里。检查 Compose 用 `docker compose config --quiet`，不要用不带 `--quiet` 的 `config`，后者会展开秘密。
- **安全保存秘密**：使用受控的秘密管理器或权限为 `600` 的 `.env`；不提交，不写进 YAML，不作为 build arg。生成时的临时文件同样要保护；项目 token 由安全环境注入，只在注册那一次通过 `-e` 传入容器。token 被导出期间宿主 shell 的子进程也能继承它，清除前只运行可信程序。
- **不执行 `docker compose down -v`**，也不删除数据卷 `crc-review_data`。
- **不改 Compose 项目名**（`name: crc-review`）：改名会挂上一个新的空卷，现有数据就看不到了。
- **不更换 `REVIEW_MASTER_KEY` 和 `TRIBUNAL_SESSION_KEY`**：前者一换，已存凭据就解不开；后者一换，所有浏览器会话失效。
- **同一时间只运行一个实例**，不要让两个容器挂同一个数据卷。
- **先问用户再做**：创建 GitLab OAuth Application、给 GitLab 项目加 Webhook、注册项目都会改动外部系统；注册项目还会调用模型，产生费用。这些动作要先得到用户确认。

## 1. 向用户索取的输入

开始前一次性问齐。标"必填"的缺一项就不能启动。

| 输入 | 用途 | 必填 |
|---|---|---|
| GitLab 根地址，例如 `https://gitlab.example.com` | `GITLAB_URL`；不是 `/api/v4` 地址，并且要能从容器内访问 | 是 |
| 模型协议：`openai-chat`、`openai-responses` 或 `anthropic` | `LLM_PROTOCOL` | 是 |
| 模型 API 地址 | `LLM_BASE_URL`；OpenAI 兼容接口一般带 `/v1`，Anthropic 用 API 根地址 | 是 |
| 模型 ID 和 API key | `LLM_MODEL`、`LLM_API_KEY` | 是 |
| 服务对外地址 | `TRIBUNAL_PUBLIC_URL`；要让 GitLab 回调 Webhook 或启用 OAuth，就必须是 GitLab 和浏览器都能访问的 HTTPS 地址 | 本机试用可用默认值 |
| 宿主机端口 | `TRIBUNAL_HTTP_PORT`，默认 8080 | 否 |
| 是否启用浏览器登录（OAuth） | 决定是否执行第 4 步 | 否 |
| 平台管理员的 GitLab 用户 ID | `GITLAB_ADMIN_USERNAME`，推荐写成 `id:<数字>` | 启用 OAuth 时需要 |
| 要接入的项目路径，以及项目 token | 第 5 步；token 需要 `api` 和 `read_repository` scope，自动建 Webhook 还需要 Maintainer | 接入项目时需要 |

不要让用户在聊天或命令中提供 API key、项目 token 等秘密；由用户或受控秘密管理器通过安全方式直接注入部署环境。

部署机器上需要有 Docker Engine 和 Compose v2.20 或更新版本，不需要 Go 或 Node。首次构建要能访问 Docker Hub、Debian 源、Go 模块源、npm registry 和 GitHub Releases（下载 codebase-memory-mcp）。

## 2. 准备配置

在仓库根目录执行：

```bash
docker compose version
cp -n .env.example .env
chmod 600 .env
```

生成三个密钥，各只生成一次，写进 `.env`，不要回显出来。下面的命令先关闭 shell 的 xtrace，在仓库外创建唯一的私有临时目录，再以不可覆盖方式创建权限为 `600` 的文件；首次写入秘密之前权限就已生效。确保 `TMPDIR`（若设置）位于可信的仓库外目录：

```bash
set +x
(
  umask 077
  KEY_DIR="$(mktemp -d "${TMPDIR:-/tmp}/tribunal-keys.XXXXXXXX")" || exit 1
  ENV_GENERATED="$KEY_DIR/.env.generated"
  if ! (
    set -C
    exec 3> "$ENV_GENERATED" || exit 1
    if ! {
      MASTER="$(openssl rand -base64 32)" &&
      ADMIN="$(openssl rand -hex 32)" &&
      SESSION="$(openssl rand -base64 32)" &&
      [ -n "$MASTER" ] && [ -n "$ADMIN" ] && [ -n "$SESSION" ] &&
      printf 'REVIEW_MASTER_KEY=%s\n' "$MASTER" >&3 &&
      printf 'REVIEW_ADMIN_TOKEN=%s\n' "$ADMIN" >&3 &&
      printf 'TRIBUNAL_SESSION_KEY=%s\n' "$SESSION" >&3
    }; then
      exec 3>&-
      rm -f -- "$ENV_GENERATED" || printf '无法删除未完成的密钥文件，请安全处理。\n' >&2
      exit 1
    fi
    exec 3>&-
  ); then
    printf '密钥生成或写入失败，请勿使用该文件；检查临时目录 %s\n' "$KEY_DIR" >&2
    exit 1
  fi
  printf '密钥文件路径（不含密钥）：%s\n' "$ENV_GENERATED"
)
```

只有命令成功才可使用打印出的文件路径。失败时停止，安全处理提示的临时目录，不使用部分结果。通过不输出文件内容的安全方式，把文件里的三行合并进 `.env`，替换模板中的空值，然后安全删除本次生成的文件和临时目录。再填第 1 步拿到的 `GITLAB_URL`、`LLM_*`、`TRIBUNAL_PUBLIC_URL`。值里含有 `$` 或 `#` 时，用单引号括起来。

提醒用户把 `REVIEW_MASTER_KEY` 单独保存在安全位置：数据备份没有这把密钥，就解不开里面的凭据。

**验证**

```bash
docker compose config --quiet && echo compose-ok
```

失败时，报错会指出缺哪个必填变量（`... is required`）。另外检查宿主机 shell 里有没有导出同名旧值：已导出的变量优先于 `.env`。

**模型配置**：`config/server.docker.yaml` 只定义了一个 provider，名为 `primary`，onboarding、评审和挑战核验都用它。要配置多个模型或多模型对抗，参照 [配置设计 · 平台配置文件](design/06-config.md#1-平台配置文件) 修改这个文件。不要删除 `profiles.onboarding.agent.provider` 和 `profiles.default.challenge.provider`。

## 3. 构建并启动

```bash
docker compose build
docker compose run --rm --no-deps review --config /app/config/server.yaml --check-config
docker compose up -d --wait --wait-timeout 120
docker compose ps
```

**验证**

```bash
docker compose exec -T review curl -fsS http://127.0.0.1:8080/readyz && echo ready
docker compose logs --tail=100 review
docker compose exec -T review /app/tribunalctl projects all
```

- `readyz` 返回 200 只说明数据库和 worker 可用，不说明模型或 GitLab 能连通。
- 日志里不应有 `error` 级别的启动错误。旧的 `CRC_*` 变量名只会产生弃用告警，可以忽略。
- `projects all` 在全新部署时返回空列表。

**常见失败**

| 现象 | 原因与处理 |
|---|---|
| `--check-config` 报 provider 或 profile 错误 | `LLM_PROTOCOL` 拼错，或 YAML 里删掉了必需的 profile provider |
| 容器反复重启，日志提到 session key | 检查 `TRIBUNAL_SESSION_KEY` 是否为空或只有空白及其安全注入方式。已有部署须恢复原来保存的值；只有首次部署，或用户明确授权并接受所有现有浏览器会话失效的轮换，才生成新值 |
| 构建时下载 codebase-memory-mcp 失败或校验和不符 | 网络无法访问 GitHub Releases。不要改 Dockerfile 里写死的 SHA-256；配置代理后重试 |
| 访问 `/auth/login` 返回 503 | OAuth 还没启用，这是正常的，见第 4 步 |

## 4. 启用浏览器登录（可选）

只有用户需要控制台登录时才做这一步。先确认对外地址是 HTTPS，反向代理按 [运维 · 健康检查与 HTTPS 反向代理](operations.md#6-健康检查与-https-反向代理) 配好。

1. **先征得用户同意**，再在 GitLab 创建 OAuth Application：
   - scope 只选 `read_user`；
   - Redirect URI 填 `<TRIBUNAL_PUBLIC_URL>/auth/callback`。
2. 先向用户确认实际 GitLab 数字用户 ID 和本次平台管理员授权，再在 `.env` 里设置 `GITLAB_OAUTH_CLIENT_ID`、`GITLAB_OAUTH_CLIENT_SECRET`，以及 `GITLAB_ADMIN_USERNAME=id:<管理员 GitLab 用户 ID>`。用户 ID 可以用 `GET <GITLAB_URL>/api/v4/users?username=<name>` 查到。
3. 在 `config/server.docker.yaml` 里取消 `oauth:`、`client_id_env:`、`client_secret_env:` 和 `bootstrap_admins:` 这几行的注释。
4. 重建容器。`restart` 不会读入新的环境变量，所以要用：

```bash
docker compose up -d --force-recreate --wait --wait-timeout 120
```

**验证**：浏览器打开 `<TRIBUNAL_PUBLIC_URL>/auth/login`，应该跳到 GitLab 授权页；管理员登录后，控制台左下角显示「管理员」。

`bootstrap_admins` 只授予平台管理标签，不授予任何项目的数据访问权限，项目权限在第 6 步单独授予。

## 5. 接入 GitLab 项目

**先向用户确认**：实际 GitLab 目标、要接入的项目、注册动作及可能产生的模型费用。注册会访问 GitLab，并创建一个会调用模型的 onboarding 任务；未得到明确授权就不要执行。

注册接口需要平台管理员权限，可以用 `.env` 里的 `REVIEW_ADMIN_TOKEN`，也就是 CLI 的默认凭据。注册后，`projects get` 需要该项目的 `read` 或 `manage` 授权；`projects webhook ...` 需要 `manage` 授权。bootstrap token 没有用户 ID，调用这些项目级接口会得到 404。所以 Webhook 最好在注册时就一起建好。

**注册**

严格按 [运维 · 浏览器登录与项目注册](operations.md#浏览器登录与项目注册) 的受控步骤执行，不要复制或使用固定的项目示例。每次将 `GITLAB_PROJECT` 设为用户本次明确确认的实际项目，不要沿用未重新确认的旧值；项目未提供或未获授权时停止，不执行注册。它可以是项目路径、数字项目 ID 或 `GITLAB_URL` 下的完整仓库 URL。

注册响应可能带有一次性的 `webhook_secret`。注入秘密之前先在当前 shell 执行 `set +x`，确认终端录制和日志不会捕获秘密，再由用户或安全秘密管理器直接注入 `GITLAB_PROJECT_TOKEN`。不要把 token 写入命令、shell history、聊天、日志或长期运行服务的 `.env`；它仅用于本次 CLI 注册。token 和打开的响应文件描述符可被当前 shell 的子进程继承，秘密处理完成前只运行可信程序；Docker 管理员仍可访问容器环境。按运维步骤设置仓库外的受控私有 `REGISTRATION_DIR` 绝对路径，并在提示时确认授权。响应只写入唯一的 0600 私有文件 `$REGISTRATION_FILE`；标准输出只能显示经过校验的正整数项目 ID，不要查看或输出完整响应。该 ID 不能证明 onboarding 已完成，须另行检查任务状态。

- 只想校验、不落库，可以在获得访问授权后调用 `POST /api/v1/projects/validate`，见 [项目 API](design/03-http-api.md#41-项目)。
- 任一步失败都停止，不解析或使用部分响应，也不要盲目重试：注册可能已提交。按运维步骤保留并安全处理私有响应，核对项目和 Webhook 状态。命令块会尝试清除当前 shell 中的项目 token；readonly、清除失败或信号中断时不能保证清除，无法确认时应结束受影响的 shell。已继承 token 的子进程和其他副本不会随当前变量的清除而清除。

**Webhook**（二选一，都要先征得用户同意）

- **自动（推荐）**：经用户同意后，在运维节注册命令的 `--profile default` 后添加 `--auto-webhook`。项目 token 需要 Maintainer 权限，并且 `TRIBUNAL_PUBLIC_URL` 能被 GitLab 访问。注册成功就说明 hook 已经建好。
- **手动**：使用运维节默认的不带 `--auto-webhook` 的注册命令，响应文件里的 `webhook_secret` 只在这一次返回。在 GitLab 项目的 Webhooks 页面添加：
  - URL：`<TRIBUNAL_PUBLIC_URL>/webhooks/gitlab`；
  - Secret token：响应文件里的 `webhook_secret`；
  - 勾选 Merge request events、Comments、Push events。

  只能按运维节说明通过受控渠道传递 secret，不要将它放入命令行参数、终端、日志或聊天。如果使用 `glab api` 或 GitLab API，须通过标准输入或受保护的请求体文件传递并禁止记录请求体；无法保证受控渠道时停止，由有权限的用户安全完成配置。
- 自动注册或手动配置完成后，在同一个 shell 按运维节安全删除本次 `$REGISTRATION_FILE`；确认项目 token 已清除、响应路径可清除且路径和文件身份与只读标记一致，才删除文件。删除后须核验打开的文件已解除链接且原路径不存在，才清除 `REGISTRATION_FILE` 并关闭描述符。清除、删除或核验失败时返回失败，以便安全处理，不要盲目重试；路径缺失或文件不存在也按失败处理。处理完毕后，下一次注册使用新 shell。预检、删除、核验和清除变量并非原子操作，也不保护不可信同 UID 进程或不受控目录；删除后变量清理意外失败时需安全核对文件状态。
- **以后再启用、重建或停用**：在控制台「项目 → 设置 → GitLab Webhook」里操作，需要该项目的 `manage` 权限。或者在第 6 步授权之后，用授权用户的 API token 调用 CLI（`--api-token-env USER_TOKEN projects webhook enable|rebuild|disable <项目ID>`）。重建会轮换 secret。

**验证**

```bash
docker compose exec -T review /app/tribunalctl projects all
docker compose logs --tail=100 review
```

GitLab Webhook 测试和在测试 MR 下评论 `/review` 会产生外部动作或模型费用；先确认目标并另获用户明确授权，未授权时不要执行。

- `projects all` 里能看到新项目。这是管理面列表，bootstrap token 可以用。
- 控制台「项目 → 工作区与上下文」里，onboarding 任务最终应显示完成，索引状态正常。查看这一页需要项目权限，见第 6 步。
- GitLab 的 Webhook 页面「Test → Merge request events」应返回 200。
- 在一个测试 MR 下评论 `/review`，MR 上应出现 eyes 表情和状态 note，评审完成后发布总结。

## 6. 授予项目权限

项目权限保存在 Tribunal 本地，不随 GitLab 组成员关系同步。默认任何人都没有项目权限，平台管理员也不例外。

1. 先向用户确认实际项目 ID、目标用户的 GitLab 用户 ID 和需要的角色；不要默认授予任何用户权限。让目标用户先用 OAuth 登录一次，本地 `users` 表里才会有这个人。
2. 用管理凭据列出用户和项目，核对用户确认的 ID：

```bash
docker compose exec -T review /app/tribunalctl users list
docker compose exec -T review /app/tribunalctl projects all
```

3. 每次在当前 shell 将 `PROJECT_ID`、`GITLAB_USER_ID` 和 `GRANT_ROLE` 设置为用户本次确认的数字 ID 和角色（`read` 或 `manage`），不要沿用未重新确认的旧值。角色、ID 缺失或无效时不会执行授权命令：

```bash
: "${PROJECT_ID:?请先设置用户确认的项目 ID}" &&
: "${GITLAB_USER_ID:?请先设置用户确认的 GitLab 用户 ID}" &&
: "${GRANT_ROLE:?请先设置用户确认的角色}" &&
[[ "$PROJECT_ID" =~ ^[1-9][0-9]*$ && "$GITLAB_USER_ID" =~ ^[1-9][0-9]*$ ]] &&
[[ "$GRANT_ROLE" == read || "$GRANT_ROLE" == manage ]] &&
docker compose exec -T review /app/tribunalctl projects grants set "$PROJECT_ID" "$GITLAB_USER_ID" "$GRANT_ROLE" &&
docker compose exec -T review /app/tribunalctl projects grants list "$PROJECT_ID"
```

`read` 只能查看；`manage` 还可以改设置、触发和取消评审、重新发布、轮换 Key、管理 Webhook。项目级的 MCP、Skill 和块绑定写入，还另外需要平台管理员身份。

## 7. 内置扩展与自定义接入

v1.2.0 开箱自带两项内置扩展，不需要额外配置：

- Skill `code-review`：挂在 `code-review`、`review` 块上。
- MCP 服务器 `codebase-memory`：只读的代码图谱工具，各项目的索引互相隔离。默认 ignore 非空时，评审中的 MCP 工具和全量图谱索引不执行，这是安全降级，不影响 PASS；详见 [ignore 安全边界](operations.md#ignore-安全边界)。

**验证**：控制台「扩展」页里，两项都带「内置」标签。在 MCP 服务器一栏点「测试」，工具列表里应有 `search_graph`、`trace_path`、`get_code_snippet`，并且没有 `index_repository`、`delete_project`。MCP 详情和列表的配置返回 `config: {}` 是预期行为。

- **不想用内置项**：在控制台禁用即可，重启和升级后保持禁用。内置项不能删除。
- **新部署不登记内置项**：配置里写 `default_extensions.skills: []`，MCP 同理，见 [默认扩展](design/06-config.md#默认扩展default_extensions)。
- **接入自己的 MCP**：控制台「扩展 → MCP 服务器」支持直接粘贴 Claude Code 格式的 `mcpServers` JSON。然后在「块绑定」里给需要的块授权只读工具。配置里的秘密不要直接写进 JSON，按下面三步把它传进容器：
  1. 在 `.env` 里加上变量，比如 `MY_MCP_TOKEN=...`。
  2. 在仓库根目录新建 `compose.override.yaml`，把变量加进容器环境。Compose 会自动加载这个文件。文件里只写变量映射，不写值：

     ```yaml
     services:
       review:
         environment:
           MY_MCP_TOKEN: "${MY_MCP_TOKEN:?MY_MCP_TOKEN is required}"
     ```

     `compose.yaml` 只会把 `environment` 里列出的变量传进容器，所以光写在 `.env` 里，服务读不到。
  3. 在 MCP 配置里写成 `${MY_MCP_TOKEN}` 模板。如果子进程也要读这个变量，把变量名加进 `x-review.env_allowlist`。然后用 `docker compose config --quiet` 检查，再执行 `docker compose up -d --force-recreate --wait` 重建容器。`env_allowlist` 只决定子进程能继承哪些变量，不能把变量带进容器。
- **接入自己的 Skill**：上传 zip，或登记服务器上的目录，格式兼容 Claude Code Skill；Skill 里的脚本不会被执行。
- 数据库里的扩展改动按进程缓存生效，改完通常要重启：`docker compose up -d --force-recreate --wait`。

## 8. 从 v1.0.0 升级

从 v1.1.0 升级到 v1.2.0 时，改按 [运维 · 从 v1.1.0 升级到 v1.2.0](operations.md#从-v110-升级到-v120) 执行，重点是不可回退的迁移 `0016`/`0017`/`0018`、MCP 配置加密和 ignore 安全降级；不要套用下面的 v1.0.0 步骤。

从 v1.0.0 升级时，严格按 [运维 · 从 v1.0.0 升级到 v1.1.0](operations.md#从-v100-升级到-v110) 执行，再按上一段完成 v1.2.0 的迁移；关键点：

1. 记下当前镜像和提交，按[运维第 7 节](operations.md#7-sqlite-一致性备份与恢复)核对生效的 `storage.dsn`、`workspace.root`、配置合并和环境展开、服务器进程实际工作目录、数据库及工作区文件和全部持久化挂载（含嵌套与覆盖）。无法确定时停止并排查；不要输出秘密。
2. 用 `docker compose stop review` 停服并保留容器；拷贝前不能执行 `docker compose down`。按运维第 7 节停止其他写入进程，完整备份并核验数据库、存在时的 WAL/SHM 和所有工作区，另行安全备份 `.env`、配置和密钥。只有默认路径及单卷等该节前提全部满足，才能使用默认例子；自定义布局须按该节逐项完整备份，并在恢复时使用匹配布局的新空位置。必须完成该节全部校验与隔离要求，不得因本摘要跳过；备份和恢复期间不要启动旧服务。
3. 完成备份核验和隔离并获得部署授权后，拉取新代码，执行 `docker compose build` 和 `docker compose up -d --wait`，沿用原来的全部数据挂载、配置和 `REVIEW_MASTER_KEY`。
4. 用 `docker compose logs review | grep -Ei 'migrat|cannot be decrypted|error'` 检查日志，用 `tribunalctl projects all` 确认项目都在。
5. 升级后 v1.0.0 读不了新数据。回退只能按运维第 7 节恢复第 2 步的完整备份，不能只换回旧镜像。

## 9. 交付给用户时要报告的内容

- 访问地址、是否已启用 OAuth、管理员是谁（写用户 ID，不写 token）。
- 已接入的项目 ID、Webhook 方式（自动或手动）和当前状态，以及已授权的用户和角色。
- 跑过哪些验证命令，结果如何。哪些没有验证，比如第 5 步的 `/review` 实测，要明确说出来。
- 提醒用户：妥善保存 `REVIEW_MASTER_KEY`，设置定期备份（[第 7 节](operations.md#7-sqlite-一致性备份与恢复)），以及从 GitLab 组移除成员时，要在 Tribunal 里同步删除对应的项目权限。
