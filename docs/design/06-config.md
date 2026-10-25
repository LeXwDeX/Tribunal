# 06 配置

> 配置分三层，后者覆盖前者：**平台配置文件** → **项目设置**（数据库 `projects.settings`，控制台可改）→ **仓库内 `.gitlab-review.yml`**（只读目标分支版本，且只能覆盖白名单内的字段）。

---

## 1. 平台配置文件

默认路径 `config/server.yaml`，可用 `--config` 指定，也可以传多个文件按顺序合并。字符串中的 `${ENV_NAME}` 在加载时展开；密钥类字段**只接受** `*_env` 形式，不允许明文写在文件里。

当前最小启动示例见 [`config/server.example.yaml`](../../config/server.example.yaml)，但它没有 `providers` 和 `profiles.*.propose`，只能启动服务、注册项目和跑路由，**不能执行真实评审**；要跑评审必须先补齐 provider、profile 的 `propose` / `challenge` 等配置。OAuth 可选：不配置时 `/auth/login` 返回 503，Bearer 仍可用。当前存储只实现 SQLite，配置 `postgres` 会在启动时明确报错。管理 API 使用 `auth.bootstrap_token_env`（默认 `REVIEW_ADMIN_TOKEN`，至少 32 字节），该静态 Bearer 仅用于平台管理面；浏览器登录、用户级 API token 和本地项目 `read`/`manage` 授权已实现。多文件加载递归合并映射、替换数组，拒绝未知字段、重复键和多文档 YAML；环境变量在解析后的字符串上展开，避免改变 YAML 结构。

```yaml
server:
  listen: ":8080"
  public_url: "https://review.example.internal"   # 用于 commit status 的 target_url、OAuth 回调
  read_timeout: 15s
  write_timeout: 60s
  trusted_proxies: ["10.0.0.0/8"]

gitlab:
  url: "https://gitlab.example.internal"
  request_timeout: 30s
  max_concurrency: 16               # 对 GitLab API 的总并发
  bot_usernames: []                 # 额外需要忽略的 bot 用户名（项目 bot 会自动识别）

auth:
  oauth:
    client_id_env: GITLAB_OAUTH_CLIENT_ID
    client_secret_env: GITLAB_OAUTH_CLIENT_SECRET
  bootstrap_admins: ["alice"]       # GitLab 用户名；仅平台 admin 标签，重启后生效，项目数据仍需 grant
  session_ttl: 168h                 # 持久化浏览器会话有效期

storage:
  driver: sqlite                    # sqlite | postgres
  dsn: "file:data/review.db"
  llm_call_retention: 720h
  delivery_retention: 336h
  job_retention: 168h
  context_versions: 5

secrets:
  master_key_env: REVIEW_MASTER_KEY # 32 字节，base64 编码

workspace:
  root: "data/workspaces"
  git_bin: "git"
  context_refresh:
    changed_files: 200
    max_age: 168h
  run_cleanup_after: 1h             # run 目录年龄阈值；后台清理周期固定 1h

jobs:
  workers: 4
  lease: 5m

engine:
  max_concurrent_nodes: 8
  default_node_timeout: 10m

providers:
  claude:
    protocol: anthropic             # anthropic | openai-chat | openai-responses
    base_url: "https://api.anthropic.com"
    model: "claude-opus-5-5"
    api_key_env: ANTHROPIC_API_KEY
    max_concurrency: 4
    rpm: 50
    max_tokens: 16000
    thinking: { budget_tokens: 8000 }
    price: { input_per_mtok: 0, output_per_mtok: 0 }   # USD / 百万 token；用于估算，按实际填写
  gpt:
    protocol: openai-responses
    base_url: "https://api.openai.com/v1"
    model: "<model>"
    api_key_env: OPENAI_API_KEY
    reasoning: { effort: medium }
  local:
    protocol: openai-chat
    base_url: "http://vllm.internal:8000/v1"
    model: "<model>"
    api_key_env: LOCAL_LLM_KEY
    max_concurrency: 8
    temperature: 0.2

profiles:
  default:
    plan:       { mode: code, package_tokens: 60000 }
    propose:    [{ block: code-review, provider: local }]
    challenge:  { provider: claude, only: [P0, P1] }
    on_uncertain: mark              # drop | mark | judge
    synthesize: { mode: code }
    budget:     { max_nodes: 30, max_extends: 0, deadline: 20m, max_tokens: 1500000 }
  deep:
    plan:       { mode: agent, provider: claude, max_packages: 6 }
    propose:
      - { block: review,      provider: claude }
      - { block: review,      provider: gpt }
      - { block: code-review, provider: local }
    reasoner:   { provider: claude, when: auto }   # auto | always | never
    consensus:  { min_votes: 2, same_model_weight: 0.5 }
    challenge:  { provider: cross, fallback: gpt, only: [P0, P1, P2] }
    on_uncertain: judge
    synthesize: { mode: agent, provider: claude }
    budget:     { max_nodes: 30, max_extends: 1, deadline: 40m, max_tokens: 4000000 }
  onboarding:
    agent:      { provider: claude }

mcp:
  config: "config/mcp.json"         # 平台级，Claude Code mcpServers 格式
skills:
  dirs: ["config/skills"]           # 平台级

blocks:                             # 平台级默认绑定；项目可覆盖
  code-review: { mcp_tools: [], skills: [] }
  review:
    mcp_tools: [mcp__codebase-memory__search_graph, mcp__codebase-memory__trace_path, mcp__codebase-memory__get_code_snippet]
    skills: [coding-standards]
  verify:
    mcp_tools: [mcp__codebase-memory__get_code_snippet]
  onboarding:
    mcp_tools: [mcp__codebase-memory__get_architecture, mcp__codebase-memory__search_graph]

review_defaults:                    # 项目设置的默认值（见 §3）
  language: zh-CN
  skip_draft: true
  ignore: ["**/*.lock", "**/vendor/**", "**/node_modules/**", "**/*.pb.go", "**/*_generated.*", "**/*.min.js"]
  max_files: 200
  max_diff_lines: 8000
  commit_status: false
  post_failure_note: true
  show_model_attribution: false
```

### Go 结构体（节选）

```go
type Config struct {
	Server         ServerConfig              `yaml:"server"`
	GitLab         GitLabConfig              `yaml:"gitlab"`
	Auth           AuthConfig                `yaml:"auth"`
	Storage        StorageConfig             `yaml:"storage"`
	Secrets        SecretsConfig             `yaml:"secrets"`
	Workspace      WorkspaceConfig           `yaml:"workspace"`
	Jobs           JobsConfig                `yaml:"jobs"`
	Engine         EngineConfig              `yaml:"engine"`
	Providers      map[string]ProviderConfig `yaml:"providers"`
	Profiles       map[string]ProfileConfig  `yaml:"profiles"`
	MCP            MCPConfig                 `yaml:"mcp"`
	Skills         SkillsConfig              `yaml:"skills"`
	Blocks         map[string]BlockBinding   `yaml:"blocks"`
	ReviewDefaults ProjectSettings           `yaml:"review_defaults"`
}

type ProviderConfig struct {
	Protocol       string          `yaml:"protocol"`
	BaseURL        string          `yaml:"base_url"`
	Model          string          `yaml:"model"`
	APIKeyEnv      string          `yaml:"api_key_env"`
	MaxConcurrency int             `yaml:"max_concurrency"`
	RPM            int             `yaml:"rpm"`
	MaxTokens      int             `yaml:"max_tokens"`
	Temperature    *float64        `yaml:"temperature"`
	Thinking       *ThinkingConfig `yaml:"thinking"`
	Reasoning      *ReasoningConfig `yaml:"reasoning"`
	Price          *PriceConfig    `yaml:"price"`
}

type ProfileConfig struct {
	Plan        PlanConfig        `yaml:"plan"`
	Propose     []ProposeConfig   `yaml:"propose"`
	Reasoner    *ReasonerConfig   `yaml:"reasoner"`
	Consensus   *ConsensusConfig  `yaml:"consensus"`
	Challenge   ChallengeConfig   `yaml:"challenge"`
	OnUncertain string            `yaml:"on_uncertain"`
	Synthesize  SynthesizeConfig  `yaml:"synthesize"`
	Budget      BudgetConfig      `yaml:"budget"`
	Agent       *AgentConfig      `yaml:"agent"` // 仅 onboarding
}
```

### 启动校验（`Config.Validate()`）

- 每个 provider 的 `protocol` 合法，`api_key_env` 对应的环境变量存在且非空。
- profile 中引用的 provider 都存在；`challenge.provider` 为 `cross` 时必须提供 `fallback`。
- `on_uncertain` 为 `judge` 时，`synthesize.mode` 必须是 `agent`。
- `budget.max_nodes` ≤ 100；`max_extends` ≤ 2。
- `blocks` 中的 `mcp_tools` 名称格式为 `mcp__<server>__<tool>`，且 server 在 MCP 配置中存在（工具是否存在在启动后探测，不存在只告警）。
- `secrets.master_key_env` 解码后必须为 32 字节。

校验失败直接退出，并逐条打印错误。

---

## 2. MCP 配置文件

格式与 Claude Code 的 `.mcp.json` 一致，额外允许一个平台扩展字段 `x-review`（Claude Code 会忽略未知字段，所以同一份文件可以两边共用）：

```json
{
  "mcpServers": {
    "codebase-memory": {
      "command": "/usr/local/bin/tribunal-codebase-memory-mcp",
      "args": ["--tool-profile=analysis", "--ui=false"],
      "env": {},
      "x-review": {
        "index_command": ["/usr/local/bin/tribunal-codebase-memory-mcp", "cli", "--quiet", "index_repository", "{\"repo_path\":\"{base_dir}\"}"],
        "env_allowlist": ["PATH"],
        "home": ".mcp/codebase-memory/home",
        "call_timeout": "60s",
        "max_output_chars": 20000,
        "idle_timeout": "10m"
      }
    }
  }
}
```

| `x-review` 字段 | 含义 |
|---|---|
| `index_command` | onboarding 和刷新时执行的建索引命令；`{base_dir}`、`{workspace}` 为占位符 |
| `env_allowlist` | 子进程可继承的环境变量；默认只有 `PATH` |
| `home` | 工作区内的相对目录或绝对目录；应用创建并校验该目录，用作子进程 `HOME`。此字段不展开 `{workspace}` |
| `call_timeout`、`max_output_chars`、`idle_timeout` | 调用超时、输出上限、空闲回收时间 |

`index_command` 中的工具参数以实际版本的 `codebase-memory-mcp cli --help` 为准。

示例使用仓库的 [MCP 隔离启动脚本](../../scripts/codebase-memory-mcp.sh)，而不是直接启动上游二进制。由部署管理员把脚本安装或只读挂载到 `/usr/local/bin/tribunal-codebase-memory-mcp`，并把上游 `codebase-memory-mcp` 放到服务的 `PATH`。主机上已安装的旧名 wrapper 必须重命名或提供兼容符号链接：旧路径 `crc-codebase-memory-mcp` 只有在主机仍保留该名称时才可解析，否则 MCP server 启动失败。标准镜像不包含上游二进制；容器部署需自行安装或挂载它及其许可证，不要挂载 Docker socket。

脚本同时供 MCP server 和 `index_command` 使用。它把 `CBM_CACHE_DIR` 与 `CBM_RUNTIME_DIR` 固定在应用提供的项目 `HOME` 下，并关闭自动索引、watcher 和 UI。0.12.1 的 daemon 默认按账号共享，只有不同 `HOME` 或 `XDG_RUNTIME_DIR` 不足以隔离：第二个项目可能因 cache 不同而连接失败。不要把所有项目改用共享 cache 来消除报错，那会让代码地图跨项目可见。

`HOME` 必须由应用在工作区内创建并校验；脚本不是文件系统沙箱，也不替代应用的环境变量和工具白名单。Unix socket 对路径长度有限制，应保持工作区根路径较短，例如 `/app/data/workspaces`。在新平台或新上游版本上，需重新验证两个项目并发连接、索引复用和未授权工具拒绝，不能只凭 `index_status=ready` 判断隔离成功。

---

## 3. 项目设置（`projects.settings`）

```json
{
  "language": "zh-CN",
  "skip_draft": true,
  "target_branches": ["main", "release/*"],
  "labels_include": [],
  "labels_exclude": ["no-ai-review"],
  "ignore": ["docs/generated/**"],
  "max_files": 200,
  "max_diff_lines": 8000,
  "commit_status": false,
  "post_failure_note": true,
  "show_model_attribution": false,
  "allow_repo_config": true,
  "allow_repo_skills": false,
  "profile_by_label": { "deep-review": "deep" },
  "custom_rules": [
    "金额计算必须使用 decimal 类型",
    "对外 API 的错误码不得变更"
  ]
}
```

- 未设置的字段取平台 `review_defaults`。
- `profile_by_label`：MR 带指定标签时使用对应档位。
- `custom_rules` 作为项目规则注入 prompt 的项目背景段。
- `ignore` 与平台默认值**合并**，不是替换。

---

## 4. 仓库内 `.gitlab-review.yml`

平台 → 项目设置 → 仓库配置的三层优先级在评审 worker 内由 `resolveReviewConfig` 合并。仓库配置只读取**目标分支**上的版本；`allow_repo_config=false` 时完全忽略。webhook 不发起 GitLab 请求，因此 `allow_repo_config=true` 时 Draft 的最终判断在 worker 完成；仓库的 `review.skip_draft` 覆盖项目设置，可过滤 Draft 或关闭过滤。

标签的早期过滤仍可保留：`review.labels_exclude` 只追加到项目设置，不能取消项目级排除，所以 webhook 先按项目设置过滤不会误删本应评审的 MR；worker 合并后还会复核。目标分支条件不能被仓库配置覆盖，worker 会按实际目标分支再检查。启用仓库配置时，自动评审、裸 `/review` 和未带 profile 的手动 API 都先采用仓库 `profile`；有效显式命令或显式 API profile 优先于仓库 profile。队列 payload 的 `profile_explicit` 缺失时，`command` / `manual` 旧 payload 仍按显式处理，自动 webhook 按非显式处理。

```yaml
version: 1
language: zh-CN
profile: default
ignore:
  - "migrations/**"
rules:
  - "所有 SQL 必须参数化"
review:
  skip_draft: true
  labels_exclude: ["wip"]
```

允许覆盖的字段（白名单）：`language`、`profile`（必须是平台已有的档位）、`ignore`（只能追加）、`rules`（追加到 `custom_rules`）、`review.skip_draft`、`review.labels_exclude`。

**不允许**覆盖：provider、预算、commit_status、MCP 和 Skill 绑定。非白名单字段忽略并在汇总中提示一次。

`language` 在合并时折叠为单行，最多 128 个字符。超长值整项忽略并写入警告，评审仍继续。有效值只作为「评审语言」放入项目背景的 scoped 数据块，不能修改平台规则、工具权限或输出结构；prompt 渲染会中和可闭合该数据块的标签。

---

## 5. 环境变量

| 变量 | 用途 |
|---|---|
| `REVIEW_MASTER_KEY` | 主密钥（base64 32 字节），加密项目 token 和 webhook secret |
| `REVIEW_ADMIN_TOKEN` | 平台管理面的 bootstrap Bearer token，至少 32 字节；无用户 ID 和项目 grant，轮换由部署方修改环境变量并重启 |
| `TRIBUNAL_SESSION_KEY` | base64，解码后至少 32 字节；签名会话 cookie、OAuth state 和派生 CSRF。固定该值并复用 SQLite 时重启可继续登录；未设置或恰好为空字符串时回退旧名，旧名也未配置时每进程随机生成并告警，重启后旧会话失效。旧名 `CRC_SESSION_KEY` 仍兼容；新名非空时优先。新名非空但去除空白后为空（例如只含空格）或值非法时，启动失败，不回退旧值，也不生成临时 key |
| `GITLAB_OAUTH_CLIENT_ID` / `GITLAB_OAUTH_CLIENT_SECRET` | 控制台登录 |
| `ANTHROPIC_API_KEY`、`OPENAI_API_KEY`、… | 按 `providers.*.api_key_env` 自定义 |
| `REVIEW_LOG_LEVEL` | `debug` / `info` / `warn` / `error` |
| `REVIEW_LOG_FORMAT` | `json`（默认）/ `text` |

部署示例还使用 Compose 插值变量 `TRIBUNAL_PUBLIC_URL` 和 `TRIBUNAL_HTTP_PORT`。`server.docker.yaml` 把 `TRIBUNAL_PUBLIC_URL` 展开为 `server.public_url`；`TRIBUNAL_HTTP_PORT` 只控制宿主机端口映射，应用不读取，容器监听仍由 `server.listen` 决定。旧名 `CRC_PUBLIC_URL`、`CRC_HTTP_PORT` 仍兼容；新旧同时设置时新名优先。旧占位符和新占位符都接受任一非空的公开地址变量，因此旧持久化 YAML 在只设置新名时仍可用。仅设置 `CRC_HTTP_PORT` 时，Compose 把旧值作为 `TRIBUNAL_HTTP_PORT` 的插值回退值，仅用于宿主机端口映射；应用仍不读取它，只记录一次旧名弃用日志。仅设置 `CRC_SESSION_KEY` 时，Compose 同样把旧值复制给容器的 `TRIBUNAL_SESSION_KEY`。

应用只检查自身进程可见的环境变量；三个旧名在这些变量中只要配置了非空值，都会各记录一次弃用日志。日志表示旧名仍配置，不代表旧值一定生效或一定被忽略；例如仅设置 `CRC_SESSION_KEY` 时 Compose 会把它作为容器的 `TRIBUNAL_SESSION_KEY` 回退值，仅设置 `CRC_HTTP_PORT` 时该值仍用于宿主机端口映射。日志只记录变量名，不记录值。

兼容契约如下（`HTTP_PORT` 两项只指 Compose 宿主机端口映射，不是应用监听端口；应用监听由 `server.listen` 决定）：

| 设置 | 结果 |
|---|---|
| 仅旧名 `CRC_SESSION_KEY` / `CRC_PUBLIC_URL` | 按旧值工作；两个旧名各记录一次弃用日志（只含变量名，不含值） |
| 仅旧名 `CRC_HTTP_PORT` | Compose 用该值作为宿主机端口映射回退值，容器仍监听 `server.listen`；应用不读取该变量，仍记录一次弃用日志 |
| 仅新名 `TRIBUNAL_SESSION_KEY` / `TRIBUNAL_PUBLIC_URL` | 按新值工作；不记录旧名弃用日志 |
| 仅新名 `TRIBUNAL_HTTP_PORT` | 只控制 Compose 宿主机端口映射，容器仍监听 `server.listen`；应用不读取，不记录旧名弃用日志 |
| 新旧同时设置 | 新名优先；旧值忽略，不报冲突；已配置的旧名仍各记录一次弃用日志 |
| 新名已设置但值非法 | 启动失败（fail closed），不回退旧值 |

`TRIBUNAL_SESSION_KEY` 的“空”只指未设置或恰好为空字符串；非空但去除空白后为空（例如只有空格）属于非法值，启动失败，不回退旧名，也不生成临时 key。直接运行应用时，旧名 `CRC_SESSION_KEY` 仅含空白仍按历史行为视为未设置；使用本仓库 Compose 时，该值会复制到新名，因此启动失败。

升级时把原 session key 的同一个值传给 `TRIBUNAL_SESSION_KEY`，不要重新生成；重新生成会使现有浏览器会话失效。

---

## 6. prompt 覆盖

`prompts/` 目录 embed 进二进制。部署时可设置 `prompts_dir` 指向外部目录，同名文件优先使用外部版本；每个 prompt 文件首行写版本号注释（如 `<!-- v3 -->`），版本号参与 fingerprint 计算。
