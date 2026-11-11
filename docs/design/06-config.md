# 06 配置

> 配置分三层，后者覆盖前者：**平台配置文件** → **项目设置**（数据库 `projects.settings`，控制台可改）→ **仓库内 `.gitlab-review.yml`**（只读目标分支版本，且只能覆盖白名单内的字段）。

---

## 1. 平台配置文件

默认路径 `config/server.yaml`，可用 `--config` 指定，也可以传多个文件按顺序合并。字符串中的 `${ENV_NAME}` 在加载时展开；密钥类字段**只接受** `*_env` 形式，不允许明文写在文件里。

当前最小启动示例见 [`config/server.example.yaml`](../../config/server.example.yaml)，但它没有 `providers`，也没有 `profiles.default.provider`，只能启动服务、注册项目和跑路由，**不能执行真实评审**；要跑评审必须先补齐 `providers` 并让 `profiles.default.provider`（或 `lanes.*` / `challenge.provider`）引用其中一个别名。OAuth 可选：不配置时 `/auth/login` 返回 503，Bearer 仍可用。当前存储只实现 SQLite，配置 `postgres` 会在启动时明确报错。管理 API 使用 `auth.bootstrap_token_env`（默认 `REVIEW_ADMIN_TOKEN`，至少 32 字节），该静态 Bearer 仅用于平台管理面；浏览器登录、用户级 API token 和本地项目 `read`/`manage` 授权已实现。多文件加载递归合并映射、替换数组，拒绝未知字段、重复键和多文档 YAML；环境变量在解析后的字符串上展开，避免改变 YAML 结构。

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
  catch_up:                         # 启动恢复后一次性补评停机期间错过的 MR head（#362）
    enabled: true                   # false 关闭；仅启动时读取，修改需重启
    max_per_project: 10             # 每个项目一次扫描最多入队的评审数，须为正
    max_total: 20                   # 一次扫描最多入队的评审总数，须为正
    max_age: 24h                    # 只考虑此时间窗内更新过的 MR，须为正

auth:
  oauth:
    client_id_env: GITLAB_OAUTH_CLIENT_ID
    client_secret_env: GITLAB_OAUTH_CLIENT_SECRET
  bootstrap_admins: []              # 默认不授予平台管理员；管理员隐式管理全部已登记项目，其他用户仍需 grant
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
    forced_tool_choice: auto        # 可选；auto（默认）| supported | unsupported，见运维说明「最后一轮强制提交」
    context_window: 176000          # 可选；可用于输入的 token 数（窗口减去 max_tokens 与思考预算），agent 用量达 85% 时压缩旧工具输出，0 或不填则不压缩
    thinking: { budget_tokens: 8000 }
    price: { input_per_mtok: 0, output_per_mtok: 0 }   # USD / 百万 token；用于估算，按实际填写
  gpt:
    protocol: openai-responses
    base_url: "https://api.openai.com/v1"
    model: "<model>"
    api_key_env: OPENAI_API_KEY
    max_tokens: 16000             # 必填且为正数，按供应方上限调整
    reasoning: { effort: medium }
  local:
    protocol: openai-chat
    base_url: "http://vllm.internal:8000/v1"
    model: "<model>"
    api_key_env: LOCAL_LLM_KEY
    max_concurrency: 8
    max_tokens: 4096              # 必填且为正数，按供应方上限调整
    temperature: 0.2
    strict_role_alternation: false  # 仅 openai-chat；网关要求角色严格交替时设为 true，见下文

profiles:
  default:                          # 唯一的评审档位，字段见下文「评审档位」
    provider: claude                # 所有节点的默认 provider
    explore:  { provider: claude }  # 可选：覆盖 explore 节点
    lanes:                          # 可选：覆盖四路评审各自的 provider
      code_review: { provider: local }
      review:      { provider: claude }
      reasoner:    { provider: claude }
      validate:    { provider: gpt }
    challenge: { provider: gpt }    # 可选：覆盖逐条反证（verify）
    plan:      { package_tokens: 60000, max_packages: 6 }
    budget:    { max_nodes: 30, deadline: 30m, max_tokens: 0 }   # 0 = 不设 token 上限
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
  explore:
    mcp_tools: [mcp__codebase-memory__search_graph, mcp__codebase-memory__get_code_snippet]
  validate:
    mcp_tools: [mcp__codebase-memory__get_code_snippet]
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

需要授予平台管理员时，先向用户确认实际 GitLab 数字用户 ID 和本次授权，再填写 `id:<GitLab 用户 ID>`；修改后重启生效。用户名写法已弃用，将在下一个大版本移除。

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
	ContextWindow  int             `yaml:"context_window"` // 可选，不能为负；0 表示不压缩
	ForcedToolChoice string        `yaml:"forced_tool_choice"` // 可选：auto | supported | unsupported
	Temperature    *float64        `yaml:"temperature"`
	Thinking       *ThinkingConfig `yaml:"thinking"`
	Reasoning      *ReasoningConfig `yaml:"reasoning"`
	Price          *PriceConfig    `yaml:"price"`
	StrictRoleAlternation bool `yaml:"strict_role_alternation"` // 仅 openai-chat，默认 false
}

type ProfileConfig struct {
	Provider string         `yaml:"provider"` // 评审档位：所有节点的默认 provider
	Explore  *ExploreConfig `yaml:"explore"`  // { provider }
	Lanes    *LanesConfig   `yaml:"lanes"`    // code_review / review / reasoner / validate，各 { provider }
	Plan     PlanConfig     `yaml:"plan"`     // package_tokens、max_packages
	Challenge ChallengeConfig `yaml:"challenge"` // provider
	Budget   BudgetConfig   `yaml:"budget"`
	Agent    *AgentConfig   `yaml:"agent"`    // 仅 onboarding
	// 另有 Propose、Reasoner、Consensus、OnUncertain、Synthesize 等旧字段：
	// 仍可解码，由加载器迁移或忽略，见「评审档位」。
}
```

### 启动校验（`Config.Validate()`）

- 每个 provider 的 `protocol` 合法，`model` 非空且不全为空白，`api_key_env` 对应的环境变量存在且非空；`base_url` 可为空，非空时校验地址格式。
- profile 中引用的 provider 都存在：`provider`、`explore.provider`、`lanes.*.provider`、`challenge.provider` 和 onboarding 的 `agent.provider`。留空表示沿用 `provider`，不做检查。
- `budget.max_nodes` 在 1 到 100 之间，`budget.deadline` 为正。
- `blocks` 中的 `mcp_tools` 名称格式为 `mcp__<server>__<tool>`，且 server 在 MCP 配置中存在（工具是否存在在启动后探测，不存在只告警）。
- `secrets.master_key_env` 解码后必须为 32 字节。

校验失败直接退出，并逐条打印错误。

### 评审档位（`profiles`）

Tribunal 只有一个评审档位 `default`，评审图固定：`explore` → 四路并行评审（`code-review`、`review`、`reasoner`、`validate`）→ 裁决（`gather` → `verify.N` 反证 P0/P1 → `adjudicate`）→ 汇报（`synthesize`，代码渲染）。节点和契约见 [04 DAG 引擎与节点契约](04-dag-and-contracts.md)。`onboarding` 档位仍存在，只驱动项目接入，不是评审档位。没有可配置的旧图回退，也没有档位选择。

`profiles.default` 的字段：

| 字段 | 说明 |
|---|---|
| `provider` | 所有节点的默认 provider。留空时依次取 `lanes.code_review.provider`、`challenge.provider`；仍为空则无法执行真实评审 |
| `explore.provider` | 可选，覆盖 `explore` 节点；留空取 `provider` |
| `lanes.code_review.provider`、`lanes.review.provider`、`lanes.reasoner.provider`、`lanes.validate.provider` | 可选，分别覆盖四路评审；留空取 `provider` |
| `challenge.provider` | 可选，覆盖逐条反证（`verify`）；留空取 `provider` |
| `plan.package_tokens`、`plan.max_packages` | 分包上限：每包估算 token（默认 60000）和最大包数（默认 6，受下述上界限制） |
| `budget.max_nodes`、`budget.deadline`、`budget.max_tokens` | 节点总数（默认 30）、总时长（默认 30m）、token 上限（默认 0，即不设上限，用量由上游 LLM key 的额度决定；填正数才启用工作流 token 预算，见 [#392](https://github.com/LeXwDeX/Tribunal/issues/392)） |

其余行为由代码固定，不可配置：plan 为 code 模式（不调用模型），P0/P1 逐条挑战，挑战结果不确定的条目标记为「存疑」后发布，报告由代码生成，评审图不在汇报阶段扩展。

`Load` 会把 `plan.max_packages` 降到上界 `min(17, (budget.max_nodes − 9) / 2)`（至少 1）：配置值高于上界时降到上界，并在启动日志产生一行 WARN；未配置且上界低于默认值 6 时静默降到上界。此外 `budget.deadline` 低于 30m 时启动日志也会提示（探索、四路评审和逐条反证可能来不及完成）。

评审执行时（构建评审图）还会检查扇出上限：`plan.max_packages × 2 + 2 ≤ max_fan_out`（`max_fan_out` 默认 36），超出时拒绝该配置。每个包生成 `code-review` 和 `review` 各一个节点，`reasoner` 和 `validate` 每个 MR 各一个。

#### 档位覆盖：节点模型与参数

平台管理员可通过 `GET/PUT/DELETE /api/v1/profiles/default/overrides`（见 [03 HTTP API](03-http-api.md)）在配置文件之上覆盖 `default` 档位的模型和请求参数（[#291](https://github.com/LeXwDeX/Tribunal/issues/291)、[#334](https://github.com/LeXwDeX/Tribunal/issues/334)）。覆盖存于数据库，不修改配置文件，只作用于之后启动的工作流。

覆盖文档的结构与配置的节点布局一致，所有字段可选，空值表示沿用配置：

```json
{
  "default": {"model": "config:primary", "params": {"max_tokens": 8000}},
  "explore": {"model": "user:<id>"},
  "lanes": {
    "code_review": {}, "review": {}, "reasoner": {}, "validate": {}
  },
  "challenge": {"params": {"reasoning": {"effort": "high"}}}
}
```

- 可配置七项：`default`、`explore`、`lanes.code_review`、`lanes.review`、`lanes.reasoner`、`lanes.validate`、`challenge`；每项含 `model` 和 `params`。
- `model` 是目录 ID（`config:<name>` 或 `user:<id>`）；不存在、已删除、不可用（出站被拒或记录损坏）或首尾带空白的 ID 返回 400，字段路径如 `overrides.default.model`。
- `params` 只允许 `max_tokens`、`temperature`、`reasoning.effort`、`thinking.budget_tokens`，其他键返回 400。校验规则与模型参数相同：`max_tokens` > 0，`temperature` 在 0–2，`effort` 为 `low` / `medium` / `high` / `max`，`budget_tokens` > 0。`budget_tokens: 0` 和空 `effort` 不会生效，返回 400，提示省略该字段以继承；节点的生效模型是 Anthropic 协议时 `budget_tokens` 须 ≥ 1024，且 `max_tokens`（覆盖值，未覆盖则取模型值）须大于 `budget_tokens`。
- 参数作为该节点的请求值注入，优先于模型自身默认；节点参数逐字段叠加在 `default` 参数之上。不创建独立 client，并发与限流仍按模型。

**模型解析顺序**（每个节点）：节点覆盖 → `default` 覆盖 → 配置中该节点的 provider → 配置中档位的 provider → 派生回退。对应 `source` 取值 `override`、`override_default`、`config`、`config_default`、`derived`。

**与配置文件的关系**：覆盖先合并进未归一化的 `default` 配置，再归一化一次，因此 `default` 和各节点（包括 `lanes.code_review`）的覆盖都会改变实际执行的模型，而不只是显示。由此，`default` 覆盖优先于配置文件里各节点自己的 provider；清除覆盖即恢复配置行为。档位哈希仅在设置了参数覆盖时才包含参数，未设置时哈希与此前一致。 覆盖 `lanes.code_review` 或 `challenge` 也会带动由它们派生的节点一起变化（这些节点的来源仍标为 `derived`）。

#### 旧配置的兼容处理

`config.Load` 不会因下列旧配置失败。每一项都在启动日志中产生一行 WARN，格式为 `configuration was migrated or ignored: <字段…>`，只含字段名，不含配置值。

| 旧配置 | 处理 |
|---|---|
| `default` 和 `onboarding` 以外的档位（如 `deep`） | 整个档位被移除 |
| `propose` 中第一个 `code-review` 条目 | 迁移为 `lanes.code_review.provider` |
| `propose` 中第一个 `review` 条目 | 迁移为 `lanes.review.provider` |
| `propose` 的其他条目 | 忽略（每路只运行一次） |
| `reasoner.provider` | 迁移为 `lanes.reasoner.provider`；`reasoner.when` 单独告警「was ignored」并忽略（reasoner 一路始终运行） |
| `challenge.provider: cross` | 改用 `challenge.fallback` 作为 `challenge.provider`；没有 `challenge.fallback` 时告警，由档位 `provider` 担任挑战者 |
| `challenge.fallback`（非 `cross` 时） | 忽略 |
| `challenge.only`（不等于 `P0, P1` 时） | 忽略（P0/P1 始终被挑战） |
| `plan.mode`（非 `code`）、`plan.provider` | 忽略 |
| `consensus` | 忽略 |
| `on_uncertain`（非 `mark`） | 忽略 |
| `synthesize.mode`（非 `code`）、`synthesize.provider` | 忽略 |
| `budget.max_extends`（非 0） | 忽略 |
| `agent`（写在 `default` 下） | 忽略（只属于 `onboarding`） |

迁移时，同一字段上显式写的新字段优先于从旧字段迁移来的值；此时旧字段的告警为「`<旧字段>` is deprecated and was ignored: `<新字段>` is set」。迁移得到的 provider 仍会被校验，错误信息指向新字段，例如 `profiles.default.lanes.reasoner.provider "x" is not defined`。

评审档位既没有 `provider` 也没有 `lanes.code_review.provider` 时，构建评审图失败，字段为 `provider`（「is required: set profiles.default.provider or lanes.code_review.provider」）。配置校验不再报告由此派生的 `propose[0].provider`。

引用已移除档位的其他位置的处理：

- 项目已保存的档位、`.gitlab-review.yml` 的 `profile`、标签映射：评审时按 `default` 处理（仓库配置中无法识别的 `profile` 被忽略，并在汇总里提示一次）。
- 会被拒绝的情形：注册或更新项目时 `profile` 不是 `default`（HTTP 400，`unknown profile`）；评论命令 `/review <名称>` 的名称不是 `default`（命令被忽略，webhook 记为 `unknown_profile`）。`onboarding` 在上述所有位置都不被接受为评审档位。
- 会被丢弃的情形：保存 `settings.profile_by_label` 时，值不是 `default` 的映射（包括 `onboarding` 和已移除的档位）被丢弃而不是以 400 拒绝，因此遗留的旧设置仍可编辑；已存储的旧值在评审时仍按 `default` 处理。

升级操作见 [部署与运维 · 单一评审档位](../operations.md#单一评审档位升级)。

### 多 Provider 与档位引用

`providers` 的键是 provider 别名。每个别名绑定一个 `protocol`、`base_url`、`model` 和一个 `api_key_env`；`api_key_env` 只保存**变量名**，密钥值由部署环境注入，不写进 YAML、命令、日志或接口响应。同一个供应方的两个模型需要两个别名，例如 `primary` 和 `secondary`，即使二者共用 `base_url` 和密钥变量：一个别名只有一个 `model`。

Docker Compose 默认只启用 `primary`。`config/server.docker.yaml` 另有一个注释掉的 `secondary` 示例，四个值分别来自 `LLM_SECONDARY_PROTOCOL`、`LLM_SECONDARY_BASE_URL`、`LLM_SECONDARY_MODEL` 和 `LLM_SECONDARY_API_KEY`。`compose.yaml` 的 `environment` 已允许这四个变量，但 `.env` 只用于 Compose 插值，不会自动注入容器；未列在 `environment` 中的变量容器看不到。若新增第三个 provider 并使用新变量名，须在 `compose.override.yaml` 的 `services.review.environment` 中逐一透传它引用的变量；只透传 key 会使未透传的 protocol 和 model 为空，导致启动失败。`base_url` 可为空以使用协议适配器默认地址；复用容器已有变量时不需要重复透传。

可引用模型的字段：

| 字段 | 使用模型的条件 | 说明 |
|---|---|---|
| `profiles.default.provider` | 评审 | 所有评审节点的默认 provider。 |
| `profiles.default.explore.provider` | 评审（可选） | 覆盖 `explore` 节点。 |
| `profiles.default.lanes.{code_review,review,reasoner,validate}.provider` | 评审（可选） | 分别覆盖四路评审；每个包的 `code-review` 与 `review` 节点各用对应字段。 |
| `profiles.default.challenge.provider` | 评审（可选） | 逐条反证 `verify` 使用的 provider；`verify` 没有独立字段。 |
| `profiles.onboarding.agent.provider` | onboarding | 工作区初始化使用；添加第二 provider 时保留 primary。 |

`plan`、`gather`、`adjudicate` 和 `synthesize` 是代码节点，不调用模型，也没有 provider 字段；旧配置中的 `plan.provider`、`synthesize.provider` 会被忽略并在启动日志中提示。`verify` 使用 `challenge.provider`；挑战者与提出方按 endpoint 和 model 身份归一后相同，或身份无法解析时，该条挑战被标记为同模型，不能证明跨模型独立，不同别名也不保证实际模型独立。不再支持 `cross`，也没有自动选择独立候选或调用失败后的自动故障切换。

每个 MR 至少运行 `explore`、每个包的 `code-review` 和 `review`、`reasoner`、`validate` 这些 agent 节点，另有每条 P0/P1 finding 一个 `verify` 节点。因此一次评审调用模型的次数多于此前每次一两个 agent 节点的评审，token 成本预期更高；具体数字以后另行测量，此处不给出估计。节点数受 `budget.max_nodes` 与 fan-out 限制约束。

平台管理员可在控制台「模型与档位」查看配置中的全部 provider 和评审档位。页面只把 `default` 显示为「评审档位」，并按固定的八步拓扑列出各步骤的 provider：探索（`explore`）、提出（`code-review`、`review`、`reasoner`、`validate`）、挑战（`verify`）、裁决（`adjudicate`）、汇报（`synthesize`）；`onboarding` 单独标为「项目接入」，不是评审档位。该页面只读，界面不显示密钥值。逐个 provider 的连通性测试发送一条最小真实请求，会产生费用，需事先取得用户授权；成功只代表协议、地址、密钥和模型 ID 基本可用，不证明工具调用、长上下文、推理参数或完整业务流程兼容。

修改 provider 或档位需要重启服务，当前进程不热加载配置；`docker compose restart` 会重新读取已挂载的配置文件，但不会加载新的容器环境变量。新增 provider（即使没有被任何档位引用）会改变评审执行配置；启动恢复会对未终态工作流重新检查配置、档位和执行身份，不匹配时取消，而不是续跑。待发布结果是否继续发布由发布时校验决定，不能假定会自动跳过或一定发布。修改前应核对并等待或处理未完成和待发布结果；已发布或失败的评审不会被改写，被取消的评审需要重新触发。移除已配置的 provider 前，检查 `profiles.default` 的 `provider`、`explore.provider`、`lanes.*.provider`、`challenge.provider`，以及 `profiles.onboarding.agent.provider`；仍留在配置里的旧字段（`propose[].provider`、`reasoner.provider`、`challenge.fallback`）也一并清理：被保留或迁移的 provider 字段才会被校验，被忽略的旧字段先被丢弃，不参与校验，但仍应清理以免告警。对已移除 provider 的残留引用若出现在被保留或迁移的字段上，会使配置校验失败；不要清空 onboarding 的 `agent.provider`，`profiles.default` 也必须至少能解析出一个 provider。完整操作步骤见 [多 Provider 配置](../operations.md#多-provider-配置) 和 [Agent 部署指南](../agent-deployment.md#多-provider-配置)。

### 严格角色交替（`strict_role_alternation`）

agent 循环在最后一轮、或节点剩余时间不足时，会在请求末尾追加一条一次性的提交提示（不写入 transcript，见 [#255](https://github.com/LeXwDeX/Tribunal/issues/255)）。`openai-chat` 协议下，这条提示前面通常是 tool 结果，个别情况下是另一条 user 文本。

- **相邻的 user 文本始终合并**：`openai-chat` adapter 把相邻的纯文本 user 消息合并为一条，以空行分隔，与 `anthropic` adapter 的规则一致。这一条不受开关控制。
- **`strict_role_alternation: true`**：紧跟在 tool 结果之后的 user 文本不再单独发送，而是追加到最后一条 tool 消息内容的末尾，前面加 `System notice (from Tribunal, not tool output):` 标记。追加位置在整段 tool 输出之后，也就是在 agent 的不可信数据包裹（`<tool_output …>…</tool_output …>`）之外。请求中因此不再出现 `tool → user` 序列。

默认值是 `false`，请求形状与之前相同。只有当 `openai-chat` 网关或其聊天模板因角色顺序返回 400（例如 vLLM 部分模板报「roles must alternate」，或拒绝 tool 之后接 user）时才开启。OpenAI 官方 API 接受默认形状，不需要开启。开启后，模型看到的提示位于 tool 消息中，个别模型对这种位置的服从度可能低于独立 user 消息；开启后应在测试 MR 上验证一次完整评审。

`anthropic` 和 `openai-responses` 协议忽略这个字段。`anthropic` adapter 已把提示和 tool 结果合并在同一个 user 轮次中；`openai-responses` 使用独立的 input item 列表，不要求角色交替。修改该字段会改变评审执行配置，需要重启服务，未终态工作流在恢复时会因执行身份不匹配被取消，处理方式同上一节。

### 默认扩展（`default_extensions`）

平台自带的内置扩展在这里声明，首次启动时自动登记为**平台级**扩展并写入平台块绑定。内置默认值与 `config/server.example.yaml`、`config/server.docker.yaml` 相同：

```yaml
default_extensions:
  skills:
    - name: code-review            # 内置 Skill，随二进制分发（internal/skill/builtin/）
      blocks: [code-review]
```

示例配置和 Docker 配置还声明内置 MCP 服务器（内置默认值中没有它，因为非容器主机不一定装了二进制）：

```yaml
default_extensions:
  mcp_servers:
    - name: codebase-memory          # 平台级 stdio 服务器
      config:                        # 单个 Claude Code mcpServers 条目，可含 x-review
        command: /usr/local/bin/tribunal-codebase-memory-mcp
        args: ["--tool-profile=analysis"]
        x-review:
          index_command: [/usr/local/bin/tribunal-codebase-memory-mcp, cli, --quiet, index_repository, '{"repo_path":"{base_dir}"}']
          env_allowlist: ["PATH"]
          home: .mcp/codebase-memory/home
      tools:                         # 块 → 本服务器的工具名，授权为 mcp__codebase-memory__<tool>
        code-review: &read_tools [list_projects, index_status, search_graph, trace_path, get_code_snippet,
          query_graph, get_graph_schema, get_file_outline, get_architecture, search_code, check_index_coverage]
        review: *read_tools
        explore: *read_tools
        validate: &verify_tools [get_code_snippet, search_graph, trace_path, search_code]
        verify: *verify_tools
        onboarding:  [get_architecture, get_graph_schema]
```

#### 默认授权矩阵

两份随仓库分发的 YAML 使用同一矩阵。默认只授予只读工具：

| 块 | 默认 MCP 工具 | 职责理由 |
|---|---|---|
| `code-review` | 11 项只读工具（见下） | 单次双轴评审需要跨文件定位定义、调用链和既有实现，以验证 diff 内外的判断。 |
| `review` | 与 `code-review` 相同 | `rv.<pkg>` 终审通道同样需要只读图谱证据，避免只凭 diff 猜测上下文。 |
| `explore` | 与 `code-review` 相同（11 项） | 只读探索要查定义、调用方、配置和测试，需要完整的只读检索面。 |
| `validate` | `get_code_snippet`、`search_graph`、`trace_path`、`search_code` | 验证通道读代码与测试，对照 pipeline 证据；不授予探索或架构概览工具。 |
| `verify` | `get_code_snippet`、`search_graph`、`trace_path`、`search_code` | 核对一条 finding 只需读取片段、查找定义与调用链、搜索用法；不授予探索或架构概览工具。 |
| `onboarding` | `get_architecture`、`get_graph_schema` | 建立项目画像需要架构概览和图 schema；逐点检索、覆盖检查留给评审块。 |
| `reasoner` | 无 | 只审查设计声明和已有材料，不直接检索代码图谱；管理员可按需绑定。 |
| `synthesize` | 无 | 只消费结构化 finding 和 challenge 结果，不直接检索。 |

11 项只读工具：`list_projects`、`index_status`、`search_graph`、`trace_path`、`get_code_snippet`、`query_graph`、`get_graph_schema`、`get_file_outline`、`get_architecture`、`search_code`、`check_index_coverage`。默认授权不得包含 `index_repository`、`delete_project`、`ingest_traces`、`manage_adr` 等写工具，也不包含 `detect_changes`、`compare_graphs`。

`code-review` Skill 默认只绑定 `code-review` 块。管理员仍可按需把 Skill 或工具绑定到其他可绑定块。可绑定块共八个：`explore`、`code-review`、`review`、`reasoner`、`validate`、`verify`、`synthesize`、`onboarding`；`reasoner` 和 `synthesize` 是合法的可绑定目标，只是没有默认授权。

- `skills[].name` 必须是内置 Skill，`blocks` 只能取 `explore`、`code-review`、`review`、`reasoner`、`validate`、`verify`、`synthesize`、`onboarding`；同名不能重复。校验失败与其他配置错误一起报告。
- 内置文件复制到 `<workspace.root>/skills/platform/<id>/skill`，行上 `builtin` 列记录内置名。API 以 `builtin: true` 返回，控制台显示「内置」标签。
- **始终启用**：内置 Skill 不能用 `PATCH /api/v1/skills/{id}` 或控制台禁用；禁用请求返回 409，删除也返回 409。重启和升级都保持启用，启动同步会把历史禁用状态恢复为启用。
- **随平台升级**：每次启动比较嵌入文件和已登记副本，不同就原地替换目录内容（多余的旧文件一并删除），并更新描述；行 ID、路径和已启用状态在刷新期间保持不变。刷新失败（例如修改 `workspace.root` 后已登记目录不在受管 Skill 根目录下）只记 Warn 日志，服务照常启动；已登记副本保持原样，能否加载与普通 Skill 相同。
- **内置 MCP 服务器**：`mcp_servers[].name` 须为合法服务器名，`config` 非空，`tools` 的键只能取上述块，工具名不带 `mcp__` 前缀；登记时按 MCP 规则严格校验 `config`，未知字段或禁止的环境变量引用会让启动失败。行上 `builtin` 列记录内置名，API 返回 `builtin: true`。每次启动把 `config` 同步为当前版本附带的内容，并把 `enabled` 恢复为 true；API 不允许禁用、改名、改配置或删除，均返回 409；导入接口不会覆盖内置行，同名导入返回 409 并回滚整批。数据库中已有管理员自建的同名平台服务器，或 `mcp.config` 中已定义同名服务器时，视为管理员所有，不登记也不绑定。
- **块绑定**记录在 `extension_defaults` 表：
  - `<kind>:<name>`：已登记，或因已有管理员自建的同名平台扩展而跳过（不覆盖，也不绑定）。
  - `<kind>:<name>@<block>`：已授权到该块，或跳过。配置文件 `blocks` 已声明该块、或数据库中该块的平台绑定已被管理员修改时跳过，升级不会改写自定义绑定。
  - `block:<block>`：默认机制最后写入的绑定。只要该行未被改动，后续版本新增的内置扩展（例如 MCP）可以继续合并进来。绑定行与对应的 ledger 记录在同一事务中写入，中途失败不会让该块被误判为管理员已修改。
  - 默认机制写入某块之后，又在配置文件 `blocks` 中声明了该块：只有数据库中该块的平台绑定仍等于 `block:<block>` 快照，且行内每个 MCP 工具和 Skill 都能由 `extension_defaults` 中 `applied` 的默认 grant detail 证明时，启动时才删除这行（记一次 Info 日志，快照改记为跳过），以 YAML 为准。行含自建授权、或 grant detail 缺失/无法解析时保留该行，数据库绑定继续覆盖 YAML。管理员改过的绑定仍以数据库为准。即使 `default_extensions` 已清空也会执行这一步。
  - 已授权的 `<kind>:<name>@<block>` 在 detail 中记录当时授予的条目。后续版本改变授权内容（例如更新 MCP 工具白名单）时，只要该块绑定仍等于 `block:<block>` 快照，就用新授权替换旧授权的条目；管理员改过的绑定保持不变。
- **升级与恢复边界**：自动升级只刷新已登记的内置扩展，不覆盖管理员改过或解绑的绑定，保留管理员自建的同名扩展。显式恢复只处理选定块上台账记录为已登记的内置扩展：移除这些内置扩展在该块的授权并重新应用当前默认，因此会重置该块上被管理员改过或解绑的内置授权；管理员自建扩展的授权和项目级绑定保留。
- **文件显式声明优先**：配置文件 `blocks` 显式声明某个块时，该块的默认授权跳过；数据库平台行释放后，以文件为准。释放条件见[文件声明与数据库绑定](#文件声明与数据库绑定)。即使 `default_extensions` 已清空，这条规则也不变。
- **始终启用**：内置 Skill 和 MCP 服务器始终启用；升级、重启和内置配置同步都不会改变这一点。历史数据库中已被禁用的内置行会在下次启动时恢复启用。
- **生效时机**：修改配置文件中的 `default_extensions` 或数据库中的扩展、绑定后，需要重启服务才生效；运行中的进程继续使用启动时读取的扩展和绑定。管理员从某个块解绑后，重启不会恢复。
- `skills: []` 只是不再登记新的默认项，不删除已登记的扩展和绑定。配置文件和 `mcp.config` 是部署者特权入口；HTTP API 的自定义 MCP 新增/导入仅平台管理员可用，自定义 Skill 创建入口仍关闭，控制台尚未提供新增/导入界面。
- 登记或更新失败（例如工作区不可写）会让启动失败；登记失败不写台账，下次启动重试。

#### 文件声明与数据库绑定

顶层 `blocks` 是文件声明的块绑定，`default_extensions` 只是内置扩展的默认授权声明，两者不是同一层。启动时先执行默认扩展，再加载平台扩展；数据库中的平台块绑定按块覆盖文件 `blocks`（`internal/app/adapters.go` 的 `loadStoredPlatformExtensions`）。因此，文件声明只有在数据库没有对应平台行时才直接生效。

配置文件声明某块后，默认机制会在启动时尝试释放它此前写入的平台行（`releaseFileBlocks`）。释放条件是 `purelyDefaultOwned` 能证明整行纯默认：

- 行内容等于 `block:<block>` 快照；
- 行内每个 MCP 工具和 Skill 都能由 `extension_defaults` 中 `applied` 的默认 grant detail 认领。

行上存在管理员自建授权，或任一 grant detail 缺失、无法解析，证明失败，数据库行保留并继续覆盖文件 `blocks`。快照相等不等于纯默认：快照记录的是默认机制最后一次写入的整行，可能已经包含当时行上的自建授权，例如执行恢复默认后保留的自建授权。

恢复默认只移除已登记内置扩展在选中块上的授权，保留自建扩展授权和项目级绑定，恢复本身不改变启用状态，也不修改文件 `blocks`。文件已声明该块时，恢复返回 `skipped`；有效值需看数据库行是否释放，不能把“跳过恢复”当作文件绑定已经生效。文件声明生效后，块绑定 API 不返回它的数据库行；控制台仍列出该块，但平台绑定显示为空，不要把空绑定保存回数据库。

从数据库绑定迁移到文件声明时，按 [默认扩展授权迁移](../operations.md#默认扩展授权迁移) 操作。关键限制：

1. 先备份数据库、工作区和实际挂载配置。
2. 在独立草稿中准备顶层 `blocks` 的完整绑定，先不要写入实际挂载配置；文件声明是完整绑定，必须同时写入该块需要的默认授权和要保留的自建授权。
3. 若当前生效配置已声明该块，先临时移除声明并重启；否则恢复默认会被 `file_binding` 跳过。草稿保持不启用。
4. 在控制台“扩展 → 块绑定”的平台范围清除该块的自建授权并保存，或对 `PUT /api/v1/block-bindings/{block}` 提交完整绑定。不要删除数据库行。
5. 对该块执行恢复默认，使行和 ledger 重新由默认机制接管。
6. 把草稿中的完整声明写入实际挂载配置并重启，确认日志 `file configuration replaces default block binding`。
7. 账本缺失、无法证明纯默认或行仍未释放时停止核对，不要手工删除数据库行。

---

## 2. MCP 配置文件

通过 API 登记的完整 MCP 配置存于 SQLite 的 `config_enc`，使用主密钥加密，AAD 绑定格式版本、作用域、项目 ID、服务器 ID 和配置字段。服务器改名不改变加密身份。创建、更新、导入和内置配置刷新都走同一加密存储路径；没有成功配置密钥时拒绝写入，读取也不回退到明文。API 响应只返回 `config: {}`，内部运行和测试仍可读取解密后的配置。`POST /api/v1/mcp-servers` 与 `/import` 仅平台管理员可用，项目 scope 还要求项目已登记；同 scope 重名或内置同名返回 409 且不写库，导入在单个事务内按名 upsert，任一非法整批不写，已存在同名保留 ID 与 `enabled`。`POST /api/v1/mcp-servers:test` 可在保存前测试草稿配置，不写库；带 `id` 时使用已存储服务器的 scope，并把 `****` 从已存配置恢复。测试失败只返回固定错误类别，不返回配置或上游原文。

启动先执行 `0017_mcp_config_seal` 结构迁移，再在服务和默认扩展启动前事务回填旧配置并写入密钥校验标记。完成后拒绝新出现的明文或损坏密文。配置文件本身不因此被加密；仍需限制文件权限并通过环境变量引用秘密。升级与旧备份边界见 [MCP 配置加密升级](../operations.md#mcp-配置加密升级v120)。

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

示例使用仓库的 [MCP 隔离启动脚本](../../scripts/codebase-memory-mcp.sh)，而不是直接启动上游二进制。由部署管理员把脚本安装或只读挂载到 `/usr/local/bin/tribunal-codebase-memory-mcp`，并把上游 `codebase-memory-mcp` 放到服务的 `PATH`。主机上已安装的旧名 wrapper 必须重命名或提供兼容符号链接：旧路径 `crc-codebase-memory-mcp` 只有在主机仍保留该名称时才可解析，否则 MCP server 启动失败。标准 Docker 镜像已内置校验过的上游 0.12.1 二进制、该 wrapper 和 `crc-codebase-memory-mcp` 兼容链接（见下文）；非容器部署仍需自行安装。不要挂载 Docker socket。

脚本同时供 MCP server 和 `index_command` 使用。它把 `CBM_CACHE_DIR` 与 `CBM_RUNTIME_DIR` 固定在应用提供的项目 `HOME` 下，并关闭自动索引、watcher 和 UI。0.12.1 的 daemon 默认按账号共享，只有不同 `HOME` 或 `XDG_RUNTIME_DIR` 不足以隔离：第二个项目可能因 cache 不同而连接失败。不要把所有项目改用共享 cache 来消除报错，那会让代码地图跨项目可见。

`HOME` 必须由应用在工作区内创建并校验；脚本不是文件系统沙箱，也不替代应用的环境变量和工具白名单。Unix socket 对路径长度有限制，应保持工作区根路径较短，例如 `/app/data/workspaces`。在新平台或新上游版本上，需重新验证两个项目并发连接、索引复用和未授权工具拒绝，不能只凭 `index_status=ready` 判断隔离成功。

### 内置 codebase-memory-mcp

- **镜像**：Dockerfile 固定 `CODEBASE_MEMORY_MCP_VERSION=0.12.1`，按 `TARGETARCH`（amd64/arm64）下载上游 `-portable` 静态链接发布包，并用写死在 Dockerfile 中的 SHA-256 校验。非 portable 的 glibc 版本需要 `GLIBC_2.38`，bookworm 运行镜像只有 2.36，不能使用。镜像只复制二进制和许可证文本，不运行上游 `install`（它会写 agent 配置和 hooks）。二进制在 `/usr/local/bin/codebase-memory-mcp`，wrapper 在 `/usr/local/bin/tribunal-codebase-memory-mcp`，许可证在 `/licenses/codebase-memory-mcp/`；均属 root，服务以 UID 10001 运行。
- **默认登记**：示例配置和 Docker 配置通过 `default_extensions.mcp_servers` 登记内置平台服务器 `codebase-memory`，规则见 [默认扩展](#默认扩展default_extensions)。
- **隔离**：项目 MCP 管理器以项目工作区为 cwd 和 `HOME` 根，wrapper 把 `CBM_CACHE_DIR`、`CBM_RUNTIME_DIR` 放在该 `HOME` 下，因此每个项目使用自己的索引和 daemon，查不到其他项目的索引；不使用账号级 daemon。`auto_index`、`auto_watch`、`watcher_enabled`、`ui_enabled` 都被关闭，不启动 CBM Lens UI。
- **降级**：二进制缺失或 MCP 启动、列工具失败时，评审节点不带 MCP 工具继续执行，并在节点输出中记录代码生成的 `tool_gaps`。只有部分绑定工具可用时（服务器未公布某个工具，或工具带写入注解被跳过），可用工具照常提供，每个缺失工具各记一条 `tool_gaps`；汇总把它们写入告警，原本的 PASS 改为 INCOMPLETE，原有 BLOCKING 结论保持不变。`explore`、`validate` 和 `reasoner` 节点与 `code-review`、`review` 一样记录 `tool_gaps`，所以任一路绑定工具缺失都会使评审不会得到干净的 PASS。建索引失败不阻断 onboarding 或刷新，工作区记录 `last_error`，刷新时继续使用上一次索引。已有自定义服务器停用后，绑定中引用它的工具直接不提供，不算降级。
- **停用**：已有自定义 MCP 可在编辑窗口把「启用」改为「停用」，或调用 `PUT /api/v1/mcp-servers/{id}` 设置 `enabled: false`，重启后生效；重启和升级都保持停用。内置 `codebase-memory` 始终启用，停用请求返回 409，启动同步会恢复启用。要让新部署不登记内置服务器，把配置中的 `mcp_servers` 设为 `[]`。
- **升级版本**：同时修改 Dockerfile 中的版本和两个 SHA-256（取自上游 release 资产和 `checksums.txt`）、`third_party/codebase-memory-mcp/VERSION` 和 `LICENSE`，用 `gh attestation verify <archive> -R DeusData/codebase-memory-mcp` 核对 SLSA provenance，再运行 `go run ./scripts/licensebundle`、`go test ./scripts/...` 和 Docker smoke。新版本须重新确认 analysis profile 不含写入工具。已部署实例在新镜像首次启动时同步内置服务器的配置和默认工具白名单，见上文规则。

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
  "profile_by_label": { "release": "default" },
  "custom_rules": [
    "金额计算必须使用 decimal 类型",
    "对外 API 的错误码不得变更"
  ]
}
```

- 未设置的字段取平台 `review_defaults`。
- `profile_by_label`：MR 带指定标签时使用对应档位。评审档位只有 `default`，值只能写 `default`，其他值的映射保存时被丢弃（不报错）；已保存的旧值（指向已移除的档位）在评审时按 `default` 处理。
- `custom_rules` 作为项目规则注入 prompt 的项目背景段。
- `ignore` 与平台默认值**合并**，不是替换。

有效 ignore 会作为版本化策略保存到新 dossier。它用于 diff、仓库工具、证据、上下文生成及发布检查，不只筛选变更文件。非空策略下暂停无法证明安全读取范围的 MCP 工具和全量索引；这是安全降级，不记 `tool_gaps`，不影响评审结论；仅索引身份无法证明（ignore 为空）时，实际启用的绑定工具才记为覆盖缺口。默认 ignore 非空，默认图谱也会降级。旧索引、上下文和管理员绑定保留，策略明确为空时才保留原工具行为。升级后的旧工作流处理和输入限制见 [ignore 安全边界](../operations.md#ignore-安全边界)。

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

允许覆盖的字段（白名单）：`language`、`profile`（只能是 `default`；其他名称被忽略并在汇总中提示一次）、`ignore`（只能追加）、`rules`（追加到 `custom_rules`）、`review.skip_draft`、`review.labels_exclude`。

**不允许**覆盖：provider、预算、commit_status、MCP 和 Skill 绑定。非白名单字段忽略并在汇总中提示一次。

`language` 在合并时折叠为单行，最多 128 个字符。超长值整项忽略并写入警告，评审仍继续。有效值只作为「评审语言」放入项目背景的 scoped 数据块，不能修改平台规则、工具权限或输出结构；prompt 渲染会中和可闭合该数据块的标签。

---

## 5. 环境变量

| 变量 | 用途 |
|---|---|
| `REVIEW_MASTER_KEY` | 主密钥（base64 32 字节），加密项目 token、webhook secret 和数据库中的 MCP 配置 |
| `REVIEW_ADMIN_TOKEN` | 平台管理面的 bootstrap Bearer token，至少 32 字节；无用户 ID 和项目 grant，轮换由部署方修改环境变量并重启 |
| `TRIBUNAL_SESSION_KEY` | base64，解码后至少 32 字节；签名会话 cookie、OAuth state 和派生 CSRF。固定该值并复用 SQLite 时重启可继续登录；未设置或恰好为空字符串时回退旧名，旧名也未配置时每进程随机生成并告警，重启后旧会话失效。旧名 `CRC_SESSION_KEY` 仍兼容；新名非空时优先。新名非空但去除空白后为空（例如只含空格）或值非法时，启动失败，不回退旧值，也不生成临时 key |
| `GITLAB_OAUTH_CLIENT_ID` / `GITLAB_OAUTH_CLIENT_SECRET` | 控制台登录 |
| `ANTHROPIC_API_KEY`、`OPENAI_API_KEY`、… | 按 `providers.*.api_key_env` 自定义 |
| `LLM_SECONDARY_PROTOCOL`、`LLM_SECONDARY_BASE_URL`、`LLM_SECONDARY_MODEL`、`LLM_SECONDARY_API_KEY` | Docker 模板中注释掉的 `secondary` provider 使用。Compose 将四个变量作为可选空值透传；只有取消 `providers.secondary` 注释后才需设置真实值。应用本身只读取 `providers.secondary.api_key_env` 指向的变量名 |
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

所选全部 prompt 的内容和版本，加上代码拥有的任务渲染版本（`prompts.TaskRenderVersion`，覆盖任务布局与覆盖语义），合成一个 bundle hash，记录为 dossier 的 `prompt_hash`，并参与工作流 fingerprint。任务渲染版本不能通过 `prompts_dir` 覆盖，因此外部 prompt 文件内容不变时，升级后的 hash 仍然变化。内置 bundle 的 hash 固定在 `prompts/prompts_test.go` 的 `embeddedBundleHash` 中：修改任何内置 prompt 或任务渲染语义都必须提升对应版本、更新这个常量，并在 PR 中写明。

**图谱时效规则**：`_platform_rules.md`（v2 起）规定 codebase-memory 图谱只反映固定 `base/` 的目标分支版本；MR 改动过的文件和符号以 worktree 中 `read_file`、`grep` 的结果为准，图谱结果只作 base 侧线索，不能作为改动后的事实或 finding 的唯一证据。规则放在所有块共享的平台规则里，管理员把图谱工具绑定到其他块时同样生效。用 `prompts_dir` 覆盖 `_platform_rules.md` 的部署须自行保留这条规则。

**hash 变化的影响**：升级到 prompt bundle 不同的版本（或修改 `prompts_dir` 中的文件）后：

- 已发布的评论、commit status 和工作流记录保持不变，不会被撤回，也不会自动重新评审。
- 用旧 hash 完成、尚未发布或正在重试发布的工作流，发布时转为 `skipped`，原因 `prompt bundle changed`；手动 republish 同样会被跳过。
- 进程重启后恢复的旧 hash 工作流会被取消，保留旧 dossier 供审计，并按新 bundle 重新建立 dossier 和评审。
- 之后 MR 的下一次触发（新推送、手动触发等）使用新 hash 评审。
