# Agent 部署指南

这份指南写给代替用户部署和接入 Tribunal 的 agent，按顺序执行即可。每一步都写明做什么、怎么验证、失败时查哪里。概念说明和完整参数见 [部署与运维](operations.md) 与 [配置设计](design/06-config.md)，本文不重复。

适用版本：v1.1.0，Docker Compose 单实例加 SQLite。从 v1.0.0 升级时，先按 [升级流程](#8-从-v100-升级) 执行，不要直接走全新部署的步骤。

## 0. 安全约束（每一步都要遵守）

- **不输出秘密**：不把主密钥、token、client secret、webhook secret 打印到终端、日志、PR、Issue 或聊天里。检查 Compose 用 `docker compose config --quiet`，不要用不带 `--quiet` 的 `config`，后者会展开秘密。
- **秘密只放 `.env`**：权限 `600`，不提交，不写进 YAML，不作为 build arg。项目 token 只在注册那一次通过 `-e` 传给 CLI 进程。
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

部署机器上需要有 Docker Engine 和 Compose v2.20 或更新版本，不需要 Go 或 Node。首次构建要能访问 Docker Hub、Debian 源、Go 模块源、npm registry 和 GitHub Releases（下载 codebase-memory-mcp）。

## 2. 准备配置

在仓库根目录执行：

```bash
docker compose version
cp -n .env.example .env
chmod 600 .env
```

生成三个密钥，各只生成一次，写进 `.env`，不要回显出来：

```bash
printf 'REVIEW_MASTER_KEY=%s\n'    "$(openssl rand -base64 32)" >> .env.generated
printf 'REVIEW_ADMIN_TOKEN=%s\n'   "$(openssl rand -hex 32)"    >> .env.generated
printf 'TRIBUNAL_SESSION_KEY=%s\n' "$(openssl rand -base64 32)" >> .env.generated
chmod 600 .env.generated
```

把 `.env.generated` 里的三行合并进 `.env`，替换模板中的空值，然后删掉 `.env.generated`。再填第 1 步拿到的 `GITLAB_URL`、`LLM_*`、`TRIBUNAL_PUBLIC_URL`。值里含有 `$` 或 `#` 时，用单引号括起来。

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
| 容器反复重启，日志提到 session key | `TRIBUNAL_SESSION_KEY` 为空或只有空白，重新生成 |
| 构建时下载 codebase-memory-mcp 失败或校验和不符 | 网络无法访问 GitHub Releases。不要改 Dockerfile 里写死的 SHA-256；配置代理后重试 |
| 访问 `/auth/login` 返回 503 | OAuth 还没启用，这是正常的，见第 4 步 |

## 4. 启用浏览器登录（可选）

只有用户需要控制台登录时才做这一步。先确认对外地址是 HTTPS，反向代理按 [运维 · 健康检查与 HTTPS 反向代理](operations.md#6-健康检查与-https-反向代理) 配好。

1. **先征得用户同意**，再在 GitLab 创建 OAuth Application：
   - scope 只选 `read_user`；
   - Redirect URI 填 `<TRIBUNAL_PUBLIC_URL>/auth/callback`。
2. 在 `.env` 里设置 `GITLAB_OAUTH_CLIENT_ID`、`GITLAB_OAUTH_CLIENT_SECRET`，以及 `GITLAB_ADMIN_USERNAME=id:<管理员 GitLab 用户 ID>`。用户 ID 可以用 `GET <GITLAB_URL>/api/v4/users?username=<name>` 查到。
3. 在 `config/server.docker.yaml` 里取消 `oauth:`、`client_id_env:`、`client_secret_env:` 和 `bootstrap_admins:` 这几行的注释。
4. 重建容器。`restart` 不会读入新的环境变量，所以要用：

```bash
docker compose up -d --force-recreate --wait --wait-timeout 120
```

**验证**：浏览器打开 `<TRIBUNAL_PUBLIC_URL>/auth/login`，应该跳到 GitLab 授权页；管理员登录后，控制台左下角显示「管理员」。

`bootstrap_admins` 只授予平台管理标签，不授予任何项目的数据访问权限，项目权限在第 6 步单独授予。

## 5. 接入 GitLab 项目

**先向用户确认**：注册会访问 GitLab，并创建一个 onboarding 任务，这个任务会调用模型、产生费用。

项目 token 只传给这一次 CLI 调用，不写进 `.env`：

```bash
GITLAB_PROJECT_TOKEN='<由用户提供>' docker compose exec -T -e GITLAB_PROJECT_TOKEN review \
  /app/tribunalctl projects register --project group/repo --token-env GITLAB_PROJECT_TOKEN --profile default
```

- `--project` 也可以写数字项目 ID，或者 `GITLAB_URL` 下的完整仓库 URL。
- 注册成功后，记下返回的项目 ID。不要把整个响应贴给用户：手动 Webhook 模式下，响应里的 `webhook_secret` 只显示这一次。
- 先只做校验、不落库，可以调用 `POST /api/v1/projects/validate`，见 [项目 API](design/03-http-api.md#41-项目)。

**配置 Webhook**（二选一，都要先征得用户同意）

- **自动**：token 需要 Maintainer 权限，并且 `TRIBUNAL_PUBLIC_URL` 能被 GitLab 访问。

  ```bash
  docker compose exec -T review /app/tribunalctl projects webhook enable <项目ID>
  ```

  也可以在控制台「项目 → 设置 → GitLab Webhook」里点「启用」。重建用 `webhook rebuild`，会轮换 secret；停用用 `webhook disable`。
- **手动**：在 GitLab 项目的 Webhooks 页面添加：
  - URL：`<TRIBUNAL_PUBLIC_URL>/webhooks/gitlab`；
  - Secret token：注册时返回的 `webhook_secret`；
  - 勾选 Merge request events、Comments、Push events。

**验证**

```bash
docker compose exec -T review /app/tribunalctl projects get <项目ID>
docker compose logs --tail=100 review
```

- 控制台「项目 → 工作区与上下文」里，onboarding 任务最终应显示完成，索引状态正常。
- GitLab 的 Webhook 页面「Test → Merge request events」应返回 200。
- 在一个测试 MR 下评论 `/review`，MR 上应出现 eyes 表情和状态 note，评审完成后发布总结。

## 6. 授予项目权限

项目权限保存在 Tribunal 本地，不随 GitLab 组成员关系同步。默认任何人都没有项目权限，平台管理员也不例外。

1. 让用户先用 OAuth 登录一次，本地 `users` 表里才会有这个人。
2. 查出用户 ID，再授权：

```bash
docker compose exec -T review /app/tribunalctl users list
docker compose exec -T review /app/tribunalctl projects grants set <项目ID> <GitLab用户ID> manage
docker compose exec -T review /app/tribunalctl projects grants list <项目ID>
```

`read` 只能查看；`manage` 还可以改设置、触发和取消评审、重新发布、轮换 Key、管理 Webhook。项目级的 MCP、Skill 和块绑定写入，还另外需要平台管理员身份。

## 7. 内置扩展与自定义接入

v1.1.0 开箱自带两项内置扩展，不需要额外配置：

- Skill `code-review`：挂在 `code-review`、`review` 块上。
- MCP 服务器 `codebase-memory`：只读的代码图谱工具，各项目的索引互相隔离。

**验证**：控制台「扩展」页里，两项都带「内置」标签。在 MCP 服务器一栏点「测试」，工具列表里应有 `search_graph`、`trace_path`、`get_code_snippet`，并且没有 `index_repository`、`delete_project`。

- **不想用内置项**：在控制台禁用即可，重启和升级后保持禁用。内置项不能删除。
- **新部署不登记内置项**：配置里写 `default_extensions.skills: []`，MCP 同理，见 [默认扩展](design/06-config.md#默认扩展default_extensions)。
- **接入自己的 MCP**：控制台「扩展 → MCP 服务器」支持直接粘贴 Claude Code 格式的 `mcpServers` JSON。然后在「块绑定」里给需要的块授权只读工具。配置里的秘密写成 `${NAME}` 模板，并把变量名加进 `env_allowlist`，不要直接写进 JSON。
- **接入自己的 Skill**：上传 zip，或登记服务器上的目录，格式兼容 Claude Code Skill；Skill 里的脚本不会被执行。
- 数据库里的扩展改动按进程缓存生效，改完通常要重启：`docker compose up -d --force-recreate --wait`。

## 8. 从 v1.0.0 升级

严格按 [运维 · 从 v1.0.0 升级到 v1.1.0](operations.md#从-v100-升级到-v110) 执行，关键点：

1. 记下当前镜像和提交。停掉服务，按 [SQLite 一致性备份](operations.md#7-sqlite-一致性备份与恢复) 备份 `/app/data`；`.env` 和配置另外备份。
2. 拉取新代码，执行 `docker compose build` 和 `docker compose up -d --wait`，继续用原来的数据卷和原来的 `REVIEW_MASTER_KEY`。
3. 用 `docker compose logs review | grep -Ei 'migrat|cannot be decrypted|error'` 检查日志，用 `tribunalctl projects all` 确认项目都在。
4. 升级后 v1.0.0 读不了新数据。回退只能恢复第 1 步的备份，不能只换回旧镜像。

## 9. 交付给用户时要报告的内容

- 访问地址、是否已启用 OAuth、管理员是谁（写用户 ID，不写 token）。
- 已接入的项目 ID、Webhook 方式（自动或手动）和当前状态，以及已授权的用户和角色。
- 跑过哪些验证命令，结果如何。哪些没有验证，比如第 5 步的 `/review` 实测，要明确说出来。
- 提醒用户：妥善保存 `REVIEW_MASTER_KEY`，设置定期备份（[第 7 节](operations.md#7-sqlite-一致性备份与恢复)），以及从 GitLab 组移除成员时，要在 Tribunal 里同步删除对应的项目权限。
