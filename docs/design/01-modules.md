# 01 模块与接口

> 本文把 [ARCH.md](../../ARCH.md) §3.1 的代码布局细化到文件，并给出各包对外接口的 Go 签名。
> 签名是**设计意图**，实现时可以调整命名，但依赖方向和职责边界不要变。

---

## 1. 依赖方向

```
cmd/server ─┬─→ internal/server ──→ internal/app ──┬─→ internal/review ──→ pkg/dag
            │                                     ├─→ internal/workspace
cmd/reviewctl (只调 HTTP API)                      ├─→ internal/gitlab
                                                  ├─→ internal/jobs
                                                  └─→ internal/store
internal/review ──→ internal/agent ──→ internal/llm
                └─→ internal/mcp、internal/skill（都实现 agent.Tool）
```

硬性规则：

1. `pkg/dag` 不 import 任何 `internal/` 包，也不认识 GitLab、MR、finding 等概念。
2. `internal/llm` 不 import `agent`、`review`；`internal/agent` 不 import `review`。
3. `internal/gitlab` 只负责与 GitLab 通信和 diff 处理，不 import `review`。
4. 接口定义在**使用方**（例如 `review` 需要的 GitLab 能力，在 `review` 包里定义一个小接口），便于测试替换。
5. `internal/app` 是组合根之下的服务层，负责把 webhook 事件翻译成工作流；HTTP handler 不直接操作 DAG。

---

## 2. 目录与文件

> 实现偏差：本节文件清单是设计目标，当前实现有几处不同：
> - 评审发布器在 `internal/app/publisher.go`，不在 `internal/gitlab/publish.go`。
> - GitLab 发布和评论命令处理在 `internal/app` 层（`publisher.go`、`webhook_service.go`、`command_worker.go`），不在 `internal/gitlab/publish.go` / `command.go`。
> - `pkg/dag/validate.go` 未单独存在，图校验合并在 `pkg/dag/graph.go`。

```
cmd/
  server/main.go                 读配置 → 建依赖 → 启动 HTTP、job worker、恢复未完成工作流
  reviewctl/main.go              管理 CLI，只通过 HTTP API 操作（项目注册/列表、用户停用/撤销、项目授权、触发评审、查看状态）
internal/
  config/
    config.go                    配置结构体、默认值、Validate()
    load.go                      YAML 读取、${ENV} 展开、多文件合并
  server/
    server.go                    http.Server、路由、中间件（request id、日志、recover）
    webhook.go                   POST /webhooks/gitlab：校验 → 去重 → 入队
    auth.go                      GitLab OAuth 登录、会话 cookie、API token
    access.go                    路由授权策略与项目 grant 检查
    api_admin.go                 平台管理面的用户停用/凭据撤销与项目授权
    api_projects.go              项目相关 API
    api_reviews.go               评审（工作流）相关 API
    api_workspace.go             工作区、上下文文档
    api_extensions.go            MCP、Skill、块绑定
    api_system.go                providers、profiles、统计、健康检查
    sse.go                       工作流进度推送（Server-Sent Events）
    web.go                       embed 前端构建产物
  app/
    review_service.go            MR 事件 → 工作流：幂等键、取消旧工作流、启动新工作流
    project_service.go           项目注册、token 校验与加密、自动创建 webhook
    workspace_service.go         onboarding 与刷新编排
    feedback_service.go          周期性收集 👍/👎 与 resolve 信号
  jobs/
    queue.go                     持久化任务队列接口与实现（基于 store）
    worker.go                    worker 池：Claim → 执行 → Complete/Fail，租约续期
  gitlab/
    client.go                    client-go 封装；按项目缓存 client
    webhook.go                   webhook payload 结构体与解析
    diff.go                      diff 解析与 DiffIndex（行号 → GitLab position）
    publish.go                   汇总评论 upsert、行内讨论、commit status
    command.go                   解析评论命令（/review …）
  repo/
    git.go                       git 命令封装（mirror、fetch、worktree）
    env.go                       安全环境变量与认证注入
  workspace/
    manager.go                   工作区目录、读写锁、生命周期
    context.go                   上下文文档读写与锁定
  llm/
    llm.go                       Client 接口与统一类型
    errors.go                    错误分类与重试策略
    registry.go                  provider 注册、限流、重试包装
    openai_chat.go               Chat Completions adapter
    openai_responses.go          Responses adapter（无状态）
    anthropic.go                 Messages adapter
  agent/
    tool.go                      Tool 接口与注册表
    loop.go                      工具循环 Run()
    submit.go                    submit_result 工具：schema 校验与回退
    builtin.go                   内置只读工具：read_file、grep、glob、list_dir、git_log、git_blame
    sandbox.go                   路径约束（只允许 worktree / base / skill 目录）
  mcp/
    config.go                    Claude Code 兼容的 mcpServers 解析
    pool.go                      按工作区的 stdio 进程池、HTTP client
    tools.go                     把 MCP 工具适配为 agent.Tool（命名 mcp__<server>__<tool>）
  skill/
    loader.go                    扫描 SKILL.md、解析 frontmatter
    tool.go                      load_skill 工具
  review/
    dossier.go                   材料包构建
    blocks.go                    块定义与展开（profile → GraphSpec）
    executor.go                  dag.NodeExecutor 实现：按节点 kind 分派
    nodes_agent.go               agent 类节点：plan、code-review、review.*、reasoner、verify、synthesize、onboarding
    nodes_code.go                代码类节点：gather、adjudicate
    contracts.go                 各节点输出结构体与 JSON Schema
    adjudicate.go                裁决不变量 I1–I7
    dedupe.go                    finding 去重合并
    prompt.go                    prompt 组装（稳定前缀在前）
    render.go                    汇总评论与行内评论的 Markdown 渲染
  store/
    store.go                     各聚合的仓储接口
    sqlite/                      SQLite 实现（modernc.org/sqlite）
    postgres/                    Postgres 实现（pgx）
    migrations/                  *.sql（embed），按序号执行
  secret/
    secret.go                    AES-GCM 加解密，主密钥来自环境变量
pkg/
  dag/
    types.go                     状态、规格、事件类型
    transitions.go               合法迁移表与断言
    graph.go                     图构建、拓扑分层、环检测
    validate.go                  唯一的严格校验入口
    engine.go                    Engine：Create / Run / Cancel / Recover / Extend
    scheduler.go                 ready 集合与并发许可
    events.go                    事件定义与序列化
    projector.go                 事件 → 读模型
prompts/
  main.plan.md  main.synthesize.md  code-review.md  review.standards.md  review.intent.md
  review.gate.md  reasoner.md  verify.md  onboarding.md  _platform_rules.md
web/                             Web 控制台（M6，见 07-web-console.md）
```

---

## 3. pkg/dag

```go
package dag

type NodeStatus string

const (
	NodePending   NodeStatus = "pending"
	NodeQueued    NodeStatus = "queued"
	NodeRunning   NodeStatus = "running"
	NodeCompleted NodeStatus = "completed"
	NodeFailed    NodeStatus = "failed"
	NodeAborted   NodeStatus = "aborted"
	NodeSkipped   NodeStatus = "skipped"
)

type WorkflowStatus string

const (
	WorkflowPending   WorkflowStatus = "pending"
	WorkflowRunning   WorkflowStatus = "running"
	WorkflowCompleted WorkflowStatus = "completed"
	WorkflowFailed    WorkflowStatus = "failed"
	WorkflowCancelled WorkflowStatus = "cancelled"
)

// NodeSpec 是一个节点的静态规格。Kind 由使用方解释（dag 包不认识具体 kind）。
type NodeSpec struct {
	ID           string            `json:"id"`
	Kind         string            `json:"kind"`
	DependsOn    []string          `json:"depends_on,omitempty"`
	Required     bool              `json:"required"`
	Condition    string            `json:"condition,omitempty"`     // 例：gate.output.verdict == "BLOCKING"
	InputMapping map[string]string `json:"input_mapping,omitempty"` // 例："findings": "cr_a.output.findings"
	Params       json.RawMessage   `json:"params,omitempty"`        // kind 专用参数
	Timeout      time.Duration     `json:"timeout,omitempty"`
	Expands      bool              `json:"expands,omitempty"`       // 完成后调用 Expander 生成新节点
}

type Budget struct {
	MaxNodes   int           `json:"max_nodes"`
	MaxExtends int           `json:"max_extends"`
	Deadline   time.Duration `json:"deadline"`
	MaxTokens  int64         `json:"max_tokens"`
}

type GraphSpec struct {
	Nodes  []NodeSpec `json:"nodes"`
	Budget Budget     `json:"budget"`
}

type NodeInput struct {
	WorkflowID string
	Node       NodeSpec
	Attempt    int
	Inputs     map[string]json.RawMessage // 按 InputMapping 解析好的上游输出
	Meta       map[string]string          // 工作流元数据（project_id、mr_iid、fingerprint…）
}

type NodeOutput struct {
	Data  json.RawMessage
	Usage Usage
}

type Usage struct {
	InputTokens, OutputTokens int64
	CostMicros                int64
}

// NodeExecutor 由使用方实现（internal/review）。返回 error 即节点 failed。
type NodeExecutor interface {
	Execute(ctx context.Context, in NodeInput) (NodeOutput, error)
}

// Expander 在 Expands=true 的节点提交终态前调用。
// 扩展校验成功后，与 node.completed 在同一次 Append 中原子持久化。
type DepPatch struct {
	Node string   `json:"node"`
	Add  []string `json:"add"`
}

type Extension struct {
	Nodes      []NodeSpec `json:"nodes"`
	DepPatches []DepPatch `json:"dep_patches,omitempty"`
	Source     string    `json:"source"` // fan_out | extend；仅 extend 消耗 MaxExtends
	FromNode   string    `json:"from_node"`
}

type Expander interface {
	Expand(ctx context.Context, wf WorkflowView, from NodeSpec, out json.RawMessage) (Extension, error)
}

type EventStore interface {
	// Append 带乐观并发：expectRev 不等于当前 graph_rev 时返回 ErrStaleRevision。
	Append(ctx context.Context, wfID string, expectRev int64, evs ...Event) (int64, error)
	Load(ctx context.Context, wfID string) ([]Event, error)
	ListUnfinished(ctx context.Context) ([]string, error)
}

type Options struct {
	MaxConcurrentNodes int           // 全局节点并发，默认 8
	DefaultNodeTimeout time.Duration // 默认 10m
	Clock              func() time.Time
}

type Engine struct{ /* ... */ }

func NewEngine(es EventStore, exec NodeExecutor, exp Expander, opts Options) *Engine

func (e *Engine) Create(ctx context.Context, spec GraphSpec, meta map[string]string) (string, error)
func (e *Engine) Run(ctx context.Context, wfID string) error // 阻塞到终态；ctx 取消视同 Cancel
func (e *Engine) Cancel(ctx context.Context, wfID, reason string) error
func (e *Engine) Extend(ctx context.Context, wfID string, expectRev int64, nodes []NodeSpec) error
func (e *Engine) Recover(ctx context.Context) error // 启动时调用：重排未完成工作流
func (e *Engine) Snapshot(ctx context.Context, wfID string) (WorkflowView, error)
```

`Condition` 只支持最小语法：`<node>.output.<path> (==|!=) <literal>`，以及 `&&`、`||`。不引入表达式引擎。

---

## 4. internal/llm

```go
package llm

type Role string // system | user | assistant | tool

type PartType string // text | tool_call | tool_result | reasoning

type Part struct {
	Type       PartType
	Text       string
	ToolCall   *ToolCall
	ToolResult *ToolResult
	Reasoning  *Reasoning
}

type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

type ToolResult struct {
	CallID  string
	Content string
	IsError bool
}

// Reasoning 保存各协议的推理块原文，下一轮原样回传。
// Anthropic：thinking / redacted_thinking（含 signature）；Responses：reasoning item（含 encrypted_content）。
type Reasoning struct {
	Protocol string
	Raw      json.RawMessage
	Summary  string // 仅用于日志展示
}

type Message struct {
	Role  Role
	Parts []Part
}

type Block struct {
	Text  string
	Cache bool // 标记为可缓存前缀（Anthropic cache_control；OpenAI 自动缓存只需保证前缀稳定）
}

type ToolDef struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

type Request struct {
	System      []Block
	Messages    []Message
	Tools       []ToolDef
	ToolChoice  string // auto | required | none | <tool name>
	MaxTokens   int
	Temperature *float64
	Reasoning   *ReasoningOpts
}

type ReasoningOpts struct {
	Effort       string // low | medium | high（Responses）
	BudgetTokens int    // Anthropic thinking budget
}

type StopReason string // end_turn | tool_use | max_tokens | refusal | other

type Response struct {
	Message    Message
	StopReason StopReason
	Usage      Usage
	Model      string
	RequestID  string
}

type Usage struct {
	InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens, ReasoningTokens int64
}

type Client interface {
	Generate(ctx context.Context, req *Request) (*Response, error)
	Info() ProviderInfo
}

type ProviderInfo struct {
	Name, Protocol, Model, BaseURL string
}
```

错误分类（`errors.go`）：

```go
type ErrKind string // rate_limited | overloaded | context_too_long | auth | bad_request | transient | canceled

type Error struct {
	Kind       ErrKind
	Status     int
	RetryAfter time.Duration
	Err        error
}
```

`registry.go` 用装饰器包住每个 adapter：

- 并发信号量（`max_concurrency`）+ `golang.org/x/time/rate`（`rpm`）。
- 重试：`rate_limited`、`overloaded`、`transient` 指数退避，最多 4 次，尊重 `RetryAfter`；其余错误不重试。
- 每次调用写入 `llm_calls`（见 02-data-model.md）。

adapter 要点：

| adapter | 要点 |
|---|---|
| `openai_chat.go` | `tool_calls` ↔ `ToolCall`；`role=tool` ↔ `ToolResult`；不支持推理块回传，`Reasoning` 丢弃 |
| `openai_responses.go` | `store=false`，`include=["reasoning.encrypted_content"]`；输入为 item 列表，`function_call` / `function_call_output` / `reasoning` 三类 item 与统一类型互转 |
| `anthropic.go` | `system` 用 block 数组，`Cache=true` 的 block 加 `cache_control: ephemeral`；开启 thinking 时 `temperature` 不传；`thinking` 块原样回传 |

---

## 5. internal/agent

```go
package agent

type Tool interface {
	Def() llm.ToolDef
	Call(ctx context.Context, input json.RawMessage) (Result, error)
}

type Result struct {
	Content   string
	IsError   bool
	Truncated bool
}

type SubmitSpec struct {
	Schema json.RawMessage                      // 交给模型的 input_schema
	Decode func(raw json.RawMessage) (any, error) // 严格解码 + 业务校验；错误文本回给模型
}

type RunSpec struct {
	Client        llm.Client
	System        []llm.Block
	Prompt        string
	Tools         []Tool
	Submit        SubmitSpec
	MaxTurns      int // 默认 30
	MaxToolOutput int // 单次工具输出字符上限，默认 20000
	Temperature   *float64
	Reasoning     *llm.ReasoningOpts
}

type RunResult struct {
	Output     any
	Usage      llm.Usage
	Turns      int
	Transcript []llm.Message
}

var ErrNoSubmit = errors.New("agent finished without submit_result")

func Run(ctx context.Context, spec RunSpec) (*RunResult, error)
```

循环规则：

1. 每轮调用 `Generate`；模型调用工具就执行并回填结果；工具名不存在时回填错误结果，不中断。
2. 模型调用 `submit_result`：`Decode` 成功即结束；失败则把错误作为 `IsError` 的工具结果回给模型，最多重试 2 次。
3. 模型没有调用 `submit_result` 就结束（`end_turn`）：追加一条提示「必须调用 submit_result 提交结果」，补问 1 次；仍然没有则返回 `ErrNoSubmit`。
4. 达到 `MaxTurns`：最后一轮把 `ToolChoice` 设为 `submit_result` 强制提交。
5. 工具输出超出上限时截断，并在末尾注明 `[truncated: N chars omitted]`。

内置只读工具（`builtin.go`）：

| 工具 | 输入 | 说明 |
|---|---|---|
| `read_file` | `path`, `start_line?`, `end_line?`, `root?`(`head`/`base`) | 带行号输出；默认读 worktree（head） |
| `grep` | `pattern`, `path?`, `glob?`, `max_results?` | 正则搜索，优先调用系统 `rg`，没有则回退 Go 实现 |
| `glob` | `pattern` | 列出匹配文件 |
| `list_dir` | `path` | 一层目录 |
| `git_log` | `path?`, `max?` | 只读 `git log --oneline` |
| `git_blame` | `path`, `start_line`, `end_line` | 只读 |

所有路径经 `sandbox.go` 解析：拒绝绝对路径、`..` 逃逸和符号链接逃逸。

---

## 6. internal/gitlab

```go
package gitlab

type API interface {
	GetProject(ctx context.Context, pid int64) (*Project, error)
	GetMR(ctx context.Context, pid, iid int64) (*MR, error)
	ListMRDiffs(ctx context.Context, pid, iid int64) ([]FileDiff, error)           // 自动翻页
	LatestMRVersion(ctx context.Context, pid, iid int64) (*Version, error)         // base/start/head sha
	ListMRPipelines(ctx context.Context, pid, iid int64) ([]Pipeline, error)
	ListPipelineJobs(ctx context.Context, pid, pipelineID int64) ([]Job, error)
	ListClosingIssues(ctx context.Context, pid, iid int64) ([]Issue, error)
	GetRawFile(ctx context.Context, pid int64, path, ref string) ([]byte, error)   // 读目标分支上的配置和 AGENTS.md
	ListMRNotes(ctx context.Context, pid, iid int64) ([]Note, error)
	CreateMRNote(ctx context.Context, pid, iid int64, body string) (*Note, error)
	UpdateMRNote(ctx context.Context, pid, iid, noteID int64, body string) error
	CreateDiscussion(ctx context.Context, pid, iid int64, d NewDiscussion) (*Discussion, error)
	SetCommitStatus(ctx context.Context, pid int64, sha string, st CommitStatus) error
	ListNoteAwards(ctx context.Context, pid, iid, noteID int64) ([]Award, error)
	GetMemberAccess(ctx context.Context, pid, userID int64) (int, error)           // 校验评论命令发起人权限
	CreateProjectHook(ctx context.Context, pid int64, h HookSpec) (int64, error)
}

type TokenSource interface {
	ProjectToken(ctx context.Context, pid int64) (string, error)
}

func New(baseURL string, tokens TokenSource, opts ...Option) API
```

### DiffIndex：行号 → GitLab position

```go
type Side string // new | old

type Position struct {
	BaseSHA, StartSHA, HeadSHA string
	OldPath, NewPath           string
	OldLine, NewLine           int // 0 表示不设置
}

type DiffIndex struct{ /* path → 每个 hunk 的行映射 */ }

func BuildDiffIndex(v Version, diffs []FileDiff) *DiffIndex

// Locate 把 (path, line, side) 映射成 GitLab 可接受的 position；不在 diff 中返回 false。
func (d *DiffIndex) Locate(path string, line int, side Side) (Position, bool)
```

GitLab 对 position 的要求（实现时必须满足，否则返回 400）：

| 行类型 | 需要设置 |
|---|---|
| 新增行（`+`） | 只设 `new_line` |
| 删除行（`-`） | 只设 `old_line` |
| hunk 内的上下文行（未改动） | `old_line` 和 `new_line` **都要设** |
| hunk 外的行 | 不能发行内评论，降级进汇总 |

重命名文件：`old_path` 和 `new_path` 分别填写。

---

## 7. internal/repo

```go
package repo

type Auth struct{ Token string }

type Git struct {
	Bin     string // 默认 "git"
	BaseEnv []string
}

func (g *Git) EnsureMirror(ctx context.Context, dir, remoteURL string, a Auth) error
func (g *Git) Fetch(ctx context.Context, mirror string, a Auth, refspecs ...string) error
func (g *Git) AddWorktree(ctx context.Context, mirror, dir, sha string) error
func (g *Git) RemoveWorktree(ctx context.Context, mirror, dir string) error
func (g *Git) Checkout(ctx context.Context, dir, sha string) error
```

安全约束（`env.go`）：

- token **不出现在命令行参数和 remote URL 里**。用环境变量注入 git 配置：
  `GIT_CONFIG_COUNT=1`、`GIT_CONFIG_KEY_0=http.extraHeader`、`GIT_CONFIG_VALUE_0=Authorization: Basic base64("oauth2:<token>")`。
- 固定附加：`GIT_TERMINAL_PROMPT=0`、`GIT_LFS_SKIP_SMUDGE=1`、`-c core.hooksPath=/dev/null`、`-c protocol.file.allow=never`、`-c submodule.recurse=false`。
- MR 引用：`refs/merge-requests/<iid>/head:refs/mr/<iid>`。

---

## 8. internal/workspace

```go
package workspace

type Manager struct{ /* root、按项目的 *Workspace */ }

func NewManager(root string, git *repo.Git) *Manager
func (m *Manager) Get(pid int64) *Workspace

type Workspace struct {
	ProjectID int64
	Root      string
}

func (w *Workspace) Lock(ctx context.Context) (unlock func(), err error)  // 刷新时
func (w *Workspace) RLock(ctx context.Context) (unlock func(), err error) // 评审时
func (w *Workspace) MirrorDir() string
func (w *Workspace) BaseDir() string
func (w *Workspace) BaseSHA() string
func (w *Workspace) NewRun(ctx context.Context, wfID, headSHA string) (dir string, cleanup func(), err error)
func (w *Workspace) MCPConfigPath() string
func (w *Workspace) SkillsDir() string
```

首版单实例：锁用进程内 `sync.RWMutex`（每项目一把）。多实例时换成数据库咨询锁，接口不变。

---

## 9. internal/jobs

```go
package jobs

type Job struct {
	ID       string
	Kind     string          // review.run | review.cancel | workspace.onboard | workspace.refresh | feedback.collect
	Key      string          // 去重键；同 key 有排队任务时 Enqueue 返回已有 ID
	Payload  json.RawMessage
	RunAfter time.Time
	Attempts int             // 领取时递增，用于拒绝旧执行结果
	LeaseOwner string
}

type Queue interface {
	Enqueue(ctx context.Context, j Job) (id string, dup bool, err error)
	Claim(ctx context.Context, worker string, kinds []string, lease time.Duration) (*Job, error)
	Heartbeat(ctx context.Context, j Job, lease time.Duration) error
	Complete(ctx context.Context, j Job) error
	Fail(ctx context.Context, j Job, cause error, retryAt *time.Time) error
	CancelByKey(ctx context.Context, key string) (int, error)
	Recover(ctx context.Context) (int, error)
}

type Handler func(ctx context.Context, j *Job) error
```

job 和 DAG 的分工：job 是**粗粒度、持久化的入口**（webhook → job），保证事件不丢；job handler 负责创建并驱动一个 DAG 工作流。DAG 自己负责节点级的状态和恢复。

M0 中 `Job` 是 `store.Job` 的别名，完整持久化字段见实现。结算和续租校验 `LeaseOwner`、`Attempts` 及租约有效期。`CancelByKey` 只取消精确 key 的 queued 任务，不做前缀匹配。同 key 允许一个 running 和一个 queued 后继，后继在前者结束后才能领取。worker 只领取已注册处理器的种类；当前评审与工作区任务均已注册处理器。

`store.EnqueueLatest` 供已授权的评审命令使用：与 webhook 入队一样，原子替换同 key、同 kind 的 queued 内容，保留 running 内容。替换时保留单调递增的 attempt，并把 `max_attempts` 设置为当前 attempt 加上新 payload 的重试预算；旧 worker 不会因编号重用而结算新任务。

---

## 10. internal/review（编排核心）

```go
package review

type Dossier struct {
	Project     ProjectRef
	MR          MRMeta          // 标题、描述、作者、源/目标分支、标签、draft
	Version     gitlab.Version  // base/start/head sha
	DiffSummary DiffSummary     // included 文件/行数、变更总数、omitted 路径与原因
	Files       []FileChange    // 路径、状态（added/modified/deleted/renamed）、diff、行数
	Ignored     []string        // 被 ignore 规则过滤的文件；不是覆盖缺口
	Pipeline    *PipelineSummary
	Issues      []IssueRef
	ContextDoc  string          // 工作区上下文文档（base 版本）
	RepoRules   string          // 目标分支上的 AGENTS.md / CLAUDE.md
	Config      EffectiveConfig // 平台 + 项目 + 仓库配置合并后的结果
	HeadDir     string          // worktree（head）
	BaseDir     string          // 工作区 base 检出
	Incremental *IncrementalRange // 增量评审：上次已评审的 head_sha
	Fingerprint string
}

func BuildDossier(ctx context.Context, deps DossierDeps, req ReviewRequest) (*Dossier, error)

// BuildGraph 根据 profile 生成初始 GraphSpec（见 04-dag-and-contracts.md §3）。
func BuildGraph(p Profile, d *Dossier) (dag.GraphSpec, error)

type Executor struct{ /* llm.Registry、tools、prompts、store … */ }

func (e *Executor) Execute(ctx context.Context, in dag.NodeInput) (dag.NodeOutput, error) // 按 Kind 分派
func (e *Executor) Expand(ctx context.Context, wf dag.WorkflowView, from dag.NodeSpec, out json.RawMessage) (dag.Extension, error)

// Publish 在工作流完成后由 app 层调用；内部先做 fingerprint 校验。
func Publish(ctx context.Context, deps PublishDeps, wfID string) (*PublishReport, error)
```

---

## 11. 通用约定

| 项 | 约定 |
|---|---|
| context | 所有 I/O 函数第一个参数是 `context.Context`，取消要能传到 LLM 请求和 git 子进程 |
| 错误 | `fmt.Errorf("...: %w", err)` 包装；对外 API 的错误码在 `server` 层映射 |
| 日志 | `log/slog`，结构化字段固定：`wf_id`、`node_id`、`project_id`、`mr_iid`、`job_id`；**禁止**打印 token、API key、完整 prompt |
| ID | UUID v7（有序，适合作主键和排序） |
| 时间 | 一律 UTC；SQLite 中存 RFC 3339 字符串 |
| 时钟 | 需要时间的组件注入 `func() time.Time`，便于测试 |
| 全局状态 | 不使用包级可变变量；依赖都从 `main` 注入 |
| 测试 | 表驱动；`pkg/dag` 迁移表与级联规则覆盖率要求 100%；GitLab 与 LLM 用接口假实现，另备录制回放测试 |

---

## 12. 推荐依赖

| 用途 | 库 | 许可证 |
|---|---|---|
| HTTP 路由 | 标准库 `net/http`（Go 1.22+ 方法与路径模式） | BSD-3 |
| SQLite | `modernc.org/sqlite`（纯 Go，免 cgo，便于静态编译） | BSD-3 |
| Postgres | `github.com/jackc/pgx/v5` | MIT |
| 迁移 | 自写 embed 执行器，或 `github.com/pressly/goose/v3` | MIT |
| YAML | `go.yaml.in/yaml/v3` | MIT / Apache-2.0 |
| JSON Schema 生成 | `github.com/invopop/jsonschema`（从 Go 结构体生成 submit_result 的 schema） | MIT |
| ID | `github.com/google/uuid`（v7） | BSD-3 |
| 限流 | `golang.org/x/time/rate` | BSD-3 |
| 并发 | `golang.org/x/sync/errgroup` | BSD-3 |
| 指标（可选） | `github.com/prometheus/client_golang` | Apache-2.0 |
| 测试对比 | `github.com/google/go-cmp` | BSD-3 |

以上许可证以 CI 中 `licensecheck` 和 `licensebundle -check` 的结果为准；发布声明是 `licenses/THIRD_PARTY_NOTICES.txt`。
