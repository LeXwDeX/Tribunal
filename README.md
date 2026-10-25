# Tribunal

GitLab CE MR 多 agent 自动评审服务

基于 Go 的 GitLab CE MR 自动评审服务，使用 MIT 协议。

当前 M0–M6 功能已有本地实现，覆盖项目注册与 webhook、三种 LLM 协议、agent 工具循环、DAG 与持久化、主 agent 编排/对抗核验/裁决、GitLab 发布、工作区、MCP/Skill、OAuth 持久会话、项目 `read`/`manage` 授权与 CSRF、管理 API、React 控制台、API token、统计、反馈采集、人工标注与许可证清单。

已验证运行时候选/源码快照 `f143e7a7835e1e99345b1df9b289bb499e3c67416598d30e6184a558374eb0ac`（402 文件）通过本地全量检查、实际镜像构建、离线恢复/在线升级和连续两次无诊断代理 deep 运行；BLOCKING 是合成授权回归的预期结论。旧 `27c9b1c` 只是更早的已测快照，不代表本次已验证快照，也不覆盖新的 Responses header。较早的独立代码评审未发现 code blocker，但因缺少 f143 运行时 manifest 与 `12a62d1` 文档 manifest/source-equivalence 而记为 evidence BLOCKING；该缺口已关闭，最终独立 review 对 `12a62d1` 文档树给出 code/documentation PASS、0 blockers。之后的文档澄清只关联 [PR #30](https://github.com/LeXwDeX/Tribunal/pull/30)，不构成新的 review；native CI、平台评审、合并和 Release 仍需在 PR #30 回读，本文不预判结果。详见[验证](#验证)与[交付就绪](docs/delivery-readiness.md)。

总体目标见 [ARCH.md](ARCH.md)，实现层面的设计与已知偏差见 [详细设计](docs/design/README.md)，部署、备份与运行边界见 [部署与运维](docs/operations.md)。

## Docker Compose 部署

需要 Docker Engine 或 Docker Desktop，以及 Compose v2.20 或更新版本。宿主机不需要安装 Go 或 Node。镜像包含控制台、服务和 `tribunalctl`，SQLite 与工作区放在同一个持久卷中。

首次部署，在仓库根目录准备 `.env`：

```bash
cp -n .env.example .env
chmod 600 .env
```

填写真实 `GITLAB_URL`、`LLM_MODEL`、`LLM_API_KEY`，检查 `LLM_PROTOCOL` 和 `LLM_BASE_URL` 是否匹配模型接口。分别生成并持久保存 `REVIEW_MASTER_KEY`、`REVIEW_ADMIN_TOKEN`、`TRIBUNAL_SESSION_KEY`，生成方法见 `.env.example`。`.env` 已被 Git 和 Docker 构建上下文忽略；Compose 只传入显式列出的环境变量。

本文环境变量示例使用新主名 `TRIBUNAL_SESSION_KEY`、`TRIBUNAL_PUBLIC_URL`、`TRIBUNAL_HTTP_PORT`。旧名 `CRC_SESSION_KEY`、`CRC_PUBLIC_URL`、`CRC_HTTP_PORT` 仍兼容；新旧同时设置时新名优先。三个旧名只要配置了非空值，都会各记录一次弃用日志；即使新名已覆盖，日志仍提示旧名处于已配置状态，不代表旧值生效，且只含变量名、不含值。若部署的 `.env` 仍列出旧名，照常可用。升级时复用原 session key 的同一个值，不要重新生成，否则现有浏览器会话失效。

```bash
docker compose config --quiet
docker compose build
docker compose run --rm --no-deps review --config /app/config/server.yaml --check-config
docker compose up -d --wait --wait-timeout 120
docker compose exec -T review /app/tribunalctl projects all
```

`/app/tribunalctl` 是当前镜像内的 CLI。旧镜像的 CLI 路径是 `/app/reviewctl`；新镜像保留指向 `/app/tribunalctl` 的 `/app/reviewctl` 兼容符号链接，旧编排脚本可继续调用。源码用户改用 `go run ./cmd/tribunalctl`。

默认地址为 `http://localhost:8080`，端口仅绑定本机。默认支持 API/Bearer 管理，**浏览器登录需要另外启用 GitLab OAuth**：填写 `.env` 的 OAuth 变量及管理员用户名，取消 `config/server.docker.yaml` 中对应配置的注释，并配置 HTTPS 反向代理与 `TRIBUNAL_PUBLIC_URL`。修改 `.env` 后用 `docker compose up -d --force-recreate`，单纯 `restart` 不会更新容器环境变量。

停止使用 `docker compose down`，不要加 `-v`，否则会删除数据库和工作区。完整启动、项目注册、OAuth、备份及验证步骤见 [部署与运维](docs/operations.md)。已验证候选已构建实际镜像 `crc-v1-acceptance:candidate-f143e7a` 并通过配置检查；`crc-v1-acceptance` 是刻意保留的历史验收镜像名，不改写。更早的远程测试机记录仅作历史参考。

## 本地启动

需要 Go 1.26 或更新版本。SQLite 使用纯 Go 驱动，不需要 C 编译器。控制台产物通过 `//go:embed all:dist`（`web/embed.go`）打进二进制，因此**编译或运行前要先构建前端**：

```bash
cd web && npm ci && npm run build && cd ..
export REVIEW_MASTER_KEY="$(openssl rand -base64 32)"
export REVIEW_ADMIN_TOKEN="$(openssl rand -hex 32)"
export TRIBUNAL_SESSION_KEY="$(openssl rand -base64 32)"
go run ./cmd/server --config config/server.example.yaml --check-config
go run ./cmd/server --config config/server.example.yaml
```

全新检出没有 `web/dist` 时，`go build ./...` / `go run ./cmd/server` 会因 embed 找不到目录而失败。先执行 `cd web && npm ci && npm run build`。

示例配置可以启动服务，并提供 Bearer 认证。浏览器控制台登录另外需要 GitLab OAuth application，并在配置中设置 `auth.oauth.client_id_env` / `client_secret_env` 和绝对 HTTP(S) 地址 `server.public_url`；启用 OAuth 但缺少该地址时启动校验失败。示例配置未启用 `auth.oauth`，`/auth/login` 会返回 503：

```bash
export GITLAB_OAUTH_CLIENT_ID="..."
export GITLAB_OAUTH_CLIENT_SECRET="..."
```

`REVIEW_MASTER_KEY` 必须持久保存，重启时使用同一个值。丢失或更换它将无法解密已注册项目的凭据。不要在每次启动时重新生成。`REVIEW_ADMIN_TOKEN` 是管理员 bootstrap Bearer token，应由部署环境保管，不写入 YAML 或命令行参数。浏览器会话持久化在 SQLite；固定 `TRIBUNAL_SESSION_KEY`（旧名 `CRC_SESSION_KEY` 仍兼容，见上文兼容说明）时，未过期会话在重启后仍有效；每次启动重新生成 key 会让旧 cookie 失效并只记录告警。

未配置 OAuth 时，浏览器端无法登录；`REVIEW_ADMIN_TOKEN` 仍可作为 Bearer 使用，但它只是平台管理面 token：可调用用户、授权和系统配置接口，没有本地用户 ID，也不获得任何项目业务数据访问。已通过控制台创建的用户 API token（明文只在创建时显示一次）也继续可用于 Bearer，但创建 token 本身需要浏览器会话。浏览器会话和 API token 的 `admin`/`viewer` 标签在每次请求时按当前 `bootstrap_admins` 计算；项目业务数据另由本地 `project_grants` 的 `read`/`manage` 决定。平台管理员不会因为 `admin` 标签而自动获得项目权限，必须显式授予 grant；无 grant 或项目不存在统一返回 404，只有 `read` 时执行写操作返回 403。`users.role` 只记录签发时的标签，不作为已有 token 的项目权限来源。`REVIEW_ADMIN_TOKEN` 由配置固定，轮换由部署环境更新变量并重启完成，不受本地用户停用影响；从 `bootstrap_admins` 移除用户名并重启后，其平台管理标签失效。用户新签发的 API token 默认 90 天、最长 365 天；历史无过期时间的 token 在撤销前继续有效。

注册真实项目之前，需要把 `gitlab.url` 改为实际 GitLab 实例地址。可以用第二个 `--config` 指定覆盖文件；后面的映射递归合并，数组替换。

默认数据库为 `data/review.db`。服务启动时创建目录并执行迁移。

```bash
curl --fail http://localhost:8080/healthz
curl --fail http://localhost:8080/readyz
go run ./cmd/tribunalctl --url http://localhost:8080 projects all
```

`/healthz` 检查进程，`/readyz` 检查数据库和 worker 生命周期。非本机部署应使用 HTTPS 反向代理和访问控制，不能把明文 HTTP 管理接口暴露到公网。

## 注册项目

先通过部署环境设置 `GITLAB_PROJECT_TOKEN`。项目 token 需要 `api` 和 `read_repository` scope。自动创建 webhook 还需要 Maintainer 权限。

```bash
go run ./cmd/tribunalctl --url http://localhost:8080 projects register \
  --project group/backend-api \
  --token-env GITLAB_PROJECT_TOKEN \
  --profile default
go run ./cmd/tribunalctl --url http://localhost:8080 projects get 42
```

`--project` 也可以是 GitLab 项目 ID 或配置的 GitLab 根地址下的完整 HTTP(S) 仓库 URL。默认手工创建 webhook：注册响应中的 `webhook_secret` 只显示这一次，用它配置 GitLab 的 Secret token。URL 为服务地址加 `/webhooks/gitlab`，启用 Merge request、Comments 和 Push 事件。

控制台先「校验连接」，再「保存并初始化」。管理员也可调用 `POST /api/v1/projects/validate`，提交 `{project, token}`，只读验证身份、至少 Reporter 权限、scope、有效期和 Git HEAD；不会保存凭据、调用模型或创建 webhook。校验成功不证明完整 clone 或评论发布可用，详见 [项目 API](docs/design/03-http-api.md#41-项目)。设置页的「更新访问 Key」只更新本平台凭据，不在 GitLab 创建或撤销 token。

自动创建时添加 `--auto-webhook`，并把 `server.public_url` 配为 GitLab 能访问的服务地址。重复注册返回冲突，不覆盖已有凭据。自动创建 hook 成功但入库失败时会尝试删除刚创建的 hook；网络中断等异常仍可能需要管理员核对 GitLab 上的 hook。

CLI 默认读取 `REVIEW_ADMIN_TOKEN`，可以用全局参数 `--api-token-env NAME` 改变环境变量名。注册响应和普通查询都不会返回项目 access token 或数据库密文。项目业务列表按当前 bearer 所属用户的项目授权过滤；静态 bootstrap token 没有用户 ID 和 grant，因此 `tribunalctl projects list` 返回空列表是正常的。管理面列出全部已登记项目用 `tribunalctl projects all`。用户和授权管理命令：

```bash
tribunalctl users list
tribunalctl projects grants list <project-id>
tribunalctl projects grants set <project-id> <user-id> read
tribunalctl projects grants set <project-id> <user-id> manage
tribunalctl projects grants delete <project-id> <user-id>
tribunalctl users disable <user-id>
tribunalctl users enable <user-id>
tribunalctl users revoke <user-id>
```

这些用户和授权命令需要平台管理员凭据；用户自己的 API token 只用于其已有项目 grant 的业务访问。首次授权前，目标用户需要先通过 OAuth 登录一次，才能在 `users list` 中得到 GitLab 用户 ID。`users disable` 会同时撤销该用户的全部会话和 API token；`users revoke` 只撤销凭据，不禁用账号。已有部署升级后所有用户默认没有 project grant，包括平台管理员；迁移和撤销步骤见 [部署与运维](docs/operations.md#项目授权用户停用和凭据撤销)。

## 入口与队列

| 入口 | 当前行为 |
|---|---|
| MR open / reopen / update（带 oldrev） | 入队 `review.run`；Draft/标签/目标分支在 hook 或 worker 合并仓库配置后过滤 |
| MR close / merge | 入队 `review.cancel` |
| MR 评论 `/review`、`/review full`、`/review <profile>`、`/review cancel` | 先入队 `review.command`，worker 检查机器人身份、MR 状态和 Developer 以上权限，再入队目标任务 |
| 目标分支 push | 入队 `workspace.refresh`；分支删除不入队 |
| 项目注册 | 原子写入项目、初始工作区和 `workspace.onboard` 任务 |
| API 重新发布 | 入队 `review.publish` |

Webhook 不调用 GitLab 或模型。认证失败返回 401；其他情况返回 200，并通过 `accepted` 或 `ignored` 区分结果。缺少或非法的 `X-Gitlab-Event-UUID` 会被忽略。数据库失败返回 `ignored: storage_error`，不会假报已接收；按当前设计，HTTP 200 也意味着 GitLab 不会自动重试这次失败，需监控日志并重放事件。

启用仓库配置（`allow_repo_config=true`）时，Draft 的最终过滤在 worker 读取目标分支配置后完成，webhook 保持无网络调用；标签只允许仓库追加排除，项目级早期过滤仍安全；目标分支由 worker 按实际 MR 重新确认。自动评审、裸 `/review` 和未带 profile 的手动 API 优先采用仓库 `profile`；显式命令或显式 API profile 优先。

`GET /api/v1/projects` 按当前用户的项目授权过滤；静态 bootstrap bearer 或没有 grant 的账号会得到空列表。管理面的 `/api/v1/admin/projects`、`tribunalctl projects all` 返回全部登记项目，用于选择授权对象。`projects`、`users`、`grants`、`api-tokens`、`mcp-servers`、`skills`、`block-bindings` 列表使用游标分页，默认 50、最大 200。`tribunalctl projects list`、`tribunalctl projects all` 和 `tribunalctl users list` 会自动遍历所有页；只调用一次并假定返回全量列表的旧客户端现在只会得到第一页（最多 50 条），必须循环 `next_cursor`。

UUID 在项目内去重，去重记录和入队在同一事务中提交。同一 key 最多保留一个 running 和一个 queued 任务；新 webhook 或通过权限检查的评审命令替换待执行任务的内容，不覆盖正在执行的内容。同一 key 的任务不会并行领取。完成、失败及续租均校验 worker、attempt 和租约，过期 worker 无法覆盖新执行结果。替换 payload 不重置 attempt，以免旧执行编号重新生效。

服务关闭时取消正在执行的 handler；尚未结算的任务保留 running，重启后待租约到期恢复，默认最多等待约 5 分钟。

`review.run`、`review.cancel`、`review.command`、`review.publish`、`workspace.onboard` 和 `workspace.refresh` 均已注册处理器。`/review help` 和权限不足由 command worker 回复一条说明；每条回复带该源 note ID 的隐藏 marker，重试时更新同一条回复，不重复创建。受理 run/cancel 时，入队事务记录源 note 的 eyes，终态由状态 worker 协调；help/拒绝不添加表情。webhook 仍不调用 GitLab，也不添加 emoji。目标分支模式采用 Go `path.Match`，不提供递归 `**` 语义。

## 验证

```bash
cd web && npm test && npm run build && cd ..
go build ./...
go vet ./...
go test ./...
go test -race ./...
go mod verify
```

`go build` 依赖 `web/dist`，因此在全新检出上必须先执行前端构建。测试使用临时 SQLite 数据库和本地 HTTP 假 GitLab，覆盖注册、加密、认证、事件过滤、并发去重、租约恢复、旧 attempt 拒绝、CLI、工作流、发布、工作区、扩展和控制台静态资源。

已验证快照 `f143e7a` 的完整本地检查通过：`go build ./...`、`go vet ./...`、`go test -race -count=1 ./...`；语句覆盖率 77.09%（17665/22916）；`go mod verify`、65 个模块的 `licensecheck`、许可证 bundle `-check`；Web 27 个测试文件、160 个测试及 `npm run build`；Responses race 迭代 20 次通过，`dist` 未变。

实际镜像 `crc-v1-acceptance:candidate-f143e7a` 已构建并通过配置检查；离线恢复和在线替换均 PASS，保留用户、授权、会话、工作区、任务、工作流和 integrity。隔离实例完成 OAuth/项目授权、CSRF、跨项目 404、viewer 写 403、升级会话保留、SSE 撤权关闭、webhook 归属保护，以及真实 GitLab Note Hook、状态 note/表情、commit status 和应用新增行行内评论。GitLab 回读（`final-gitlab-before.json`）显示 `ai-review` status 52895/52896 在 `bae78e9980348c621cc0e1132e21b1656d44f212` 上为 failed；命令源 note 100746/100748 为 x、无 eyes，summary 100734 与 status note 100733 各一条；不同随机运行的 finding 集合和行内 diff key 可能不同，不能据此说所有运行只有一条行内评论。幂等重放/重复发布结果以 PR #30 记录为准，本文不预判。连续两次实际无诊断代理 full deep 均为 0 call errors、0 failed nodes；MCP 成功/调用分别为 10/10 和 10/11（第二次一次模型工具错误已纠正），两家 provider 均实际调用 MCP；不能声称所有工具调用成功。两次均使用 `deepseek-flash` 与 `gpt-6-astra`，完成 plan/fan-out/cross-verify/synthesis，结果 BLOCKING/published；BLOCKING 是合成授权回归的预期结论，不是验收失败。详细计数、summary 与原始响应由主任务在 PR #30 中维护，本文不复制。

旧快照的本地 review ACCEPT 不覆盖新的 Responses header。较早评审为 0 code blockers、evidence BLOCKING（缺 f143 运行时 manifest 与 `12a62d1` 文档 manifest/source-equivalence）；缺口已关闭，最终独立 review 对 `12a62d1` 文档树 PASS（code/documentation，0 blockers）。文档澄清只关联 PR #30，不构成新 review；native CI、平台评审、合并和 Release 尚未完成，以 PR #30 回读为准。

更早的 Docker、远程测试机和浏览器记录属于历史候选，不再作为已验证快照的通过证据；也不再重复其逐日日志。

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

**范围外 / 不构成本次 v1 发布门槛**

生产规模、负载和计费、物理设备/完整 WCAG、Postgres、其他协议/provider、每个 MCP HTTP backend 的具体实现不在本次 v1 范围；最终 commit、CI、平台评审和 Release 回读以 [PR #30](https://github.com/LeXwDeX/Tribunal/pull/30) 为准。以上均不新增为本次 v1 发布条件。

**已知限制**

- SSE 通过每 2 秒轮询数据库产生事件，不是推送。
- 统计按筛选范围读取 workflow 和节点，并在内存中保存耗时样本计算 P95；`/providers` 的近 24 小时统计也复用该完整聚合。未做生产规模压测，大数据量下的响应时间与内存占用仍待测量，不能据本地测试声称满足容量目标。
- 数据库中的平台/项目 MCP、Skill 和绑定变更按进程内缓存生效，通常需要重启服务。
- 数据库 MCP 配置在保存时保留 `${NAME}` 模板，在启动、项目解析和测试时展开；缺失或禁止的变量、未知字段会拒绝执行。MCP 测试始终使用当前记录的配置和作用域，不按同名平台实例复用。相对工作区路径会规范化；项目删除会清理项目工作区和受管技能目录，并拒绝通过根或父级符号链接删除外部目录。
- 数据保留清理已实现（`internal/app/retention_service.go`，默认每 6 小时）：`llm_calls`、`webhook_deliveries`、终态 `jobs` 按配置保留期删除，`0005_retention_indexes.sql` 为这些扫描增加索引；`context_docs` 对每个 `(project, kind)` 保留最近 `storage.context_versions` 个非 active 且未锁定版本，active 版本和所有 locked 版本不删除。
- 上下文文档保存采用追加版本：每个 `(project, kind)` 只有一个 active 版本。人工保存会追加 active 版本；生成任务在当前 active 版本 locked 时写入新版本但不切换；同一 base 和 workflow 的重试复用已有版本。数据库是 source of truth，人工保存和锁定会同步工作区上下文文件与 metadata；`workspaces.context_sha` 只跟踪 agents。
- 仓库级 `.gitlab-review.yml` 在评审 worker 中按目标分支合并。`allow_repo_config=true` 时 Draft 由 worker 最终判断；标签只追加排除；目标分支不能被仓库配置覆盖。手动命令不受自动 Draft/标签/分支过滤限制。
- 后台 run cleanup 在启动后立即执行、之后每小时运行。它先读取项目列表、保护集合和上下文 SHA 集合，再进入项目文件：删除超过 `workspace.run_cleanup_after` 且未被未终态 workflow、queued/running job、`publication_state=pending` 的 dossier 或本进程注册 run 引用的 `runs/<id>`；同时删除超过同一阈值、未被任何 `context_docs` 记录或当前 workspace/未终态/待发布 workflow base 保护的 `context/<sha>`。metadata 标记 locked 或 metadata 缺失/非法时保留并 fail closed。项目列表查询失败时整轮不删除；某个项目的保护查询失败时跳过该项目。缺失 mirror 时只有通过全部检查的 run 才删除；其他 Git 失败保留目录。
- 浏览器会话持久化在 SQLite，cookie 保存随机 token 及其 HMAC 签名；固定 `TRIBUNAL_SESSION_KEY`（旧名 `CRC_SESSION_KEY` 仍兼容）时重启后未过期会话仍有效。未设置该变量时每进程随机生成 key 并告警，重启后旧会话失效。浏览器会话和 API token 的 `admin`/`viewer` 标签每请求按 `bootstrap_admins` 计算，重启并移除管理员配置后平台管理权限同步降级；项目业务数据由 `project_grants` 独立控制，默认无权限，平台管理员也不例外。静态 `REVIEW_ADMIN_TOKEN` 保持平台管理权限，但没有用户 ID 和项目授权。
- 项目列表返回 `workspace` 和 `last_review`，项目详情返回 `workspace` 和最多 20 条 `recent_reviews`，并保留原有扁平项目字段；列表当前按项目逐项补充摘要（每个项目 2 次读取），分页默认 50、最大 200。评审详情的 `mr_title` 来自已存储 dossier；`publications.commit_status` 只从 `status:<sha>:<state>` 发布键解析 `success`/`failed`，旧键没有状态时视为未知。
- 反馈采集写入 `thumbsup` / `thumbsdown` 与 `resolved_*` signal。讨论只采集发布者首条 note 的 award，不把 thread reply 的表情当作对 finding 的反馈；汇总评论的 award 保持未关联 finding。`stats.feedback` 返回原始计数 `{total, by_signal, by_provider}`，按项目和 `observed_at` 的 `[from,to)` 过滤；归因只统计能按持久化 `feedback.finding_id` 精确匹配 finding 且 publication 一致的 discussion。summary、unlinked 和关联不一致的信号仍计入原始总数；同一 signal 可同时计入提出方和挑战方，两个角色的计数不能相加当作总数。它们不自动修改人工标注。顶层 `overturned_ratio` 是规则裁决中 `drop` 的占比，不是挑战推翻率或误杀率。
- 人工标注仅保留最新结论，采用最后一次成功写入，不提供历史审计或并发编辑冲突提示；清除时一并清空操作者和时间。质量指标按 workflow 创建时间筛选，但使用当前标注，因此旧时间窗的结果可随更正变化。去重后的 finding 每行只归给保存的提出方，挑战方按同一 workflow 的 `challenge_node` 关联；缺失 provider 的角色不计入该角色统计。样本可能存在人工选择偏差，不代表模型总体效果；标注不改变发布结果或 verdict。
- 覆盖缺口是硬完成条件：`DiffSummary.files` 只表示进入材料快照的文件数，不是已评审数；`omitted` 是容量截断或 GitLab 未提供 diff 等原因，`ignored` 是用户规则排除，两者不同。存在 omitted、评审节点实际遇到的提示词截断、完成 plan 后未分配的非 generated included 文件或上游证据不足时，最终结论为 `INCOMPLETE`；全部 omitted 不建 workflow，只发布 `INCOMPLETE` 摘要，列出 omitted 总数、原因计数和最多 4 条路径，其余只计数；正常 dossier 保留完整的 omitted 列表。全部被 ignore 时发布「无需评审」。
- workflow token 预算是共享上限，按 `input_tokens + output_tokens` 计算；cache read/write 和 reasoning 在 usage 中分别表示，但不额外相加，避免与输入/输出重复计数。每次模型请求前检查已提交用量加当前 attempt 已知 partial usage；并发或已发出的请求仍可能在检查后完成并产生有界超支，不能当精确账单。失败重试的已知 usage 计入下一次尝试的预算和最终节点用量；预算不足保留已有结果，按 required/optional 和确定性兜底处理，不无提示 PASS。
- `synthesize` 的预算/传输失败直接走确定性规则兜底，不重试；其他 agent 节点仍对 transport 错误重试。
- 共识投票和 challenger 选择按配置的 endpoint + model 身份归一，同端点同模型的 provider 别名不增加独立票数。无法解析身份的多个 provider 共用同一个 unknown bucket：known 与 unknown 可各计一票，unknown 之间不增加独立票；metadata 缺失会限制挑战者独立性，不能据此声称跨模型确认。没有独立 challenger 时使用已声明的 fallback 并显式提示。`CodeRef` 反证在固定 head/base 快照中检查路径和行号，缺失快照或读取失败降为 `UNCERTAIN`。
- 评审执行状态和 GitLab 发布状态分开保存：`publication_state` 为 `pending`、`published`、`skipped` 或 `failed`，失败原因有界脱敏保存。pending 在重启后恢复；stale、取消和关闭 failure note 记为 skipped；永久发布失败需手动 republish。`INCOMPLETE` 和 `BLOCKING` 都不会写成 commit status success。
- Web 依赖为 `react-router-dom 7.18.4`、`vite 7.3.7`、`vitest 4.1.12` / `@vitest/coverage-v8 4.1.12`。Vitest 4 改用 AST 重映射 V8 覆盖率，阈值现为语句 33%、分支 28%、函数 27%、行 35%；升级前旧记录中的 41.86% 与当前数字不可直接比较。
- 统计页项目筛选使用游标分页，每次加载 100 项，可继续加载或重试；使用 `usePaginated` 的分页列表的“重试加载更多”沿用失败请求的同一 cursor，首屏重试或“刷新”才从第一页重新加载。统计日期筛选把结束日映射到次日本地午夜，服务端按 `[from,to)` 计数。
- 统计页、总览和项目详情均把 `stats.runs` 标为“运行次数”，并说明包含评审和 onboarding。`blocking_ratio` 的分母仍是全部 workflow，因此 onboarding 会稀释该比例，不应把它解释为仅评审的阻断比例。
- 节点 provider/model 表示启动时选择的配置，不等于模型调用次数或响应实际模型标识；reasoner 自跳过和 agent synthesize 回退仍可能带归属。修复前历史节点的空归属不回填，因此旧时间窗的按 provider 节点统计仍不完整；逐次调用信息可查 `llm_calls`。
- 许可证发布清单是单一文件 `licenses/THIRD_PARTY_NOTICES.txt`，由 `go run ./scripts/licensebundle` 生成，CI 用 `-check` 验证。内容覆盖 Go 标准库、Linux/amd64 服务端的 40 个模块和 9 个 production npm 包；主机模块清单是 42 个，与运行时清单不同。当前 Dockerfile 仍复制遗留的 `THIRD_PARTY_LICENSES`，但发布声明以新单文件为准。
- 实际镜像 `crc-v1-acceptance:candidate-f143e7a`（ID `sha256:1a20aa96065a24986990de94d0e2e6466df04a379ed2e6b50f076271ff52574e`）已构建并通过配置检查；更早的 Docker 和远程测试机记录仍属历史。
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

最终 commit、CI、平台评审和 Release 回读以 [PR #30](https://github.com/LeXwDeX/Tribunal/pull/30) 为准。

本次文档修正没有运行构建、测试、真实服务或 CI；上述验证数字来自 f143 快照记录。最终 commit、CI、平台评审和 Release 回读以 [PR #30](https://github.com/LeXwDeX/Tribunal/pull/30) 为准。
