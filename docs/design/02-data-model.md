# 02 数据模型

> 以 SQLite 方言给出 DDL，Postgres 差异见 §4。迁移文件放在 `internal/store/migrations/NNNN_name.sql`，按序号执行。
> 时间字段存 RFC 3339 UTC 字符串；JSON 字段存 TEXT（Postgres 用 JSONB）。

---

## 1. 表一览

| 表 | 聚合 | 说明 |
|---|---|---|
| `projects` | 项目 | 注册信息、加密 token、项目级设置 |
| `workspaces` | 项目 | 工作区状态 |
| `context_docs` | 项目 | 生成或人工维护的上下文文档版本，按 `(project, kind)` 维护 active 版本 |
| `mcp_servers`、`skills`、`block_bindings` | 扩展 | MCP、Skill 注册与按块授权 |
| `webhook_deliveries` | 入口 | webhook 去重 |
| `jobs` | 入口 | 持久化任务队列 |
| `review_controls`、`workflow_dossiers` | 入口/恢复 | 持久化评审代次和脱敏材料快照（0003） |
| `workflows`、`workflow_nodes`、`events` | 工作流 | DAG 读模型与事件日志 |
| `findings` | 评审结果 | 统一 finding、挑战结果、裁决 |
| `publications` | 发布 | 已发布到 GitLab 的对象，用于幂等 |
| `llm_calls` | 审计 | 每次模型调用 |
| `feedback` | 反馈 | 👍/👎、resolve 信号 |
| `users`、`sessions`、`api_tokens` | 访问控制 | 本地用户、停用状态、持久会话与 CLI token |
| `project_grants` | 访问控制 | 每项目 `read` / `manage` 授权，默认无权限（0007） |

---

## 2. DDL（SQLite）

```sql
-- 最终结构：0001_init.sql 与 0003–0009 迁移后的主要表；0003 和 0007/0008/0009 的表/列见下文说明

CREATE TABLE projects (
  id                  INTEGER PRIMARY KEY,          -- GitLab project_id
  path_with_namespace TEXT    NOT NULL,
  web_url             TEXT    NOT NULL,
  default_branch      TEXT    NOT NULL,
  token_enc           BLOB    NOT NULL,             -- AES-GCM: nonce || ciphertext
  token_expires_at    TEXT,
  token_scopes        TEXT    NOT NULL DEFAULT '[]',-- JSON 数组，注册时从 GitLab 读取
  webhook_secret_enc  BLOB    NOT NULL,
  hook_id             INTEGER,                      -- 本服务管理的 webhook ID
  webhook_state       TEXT    NOT NULL DEFAULT 'disabled', -- disabled|pending|enabled|failed|unknown
  webhook_events      TEXT    NOT NULL DEFAULT '[]',-- JSON 数组，如 ["push_events",...]
  webhook_error       TEXT    NOT NULL DEFAULT '',  -- 有界脱敏错误，不含上游响应体或凭据
  profile             TEXT    NOT NULL DEFAULT 'default',
  settings            TEXT    NOT NULL DEFAULT '{}',-- JSON，结构见 06-config.md §3
  enabled             INTEGER NOT NULL DEFAULT 1,
  created_at          TEXT    NOT NULL,
  updated_at          TEXT    NOT NULL
);

CREATE TABLE workspaces (
  project_id      INTEGER PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
  state           TEXT    NOT NULL,   -- new | onboarding | ready | refreshing | error
  base_sha        TEXT,
  index_sha       TEXT,               -- 外部索引对应的 sha
  context_sha     TEXT,               -- agents 当前生效上下文 base sha
  last_refresh_at TEXT,
  last_error      TEXT,
  updated_at      TEXT    NOT NULL
);

CREATE TABLE context_docs (
  id          TEXT    PRIMARY KEY,
  project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  base_sha    TEXT    NOT NULL,
  kind        TEXT    NOT NULL,       -- agents | claude
  source      TEXT    NOT NULL,       -- generated | manual
  content     TEXT    NOT NULL,
  locked      INTEGER NOT NULL DEFAULT 0,
  model       TEXT,
  workflow_id TEXT,
  created_at  TEXT    NOT NULL,
  updated_by  INTEGER,                -- users.id，人工编辑时
  active      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_context_docs_base
  ON context_docs (project_id, base_sha, kind, created_at, id);
CREATE INDEX idx_context_docs_kind
  ON context_docs (project_id, kind, created_at, id);
CREATE INDEX idx_context_docs_workflow
  ON context_docs (project_id, base_sha, kind, workflow_id);
CREATE UNIQUE INDEX idx_context_docs_active
  ON context_docs (project_id, kind) WHERE active = 1;

CREATE TABLE mcp_servers (
  id         TEXT    PRIMARY KEY,
  scope      TEXT    NOT NULL,        -- platform | project
  project_id INTEGER REFERENCES projects(id) ON DELETE CASCADE,
  name       TEXT    NOT NULL,
  config     TEXT    NOT NULL,        -- 单个 mcpServers 条目的 JSON（Claude Code 格式）
  enabled    INTEGER NOT NULL DEFAULT 1,
  created_at TEXT    NOT NULL,
  updated_at TEXT    NOT NULL,
  UNIQUE (scope, project_id, name)
);

CREATE TABLE skills (
  id          TEXT    PRIMARY KEY,
  scope       TEXT    NOT NULL,       -- platform | project
  project_id  INTEGER REFERENCES projects(id) ON DELETE CASCADE,
  name        TEXT    NOT NULL,
  description TEXT    NOT NULL,
  dir         TEXT    NOT NULL,       -- 服务器上的目录
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  TEXT    NOT NULL,
  UNIQUE (scope, project_id, name)
);

CREATE TABLE block_bindings (
  scope      TEXT    NOT NULL,        -- platform | project
  project_id INTEGER NOT NULL DEFAULT 0,
  block      TEXT    NOT NULL,        -- code-review | review | reasoner | verify | synthesize | onboarding
  mcp_tools  TEXT    NOT NULL DEFAULT '[]',
  skills     TEXT    NOT NULL DEFAULT '[]',
  PRIMARY KEY (scope, project_id, block)
);

CREATE TABLE webhook_deliveries (
  uuid        TEXT    NOT NULL,       -- X-Gitlab-Event-UUID，按项目去重
  project_id  INTEGER NOT NULL,
  event       TEXT    NOT NULL,       -- Merge Request Hook | Note Hook | Push Hook
  outcome     TEXT    NOT NULL,       -- enqueued | ignored | rejected
  reason      TEXT,
  job_id      TEXT,
  received_at TEXT    NOT NULL,
  PRIMARY KEY (project_id, uuid)
);

CREATE TABLE jobs (
  id          TEXT    PRIMARY KEY,
  kind        TEXT    NOT NULL,
  key         TEXT,
  payload     TEXT    NOT NULL,
  state       TEXT    NOT NULL,       -- queued | running | done | failed | cancelled
  attempts    INTEGER NOT NULL DEFAULT 0,
  max_attempts INTEGER NOT NULL DEFAULT 3,
  run_after   TEXT    NOT NULL,
  lease_owner TEXT,
  lease_until TEXT,
  last_error  TEXT,
  created_at  TEXT    NOT NULL,
  updated_at  TEXT    NOT NULL
);
CREATE INDEX jobs_claim ON jobs (state, run_after);
-- 运行期间的新事件保留为一个排队后继，不被旧任务去重丢弃。
CREATE UNIQUE INDEX jobs_active_key ON jobs (key, state)
  WHERE key IS NOT NULL AND state IN ('queued', 'running');

CREATE TABLE workflows (
  id           TEXT    PRIMARY KEY,
  kind         TEXT    NOT NULL,      -- review | onboarding
  project_id   INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  mr_iid       INTEGER,
  head_sha     TEXT,
  base_sha     TEXT,
  fingerprint  TEXT,
  profile      TEXT,
  trigger      TEXT    NOT NULL,      -- mr_open | mr_update | command | api | push | register
  triggered_by INTEGER,               -- GitLab user id（命令或 API 触发时）
  status       TEXT    NOT NULL,      -- pending | running | completed | failed | cancelled
  graph_rev    INTEGER NOT NULL DEFAULT 0,
  extends_used INTEGER NOT NULL DEFAULT 0,
  verdict      TEXT,                  -- PASS | BLOCKING
  cancel_reason TEXT,
  error        TEXT,
  tokens_in    INTEGER NOT NULL DEFAULT 0,
  tokens_out   INTEGER NOT NULL DEFAULT 0,
  cost_micros  INTEGER NOT NULL DEFAULT 0,
  created_at   TEXT    NOT NULL,
  started_at   TEXT,
  finished_at  TEXT
);
CREATE INDEX workflows_mr ON workflows (project_id, mr_iid, created_at DESC);
CREATE INDEX workflows_status ON workflows (status, created_at DESC);
CREATE UNIQUE INDEX workflows_active_review
  ON workflows (project_id, mr_iid)
  WHERE kind = 'review' AND status IN ('pending', 'running');

CREATE TABLE workflow_nodes (
  workflow_id TEXT    NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
  id          TEXT    NOT NULL,
  kind        TEXT    NOT NULL,
  status      TEXT    NOT NULL,
  required    INTEGER NOT NULL,
  depends_on  TEXT    NOT NULL DEFAULT '[]',
  params      TEXT    NOT NULL DEFAULT '{}',
  attempt     INTEGER NOT NULL DEFAULT 0,
  provider    TEXT,
  model       TEXT,
  output      TEXT,                   -- 通过 schema 校验后的 JSON
  error       TEXT,
  tokens_in   INTEGER NOT NULL DEFAULT 0,
  tokens_out  INTEGER NOT NULL DEFAULT 0,
  cost_micros INTEGER NOT NULL DEFAULT 0,
  added_rev   INTEGER NOT NULL DEFAULT 0, -- 由哪次 graph_rev 加入（fan_out / extend）
  queued_at   TEXT,
  started_at  TEXT,
  finished_at TEXT,
  PRIMARY KEY (workflow_id, id)
);

CREATE TABLE events (
  workflow_id TEXT    NOT NULL,
  seq         INTEGER NOT NULL,       -- 每个工作流内自增，等于写入后的 graph_rev
  type        TEXT    NOT NULL,
  payload     TEXT    NOT NULL,
  at          TEXT    NOT NULL,
  PRIMARY KEY (workflow_id, seq)
);

CREATE TABLE findings (
  id                 TEXT    PRIMARY KEY,
  workflow_id        TEXT    NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
  origin_node        TEXT    NOT NULL, -- 产生它的节点；主 agent 自提为 synthesize 节点
  origin_index       INTEGER NOT NULL, -- 在该节点输出 findings 数组中的下标
  origin_provider    TEXT,
  severity           TEXT    NOT NULL, -- P0 | P1 | P2 | info（归一化后）
  category           TEXT    NOT NULL,
  path               TEXT,
  line               INTEGER,
  side               TEXT,             -- new | old
  title              TEXT    NOT NULL,
  body               TEXT    NOT NULL,
  evidence           TEXT,
  confidence         TEXT,             -- high | medium | low
  dedupe_group       TEXT,             -- 合并后的组 ID
  votes              REAL    NOT NULL DEFAULT 1, -- 拓扑 B 的加权票数
  challenge_status   TEXT,             -- confirmed | refuted | uncertain | skipped
  challenge_node     TEXT,
  challenge_evidence TEXT,
  decision           TEXT,             -- publish | drop | mark | downgrade
  decision_reason    TEXT,
  final_severity     TEXT,
  position_ok        INTEGER,          -- I2：行号是否落在 diff 内
  publish_key        TEXT,             -- 跨次运行去重：hash(path, 规范化标题, 行附近内容)
  annotation         TEXT NOT NULL DEFAULT '', -- '' | valid | invalid | uncertain，API 与 store 校验
  annotated_by       TEXT,             -- 认证用户的 username；配置 Bearer 为 api
  annotated_at       TEXT              -- 服务端 UTC 时间；清除标注时为 NULL
);
CREATE INDEX findings_wf ON findings (workflow_id);

CREATE TABLE publications (
  id          TEXT    PRIMARY KEY,
  project_id  INTEGER NOT NULL,
  mr_iid      INTEGER NOT NULL,
  kind        TEXT    NOT NULL,       -- summary | discussion | commit_status | failure_note
  publish_key TEXT    NOT NULL,       -- summary 固定为 'summary'
  gitlab_id   TEXT,                   -- note_id / discussion_id
  workflow_id TEXT    NOT NULL,
  fingerprint TEXT    NOT NULL,
  body_hash   TEXT,
  created_at  TEXT    NOT NULL,
  updated_at  TEXT    NOT NULL,
  UNIQUE (project_id, mr_iid, kind, publish_key)
);

CREATE TABLE llm_calls (
  id            TEXT    PRIMARY KEY,
  workflow_id   TEXT    NOT NULL,
  node_id       TEXT    NOT NULL,
  turn          INTEGER NOT NULL,
  provider      TEXT    NOT NULL,
  model         TEXT    NOT NULL,
  request       BLOB,                 -- gzip(JSON)，保留期可配置
  response      BLOB,                 -- gzip(JSON)
  input_tokens  INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read    INTEGER NOT NULL DEFAULT 0,
  cache_write   INTEGER NOT NULL DEFAULT 0,
  latency_ms    INTEGER NOT NULL,
  stop_reason   TEXT,
  error_kind    TEXT,
  request_id    TEXT,
  created_at    TEXT    NOT NULL
);
CREATE INDEX llm_calls_node ON llm_calls (workflow_id, node_id, turn);

CREATE TABLE feedback (
  id             TEXT    PRIMARY KEY,
  project_id     INTEGER NOT NULL,
  mr_iid         INTEGER NOT NULL,
  publication_id TEXT    NOT NULL,
  finding_id     TEXT,
  signal         TEXT    NOT NULL,    -- thumbsup | thumbsdown | resolved_changed | resolved_unchanged
  gitlab_user_id INTEGER,
  observed_at    TEXT    NOT NULL,
  UNIQUE (publication_id, signal, gitlab_user_id)
);
```

> `observed_at` 是最近一次采集到该 signal 的时间，不是不可变的创建时间。重复采集同一 `(publication_id, signal, gitlab_user_id)` 会更新该列，因此 `stats.feedback` 按 `project_id` 和 `observed_at` 的 `[from, to)` 计数时，统计的是最近观察时间落入窗口的信号，而不是首次看到这些信号的时间。空范围返回 `total=0`、`by_signal={}` 的已知零值；旧响应缺少 `feedback` 字段才表示不可用。该计数只是原始反馈，不是模型准确率；人工结论另存于 `findings.annotation`。
>
> `finding_id` 是归因到具体 finding 的唯一依据；只有 publication 能解析出同一 workflow/project/MR 下已持久化 finding 时才写入。汇总评论的 award 和无法解析的 discussion award 保持为空。`stats.feedback.by_provider` 按 `{provider, role, total, by_signal}` 返回：`proposer` 使用 `findings.origin_provider`，`challenger` 使用 `findings.challenge_node` 对应节点的 provider；只统计 `finding_id` 能精确连接 `findings.id` 的 discussion 反馈，summary/unlinked 不计入这组归因，但仍计入 `total`/`by_signal`。同一 signal 可以同时出现在两个角色中，因此这些是归因计数，不是准确率或误杀率，也不能把两个角色相加当作反馈总数。

```sql
CREATE TABLE users (
  id            INTEGER PRIMARY KEY,  -- GitLab user id
  username      TEXT    NOT NULL,
  name          TEXT    NOT NULL,
  role          TEXT    NOT NULL,     -- 平台标签 admin | viewer；不是项目授权
  last_login_at TEXT,
  disabled      INTEGER NOT NULL DEFAULT 0  -- 0007 添加；停用不恢复旧凭据
);

CREATE TABLE project_grants (
  project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role       TEXT    NOT NULL CHECK (role IN ('read', 'manage')),
  PRIMARY KEY (project_id, user_id)
);
CREATE INDEX project_grants_user ON project_grants (user_id, project_id);

CREATE TABLE sessions (
  id         TEXT    PRIMARY KEY,     -- cookie token 的 SHA-256 hex；明文 token 不存库
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at TEXT    NOT NULL,
  created_at TEXT    NOT NULL
);
```

> 当前浏览器会话持久化在 `sessions` 表，沿用 `0001_init.sql`，没有新增迁移。cookie 保存随机 token 及其 HMAC 签名；数据库只保存 token 的 SHA-256。CSRF token 由会话 token 和 `CRC_SESSION_KEY` 按 HMAC 域派生，不单独存储。固定 `CRC_SESSION_KEY` 时，未过期会话在重启后仍可用；未设置时每个进程生成随机 key 并告警，重启后旧 cookie 失效。浏览器请求的 `admin`/`viewer` 标签每次按当前 `auth.bootstrap_admins` 计算；读取会话时要求 `users.disabled=0`。停用用户的 SSE 在下一轮授权检查时关闭。
>
> `project_grants` 是项目业务数据访问的唯一来源。默认没有行；平台管理员也不会自动获得 grant。`read` 可读取项目、评审、工作区、统计、SSE 和项目扩展，`manage` 还可执行项目写操作。无 grant 或项目不存在统一返回 404，`read` 执行写操作返回 403。删除 grant 会立即影响列表、详情、统计和已打开的 SSE；停用用户还会删除全部会话和 API token。

> API token 的 `admin`/`viewer` 标签与浏览器会话一致，在每次请求时按当前 `auth.bootstrap_admins` 计算。`users.role` 仍记录 OAuth 登录或 token 签发时的标签，但不作为已有 token 的项目权限来源；项目访问始终读取当前 `project_grants`。重启并更新管理员配置后，该用户已有的 API token 在下一个请求失去平台管理标签；静态 `auth.bootstrap_token_env` Bearer 保持平台管理标签，但没有用户 ID 和 grant。新签发的 API token 默认 90 天、最长 365 天；历史 `expires_at IS NULL` 的行可继续使用到撤销。`0007_project_access.sql` 为现有用户补 `disabled=0`，不创建 grant；升级后包括管理员在内都需显式授权。

```sql
CREATE TABLE api_tokens (
  id           TEXT    PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         TEXT    NOT NULL,
  token_hash   BLOB    NOT NULL,      -- SHA-256；明文只在创建时显示一次
  last_used_at TEXT,
  expires_at   TEXT,
  created_at   TEXT    NOT NULL
);
```

> `0003_review_state.sql` 另建 `review_controls`（每 MR 的 generation、head 和取消意图）和 `workflow_dossiers`（脱敏后的 gzip 材料快照，`job_id` 唯一）。它们用于取消旧评审和崩溃恢复，详见迁移文件。

> `0004_context_revisions.sql` 重建 `context_docs`，删除旧的 `UNIQUE (project_id, base_sha, kind)`，增加 `active` 和上述索引。迁移保留旧 `rowid` 以免相同 `created_at` 的版本顺序变化；把 `workspaces.context_sha` 对应的 agents 版本设为 active，claude 没有匹配版本时取该 kind 的最新版本。保存上下文文档会追加版本：人工保存总会成为 active；生成保存只有在当前 active 版本未锁定时才切换，同一 base 和 workflow 的重试返回已有版本。`GetCurrentContextDoc` 优先读取 active，没有 active 时回退到最新版本。

> `0005_retention_indexes.sql` 为保留清理增加索引：`llm_calls (created_at, id)`、`webhook_deliveries (received_at)`，以及只覆盖终态 job 的 `jobs (updated_at, id) WHERE state IN ('done','failed','cancelled')`。清理按这些 keyset 列分批删除，使每批按索引顺序扫描并在 LIMIT 后停止，同时保留 queued/running 任务。

> `0006_finding_annotations.sql` 为 `findings` 增加上述三个人工标注字段。旧行默认为未标注；只有标注写接口修改这些字段，producer 传入的值被忽略。`ReplaceFindings` 在同一事务中保留相同 ID 的人工字段，移除不再出现的 finding；跨 workflow ID 冲突或重复 ID 导致整次替换失败。标注为最新状态，不存修订历史，也不会按 `publish_key` 自动转移到其他 workflow。

> `0008_publication_state.sql` 为 `workflow_dossiers` 增加 `publication_error`。发布状态独立于 workflow 执行状态，取值为 `pending`、`published`、`skipped` 或 `failed`；失败原因是有界、脱敏文本，不含上游响应正文或凭据。`skipped` 可表示 stale、取消/被新代次替换或关闭 failure note；`failed` 是永久发布失败，需要手动 republish。

---

## 3. 事件 payload

事件类型前缀统一为 `dag.`，payload 为 JSON。

| 事件 | payload |
|---|---|
| `dag.workflow.created` | `{spec: GraphSpec, meta}` |
| `dag.workflow.started` | `{}` |
| `dag.workflow.extended` | `{nodes: [NodeSpec], source: "fan_out" \| "extend", from_node}` |
| `dag.workflow.completed` | `{}` |
| `dag.workflow.failed` | `{reason, node_id?}` |
| `dag.workflow.cancelled` | `{reason}`（如 `superseded_by:<wf_id>`、`user`、`deadline`） |
| `dag.node.registered` | `{node_id}`（随 created / extended 批量写入） |
| `dag.node.queued` | `{node_id, attempt}` |
| `dag.node.started` | `{node_id, attempt, provider?, model?}` |
| `dag.node.completed` | `{node_id, attempt, output, usage}` |
| `dag.node.failed` | `{node_id, attempt, error, error_kind}` |
| `dag.node.aborted` | `{node_id, attempt, reason}` |
| `dag.node.skipped` | `{node_id, reason}`（`condition_false` \| `deps_skipped` \| `workflow_cancelled`） |
| `dag.node.cancelled` | `{node_id, reason}`（被 extend 替换） |
| `dag.node.restarted` | `{node_id, next_attempt}`（崩溃恢复） |

规则：

- 写事件和更新读模型（`workflows`、`workflow_nodes`）在**同一个数据库事务**里完成。
- `events.seq` 即写入后的 `graph_rev`；`Append` 时带 `expectRev`，不一致返回 `ErrStaleRevision`。
- 迟到的节点结果（attempt 不等于当前 attempt）直接丢弃，并记 debug 日志。

---

## 4. Postgres 差异

| SQLite | Postgres |
|---|---|
| `INTEGER PRIMARY KEY`（project_id） | `BIGINT PRIMARY KEY` |
| `BLOB` | `BYTEA` |
| 时间 `TEXT`（RFC 3339） | `TIMESTAMPTZ` |
| JSON `TEXT` | `JSONB` |
| `REAL` | `DOUBLE PRECISION` |
| 部分索引 `WHERE ...` | 相同语法 |

SQLite 必要的 PRAGMA：`journal_mode=WAL`、`busy_timeout=5000`、`foreign_keys=ON`、`synchronous=NORMAL`。写操作走单连接或串行化，读用连接池。

---

## 5. 保留与清理

> 实现状态：定时清理已实现（`internal/app/retention_service.go`，默认每 6 小时）。`llm_calls` 超过保留期时删除整行，而不只是清空 payload；项目注销时的关联删除属于注销路径，不是保留策略。`0005_retention_indexes.sql` 为下表的三类删除扫描提供索引。

| 数据 | 默认保留 | 配置 | 清理方式 |
|---|---|---|---|
| `llm_calls` | 30 天 | `storage.llm_call_retention` | 定时删除整行 |
| `webhook_deliveries` | 14 天 | `storage.delivery_retention` | 定时删除 |
| `jobs`（done/failed/cancelled） | 7 天 | `storage.job_retention` | 定时删除；queued/running 永不删除 |
| `events`、`workflows`、`findings` | 长期 | — | 不清理；按需归档 |
| `context_docs` | 每个 `(project, kind)` 保留最近 5 个非 active 且未锁定的历史版本；active 版本和所有 locked 版本永不删除 | `storage.context_versions` | 定时按 `(project_id, kind)` 窗口清理，每批最多 500 行 |

> 本地验收口径（2026-10-23 用户确认）：维持上表策略。每个 `(project, kind)` 保留 5 个未锁定且非 active 的历史版本；active 版本和所有 locked 版本额外保护，总数可以超过 5。不扩大删除范围。

> 工作区文件清理与数据库保留是两条路径。run cleanup 在每小时任务中删除超过 `workspace.run_cleanup_after`（默认 1h）的 `context/<sha>` 目录，但必须保留所有 `context_docs` 记录引用的 base SHA、workspace 的 `base_sha`/`context_sha`、未终态 workflow 的 base，以及 `publication_state=pending` dossier 的 base。目录 metadata 标记 locked、缺失或非法时保留该目录并记录错误（fail closed）。只有查询保护集合成功后，才允许触碰项目文件。
