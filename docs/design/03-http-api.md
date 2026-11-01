# 03 HTTP API

> 服务对外提供三类接口：GitLab webhook、管理 API（Web 控制台和 `tribunalctl` 共用）、健康检查。
> 管理 API 前缀 `/api/v1`，请求和响应都是 JSON（SSE 除外）。

> 主体接口已有本地实现，覆盖 webhook、健康检查、OAuth 持久会话、管理 API 和 API token，并补充项目/评审摘要与 `stats.feedback`（含 `by_provider` 归因）。`auth.bootstrap_token_env` 指向的管理员 Bearer token 仍可用于平台管理脚本，但不授予项目业务数据访问；浏览器登录需要配置 `auth.oauth`。已验证运行时候选/源码快照 `f143e7a7835e1e99345b1df9b289bb499e3c67416598d30e6184a558374eb0ac`（402 文件）的隔离实例覆盖 OAuth/项目授权、CSRF、webhook 归属保护、GitLab Note Hook/状态 note/commit status/应用新增行行内评论、升级恢复和 SSE 撤权关闭；连续两次无诊断代理 deep 均 0 call errors/0 failed nodes（MCP 10/10、10/11）。最终 commit/CI/Release 事实以 [PR #30](https://github.com/LeXwDeX/Tribunal/pull/30) 为准。实现差异见正文对应小节。

---

## 1. 通用约定

### 认证

| 调用方 | 方式 |
|---|---|
| GitLab webhook | `X-Gitlab-Token` 与项目的 webhook secret 比对（常量时间比较） |
| Web 控制台 | GitLab OAuth 登录后下发会话 cookie（`HttpOnly`、`Secure`、`SameSite=Lax`）；写操作要求 `X-CSRF-Token` |
| `tribunalctl` / 脚本 | `Authorization: Bearer <api_token>`；bootstrap Bearer 仅用于平台管理面 |

平台角色只有 `admin` / `viewer`，由当前 `auth.bootstrap_admins` 计算：`id:<n>` 条目按 GitLab 用户 ID 匹配；用户名条目在首次以 admin 身份出现时绑定到当时的 GitLab 用户 ID（`admin_bindings`），之后同名但 ID 不同的账号只得到 `viewer` 并记录告警；`users.role` 只记录签发时标签，不作为已有 token 的权限来源。平台管理员可管理用户、项目授权、项目注册和系统配置，但不会自动获得项目业务数据权限。

项目业务数据由本地 `project_grants` 的 `read` / `manage` 决定，平台管理员也必须被显式授权。无 grant 或项目不存在统一返回 404；只有 `read` 时执行写操作返回 403。列表、统计、SSE、上下文和项目扩展在查询或流式读取时按 grant 过滤；删除 grant 会让已有 API token 失去该项目访问并关闭已打开的 SSE，停用用户会撤销全部会话和 API token。

`auth.bootstrap_token_env` 指定的静态 Bearer 只用于平台管理面：没有本地用户 ID，也没有任何项目 grant，不能读取项目业务数据。它的轮换由部署方更新环境变量并重启；`bootstrap_admins` 变化同样在重启后生效。

浏览器会话保存在 SQLite `sessions` 表，cookie 保存随机 token 及其 HMAC 签名，数据库只保存 token 的 SHA-256。CSRF token 由会话 token 和 `TRIBUNAL_SESSION_KEY` 派生，不单独存储。固定 `TRIBUNAL_SESSION_KEY`（旧名 `CRC_SESSION_KEY` 仍兼容，新名优先）时，重启后未过期会话仍有效；未设置时进程内随机生成 key 并输出告警，旧 cookie 在重启后失效。

### 错误格式

```json
{ "error": { "code": "not_found", "message": "project 42 not registered", "request_id": "01J…" } }
```

| HTTP | code |
|---|---|
| 400 | `bad_request`、`validation_failed`（附 `details` 字段数组） |
| 401 | `unauthenticated`（无 session cookie，或 Bearer 凭据无效）；`session_expired`（无 Authorization 且带 session cookie，但会话不可用） |
| 403 | `forbidden` |
| 404 | `not_found` |
| 409 | `conflict`（如工作流已在运行、项目已注册） |
| 422 | `gitlab_rejected`（GitLab 返回错误，附原始状态码） |
| 500 | `internal` |
| 503 | `unavailable`（依赖未配置，或服务端内部超时且客户端仍在连接） |

### 分页

列表使用游标分页：`?limit=50&cursor=<opaque>`，响应带 `next_cursor`（没有更多时为空）。`limit` 默认 50，范围为 1–200；超出范围返回 `400`。当前覆盖评审、finding、节点调用、上下文文档，以及 projects、api-tokens、mcp-servers、skills、block-bindings，还包括管理面的 users、admin projects 和 grants。

上述管理列表的 cursor 绑定列表类型和筛选条件：projects 绑定 `q` / `enabled` 和访问范围，API token 绑定当前用户，扩展列表绑定 `scope` / `project_id`，users/grants 绑定各自列表类型和项目。跨列表、跨用户、跨访问范围或改变筛选后续用 cursor 返回 `400 bad_request`，不能复用。

兼容性：只请求一次列表的旧客户端现在只会看到第一页（最多 50 条），必须循环 `next_cursor` 才能取全。`tribunalctl projects list` 已自动遍历所有页；对没有 `next_cursor` 的旧服务，它按单页处理。

Web 控制台的“重试加载更多”复用失败请求的同一 cursor，不会跳到下一页；“刷新”会重新加载已加载页（从第一页开始）。首屏失败的重试同样从第一页发起。

---

## 2. Webhook

### `POST /webhooks/gitlab`

请求头：`X-Gitlab-Event`、`X-Gitlab-Token`、`X-Gitlab-Event-UUID`、`X-Gitlab-Instance`。

处理顺序：

1. 读取 body（上限 5 MB），解析出 `project.id`。
2. 查项目；未注册或已禁用 → `200 {"ignored":"project_not_registered"}`。
3. 校验 `X-Gitlab-Token`；不通过 → `401`。
4. `X-Gitlab-Event-UUID` 缺失或非法时忽略；同一项目内已存在于 `webhook_deliveries` → `200 {"ignored":"duplicate"}`。
5. 按事件类型过滤（见下表），命中则入队，返回 `200 {"accepted":true,"job_id":"…"}`；否则 `200 {"ignored":"<reason>"}`。

认证失败返回 401；请求体读取返回超时错误时返回 `503 {"ignored":"request_timeout","request_id":...}`，未读取完整的请求不会写投递记录或入队。存储处理失败返回 `503 {"ignored":"storage_error","request_id":...}`。请求体超限和其他非超时读取错误分别返回 `200 {"ignored":"body_too_large",...}` 和 `200 {"ignored":"bad_request",...}`；输入错误、未登记项目、重复投递等也返回 200。处理超时预算为 900 毫秒，不做任何 GitLab 或 LLM 调用；读取失败时保留读取期限并要求关闭连接，客户端断连时响应未必送达。

| 事件 | 条件 | 入队 |
|---|---|---|
| `Merge Request Hook` | `action` 为 `open` / `reopen` | `review.run`，trigger=`mr_open` |
| `Merge Request Hook` | `action=update` 且带 `oldrev` | `review.run`，trigger=`mr_update` |
| `Merge Request Hook` | `action` 为 `close` / `merge` | `review.cancel` |
| `Merge Request Hook` | `draft=true` 且项目设置跳过 Draft；`allow_repo_config=true` 时延迟到 worker 最终判断 | 忽略：`draft`（或在 worker 过滤） |
| `Note Hook` | `noteable_type=MergeRequest` 且正文以 `/review` 开头 | 按命令入队（见 §6） |
| `Push Hook` | `ref` 为目标分支（默认分支或设置中的分支） | `workspace.refresh` |

`review.run` 的 job key：`review:<project_id>:<mr_iid>`。去重记录与入队在同一事务中提交，新 webhook 就地替换同 key、同 kind 的 queued 内容；running 内容不变，允许保留一个 queued 后继。取消任务使用独立 key `review.cancel:<project_id>:<mr_iid>`。工作流取消由评审处理器负责（见 05-flows.md F2）；webhook handler 本身不执行评审。

Note Hook 先入队 `review.command`，由 worker 请求 GitLab 校验身份、MR 状态和权限后入队 run/cancel；这样 webhook handler 不等待网络调用。通过校验的 run 命令使用最新参数替换已有 queued run，保留 `full`、`profile` 和最新 `head_sha`；不覆盖 running 任务。`/review help` 和权限不足由 worker 各回复一条 GitLab note；回复正文以 `<!-- gitlab-ce-review:command-reply note=<note_id>` 隐藏 marker 开头，按源 note 幂等，重试时更新同一条回复。受理 run/cancel 时，入队事务记录源 note 的 eyes，终态由状态 worker 协调；help/拒绝不添加表情。Webhook 自身仍无网络调用，也会忽略机器人自己发出的 command-reply note。数据库失败返回 `503 {"ignored":"storage_error"}`，不会写入去重记录或假报 accepted。不得假定 GitLab 自动重试；记录仍可用且版本支持时可在「Recent events」手动重发。如果重发保留原有效事件 UUID、此前没有去重记录且认证和过滤通过，则可再次入队。连续失败可能使 hook 临时或永久停用，条件随版本和功能开关而异，见 [Webhook 失败与恢复](../operations.md#webhook-失败与恢复)。

自动触发（`mr_open` / `mr_update`）在 worker 合并配置后做最终 Draft、标签和目标分支过滤。`allow_repo_config=true` 时，Draft 检查不在 webhook 做，避免 webhook 调用 GitLab；标签仍可先用项目设置快速过滤，仓库配置只能追加排除；目标分支条件不被仓库配置覆盖。手动命令不受这些自动过滤限制。

---

## 3. 认证与当前用户

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/auth/status` | 公开；`{oauth_enabled}`，供控制台决定是否显示登录按钮 |
| GET | `/auth/login` | 跳转 GitLab OAuth（scope：`read_user`）；未启用时 API 客户端收到 503，浏览器跳回 `/?auth_error=oauth_disabled` |
| GET | `/auth/callback` | 回调；创建或更新 `users`，写会话 |
| POST | `/auth/logout` | 删除会话 |
| GET | `/api/v1/me` | `{id, username, name, role, csrf_token}`；CSRF 由会话派生，不保存到 localStorage |
| GET | `/api/v1/api-tokens` | 当前用户的 token 列表：`?limit=&cursor=`，响应 `{tokens, next_cursor}`，不含明文 |
| POST | `/api/v1/api-tokens` | `{name, expires_in_days}` → `{id, token}`，明文只返回这一次 |
| DELETE | `/api/v1/api-tokens/{id}` | 吊销 |

浏览器导航请求（`Accept: text/html`）的登录和回调失败会以 303 跳回控制台，查询参数 `auth_error` 使用固定代码（`oauth_disabled`、`oauth_denied`、`oauth_state`、`oauth_forbidden`、`oauth_failed`）；API 客户端仍收到原有 JSON 错误。`api-tokens` 三个接口都要求浏览器会话；静态 bootstrap Bearer 和其他 bearer 客户端不能创建、列出或删除用户 token。新 token 的 `expires_in_days` 默认 90，范围 1–365；历史无过期时间的 token 可继续使用到撤销。普通用户只能管理自己的 token，不能停用账号或撤销他人凭据。`/api/v1/me` 只返回平台标签，不返回项目授权；项目访问角色由项目或评审响应中的 `access` 字段给出。

---

## 4. 管理 API

### 4.1 项目

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/projects` | 当前用户已授权的项目列表：`?q=&enabled=&limit=&cursor=`，响应 `{projects, next_cursor}`，每项带 `access` |
| POST | `/api/v1/projects/validate` | 平台管理员只读连接校验：`{project, token}`；不保存项目或凭据 |
| POST | `/api/v1/projects` | 平台管理员注册项目 |
| GET | `/api/v1/projects/{id}` | 项目 `read`：详情（含工作区状态、最近评审和 `access`） |
| PATCH | `/api/v1/projects/{id}` | 项目 `manage`：修改 `profile`、`settings`、`enabled` |
| PUT | `/api/v1/projects/{id}/token` | 项目 `manage`：轮换 token：`{token}` |
| POST | `/api/v1/projects/{id}/webhook` | 项目 `manage`：显式启用 GitLab webhook；只复用仍存在的已记录 `hook_id`，沿用已存 secret |
| POST | `/api/v1/projects/{id}/webhook/rebuild` | 项目 `manage`：轮换 secret 并重建 GitLab webhook；只复用仍存在的已记录 `hook_id`，没有可复用 hook 时创建 |
| DELETE | `/api/v1/projects/{id}/webhook` | 项目 `manage`：停用 webhook，只删除已记录的 service hook，保留 secret 供再次启用 |
| DELETE | `/api/v1/projects/{id}` | 项目 `manage`：注销并删除 webhook（若由本服务创建）、清理工作区 |

`GET /api/v1/projects` 是业务列表，按当前用户的项目授权过滤；平台管理员或静态 bootstrap Bearer 没有 grant 时得到空列表。选择授权对象时使用 §4.6 的 `/api/v1/admin/projects` 管理列表。列表中的 `access` 为 `read` 或 `manage`；授权在分页读取后被撤销的项目不会出现在结果中。

`project` 支持正整数 ID、`namespace/path` 或配置的 GitLab 根地址下的完整 HTTP(S) 仓库 URL，可带 `.git` 后缀和尾斜杠。拒绝外域、URL 用户信息、query/fragment 和路径穿越；不会按用户输入连接任意主机。注册和连接校验要求平台管理员；项目详情读取需要 `read`，项目设置写入、Key 轮换、webhook 启用/重建/停用和注销需要 `manage`，不要求平台管理员。

只读校验依次确认项目身份、当前用户及至少 Reporter 的项目权限、`api` 和 `read_repository` scope、有效期，以及 `git ls-remote ... HEAD`。全程默认限时 30 秒。成功返回 `200 {project_id, path_with_namespace, web_url, default_branch, username, access_level, token_scopes, token_expires_at?, git_head}`；未配置可信 GitLab 根地址或 Git 探针时返回 503。校验不落库、不创建 webhook、不入队、不调用模型；读取 HEAD 不等于完整 clone 或评论写权限验证。注册仍独立复核凭据，不依赖先前校验结果。

注册请求：

POST /api/v1/projects

```json
{
  "project": "group/backend-api",
  "token": "glpat-…",
  "profile": "default",
  "auto_webhook": true,
  "settings": { "skip_draft": true, "target_branches": ["main", "release/*"] }
}
```

服务端校验步骤：

1. 用 token 调 `GET /projects/:id` 确认可访问；读出 `default_branch`、`web_url`。
2. 调 `GET /personal_access_tokens/self` 读取 scope 和过期时间；缺少 `api` 或 `read_repository` → `400 validation_failed`。
3. `auto_webhook=true` 时直接调 `POST /projects/:id/hooks` 创建新 hook，不列出或复用已有 hook；需要 Maintainer。新 hook 使用期望 URL，勾选 Merge request、Comments、Push 事件并写入生成的 secret。注册流程不会修改或删除已有 hook，即使存在同 URL hook 也只创建新 hook。创建成功但入库失败时会补偿删除刚创建的 hook；创建失败则注册失败且不落库。
4. 入库后入队 `workspace.onboard`。

注册响应：`201 {project, job_id, webhook_secret?}`。`auto_webhook` 默认 false，此时只在注册响应中返回一次 `webhook_secret`，供手工配置 webhook，`webhook_state=disabled`；自动创建时不返回 secret，`webhook_state=enabled`。项目列表支持 `q` / `enabled` 筛选和游标分页；筛选值会绑定到 cursor。

项目 `webhook_state` / `webhook_events` / `webhook_error` 是安全字段，随项目详情和列表返回；secret 始终不返回。

- `disabled`：未启用自动 webhook。
- `pending`：显式启用已开始，GitLab 调用尚未完成。
- `enabled`：本服务管理的 hook 已确认，`webhook_events` 为 `["push_events","merge_requests_events","note_events"]`。
- `failed`：最近一次启用或重建失败，`webhook_error` 为有界脱敏文本（不含上游响应体或凭据）。
- `unknown`：升级前已有 `hook_id` 的项目；hook 可能存在但状态未验证。

`POST /api/v1/projects/{id}/webhook` 是显式幂等 provisioning：先将状态置为 `pending`，列出 hook 后只按持久化的 `hook_id` 查找已记录 hook；URL 不参与归属判断。已记录 hook 仍存在时，沿用已存 secret 并更新为期望的 URL 和事件；已记录 hook 已被外部删除时创建新 hook。同 URL 但未记录的 hook 从不复用、修改或删除。返回 `200 {project, hook_id, state, events, last_error, managed_by?, created_at?, updated_at?}`（`last_error` 为空字符串表示无错误），secret 永不返回。创建后本地持久化失败会补偿删除新建 hook；失败状态不报告为 `enabled`。重复启用不会重复创建 hook。

升级迁移 `0012_project_webhook_ownership.sql` 写入的持久化归属值仍为 `crc`；它是刻意保留的数据兼容值，不随产品改名，也不能用产品名替换。

`POST /api/v1/projects/{id}/webhook/rebuild` 使用同一归属规则并轮换 secret：已记录 hook 仍存在时更新为新 secret 和期望 URL/事件，已被外部删除时创建新 hook。`DELETE /api/v1/projects/{id}/webhook` 只删除已记录的 service hook 并把状态置为 `disabled`；上游返回 404 视为已停用。停用保留加密 secret 供再次启用。这些接口都不会修改或删除用户自建且未被记录的 hook。Key 轮换不隐式创建或修改 hook。

项目详情保留项目行的全部扁平字段，并附加 `access`、`workspace`（无工作区记录时省略）和 `recent_reviews`（仅 `kind=review`，按创建时间最新优先，最多 20 条）。项目列表同样保留扁平项目字段，并附加 `access`、`workspace` 和 `last_review`（仅最新一条 review，缺失时省略）。当前列表在分页结果上为每个项目额外读取工作区和最近评审（2 次/项目，属于 N+1）；分页默认 50、最大 200。

### 4.2 评审（工作流）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/reviews` | 列表：`?project_id=&mr_iid=&status=&verdict=&publication_state=&profile=&trigger=&since=&limit=&cursor=`；所有筛选和项目授权过滤在服务端分页前应用；每项附 `nodes` 与 `findings_summary` |
| POST | `/api/v1/projects/{id}/merge_requests/{iid}/reviews` | 项目 `manage` 手动触发：`{profile?, full?}` → `202 {job_id}` |
| GET | `/api/v1/reviews/{wf_id}` | 工作流详情：基本信息 + `access` + 节点 + finding 统计 + 发布状态 |
| GET | `/api/v1/reviews/{wf_id}/graph` | 节点和边，供 DAG 视图渲染 |
| GET | `/api/v1/reviews/{wf_id}/findings` | finding 列表：`?decision=&severity=&limit=&cursor=`；severity 按非空 `final_severity` 筛选，未裁决时回退原始 `severity`，与界面和汇总一致 |
| PUT | `/api/v1/reviews/{wf_id}/findings/{finding_id}/annotation` | 项目 `manage` 人工标注：`{annotation}`，返回完整 Finding |
| GET | `/api/v1/reviews/{wf_id}/nodes/{node_id}` | 节点详情：参数、输出、错误 |
| GET | `/api/v1/reviews/{wf_id}/nodes/{node_id}/calls` | 该节点的 LLM 调用：`?include_payload=&limit=&cursor=` |
| GET | `/api/v1/reviews/{wf_id}/stream` | SSE：节点状态变化、finding 新增、发布结果 |
| POST | `/api/v1/reviews/{wf_id}/cancel` | 项目 `manage` 取消 |
| POST | `/api/v1/reviews/{wf_id}/republish` | 项目 `manage`：对 completed 工作流重新发布，或对 failed 工作流重新发布未完成摘要；fingerprint 失效仍会转为 skipped |

工作流详情示例：

```json
{
  "id": "0192f0c1-…",
  "project_id": 42,
  "mr_iid": 318,
  "mr_title": "feat: 支持批量导入",
  "head_sha": "9f3c2a1…",
  "profile": "deep",
  "trigger": "mr_update",
  "status": "completed",
  "verdict": "BLOCKING",
  "access": "manage",
  "publication_state": "published",
  "nodes": [
    { "id": "plan", "kind": "plan", "status": "completed", "provider": "claude", "duration_ms": 8120 },
    { "id": "review.std.p1", "kind": "review.standards", "status": "completed", "provider": "claude" },
    { "id": "verify.f7", "kind": "verify", "status": "completed", "provider": "gpt" }
  ],
  "findings_summary": { "published": 4, "marked": 1, "dropped": 3, "by_severity": { "P0": 0, "P1": 2, "P2": 3 } },
  "usage": { "tokens_in": 182340, "tokens_out": 12011, "cost_micros": 0 },
  "publications": { "summary_note_id": 99102, "discussions": 4, "commit_status": "failed" },
  "created_at": "…", "finished_at": "…"
}
```

SSE 事件（`event:` 字段）：`node`（`{id, status, attempt}`）、`finding`（`{id, decision}`）、`workflow`（`{status, verdict, publication_state?, publication_error?}`）、`published`（`{kind, gitlab_id}`）。`workflow` 事件同时传播执行状态和发布状态；只有在 workflow 终态且 `publication_state` 为 `published`、`skipped`、`failed` 或空（旧数据）时，服务端才结束流。每轮轮询都重新认证并检查项目 grant；撤权、停用或 token 失效会关闭已打开的流。

`mr_title` 从该工作流已存储的 dossier 快照读取；没有 dossier、数据为空或标题为空时省略，不在详情请求中调用 GitLab。`publications.commit_status` 从成功发布写入的 `status:<head_sha>:<state>` 发布键解析，只接受 `success` 或 `failed`；旧格式 `status:<head_sha>` 没有状态记录，此时字段缺失（unknown），不根据 verdict 猜测。SSE 仍由服务端每 2 秒轮询数据库生成，不是事件推送。

评审列表响应为 `{reviews, next_cursor}`。每项保留工作流的扁平字段和 `duration_ms`，并附加 `nodes` 与 `findings_summary`，两者的形状和口径与详情完全一致：`nodes` 每项含 `id`、`kind`、`status`、`depends_on`（无依赖时省略），以及 `provider`、`attempt`、`duration_ms`，尚无节点时为 `[]`；`findings_summary` 的 `published`/`marked`/`dropped` 按 `decision` 计数，`by_severity` 按非空 `final_severity`、否则原始 `severity` 计数，并始终包含 `P0`、`P1`、`P2`、`info` 四个键。每项还带 `mr_title`：它在保存 dossier 时从 `mr.title` 去掉首尾空白后写入 `workflow_dossiers.mr_title`，列表只读这一列，不解压 dossier，也不请求 GitLab；没有 dossier、标题为空或是迁移 `0016` 之前保存的评审时省略该字段，控制台回退显示 `!iid`。列表不返回 `access`、`usage`、`publications`。服务端在已授权过滤并分页的结果上，按本页全部工作流 ID 各执行一次节点查询、一次 finding 分组计数查询和一次标题查询（不按行 N+1）；空页不发起这三次查询。

```json
{
  "reviews": [
    {
      "id": "0192f0c1-…", "project_id": 42, "mr_iid": 318, "status": "running",
      "nodes": [
        { "id": "plan", "kind": "plan", "status": "completed", "provider": "claude", "attempt": 1, "duration_ms": 8120 },
        { "id": "review.std.p1", "kind": "review.standards", "status": "running", "depends_on": ["plan"], "attempt": 1 }
      ],
      "findings_summary": { "published": 0, "marked": 0, "dropped": 0, "by_severity": { "P0": 0, "P1": 0, "P2": 0, "info": 0 } },
      "created_at": "…"
    }
  ],
  "next_cursor": ""
}
```

`GET /api/v1/reviews` 的 `publication_state` 只接受 `pending`、`published`、`skipped`、`failed`，其他值返回 `400`；筛选在分页前应用。`publication_state` 独立于 `status`，取值为 `pending`、`published`、`skipped`、`failed`。`pending` 表示评审执行已结束但 GitLab 发布尚未完成，可在重启或手动重试后继续；`published` 表示汇总评论/失败摘要已成功写入；`skipped` 表示 stale（head、配置或 prompt 变化）、取消/被新代次替换，或关闭 failure note，不会自动重发；`failed` 是永久发布失败，需要手动 republish。`publication_error` 是有界、脱敏的失败或跳过原因，不含上游响应正文或凭据。发布状态与 GitLab commit status 不同：BLOCKING 的 `commit_status=failed` 是评审结论，不是发布失败；只有 PASS 会写 success，INCOMPLETE 也不会写 success。没有 dossier 的工作流和旧数据不返回 `publication_state`；界面把缺失值显示为“不表示发布成功”，不能当作 published。

图响应为 `{nodes, edges}`。节点包含 `id`、`kind`、`status`、`depends_on`，以及可选的 `provider`、`attempt`；边为 `{from, to}`。节点 ID 允许点号，必须作为完整路径参数编码，不能按点号拆分。节点详情直接返回对象，不增加 `node` 包装层。

Finding 始终返回 `annotation`：`""` 为未标注，`valid` 为有效，`invalid` 为无效，`uncertain` 为未确定。非空时附 `annotated_by`（认证用户名）和 `annotated_at`（服务端 RFC 3339 时间）。标注 PUT 仅接收 `{"annotation":"valid"}` 这样的单字段请求；缺失、`null`、非法值、未知字段或非法 JSON 返回 `400`，不属于所选 workflow 的 finding 返回 `404`。只有项目 `read` 的账号写入、其他项目或无项目 grant 返回 403/404；浏览器会话缺少 CSRF 返回 `403`。发送空字符串可清除结论及操作者/时间。支持更正，采用最后一次成功写入；不改变 finding 处置、发布或 workflow verdict。相同 ID 的重新发布保留标注，其他 workflow 不继承标注。

### 4.3 工作区与上下文文档

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/projects/{id}/workspace` | 状态、`base_sha`、`index_sha`、`context_sha`、最近刷新和错误 |
| POST | `/api/v1/projects/{id}/workspace/refresh` | `{regenerate_context?: bool, reindex?: bool}` → `202 {job_id}` |
| GET | `/api/v1/projects/{id}/context-docs` | 版本列表：`?kind=&limit=&cursor=` |
| GET | `/api/v1/projects/{id}/context-docs/current` | 当前生效版本（`?kind=agents`） |
| PUT | `/api/v1/projects/{id}/context-docs/current` | 人工编辑：`{kind, content, locked}`，生成一条 `source=manual` 的新版本 |
| POST | `/api/v1/projects/{id}/context-docs/current/lock` | `{locked: bool}` |

工作区状态、上下文版本和当前文档的读取需要项目 `read`；刷新、保存和锁定需要项目 `manage`。`current` 的读取、保存与锁定均用 `?kind=agents|claude` 选择文档，默认 `agents`。保存请求同时给出 `kind` 时，必须与查询参数一致。工作区和当前文档直接返回对象；文档列表用 `{documents, next_cursor}`。

当前实现语义：数据库是上下文版本的 source of truth；`PUT .../context-docs/current` 和 lock 接口会同步工作区 `context/<base>` 下的文档和 `meta.json`，同步失败返回 500，不报告成功。`workspaces.context_sha` 只跟踪 agents 的 active base。`PUT .../context-docs/current` 追加一个 `source=manual` 的 active 版本，旧版本内容和锁定状态保持不变。生成保存同样追加版本；当前 active 版本 locked 时，新生成版本保存但不切换。同一 base 和 workflow 的重试返回已有版本，不重复追加。`GET .../current` 优先返回 active 版本，没有 active 时回退到最新版本。工作区响应包含 `state`、`base_sha`、`index_sha`、`context_sha`、`last_refresh_at`、`last_error` 和 `index_status`。

工作区详情、项目详情和项目列表中的工作区 `last_error` 使用相同的固定消息，不返回存储的错误原文。空字符串或数据库 `NULL` 省略该字段；`state=ready` 且存储值以 `index: ` 开头时返回 `index: code graph indexing failed`；其他非空值返回 `workspace operation failed`。旧版索引错误使用该前缀，但历史值可能被伪造或无法准确分类，前缀不是索引失败的可信证明。读取不会改写数据库；正常刷新仍可能覆盖旧值。依赖旧版完整错误文本的客户端需要调整。

`index_status` 只描述已存储的索引检查点：两项 SHA 相同为 `ready`，不同为 `stale`，缺少任一项为 `unknown`。平台 MCP 配置为空不能证明项目级索引已禁用，因此不返回推测的 `disabled`。新 onboarding/refresh 只有实际成功执行至少一个 `index_command` 才推进 `index_sha`；没有命令时不伪造新 SHA，refresh 保留旧检查点。历史版本可能在未执行命令时写过 SHA，本次没有迁移重写，`ready` 不是当前外部索引健康或历史命令执行的证明。

手动 `POST .../workspace/refresh` 的两个布尔参数默认都是 `false`：空请求只更新 base，重建索引和重新生成上下文由界面的独立操作显式请求。目标分支 push 触发的刷新仍使用后台默认策略。项目扩展配置目前按项目缓存，已加载项目的 MCP/Skill/块绑定更新需重启服务才能可靠生效；缓存失效机制尚未实现。

### 4.4 扩展：MCP、Skill、块绑定

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/mcp-servers` | `?scope=&project_id=&limit=&cursor=`，响应 `{servers, next_cursor}` |
| POST | `/api/v1/mcp-servers` | `{scope, project_id?, name, config}`；`config` 为单个 Claude Code `mcpServers` 条目 |
| PUT / DELETE | `/api/v1/mcp-servers/{id}` | 修改 / 删除；内置服务器（`builtin: true`）只能修改 `enabled`，改名、改配置和删除返回 409 |
| POST | `/api/v1/mcp-servers/{id}/test` | 启动并列出工具：`{tools:[{name, description}]}` |
| POST | `/api/v1/mcp-servers/import` | 粘贴整份 `{"mcpServers": {...}}` 批量导入，整体在一个事务内完成，任一条失败则全部不写入；与内置服务器同名时返回 409 |
| GET | `/api/v1/skills` | `?scope=&project_id=&limit=&cursor=`，响应 `{skills, next_cursor}` |
| POST | `/api/v1/skills` | multipart 上传 zip（含 `SKILL.md`），或 `{scope, dir}` 登记服务器上的目录 |
| PATCH | `/api/v1/skills/{id}` | `{enabled: bool}` 启用或禁用；禁用后不加载，仍引用它的块绑定会跳过它 |
| DELETE | `/api/v1/skills/{id}` | 删除；内置 Skill（`builtin: true`）返回 409，只能禁用 |
| GET | `/api/v1/block-bindings` | `?scope=&project_id=&limit=&cursor=`，响应 `{bindings, next_cursor}`；每个已有绑定带 `revision` |
| PUT | `/api/v1/block-bindings/{block}` | `{scope, project_id?, mcp_tools: [], skills: [], expected_revision}`；`expected_revision` 必填 |
| POST | `/api/v1/block-bindings/restore-defaults` | 预览或恢复当前进程内置默认扩展对平台块绑定的授权；仅平台管理员 |

块绑定写入使用内容版本做乐观并发控制。`GET /api/v1/block-bindings` 返回的每个已有绑定都带 `revision`；它是 `scope`、项目 ID、`block`、`mcp_tools` 和 `skills` 的无歧义 SHA-256 摘要。`nil` 与 `[]` 等价，数组顺序和重复项参与计算。没有数据库行时使用字面值 `absent`；内容摘要不会等于该值。

`PUT /api/v1/block-bindings/{block}` 必须带 `expected_revision`，直接使用完整列表（不是当前分页）中同一 `scope`、`project_id`、`block` 行的 `revision`；没有该行时用 `absent`。字段缺失、`null`、空字符串、大写或不完整摘要等格式错误返回 `400`。基线过期，或另一个客户端先创建该行，返回 `409`，请求不写入；成功响应返回新 `revision`。服务端不提供无版本写入的兼容路径。比较与写入在同一个 SQLite 事务内完成，所有块绑定写入与恢复默认通过该事务串行；项目范围还会先取得项目 workspace 读锁，与项目删除串行。内容相同时重复保存保持同一 `revision`（事件 ABA 不另记版本）。

`scope=project` 的数据库行覆盖完整的项目级授权：没有行表示继承，保存空数组表示显式撤销该块的全部 MCP 工具和 Skill。平台 scope 没有数据库行时可能由配置文件 `blocks` 提供生效绑定；首次用 `absent` 保存平台空行会创建数据库覆盖，该块的文件声明不再生效。调用方必须先读取完整列表并确认这一影响，服务端不推断调用方是否只加载了部分页。

权限边界：`scope=project` 的读取需要该项目 `read` grant；`scope=platform` 或省略 scope 的全局列表只允许平台管理员。创建、导入、更新、删除、测试和块绑定写入都要求平台管理员；项目 scope 写入还必须同时具备该项目 `manage`。项目 `manage` 本身不是宿主执行权限，不能写平台配置或绕过平台管理员。MCP 删除/更新/测试和 Skill 删除/启停按资源已存储的 scope 重新检查，不信任请求里的 scope。

导入 MCP 时，查询参数指定 `scope`、`project_id`，body 直接使用 `{"mcpServers": {...}}`，不增加包装层。项目作用域必须有有效的项目 ID。MCP 更新允许只传 `name` 或 `enabled`；省略 `config` 会保留原配置，传入 `config` 则完整替换配置。列表、创建、更新和导入响应的 `config` 统一为 `{}`，任何角色都不能通过这些接口读回配置；不要把响应中的空对象当作原配置再次提交。旧客户端提交 env 值或敏感 header 的 `****` 时仍恢复已存值，没有对应旧值的占位项会移除；新客户端替换配置应提交完整原始模板。保存时对配置做严格语义校验：未知字段、非法值、越界字段和尾随 JSON 都返回 `400`；`${NAME}` 引用按模板加密保存，不在保存阶段展开环境变量，也不把解析值写入数据库。运行或测试时才展开。

项目级扩展解析使用按项目构建的合并配置：每个项目都有独立的 MCP manager，空绑定表示显式撤销该块的平台工具，不回退到平台工具；项目合并配置读取或构建失败会中止本次解析（返回错误），不会静默降级为仅平台配置。账号和扩展的数据库变更仍受进程内缓存影响，通常需要重启服务；当前没有可靠的跨进程缓存失效。

Skill zip 上传字段为 `file`，另带 `scope`、可选 `project_id`；名称和描述由服务端从 `SKILL.md` 读取。目录登记请求为 `{scope, project_id?, dir}`。创建成功直接返回 Skill 对象。

MCP、Skill、块绑定列表使用游标分页。Skill zip 上传已实现，请求体上限 8 MiB，归档内单文件上限 4 MiB、总解压上限 16 MiB、最多 512 个条目，`SKILL.md` 读取上限 64 KiB；目录登记同样受单文件和总量限制。

#### 恢复内置默认授权

`POST /api/v1/block-bindings/restore-defaults` 只允许平台管理员，和其他扩展写接口一样经过 cookie 会话的 CSRF 校验；Bearer 管理令牌不需要 CSRF。请求不携带授权列表，服务端只使用当前进程加载的 `default_extensions` 配置和存储中的内置资源身份计算恢复内容。

预览请求：

```json
{"blocks": ["review"], "dry_run": true}
```

`blocks` 省略或为空数组表示全部六个可绑定块。执行请求必须显式传 `dry_run: false`，并携带预览返回的 `preview_token`：

```json
{"blocks": ["review"], "dry_run": false, "preview_token": "<preview_token>"}
```

响应固定包含 `changes`、`preview_token` 和 `restart_required`。`changes` 按块名排序，每个块的 `add` / `remove` 数组排序，`status` 为 `change`、`unchanged` 或 `skipped`。`skipped` 的 `reason` 为 `file_binding`（块在配置文件中显式声明，包括空块）或 `no_registered_defaults`（没有登记为 applied 且内置身份一致的可恢复默认）。只有 ledger 状态变化、绑定内容不变时仍返回 `change`。`ownership_changed` 表示恢复是否创建或改写平台绑定/块快照，使其转为由默认扩展管理；仅更新 grant ledger 时为 `false`。

恢复只处理选中块中的平台级绑定。恢复范围是当前配置中登记为 applied、且存储行的 `builtin` 与默认名称一致的内置扩展。对每个选中块，先移除这些内置扩展在该块上的现有授权：内置 Skill 的同名条目和内置 MCP 的 `mcp__<server>__*` 条目；再按当前 `default_extensions` 对该块的声明补回默认授权。因此管理员手动加到未声明块（例如 `reasoner`、`synthesize`）的内置 Skill 或内置 MCP 工具也会被清除；其他服务器、自建 Skill 和未知授权保留。被移除授权的 ledger 记为已移除，后续配置重新授予该块时仍可自动跟随。目标块既没有平台行、也没有应授予的默认授权和 ledger 变化时，恢复不创建空行。

自动升级与显式恢复对旧 ledger 明细缺失的处理不同。自动升级无法从明细证明某个 grant 加入过哪些 MCP 工具时，会保留授权和 applied 状态，下次启动重试，不会声称已移除；内置 Skill 的 grant 恒为该 Skill 自身名称，因此升级可按名称安全移除。显式恢复不依赖旧 grant 明细：它按当前配置中已登记为 applied、且存储 resource 的 `builtin` 与名称一致的内置扩展，清理目标块上的内置 Skill 名和内置 MCP 前缀，再按当前块声明重新授予，预览的 `add` / `remove` 会列出实际变化。恢复不修改内置资源、启用状态、项目级绑定或其他块的绑定。若选中块没有任何已登记且身份匹配的内置扩展，返回 `skipped`，不会清空现有授权。

`preview_token` 是选中块、文件声明、平台绑定、相关 ledger、内置身份和启用状态，以及当前 `default_extensions` 对选中块的授权投影（默认名、种类、每块是否存在及具体 grants）的稳定指纹，不是认证凭据。另一个进程载入不同配置时旧 token 会失效。执行时在同一 SQLite 事务内重新读取并计算，指纹不一致返回 `409`，不覆盖新值。所有变更块和相关 ledger 在一个事务内原子写入；预览不写任何行。恢复后，相关块重新由默认扩展管理，后续升级会继续跟随默认授权的增加和删除。未被管理员改动的默认块在升级时自动跟随内置授权的增删；管理员改过的整块保持不变。移除整个默认扩展声明（例如 `default_extensions.skills: []`）不会删除已登记扩展或其绑定。

执行恢复只在平台绑定的内容或存在性改变时更新相关块的 `revision`；仅更新 ledger 或没有变化时 `revision` 不变。预览后到执行前，其他写入若改变 `preview_token` 覆盖的输入（相关平台绑定、ledger、内置身份或启用状态等），执行返回 `409`。执行成功且绑定变化后，旧 `revision` 不能再用于 PUT，保存中的草稿必须重新读取。

**恢复后需要重启服务**才能让运行时重新读取平台绑定，因此 `restart_required` 始终为 `true`。

### 4.5 系统

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/providers` | provider 列表（**不含** API key）及近 24 小时调用统计 |
| POST | `/api/v1/providers/{name}/test` | 发一条最小请求验证连通性和模型名 |
| GET | `/api/v1/profiles` | 评审档位定义（来自配置文件，只读） |
| GET | `/api/v1/stats` | `?from=&to=&project_id=`：运行次数（含 onboarding）、耗时、成本、项目/provider/块统计与 UTC 日趋势、finding 处置分布、人工质量指标、按模型调用与反馈原始计数；`to` 为排他上界 |
| GET | `/api/v1/settings` | 平台配置的只读视图（脱敏） |

首版 provider 和 profile 以配置文件为准，API 只读；改动需修改配置并重启服务。当前进程不处理 `SIGHUP` 热加载。`/providers`、`/profiles` 和 `/settings` 仅平台管理员可访问。

`/stats` 按当前用户的项目授权在聚合和分页前过滤；指定 `project_id` 时还要求该项目 `read`。平台管理员也只会看到自己已获授权项目的业务统计，平台管理标签不扩大项目数据范围。

控制台集合响应按资源命名：`{projects}`、`{reviews}`、`{findings}`、`{servers}`、`{skills}`、`{bindings}`、`{providers}`、`{profiles}`、`{calls}`、`{documents}`、`{tokens}`；可分页的集合另带 `next_cursor`。不提供未发布的旧字段别名。

`stats` 使用字段 `runs`、`blocking`、`blocking_ratio`、`published`、`overturned_ratio`、`avg_duration_ms`、`p95_duration_ms`、`duration_samples`、`cost_micros`、`trend`、`by_project`、`by_provider`、`by_block`、`by_model`、`decisions`、`quality_by_provider` 和 `feedback`。`runs` 统计窗口内全部 workflow（包括评审和 onboarding），不是仅评审次数；`blocking_ratio` 当前也是 `blocking / runs`，所以 onboarding 会稀释该比例。

运行统计按 workflow 的 `created_at` 落在 `[from, to)` 内筛选，所有维度都应用同一个项目筛选。每日趋势按该创建时间归入 UTC 日，不按完成时间或 LLM 调用时间分桶。日期控件使用本地日，因而一个本地日可能跨两个 UTC 日桶。

| 字段 | 每项内容与计数口径 |
|---|---|
| `trend` | `{day, runs, cost_micros, duration_samples, avg_duration_ms, p95_duration_ms}`；`day` 为 `YYYY-MM-DD`。顶层和项目的 `trend.runs` 是 workflow 数，provider/块内同形的 `trend.runs` 是已启动节点数 |
| `by_project` | `{project_id, runs, blocking, published, cost_micros, duration_samples, avg_duration_ms, p95_duration_ms, trend}`；按项目统计 workflow |
| `by_provider` | `{provider, runs, cost_micros, duration_samples, avg_duration_ms, p95_duration_ms, trend}`；只统计 `started_at` 存在且 provider 非空的节点 |
| `by_block` | `{block, runs, cost_micros, duration_samples, avg_duration_ms, p95_duration_ms, trend}`；按节点 `kind` 分组，包括没有 provider 的代码节点 |

provider/块的 `runs` 是当前保存的已启动节点行数，每行只计一次，不是 LLM 调用次数，也不是重试次数。运行中、完成和失败节点只要保留 `started_at` 都可计入；从未启动的排队或跳过节点不计入。节点字段只反映最新一次尝试，不包含完整重试历史。总体/项目成本取 workflow 的持久化成本，provider/块成本取节点成本，不因多轮 LLM 调用重复累加。

Anthropic 的输入 token 将未缓存输入、缓存读取和缓存创建各计一次；内部缓存读写字段是总输入的明细，不能再次相加。评审与节点的 `usage` 仍只含 `tokens_in`、`tokens_out` 和 `cost_micros`。评审 token 预算使用归一化后的输入和输出。成本使用配置的输入、输出单价，不按缓存读写分别计价，是估算而非供应方账单；未配置价格时估算为零。已保存的历史用量不会追溯回填。

耗时仅计入 `started_at`、`finished_at` 都存在且间隔非负的记录；总体/项目使用工作流耗时，provider/块使用节点耗时。`duration_samples` 是有效配对数，无样本时平均/P95 均为 `null`；P95 取排序后第 `ceil(0.95 * 样本数)` 个值。所有 `trend` 均为稀疏序列，只返回有记录的日，不补零。数组按项目 ID、provider/块名称、日期升序返回，已统计的空数组为 `[]`。旧响应缺少新增字段时，界面显示不可用而不是零。

`quality_by_provider` 按 provider 名称升序返回；空范围为 `[]`。按 workflow 的 `created_at [from,to)` 与 `project_id` 筛选，而非标注时间；使用当前人工结论，旧时间窗可随更正变化。每个去重后的 finding 只按保存的 `origin_provider` 计一次提出方；挑战方通过同一 workflow 的 `challenge_node = workflow_nodes.id` 取节点 provider。无法确定某个角色的 provider 时，不计入该角色；一个 provider 可以只有其中一类样本。

| 每行字段 | 定义 |
|---|---|
| `provider` | provider 名称，不按 model 版本再细分 |
| `proposed` | 归给该提出方的 finding 总数 |
| `valid` / `invalid` / `reviewed` | 人工有效数 / 无效数 / 两者之和；未标注和未确定不进入 `reviewed` |
| `accuracy` | `valid / reviewed`；零分母为 `null` |
| `overturned` / `overturned_ratio` | 该提出方被挑战判为 `refuted` 的数量 / 该数量除以 `proposed`；零分母为 `null` |
| `refuted` | 该挑战方判为 `refuted` 的 finding 数 |
| `refuted_reviewed` / `false_refuted` | 反驳条目中人工有效或无效的总数 / 其中人工判有效数 |
| `false_kill_rate` | `false_refuted / refuted_reviewed`；零分母为 `null` |

这些指标不会把点赞、点踩或 resolve 自动转为人工结论。界面同时展示分子、分母，零分母显示「无样本」。`quality_by_provider[].overturned_ratio` 是挑战推翻率，与顶层同名字段的规则丢弃比例不同；准确率/误杀率只描述已标注样本，不代表模型总体效果。

`feedback` 为 `{total, by_signal, by_provider}`，按 `project_id` 筛选，并按 `observed_at` 落在 `[from, to)` 内计数；`from` 和 `to` 是 RFC 3339 时间。Web 日期选择把开始日映射到本地零点，把结束日映射到次日本地零点（不是当日 23:59:59.999），因此选择同一天也覆盖完整一天。`observed_at` 在重复采集时会更新，所以窗口表示最近观察时间，不是首次创建时间。空范围返回 `total=0`、`by_signal={}`、`by_provider=[]` 的已知空值；旧响应缺少整个 `feedback` 字段时表示不可用，两者不能混同。

`feedback.by_provider` 的每项为 `{provider, role, total, by_signal}`，与顶层 `by_provider` 的运行统计不同；`role` 是 `proposer` 或 `challenger`。提出方取 `findings.origin_provider`，挑战方取该 finding 的 `challenge_node` 对应节点 provider；归因按持久化 `feedback.finding_id = findings.id` 精确连接，publication 必须是同一 workflow/project/MR 的 discussion。summary、未关联 finding 和关联不一致的反馈不计入 `feedback.by_provider`，但仍计入 `feedback.total`/`by_signal`。同一 signal 可以同时计入两个角色，两个角色的合计不是反馈总数。顶层 `overturned_ratio` 是规则裁决中 `drop` 占已处置 finding 的比例；`by_model` 只有调用数、错误数、token、成本和延迟。不能用原始反馈计数代替人工质量指标。无法计算的比例和耗时返回 `null`，不能伪装成零错误或零耗时。

比例字段（包括 provider 的 `error_rate`）统一为 `[0, 1]` 的小数，不混用百分数。`cost_micros` 是按配置价格估算的微美元（1 USD = 1,000,000 micros），不是 provider 账单核对结果。未配置价格时不可把估算零值解释为免费。

Provider 返回 `name`、`protocol`、`base_url`、`model`、`max_concurrency`、`rpm` 及 `stats_24h`（`calls`、`errors`、`error_rate`、`p50_latency_ms`），不返回 API key。连通性测试必须返回 `ok: boolean`，可附 `message`、`model`、`latency_ms`；缺少 `ok` 不能当作成功。

### 4.6 访问管理（平台管理员）

`/admin` 和 `/api/v1/admin` 提供相同处理，以下以 `/api/v1/admin` 记录。所有端点都要求平台 `admin` 标签；静态 bootstrap Bearer 可调用，但它没有用户 ID，也不会因此获得项目 grant。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/admin/users` | `?limit=&cursor=` → `{users, next_cursor}`；每项含 `id`、`username`、`name`、`disabled`、`last_login_at?` |
| PATCH | `/api/v1/admin/users/{id}` | `{disabled: bool}`；`disabled=true` 时在同一事务撤销该用户全部会话和 API token；启用不恢复旧凭据 |
| DELETE | `/api/v1/admin/users/{id}/credentials` | 只撤销全部会话和 API token，不禁用账号；`204` |
| GET | `/api/v1/admin/projects` | `?limit=&cursor=` → 全部已登记项目的管理视图 `{projects:[{id, path_with_namespace}], next_cursor}`；不含业务摘要 |
| GET | `/api/v1/admin/projects/residues` | 平台管理员：只读列出可归属到未登记项目的残留（项目 ID、路径、类型、是否可清理），不修改文件 |
| POST | `/api/v1/admin/projects/residues/cleanup` | 平台管理员：`{confirm:true}` 清理归属明确的残留；未知归属保留并报告 |
| GET | `/api/v1/admin/projects/{id}/grants` | `?limit=&cursor=` → `{grants:[{project_id, user_id, role, username?}], next_cursor}` |
| PUT | `/api/v1/admin/projects/{id}/grants/{user_id}` | `{role: "read"|"manage"}`，返回保存后的 grant |
| DELETE | `/api/v1/admin/projects/{id}/grants/{user_id}` | 删除一个 grant；`204` |

授权角色只有 `read` 和 `manage`，默认没有 grant。`admin projects` 只用于选择授权对象，不返回项目业务数据；平台管理员要读取项目详情、评审、统计或 SSE，仍必须有对应 grant。停用用户不删除 `users` 行或 grant；重新启用后可以再次登录，但已撤销的会话和 token 不会恢复。撤销凭据不禁用账号，用户仍可通过 OAuth 重新登录。只有平台管理员可以跨用户停用、撤销或管理 grant；普通用户只能管理自己的 API token。

---

## 5. 健康检查与指标

| 路径 | 说明 |
|---|---|
| `GET /healthz` | 进程存活 |
| `GET /readyz` | 数据库可用、job worker 已启动 |
| `GET /metrics` | Prometheus 指标（可选，默认关闭）：工作流数与耗时、节点失败率、LLM 延迟与 token、队列长度 |

---

## 6. MR 评论命令

在 MR 下发表以 `/review` 开头的评论：

| 命令 | 作用 |
|---|---|
| `/review` | 用项目默认 profile 重新评审（增量） |
| `/review full` | 全量评审 |
| `/review <profile>` | 指定档位，如 `/review deep` |
| `/review cancel` | 取消正在运行的评审 |
| `/review help` | 回复命令说明 |

权限：发起人在该项目的访问级别必须 **≥ Developer（30）**；否则 worker 回复一条说明并忽略命令，不创建评审任务。机器人自己的评论和命令回复 note 一律忽略，防止循环触发。`/review help` 回复可用命令和档位说明；两类回复都按源 note ID 使用隐藏 marker，重试幂等。受理 run/cancel 时通过入队事务记录源 note eyes；终态由状态 worker 协调为完成/失败标记或清除。help/拒绝不添加表情。
