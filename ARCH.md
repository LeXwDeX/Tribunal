# ARCH — GitLab CE 自动评审服务设计基线

> 状态：**设计基线 v0.2（讨论稿）**。主体功能已有本地实现，管理 API 的游标分页、项目/评审摘要、反馈原始计数、人工标注与质量指标、上下文版本与保留清理均已落地；仓库配置在评审 worker 中合并。2026-10-25 的有界隔离实例（Linux/amd64、真实 GitLab 19.5.1 CE）已验证 GitLab 自动投递 webhook、真实 OAuth、项目授权/CSRF、应用内 MCP 调用和 onboarding CLI 索引；连续两次实际 deep 均为 0 call errors、0 failed nodes，verdict 为预期的 BLOCKING。尚未验收或实现：协议、provider 与 MCP HTTP backend 的全部组合，大型仓库与生产负载，操作系统级文件沙箱，物理设备/完整 WCAG；Postgres 未实现。实际能力、未验证项与限制见 [README.md](README.md)，最终交付事实见 PR #30。
> 本文汇总了立项讨论中的全部结论。标注「已定」的条目是用户确认过的决策；标注「建议默认」的条目是讨论中提出、尚待确认的方案，见 §16 待决事项。
> 实现层面的细化（接口签名、表结构、API、流程、配置）见 [docs/design/](docs/design/README.md)。

---

## 1. 定位

一个开源、MIT 协议、基于 Go 的 Web 服务，接入 **GitLab CE（社区版）**，对 Merge Request（下文统称 MR，即 PR）做 AI 自动评审：

- 通过**项目级 Token** 授权，既能被 GitLab webhook 回调触发，也能主动触发评审，并把结果回写到 MR。
- 服务拥有 clone 项目代码的权限，评审时不只看 diff，也能读取完整仓库上下文。
- 评审由一个**主 agent 单入口**统一编排：主 agent 派发多种评审模式，经过跨模型或同模型的**对抗性核验**后，回到主 agent 评估，最终输出结论到 MR。
- 编排层以 **DAG 引擎**为唯一执行架构。当前 `pkg/dag` 按本设计独立实现，用户已于 2026-10-23 确认接受该实现用于本地验收，不要求移植；未来如移植，授权确认见 §13。
- 每个项目有一个常驻**工作区**：模型可以预先生成 AGENTS.md / CLAUDE.md 等项目上下文，并通过 **MCP 和 Skill** 接入外部能力（如 codebase-memory-mcp 的代码地图检索）。这些内容只在平台内部使用（见 §8）。

### 非目标

- 不执行仓库代码，不在服务内跑测试或构建。验证证据来自 GitLab CI 的结果。
- 不修改仓库，不推送代码，不自动合并 MR。平台生成的上下文文档、索引等只保存在平台内部，不提交到仓库。
- 不支持订阅制账号登录（ChatGPT、Claude 订阅等 OAuth 方式），只支持 API Key。
- 不做 IDE 或交互式功能（暂停、单步、人工裁决超时等）。

---

## 2. 决策总表

| # | 决策 | 状态 |
|---|---|---|
| D1 | 技术栈：Go Web 服务 | 已定 |
| D2 | 对接 GitLab CE，使用项目级 Token 授权，具备 clone 权限 | 已定 |
| D3 | 许可证 MIT，依赖必须与 MIT 兼容 | 已定 |
| D4 | 模型只用 API Key 计费方式，不支持订阅登录 | 已定 |
| D5 | 支持三种协议：OpenAI Chat Completions、OpenAI Responses、Anthropic Messages | 已定 |
| D6 | 能用官方 SDK 就用官方 SDK（`openai-go/v3`、`anthropic-sdk-go`、GitLab `client-go`） | 已定 |
| D7 | 评审模式：`reasoner`、`review`、`verify`、`code-review`（传统） | 已定 |
| D8 | 支持跨模型和同模型的对抗性审核 | 已定 |
| D9 | 主 agent 单入口 → 多模式评审 → 回到主 agent 评估 → 输出结论到 MR | 已定 |
| D10 | 参考 OpenCode-GraphAgent 的 DAG 引擎；当前 `pkg/dag` 为独立实现，用户已确认按本地实现验收，不要求移植。未来移植仍需先完成授权确认（§13） | 已定 |
| D11 | `verify` 在 MR 场景下定位为**挑战者**：对单条 finding 做只读反证，不跑测试 | 建议默认 |
| D12 | 驱动方式：规划 → 执行 → 汇总，允许有限轮补派（extend），不用自由工具循环 | 建议默认 |
| D13 | 部署形态：单个二进制，默认 SQLite、可选 Postgres，提供 Docker 镜像，任务队列在进程内 | 建议默认 |
| D14 | 评审图由**块模板**生成，主 agent 只输出受限的规划参数，不手写整张图 | 建议默认 |
| D15 | 采用项目工作区，允许模型生成 AGENTS.md / CLAUDE.md 等上下文文档；**生成内容只存平台内部，不提交到仓库** | 已定 |
| D16 | 支持 MCP 接入（配置格式兼容 Claude Code），首个接入 codebase-memory-mcp 用于代码地图检索 | 已定 |
| D17 | 支持 Skill 接入（格式兼容 Claude Code Skill） | 已定 |
| D18 | Web 控制台：SPA（Vite + React + TypeScript）embed 进二进制；用 GitLab OAuth 登录，平台角色分 admin / viewer，项目业务数据另由本地 read / manage grant 控制 | 建议默认 |
| D19 | DAG 节点分 agent 节点和代码节点（如 gather、adjudicate）两类，对引擎透明 | 建议默认 |

---

## 3. 总体架构

```
GitLab webhook / 评论命令 / 手动 API
        │  校验 X-Gitlab-Token → 入队 → 立即返回 200
        ▼
┌─[采集阶段 · 纯代码]──────────────────────────────────────────────┐
│ MR 元数据、diff、versions(base/start/head sha)、pipeline 状态、  │
│ 关联 issue、worktree、工作区上下文文档(base) → MR 材料包         │
└───────────────────────────────────────────────────────────────────┘
        ▼
┌─[DAG 工作流 · 每个 (MR, head_sha) 一个]──────────────────────────┐
│ plan(主 agent) ──fan_out──┬─ code-review      (provider A)       │
│                           ├─ review:standards ┐                  │
│                           ├─ review:intent    ┘─ review 裁决     │
│                           └─ reasoner (可选)                     │
│                                   │                              │
│              adversarial：verify 跨模型逐条反证                  │
│                                   │                              │
│        synthesize(主 agent)：逐条采纳/反驳 → 结论                 │
│           └─ 可申请 extend 补派节点（有上限）                     │
└───────────────────────────────────────────────────────────────────┘
        ▼
┌─[发布阶段 · 纯代码]──────────────────────────────────────────────┐
│ fingerprint 比对 → 行号校验 → 汇总评论就地更新 → 行内讨论 →      │
│ commit status (ai-review)                                        │
└───────────────────────────────────────────────────────────────────┘
```

原则：**LLM 负责判断，代码负责执行和兜底。** 采集、调度、校验、发布都不交给 LLM。

另有一条独立的工作区流程：项目注册或目标分支有新 push 时，运行 onboarding 工作流（建索引、生成上下文文档），见 §8。

### 3.1 代码布局（建议）

```
cmd/server/            入口
cmd/reviewctl/         管理 CLI（只调用 HTTP API）
internal/server/       HTTP、webhook handler、管理 API、SSE、embed 前端
internal/app/          服务层：MR 事件 → 工作流、项目注册、工作区编排、反馈收集
internal/jobs/         持久化任务队列与 worker
internal/secret/       AES-GCM 加解密
internal/gitlab/       client-go 封装、webhook payload、diff 与 position 计算、发布器
internal/repo/         bare mirror 缓存、worktree、安全的 git 参数
internal/workspace/    项目工作区：base 检出、上下文文档、生命周期与读写锁
internal/llm/          LLMClient 接口 + 三个协议 adapter
internal/agent/        工具循环、只读工具集、submit_result 结构化交付
internal/mcp/          MCP 客户端、进程池、Claude 兼容配置解析
internal/skill/        Skill 加载与按块挂载
internal/review/       块模板、节点执行器、契约、裁决不变量、渲染
internal/store/        数据库访问与迁移（SQLite / Postgres）
internal/config/       服务配置与仓库内配置解析
pkg/dag/               DAG 引擎核心（场景无关，可被 agencoding 复用）
prompts/               各 agent 的 prompt（embed 进二进制）
web/                   Web 控制台（M6）
```

`pkg/dag` 不依赖任何评审或 GitLab 概念，`internal/review` 在它之上定义评审专用的块和执行器。文件级划分和各包接口见 [docs/design/01-modules.md](docs/design/01-modules.md)。

---

## 4. GitLab 集成

### 4.1 授权

- 以**项目级 Project Access Token** 为主。每个项目一个 token，服务维护 `project_id → token` 注册表，token 加密存储（AES-GCM，主密钥来自环境变量）。
- 所需 scope 和角色：

| 能力 | 需要 |
|---|---|
| clone / fetch | `read_repository`，至少 Reporter |
| 读 MR、diff、versions、pipeline | `read_api` 或 `api` |
| 发评论、行内讨论 | `api`，私有项目至少 Reporter |
| 写 commit status、resolve 讨论 | `api`，一般需要 Developer |

- 是否同时支持 Group Token 或 bot 账号 PAT，见 §16。

平台登录授权与 GitLab 项目 token 分离：

- 用户首次通过 GitLab OAuth 登录后建立本地 `users` 行；`auth.bootstrap_admins` 只决定平台管理角色。项目业务数据的读取和写入由本地 `project_grants` 的 `read` / `manage` 决定，默认无权限，平台管理员也不例外。
- 平台管理员可以管理用户、项目授权和系统配置；读取项目、评审、工作区、统计、SSE 或项目扩展仍需该项目 grant。无 grant 或项目不存在统一返回 404，只有 `read` 时执行写操作返回 403。
- `auth.bootstrap_token_env` 指向的静态 Bearer 只用于平台管理面，没有本地用户 ID 和 grant，不接受项目业务路由。它的轮换由部署方更新环境变量并重启完成。
- 用户被本地停用后不能登录、使用已有会话/API token 或获得新凭据；撤销凭据与停用都是本地控制。GitLab 组成员关系变化不会自动同步到 CRC，退组后仍需删除 grant 或停用本地用户。

### 4.2 触发

| 来源 | 条件 | 说明 |
|---|---|---|
| Merge Request Hook | `open`、`reopen`；`update` 且带 `oldrev`（表示有新 push） | 主路径 |
| Note Hook | 评论 `/review`、`/review <profile>` | 手动重跑或切换评审档位 |
| 过滤 | Draft 状态、标签、目标分支 | 由项目配置决定 |
| 手动 API | 服务自身的 REST 接口 | CI job 或运维调用 |
| Push Hook | 目标分支有新 push | 刷新项目工作区（见 §8.1），不触发评审 |

- webhook handler 只做三件事：校验 `X-Gitlab-Token`、入队、返回 200。GitLab 有 webhook 超时限制，连续失败还会自动禁用 hook。
- **投递去重**：按 GitLab event UUID 去重；显式新命令可对相同 head/profile 创建新代次。同一个 MR 有新 push 时，旧工作流转为 `cancelled`，旧结果作废。发布去重边界见 §6.7。
- **增量评审**：记录上次已评审的 `head_sha`，默认只评审新增变更，`/review full` 强制全量。

### 4.3 取 diff 与代码

- diff：`GET /projects/:id/merge_requests/:iid/diffs`（`/changes` 已废弃）。
- 三个 sha：`GET .../versions` 取 `base_sha`、`start_sha`、`head_sha`。行内评论必须带这三个值。
- 代码：在项目工作区（见 §8.1）中维护 bare mirror，评审时 `git fetch origin refs/merge-requests/<iid>/head`，再 `git worktree add` 检出。
- git 安全参数：`core.hooksPath=/dev/null`，跳过 LFS（`GIT_LFS_SKIP_SMUDGE=1`），禁用 submodule 自动拉取，**绝不执行仓库内任何脚本**。
- 忽略规则：`*.lock`、生成代码、`vendor/`、二进制文件等不发送给模型，可在项目配置中扩展。

### 4.4 回写

- **汇总评论**：每个 MR 一条，内嵌隐藏标记 `<!-- gitlab-ce-review:summary -->`，重跑时就地更新，不重复发布。
- **行内讨论**：Discussions API，`position_type=text`，带 `new_path` / `old_path` 和 `new_line` / `old_line`。行号**必须落在 diff hunk 内**，否则 GitLab 返回 400。校验不通过的条目降级并入汇总评论。
- **commit status（可选）**：只有明确 PASS 写 `success`；BLOCKING 和 INCOMPLETE 都写名为 `ai-review` 的 `failed` 状态。项目开启「Pipelines must succeed」后即可拦截合并，这是 CE 不依赖付费审批规则也能做门禁的方式。是否默认开启，见 §16。
- 发布前再次比对 MR 当前的 `head_sha`。如果已经变化，丢弃本次结果，不发布。

---

## 5. 评审编排

### 5.1 主 agent 单入口

主 agent 在 DAG 中占两个节点：

1. **plan**：读取材料包摘要（文件列表、变更统计、pipeline 状态、MR 描述），判断风险，输出受限的规划参数：
   ```json
   { "packages": [ { "id": "...", "block": "code-review|review|reasoner", "files": ["..."], "provider": "..." } ],
     "challenge": { "only": ["P0", "P1"] } }
   ```
   这些参数由代码严格校验：块类型、provider 必须在项目允许列表内，文件必须属于本次变更，总节点数受预算限制。
2. **synthesize**：接收所有子节点的结构化结果（**只接收结论，不接收子 agent 的完整对话**），逐条采纳或反驳，给出最终 findings 和汇总文案。模型合同仍为 PASS/BLOCKING；代码可在存在未解决证据或覆盖缺口时把最终 SynthesisResult 覆盖为 INCOMPLETE。最多可申请 1 次 `extend` 补派节点（例如对争议条目追加反证）。

`brain.md` 的「严肃决策、风险取舍」定位可以作为 synthesize 阶段 prompt 的基础。

### 5.2 四种评审模式（MR 版契约）

现有 agent 定义（`agents_multi-orchestration/agents/*.md`）是为开发流水线写的，在 MR 场景需要改写输入和输出：

| 模式 | 原定位 | MR 场景的改写 |
|---|---|---|
| `code-review` | 无（新写） | diff 加少量上下文，一次调用，输出带行号的 findings。轻量、高覆盖，作为默认 |
| `review` | 独立终审，需要验证证据，输出 PASS/BLOCKING | `spec_goal` 取自 MR 标题、描述和关联 issue；验证证据取自 **GitLab pipeline 和 job 状态**；保留 PASS/BLOCKING 和 P0/P1/P2（review/review.gate 子输出合同）；最终 SynthesisResult 可由代码覆盖为 INCOMPLETE；去掉 `reflow_target`、`ready_for_patcher` 等开发流水线字段。可展开为 standards 和 intent 两条独立通道，再加裁决 |
| `reasoner` | 审方案和设计文档，不审 diff | 输入改为 MR 描述、关联 issue、本次改动的设计文档（`*.md`、ADR），检查「声称要做的事在逻辑上是否成立、漏了哪些边界」 |
| `verify` | 运行测试并诊断 | 改为**挑战者**：对单条 finding 只读反证，输出 CONFIRMED / REFUTED / UNCERTAIN，并且必须引用代码证据。读取 CI 日志诊断失败原因作为后续可选能力 |

评审 agent 的工具由三部分组成：内置**只读工具**（`read_file`、`grep`、`glob`、`list_dir`、`git_log`、`git_blame`，范围限定在 worktree 和 `base/` 内，有输出长度上限）；按块授权的 **MCP 工具**（如 codebase-memory-mcp 的图谱检索）；按块挂载的 **Skill**。详见 §8。

### 5.3 统一 finding 模型

各模式的严重级别不同（review：P0/P1/P2；reasoner：Critical/Notable/Informational；verify：CONFIRMED/REFUTED/UNCERTAIN），内部统一为：

```go
type Finding struct {
    ID         string
    Origin     NodeRef   // 产生它的节点；主 agent 自提的为 origin=main
    Severity   Severity  // P0 | P1 | P2 | Info
    Category   string    // correctness | security | compat | logic | maintainability ...
    Path       string
    Line       *LineRef  // 可空：无法定位到行的 finding 只进汇总
    Title      string
    Evidence   string    // 引用的代码或依据
    Confidence string    // high | medium | low
    Challenge  *ChallengeResult // verify 的反证结果
    Decision   *Decision        // synthesize 的采纳或反驳记录
}
```

各模式的原生字段（`verdict`、`evidence_status`、`result_type` 等）保留在节点输出里，供主 agent 参考。

### 5.4 对抗性核验

**角色**：Proposer（code-review / review / reasoner）提出问题 → Challenger（verify）逐条反证 → Judge 在双方不一致时裁决。Judge 优先用确定性规则，LLM 裁决作为兜底。

**拓扑**：

| 拓扑 | 流程 | 用途 |
|---|---|---|
| A. 提出加反证 | 1 个 proposer，每条 finding 交给 1 个 challenger | `default` 档 |
| B. 多方独立加交叉 | N 个模型独立评审，合并去重；多方都提出的直接保留，单方提出的交给其他模型反证 | `deep` 档 |
| C. 辩论 | proposer 和 challenger 来回 1 到 2 轮，再由 judge 裁决 | 仅用于 P0/P1 争议条目，预留接口 |

**同模型对抗的独立性要求**（否则对抗只是走形式）：

1. challenger 新开上下文，不继承 proposer 的任何对话。
2. 信息不对称：challenger 只看到结论、位置和相关代码，看不到 proposer 的推理过程、置信度和模型身份。
3. 立场相反：proposer 被要求「找出问题」，challenger 被要求「假设它是误报，去找证据推翻」。
4. 采样差异：temperature 不同，或者给出的上下文顺序和范围不同。
5. 拓扑 B 投票时，同模型的票按权重打折（默认 0.5）。投票和 challenger 选择按配置的规范化 endpoint + model 身份归一：同端点同模型的 provider 别名视为同一模型，不增加独立票，也不能作为跨模型确认。无法解析身份的多个 provider 共用同一个 unknown bucket：known 与 unknown 可各计一票，unknown 之间不增加独立票；metadata 缺失会限制挑战者独立性，不能据此声称跨模型确认。没有独立 challenger 时使用已声明的 fallback 并显式提示。

**跨模型**：challenger 可配置为 `cross`。实现按 allowed providers 顺序选择第一个已知、且未被来源使用的 endpoint + model 身份；任一来源身份未知时会跳过候选，只能使用 fallback 并标记同模型。反证比从头发现容易，challenger 可以用较便宜的模型，但 P0/P1 条目建议用强模型反证。

**默认裁决规则**：

| 提出方 | 挑战方 | 处置 |
|---|---|---|
| 提出 | CONFIRMED | 发布 |
| 提出 | REFUTED（带代码证据） | 丢弃，记录在运行日志里 |
| 提出 | REFUTED（无证据） | 视为 UNCERTAIN |
| 提出 | UNCERTAIN | 按项目策略：`drop` / `mark`（标「存疑」后发布）/ `judge` |
| 多方都提出（拓扑 B） | 无需反证 | 发布 |

**去重**：先按「文件、行号区间重叠、类别」做规则合并，可选再用 LLM 做语义合并。

### 5.5 裁决不变量（由代码强制）

| # | 不变量 |
|---|---|
| I1 | 每个节点的输出必须通过 `output_schema` 校验。缺失时补问 1 次，仍失败则节点 `failed` |
| I2 | finding 的行号必须落在 diff 的行集合内，否则去掉行号，降级进汇总 |
| I3 | REFUTED 必须引用代码位置作为证据；生产裁决还要求在固定 head/base 快照中路径存在且行号范围有效，缺失快照或读取失败按 UNCERTAIN 处理 |
| I4 | 子节点已 CONFIRMED 的 P0/P1，synthesize 要丢弃或降级，必须给出逐条反驳及理由，否则结算失败；当前实现用 `decision.finding_id` + `reason`（见 docs/design/04） |
| I5 | synthesize 自提的 finding 标记为 `origin=main`，同样要经过 verify 反证 |
| I6 | 最终结论一致性：存在已确认的 P0/P1 时，不得给出 PASS |
| I7 | 结论绑定 fingerprint（见 §6.5）。发布前 fingerprint 不一致，结果作废 |

完成条件（不能由 synthesize 清除）：

- 上游 `review` 通道声明 `evidence_status=insufficient`、review gate 为 BLOCKING 却没有 accepted finding、材料采集/提示词截断或完成 plan 后非 generated included 文件未被任何评审节点覆盖时，最终结论为 INCOMPLETE。INCOMPLETE 必须携带具体未解决信号，不能同时写成 PASS 或 commit status success。
- `DiffSummary.files` 是进入材料快照的文件数，不是已评审数；`omitted` 记录容量截断、`too_large`、`collapsed` 等采集缺口，用户配置的 `ignored` 路径单独记录，不属于缺口。所有文件都被 ignore 时发布「无需评审」；全部变更文件都 omitted 时不创建空 workflow，只发布 INCOMPLETE 摘要，列出 omitted 总数、原因计数和最多 4 条路径，其余只计数；正常 dossier 保留完整的 omitted 列表。历史记录不得补造不存在的 dossier。

I4 的「逐条反驳」规则参考 GraphAgent 的 accept 结算规则（裁决为 ACCEPT 时不得自带 required_actions；每条 required_actions 必须逐条反驳）；当前没有复制上游代码。

---

## 6. DAG 引擎（`pkg/dag`）

当前 `pkg/dag` 是依据本设计独立实现的 Go 代码，没有复制 GraphAgent 的 TypeScript 源码。实现只覆盖场景无关的 DAG 能力，IDE 交互相关部分不包含。如未来改为移植上游代码，需先完成 §13 的授权确认。

### 6.1 数据模型

- **节点**：一次子 agent 调用，或一段纯代码逻辑（如 `gather` 汇聚去重、`adjudicate` 规则裁决）。两类对引擎透明，都实现同一个执行器接口。字段：`id`、`kind`、`depends_on`、`required`、`input_mapping`（形如 `a.output.findings`）、`condition`、`params`、`timeout`、`expands`。节点种类、图模板和契约见 [docs/design/04-dag-and-contracts.md](docs/design/04-dag-and-contracts.md)。
- **块（Block）**：可复用的子图模板，由编译器展开成节点。共定义 6 种块（5 种用于 MR 评审，1 种用于工作区）：

| 块 | 展开结果 |
|---|---|
| `code-review` | 1 个节点 |
| `review` | standards 通道 + intent 通道 + 1 个裁决节点 |
| `reasoner` | 1 个节点（默认可选） |
| `adversarial` | 针对上游每条待挑战的 finding 生成 verify 节点（运行时扇出），再加 1 个汇聚节点 |
| `synthesize` | 主 agent 的最终裁决节点 |
| `onboarding` | 1 个节点，生成项目上下文文档。只用于工作区的 onboarding 和刷新，不参与 MR 评审 |

- **fan_out**：plan 节点输出的 `packages[]` 在运行时展开成并行节点。评审是只读的，不需要原引擎里「证明 write_set 互不相交」的检查。
- **图校验**：所有图在执行前都要经过唯一的严格校验入口：未知字段报错、检测环、检查 `input_mapping` 引用存在、检查块和 provider 在允许列表内、检查节点总数不超预算。

### 6.2 状态机

两条铁律（借鉴原引擎的设计）：**状态只能通过校验过的迁移改变；终态不可逆。**

节点状态（去掉原引擎中用于交互暂停的 `paused`）：

```
pending ─→ queued ─→ running ─→ completed
   │          │         ├────→ failed
   │          │         ├────→ aborted
   │          │         └────→ pending   (restart / 被 replan 替换)
   │          ├─→ pending  (replan 替换后重置)
   │          ├─→ queued   (崩溃恢复后幂等重入)
   └──────────┴─→ skipped / failed  (条件不满足、级联、排队超时)
终态：completed / failed / aborted / skipped
```

工作流状态（去掉 `paused`、`stepping`）：`pending → running → completed | failed | cancelled`。

### 6.3 事件

采用事件溯源，事件投影成 SQLite（或 Postgres）读模型。保留原事件集的子集：

- 工作流：`created`、`started`、`extended`（对应原 `replanned`）、`completed`、`failed`、`cancelled`
- 节点：`registered`、`queued`、`started`、`completed`、`failed`、`aborted`、`skipped`、`cancelled`、`restarted`

裁掉：`paused`、`resumed`、`stepped`、`deadline_extended`、`timeout_escalated`、`config_updated`。

### 6.4 调度与并发

- Kahn 拓扑排序加 ready 集合。
- **两级限流**：全局并发信号量，加上**按 provider 的并发和速率限制**（多个 MR 同时到达时，不会把某个模型 API 打到限流）。
- 节点先持久化为 `queued`，拿到许可后才发起 LLM 调用。
- 排队等待时间计入节点超时。
- 所有写入都带 attempt / `graph_rev` 栅栏，迟到的过期结果一律拒绝。

### 6.5 Fingerprint 与取消

- fingerprint = `head_sha` + `base_sha` + 项目配置哈希 + prompt 版本。
- MR 有新 push：旧工作流 → `cancelled`，运行中的节点 → `aborted`，正在进行的 LLM 请求通过 context 取消。
- 发布前比对 fingerprint，见 I7。

### 6.6 失败语义

- `required` 节点失败 → 下游级联为不可满足 → 工作流 `failed`。此时在 MR 上发布一条简短的「评审未完成」说明（是否发布可配置）。
- 可选节点失败 → 下游照常运行，对应输入用占位文本。
- 依赖全部为 `skipped` 的节点级联 `skipped`。
- 超时没有人工裁决环节：硬超时加预算上限，超时后按 required / optional 语义处理。

### 6.7 恢复

评审是只读的，恢复比原引擎简单得多：

- 服务重启后，`running` 和 `queued` 的节点直接重新执行。LLM 调用无副作用，重跑是安全的。
- 发布阶段另有幂等保护：汇总评论按隐藏标记就地更新；行内讨论按项目/MR 内的 `publish_key` 去重。该键包含路径、规范化标题和代码片段，不是语义相似度判断。2026-10-25 用户确认按此边界验收：显式重跑时模型换标题可能再次发布同一问题；本版不增加跨次问题语义去重。
- 评审执行状态与发布状态分开恢复：`publication_state=pending` 的终态工作流在启动后重试；failed execution 在 `post_failure_note=true` 时发布未完成摘要，关闭时记为 skipped；取消或 superseded 记为 skipped；自动恢复不会覆盖 published/failed（手动 republish 是显式例外）。失败原因有界脱敏保存，永久发布失败需要手动 republish。

### 6.8 预算

| 项 | 默认值（建议） |
|---|---|
| 每个工作流的节点总数 | 30 |
| extend 次数 | 1 |
| 每个工作流的 token 上限 / 金额上限 / 时长上限 | 项目配置 |
| 超出预算时 | 降级为拓扑 A，或者只挑战 P0/P1 |

`max_tokens` 是 workflow 共享预算，按 `input_tokens + output_tokens` 计算。cache read/write 和 reasoning 在 usage 中分别表示，但不额外加入预算，避免与 input/output 重复计数。调度器和节点执行器在每次模型请求前检查已提交用量加当前 attempt 已知的 partial usage；并发节点不预留额度，已发出的请求可能在检查后完成并造成有界超支，因此这是尽力上限，不是精确账单。transport 重试的已知 usage 计入下一次 attempt 的预算检查和最终节点用量；预算不足时保留已有结果，按 required / optional 和确定性规则兜底，不能无提示地给出 PASS。

---

## 7. LLM 接入层（`internal/llm`）

### 7.1 统一接口

```go
type Client interface {
    // 一次请求-响应；工具循环在 internal/agent 中实现
    Generate(ctx context.Context, req Request) (*Response, error)
}
```

`Request` 和 `Response` 统一表示消息、工具定义与调用、推理块、用量和停止原因。DAG 节点和 agent 只依赖这个接口，不感知底层协议。

### 7.2 三个 adapter

| 协议 | SDK | 要点 |
|---|---|---|
| OpenAI Chat Completions | `github.com/openai/openai-go/v3` | 兼容端点最多（vLLM、Ollama、各家国产模型），是内网部署的主力。`base_url` 可配置 |
| OpenAI Responses | `github.com/openai/openai-go/v3`（`responses` 包） | 使用**无状态模式**（不依赖 `previous_response_id`），推理项按协议要求原样回传，便于重试和切换模型 |
| Anthropic Messages | `github.com/anthropics/anthropic-sdk-go` | 开启 thinking 时，thinking 块（含签名）在工具循环中原样回传；对材料包等稳定前缀设置 `cache_control`，降低同一 MR 多次评审的成本 |

### 7.3 结构化交付

所有子 agent 通过调用 **`submit_result` 工具**提交结果，参数结构由节点的 `output_schema` 约束。三种协议都支持工具调用，这样只需一套实现，不依赖各家不同的 structured output 参数。

### 7.4 Provider 配置

每个 provider 配置 `protocol`、`base_url`、`model`、`api_key`（从环境变量或加密存储读取），以及可选的 `max_concurrency`、`rpm`、`tpm`、`temperature`、`thinking` 参数。

---

## 8. 工作区、上下文文档、Skill 与 MCP

> 平台生成和维护的一切内容（上下文文档、索引、Skill、MCP 配置）**只存在于平台内部**，不提交到被评审的仓库。

### 8.1 项目工作区

每个注册项目有一个常驻工作区，存放跨 MR 复用的内容；每个 MR 的 worktree 是其中的临时子目录。

```
data/workspaces/<project_id>/
  mirror.git/               bare mirror
  base/                     目标分支检出，按 base_sha 刷新
  context/<base_sha>/       生成的 AGENTS.md / CLAUDE.md + meta.json（来源 sha、模型、生成时间、是否锁定）
  index/                    外部工具的索引（如 codebase-memory）
  skills/                   项目级 Skill
  mcp.json                  项目级 MCP 配置
  runs/<id>/                每个 MR 的临时 worktree，评审通常使用 job ID，结束后清理
```

| 时机 | 动作 |
|---|---|
| 项目注册 | 运行一次 **onboarding 工作流**（本身也是一个 DAG）：clone → 建索引 → 生成上下文文档 |
| 目标分支有新 push（Push Hook） | 刷新 mirror、`base/` 和索引；变更量超过阈值或距上次生成超过设定时间时，重新生成上下文文档 |
| MR 评审 | worktree 检出 head；上下文文档和索引使用 base 版本 |
| 定期 | 清理过期的 `context/<sha>/` 和遗留的 `runs/` |

同一工作区的刷新与评审通过工作区级读写锁协调：刷新持写锁，评审持读锁；评审所用的 base 快照在工作流开始时固定。

### 8.2 上下文文档（生成的 AGENTS.md / CLAUDE.md）

- **用途**：作为所有评审 agent 共享的项目背景，内容包括架构、模块划分、约定、构建与测试方式、评审时需要特别关注的风险点。
- **生成者**：`onboarding` 块（可基于 `explore.md`），用只读工具和 MCP 工具探索 `base/`，通过 `submit_result` 提交结构化结果，由代码渲染成 Markdown。
- **来源优先级**：仓库中人工编写的 AGENTS.md / CLAUDE.md（**只读目标分支版本**）作为输入纳入，生成内容是补充。**MR 中对这两个文件的修改不生效**，防止有人借此绕过评审。
- **人工修正**：管理员可通过管理 API / CLI 编辑或锁定生成的文档。锁定后刷新不会覆盖，只标记「已过期」。
- **注入方式**：作为「项目背景（非授权内容）」注入，不能覆盖平台的评审规则和裁决不变量。
- **成本**：同一 base_sha 下内容不变，放在 prompt 最前面作为稳定前缀，命中 Anthropic 的 `cache_control` 和 OpenAI 的自动缓存。

### 8.3 MCP 接入

- **SDK**：官方 `github.com/modelcontextprotocol/go-sdk`。
- **配置格式兼容 Claude Code**：沿用 `mcpServers` JSON 结构（stdio 的 `command` / `args` / `env`，HTTP 的 `url` / `headers`），可以从 Claude Code 的配置直接复制。
- **分层**：平台级（所有项目共享）加项目级（工作区 `mcp.json`），同名时项目级覆盖平台级。平台级配置和任何范围写入需要平台管理员；项目级写入还需该项目 `manage`，项目 `read` 只能查看，不能修改宿主执行配置。Skill 和块绑定使用相同边界。
- **工具命名**：`mcp__<server>__<tool>`。
- **按块授权**：每个块声明允许使用的 MCP 工具白名单，未列入的工具对该块不可见。
- **进程管理**：stdio 服务器按工作区建立进程池，cwd 指向工作区；每次调用都有超时和输出长度上限；空闲超时后回收；崩溃后自动重启，并计入节点错误。
- **安全**：
  - 子进程只继承白名单内的环境变量，**不传入 GitLab token 和模型 API key**。
  - 工具返回的内容一律作为不可信数据。
  - 只开放只读类工具。

```json
{
  "mcpServers": {
    "codebase-memory": {
      "command": "/usr/local/bin/crc-codebase-memory-mcp",
      "args": ["--tool-profile=analysis", "--ui=false"],
      "env": {}
    }
  }
}
```

### 8.4 codebase-memory-mcp 接入要点

首个接入的代码地图工具是 `codebase-memory-mcp`（本机验证版本 0.12.1）：

- MCP server 的 `--tool-profile=analysis|scout` 提供受限工具面。analysis 为 13 个、scout 为 8 个，均不含 `index_repository`、`delete_project`、`ingest_traces`、`manage_adr` 四个写入/管理工具。2026-10-25 的 Linux/amd64 容器探针已验证 analysis 的 `list_projects` 调用成功，直接调用未公开的 `delete_project` 在工具层被拒绝；不把一次负向调用推广为全部工具的安全证明。
- CLI 的 `--tool-profile` 不限制本地 CLI 工具调用；不要把 MCP profile 当作 CLI 沙箱。
- onboarding 阶段用 `cli` 模式建索引，不必由应用建立 MCP 会话；0.12.1 的 CLI 仍会使用上游 daemon。
- 用 [MCP 隔离启动脚本](scripts/codebase-memory-mcp.sh) 同时启动 server 和索引命令。脚本在项目 `HOME` 下分别设置 `CBM_CACHE_DIR` 和 `CBM_RUNTIME_DIR`；仅设置 `HOME` 或 `XDG_RUNTIME_DIR` 不能隔离默认的账号级 daemon。完整配置与部署要求见 [MCP 配置](docs/design/06-config.md#2-mcp-配置文件)。
- 服务端部署时关闭 `auto_watch` / `watcher_enabled` 和 UI（默认端口 9749）。索引刷新由 Push Hook 驱动，UI 端口不对外暴露。
- 索引只对 base 建。MR 改动的文件由 agent 在 worktree 中直接读取。prompt 中明确说明「图谱反映 base 版本，改动的文件以 worktree 为准」（借鉴 review agent 的 `check_index_coverage` 原则）。

- **索引按路径识别项目**：codebase-memory 以「项目名 + 根路径」标识索引。因此 `base/` 的路径必须**按项目固定**（`data/workspaces/<project_id>/base`），刷新时在原目录内 fetch + checkout，**不重新 clone、不换目录**，索引就能一直复用并增量更新。MR 的 worktree 路径每次不同，不建索引。
- **工作区主键用 GitLab `project_id`**：项目改名或转移 namespace 后 git 地址会变，但 `project_id` 不变；git 地址只作为属性记录，变化时更新 mirror 的 remote，不重建工作区。
- **未来方向**：推动 codebase-memory 支持以仓库身份（规范化的 git remote 地址或 `project_id`）作为索引 key，与路径解耦，使同一仓库在不同路径（如多实例、迁移数据目录）下可直接复用索引。见 §16 Q18。

历史探针记录（2026-10-23，macOS arm64，codebase-memory-mcp 0.12.1，最小环境、隔离且初始为空的 `HOME`；日志见部署验证目录）。以下第 1–4 条只代表当日结果，其中第 2 条为部分验证，不能作为当前应用内 MCP 结论：

1. 配置和索引文件落在独立 `HOME` 的 `$HOME/.cache/codebase-memory-mcp`，未观察到写入 XDG 目录。该历史探针没有证明两个 daemon 能同时运行，不能据此省略 `CBM_RUNTIME_DIR`。
2. MCP server profile 的 advertised tool 列表符合预期（analysis 13 / scout 8）；CLI 调用不受该 profile 限制。随后尝试在新的隔离 HOME/cache/runtime 下补测 `tools/call`，server 在 initialize 前退出并报告 `secure daemon endpoint could not be created`；没有发出工具调用，也未连接或停止当前会话的默认 daemon。因此调用是否被拒绝仍未知，不能把启动失败当作权限拒绝证据。结论是部分验证。
3. 小型合成 fixture（4 个 `.go` 文件和 `go.mod`，最终 20 nodes / 29 edges）可用同一项目身份增量更新：首次 fast index 约 1.72s / 25 MiB，unchanged 约 0.95s / 15.9 MiB，新增 `Difference` 符号后约 1.2s / 26.6 MiB。没有真实大型仓库、生产规模或 Linux 运行数据，因此大仓库耗时和内存仍未解决；不得把合成 fixture 当作生产基准。
4. 探针运行时没有 GitLab、模型 provider 或 OAuth 凭据，未建立真实应用端到端链路。

后续 Linux/amd64 容器探针（2026-10-25，0.12.1）复现了仅按 `HOME` 隔离时的 cache 冲突。使用上述脚本后，两个项目并发初始化、列工具和调用 `list_projects` 均成功；每个会话只列出自己固定 `base/` 路径的一个索引项目，`delete_project` 均被拒绝。此结果验证部署隔离与调用层拒绝，不替代完整评审链路、大型仓库性能或操作系统级文件隔离验收。

2026-10-25 的应用内端到端记录（f143 隔离实例，真实 GitLab 19.5.1 CE）：连续两次实际无诊断代理 deep 分别使用 `deepseek-flash` 与 `gpt-6-astra` 两家 provider，模型实际调用 `list_projects`、`search_graph`、`get_code_snippet`；MCP 成功/调用为 10/10 与 10/11（第二次一次模型工具错误已纠正），未出现跨项目索引引用。真实 stdio 会话验证 `delete_project` 不可用；onboarding 的 CLI 索引成功，未变更的固定 `base/` 索引被复用而不是重建。该记录验证应用侧 MCP 接入与有界隔离，不构成操作系统级沙箱、并发/负载、每个 HTTP backend 或通用后端兼容性结论。运行计数与 run ID 见 [README 验证](README.md#验证)和 PR #30。

### 8.5 Skill 接入

- **格式**：兼容 Claude Code Skill，目录内含 `SKILL.md`（frontmatter 包含 `name`、`description`）和附属资源文件。
- **按需加载**：system prompt 只列出 skill 名称和描述；agent 调用 `load_skill` 时才返回正文；附属文件通过只读工具读取，范围限定在该 skill 目录内。
- **来源**：平台级（如 security-review、各语言的编码规范）加项目级（工作区 `skills/`）。仓库自带的 `.claude/skills` 默认不读取，可按项目开启（只读目标分支版本）。
- **按块挂载**：例如 review 的 standards 通道挂项目编码规范，安全相关的块挂 security-review。
- **脚本**：首版只支持**指令类 skill，不执行其中的任何脚本**。

### 8.6 agent 可用工具汇总

| 类别 | 工具 | 授权方式 |
|---|---|---|
| 内置只读 | `read_file`、`grep`、`glob`、`list_dir`、`git_log`、`git_blame` | 默认可用，范围限定在 worktree 和 `base/` |
| MCP | `mcp__<server>__<tool>` | 按块白名单 |
| Skill | `load_skill` | 按块挂载的 skill 列表 |
| 交付 | `submit_result` | 所有节点必备 |

---

## 9. 数据与持久化

默认 SQLite，可选 Postgres。主要表：

| 表 | 内容 |
|---|---|
| `projects` | GitLab 项目注册、加密后的 token、webhook secret、项目级配置 |
| `workspaces` | 工作区状态：当前 base_sha、索引状态、最近刷新时间 |
| `context_docs` | 上下文文档版本：来源 sha、生成模型、是否锁定、内容 |
| `mcp_servers` / `skills` | 平台级与项目级的 MCP 服务器和 Skill 注册，以及按块授权关系 |
| `workflows` | 工作流读模型：MR、fingerprint、profile、状态、预算用量 |
| `workflow_nodes` | 节点读模型：状态、attempt、provider、用量、结构化输出 |
| `events` | 事件日志（只追加） |
| `findings` | 统一 finding、挑战结果、裁决记录 |
| `publications` | 已发布到 GitLab 的评论和讨论 ID，用于幂等更新 |
| `llm_calls` | 每次模型调用的 prompt 摘要或完整内容、模型版本、原始输出、用量（保留期可配置） |
| `feedback` | 来自 GitLab 的反馈信号（见 §12） |
| `users`、`sessions`、`api_tokens`、`project_grants` | 本地用户、会话/API token、停用状态与每项目 read/manage 授权 |
| `workflow_dossiers` | 脱敏材料快照、评审代次，以及独立于执行的 publication 状态和脱敏失败原因 |

---

## 10. 配置

### 10.1 服务配置（示例）

```yaml
server:
  listen: ":8080"
  base_url: "https://review.example.internal"
gitlab:
  url: "https://gitlab.example.internal"
storage:
  driver: sqlite            # sqlite | postgres
  dsn: "file:data/review.db"
secrets:
  master_key_env: REVIEW_MASTER_KEY
workspace:
  root: "data/workspaces"
  context_refresh: { changed_files: 200, max_age: 168h }
mcp:
  config: "config/mcp.json"     # 平台级，Claude Code mcpServers 格式
skills:
  dirs: ["config/skills"]       # 平台级

providers:
  claude: { protocol: anthropic,        base_url: "...", model: "...", api_key_env: ANTHROPIC_API_KEY }
  gpt:    { protocol: openai-responses, base_url: "...", model: "...", api_key_env: OPENAI_API_KEY }
  local:  { protocol: openai-chat,      base_url: "http://vllm:8000/v1", model: "...", max_concurrency: 4 }

profiles:
  default:
    propose:   [{ block: code-review, provider: local }]
    challenge: { provider: claude, only: [P0, P1] }
    on_uncertain: mark       # drop | mark | judge
  deep:
    main: { provider: claude }
    propose:
      - { block: review,   provider: claude }
      - { block: review,   provider: gpt }
      - { block: reasoner, provider: claude }
    consensus: { min_votes: 2, same_model_weight: 0.5 }
    challenge: { provider: cross }
    judge:     { provider: gpt, max_rounds: 1 }

blocks:                     # 按块授权 MCP 工具、挂载 Skill
  review:
    mcp_tools: [mcp__codebase-memory__search_graph, mcp__codebase-memory__trace_path, mcp__codebase-memory__get_code_snippet]
    skills:    [coding-standards]
  verify:
    mcp_tools: [mcp__codebase-memory__get_code_snippet]
```

### 10.2 仓库内配置 `.gitlab-review.yml`（可选）

由项目方放在仓库根目录，用于覆盖项目级设置：默认 profile、忽略路径、评审语言、严格度、自定义规则、触发过滤（Draft、标签、目标分支）。只读取**目标分支**上的版本，防止 MR 作者通过修改配置绕过评审。

---

## 11. 安全

| 风险 | 措施 |
|---|---|
| 源码发送给外部模型 | ignore 规则；可选的密钥扫描与脱敏；内网项目可只配置内网模型 |
| Prompt 注入（MR 描述、代码注释、评论） | 外部内容一律作为不可信数据传入，并标注「非授权内容」；agent 只有只读工具，唯一输出是 `submit_result`；发布内容由代码渲染 |
| 恶意仓库内容 | 禁用 git hooks、LFS、submodule；不执行任何仓库代码；只读工具限定在 worktree 内 |
| 伪造 webhook | 校验 `X-Gitlab-Token`，并按 `project_id` 匹配对应的 secret |
| Token 泄露 | 加密存储；日志脱敏；最小 scope |
| 配置绕过 | 仓库内配置只读目标分支版本 |
| SSRF | 只访问配置中的 GitLab 地址和 provider 地址 |
| 仓库内 AGENTS.md / CLAUDE.md 被篡改 | 只读目标分支版本，作为非授权背景注入 |
| MCP 服务器子进程 | 环境变量白名单，不传 token 和 API key；只开放只读工具；超时与输出上限 |
| Skill 内容 | 只支持指令类，不执行脚本；附属文件读取限定在 skill 目录 |
| 越权读取/写入 | 项目业务路由检查本地 grant；无 grant 统一 404，只有 read 时写操作 403；列表、统计和 SSE 按 grant 过滤 |
| 退组或离职后的残留访问 | GitLab 组成员关系不自动同步；本地停用用户并撤销全部会话/API token，或删除项目 grant |
| 静态管理 token 泄露 | 仅平台管理面，无用户 ID 和项目授权；由部署方轮换环境变量并重启 |

---

## 12. 可观测性与反馈闭环

- 每个工作流可完整回放：事件日志、每次 LLM 调用的输入输出、模型版本、用量。
- 指标：工作流耗时、节点失败率、各 provider 的延迟、错误率和 token 用量、每个 MR 的成本。
- **反馈信号**：从 GitLab 收集发布者评论的 👍/👎（award emoji），以及讨论 resolve 后对应行是否改动。这些信号仅供参考，不自动判定问题有效性。
- **原始计数**：`stats.feedback` 提供 `total`、`by_signal` 和按 `{provider, role, total, by_signal}` 的归因。归因只统计能按持久化 `feedback.finding_id` 精确关联 finding、且 publication 一致的 discussion；提出方取 `findings.origin_provider`，挑战方取 `challenge_node` 的 provider；汇总/未关联反馈不计入 provider 归因，但仍计入原始总数；同一 signal 可同时进入两个角色。讨论 award 只取发布者首条 note，不取 thread reply。
- **执行与发布状态**：workflow 执行状态和 GitLab 发布状态分开保存。`publication_state` 为 `pending`（等待或可重试）、`published`（发布完成）、`skipped`（stale、取消或关闭 failure note）或 `failed`（永久失败，需手动 republish）；失败原因和跳过原因有界、脱敏保存。SSE 和轮询在两者都达到终态后结束。
- **人工质量指标**：项目 `manage` 权限可将 finding 标为有效、无效或未确定，也可清除标注。提出方准确率为有效数除以有效与无效之和；挑战方误杀率为被反驳且人工判有效数除以被反驳且人工已有明确结论数。未标注和未确定不进入分母。`stats.quality_by_provider` 按 provider 返回分子、分母及比例；零分母返回 `null`，不猜测缺失来源。另列挑战推翻率，与顶层规则 `drop` 占比区分。指标按 workflow 创建时间筛选，反映当前标注，不是历史时点快照或模型效果保证。完整口径见 [统计 API](docs/design/03-http-api.md#45-系统)。

---

## 13. 许可证与依赖

- 本项目：**MIT**。
- 依赖许可证（已核对）：

| 依赖 | 许可证 | 说明 |
|---|---|---|
| `github.com/openai/openai-go/v3` (v3.85.0) | Apache-2.0 | 分发时附带许可证文本 |
| `github.com/anthropics/anthropic-sdk-go` (v1.84.0) | MIT | — |
| `gitlab.com/gitlab-org/api/client-go` (v1.46.0) | Apache-2.0 | 间接依赖 `hashicorp/go-retryablehttp`、`go-cleanhttp`（MPL-2.0，不修改即可正常使用） |
| `github.com/modelcontextprotocol/go-sdk` | MIT → Apache-2.0（迁移中） | 两者均与 MIT 兼容 |

- 发布物使用单一声明文件 `licenses/THIRD_PARTY_NOTICES.txt`，由 `go run ./scripts/licensebundle` 生成。内容覆盖 Go 标准库、Linux/amd64 服务端链接的 40 个模块许可证，以及 9 个 production npm 包；本机 inventory 记录的主机模块为 42 个，运行时清单为 40 个；两者不同。CI 用 `go run ./scripts/licensebundle -check` 验证文件是否最新，并运行 `licensecheck`；本机验证还运行 `go mod verify`。
- Dockerfile 把 `licenses/` 复制到 `/licenses/`，发布路径是 `/licenses/THIRD_PARTY_NOTICES.txt`。当前 Dockerfile 仍同时复制遗留的 `THIRD_PARTY_LICENSES`；新单文件是权威发布声明，旧文件不再作为生成目标。
- 外部 MCP 服务器（如 codebase-memory-mcp）以独立进程运行，不链接进本项目，其许可证不影响本项目；若打包进 Docker 镜像分发，需遵守其自身许可证。
- CI 许可证检查：允许 MIT、BSD、Apache-2.0、ISC、MPL-2.0（不修改）；**禁止 GPL、AGPL**。`licensecheck` 在主机清单上检查 65 个模块，不能把该数字与 bundle 的 40 个 Linux 运行时模块混同。
- **DAG 引擎来源**：`pkg/dag` 当前是依据本设计独立实现的 Go 代码，**不是**从 OpenCode-GraphAgent 移植的产物；没有复制上游代码，也没有添加 `NOTICE`。用户已于 2026-10-23 确认接受该独立实现作为本地验收口径，不要求移植，也不虚构上游 NOTICE、版权人或来源。若未来选择移植，需先确认上游署名与授权（原仓库标注 AGPL-3.0-or-later，Copyright (c) 2026 LeXwDeX），并保留相应版权声明；以下事项仅适用于该未来移植评估，不是当前实现的验收阻塞项：
  - 原 DAG 代码引用的上游 opencode 模块是 MIT（Copyright (c) 2025 opencode），若一并移植，需保留其版权声明。
  - 原块合约改编自 mattpocock/skills（MIT），若移植相关 prompt，需保留出处。
  - 移植前需确认原仓库中署名为 `Test <test@opencode.test>` 的两个提交（含引擎首个提交 `c8e7b676fd`）出自版权人本人。

### 13.1 未来移植参考清单（当前未执行）

下表只是未来如选择移植时的参考，不代表当前实现。

| 原模块 | 处理 |
|---|---|
| `core/types.ts`、`core/transitions.ts`（状态与迁移表、两条铁律） | 直接移植，裁掉 `paused`、`stepping` |
| `core/graph.ts`、`core/layering.ts`、`core/required-validator.ts` | 直接移植 |
| `core/scheduling.ts` | 移植，增加按 provider 限流 |
| `core/replan.ts`、attempt / graphRev 栅栏 | 移植，用于取消和 extend |
| `schema/dag-event.ts`、`projector.ts`、`sql.ts`、`store.ts` | 移植，事件集裁剪见 §6.3 |
| `blocks.ts`、`review-lifecycle.ts` | 移植思路，块类型按 §6.1 重写 |
| 状态迁移、级联 skip 等测试用例 | 转写为 Go 表驱动测试 |
| `runtime/spawn.ts`、admission、recovery、会话与权限推导 | 不移植，按 MR 服务重写 |
| hooks 体系、TUI、Goal 自治循环、并行 writer 与 `git status` 聚合 | 不移植 |

---

## 14. 部署

- 单个静态二进制，提供 Docker 镜像（内含 git）。
- 依赖：一个可写的数据目录（SQLite 数据库和项目工作区）。
- 外部 MCP 服务器（如 codebase-memory-mcp）由部署方安装，或提供包含它们的扩展镜像。
- 水平扩展（多实例、外部队列）不在首版范围内；`store` 和任务队列通过接口隔离，为将来换成 Postgres 加外部队列预留空间。

---

## 15. 里程碑（建议）

| 里程碑 | 内容 |
|---|---|
| M0 骨架 | 服务配置、项目注册、webhook 接收与校验、任务队列、SQLite |
| M1 单模式闭环 | 采集阶段、`code-review` 单节点、一个 provider、汇总评论与行内讨论发布、行号校验、幂等 |
| M2 DAG 引擎 | 实现 `pkg/dag`（状态机、图校验、调度、事件与投影；当前为独立实现，见 §13）、取消与 fingerprint、三个 LLM adapter |
| M3 主 agent 编排 | plan / fan_out / synthesize、`review` 与 `reasoner` 块、只读工具循环、裁决不变量 I1–I7 |
| M4 对抗核验 | `verify` 挑战者、拓扑 A 和 B、跨模型选择、去重、UNCERTAIN 策略、commit status |
| M5 工作区与扩展 | 项目工作区与生命周期、Push Hook、onboarding 工作流与上下文文档、MCP 接入（首个为 codebase-memory-mcp）、Skill 接入 |
| M6 运维与反馈 | 管理界面或 CLI、可观测性、反馈收集、许可证检查与 `licenses/THIRD_PARTY_NOTICES.txt` |

---

## 16. 待决事项

| # | 问题 | 当前建议 |
|---|---|---|
| Q1 | 是否同时支持 Group Token 或 bot 账号 PAT？ | 首版只支持 Project Token，数据模型预留 |
| Q2 | 项目接入方式：管理员手工登记，还是项目方自助（服务用项目 token 自动创建 webhook）？ | 首版手工登记加 CLI，自助注册放到 M6 |
| Q3 | 是否需要 Web 管理界面？ | 首版用 CLI 和管理 API，界面放到 M6 |
| Q4 | `verify` 是否就定位为挑战者（D11）？读取 CI 日志诊断是否需要？ | 挑战者为主，CI 诊断作为后续可选 |
| Q5 | 默认拓扑用 A 还是 B？ | `default` 档用 A，`deep` 档用 B |
| Q6 | UNCERTAIN 条目的默认处理？ | `mark`（标「存疑」后发布） |
| Q7 | 评论中是否标注模型来源（如「由 X 提出，经 Y 核验」）？ | 默认不标注，可配置开启 |
| Q8 | 主 agent 能否自提 finding？ | 可以，但须遵守 I5 |
| Q9 | 主 agent 和子 agent 的模型：配置写死，还是主 agent 从允许列表中自选？ | 配置给出允许列表，主 agent 在列表内选择 |
| Q10 | BLOCKING 是否默认写 commit status 卡合并？ | 默认关闭，按项目开启 |
| Q11 | 规模预期（项目数、日均 MR 数）？ | 影响 §14 是否需要多实例，待提供 |
| Q12 | reasoner 的输入是否只用 MR 描述和关联 issue，还是重点审改动的设计文档？ | 两者都给，设计文档优先 |
| Q13 | Skill 中的脚本是否执行？ | 首版不执行，只支持指令类 skill |
| Q14 | 生成的上下文文档是否支持人工编辑与锁定？ | 支持，首版通过管理 API / CLI |
| Q15 | 图谱索引只对 base 建，还是对 MR head 也做增量索引？ | 只对 base 建，改动的文件以 worktree 为准 |
| Q16 | 是否读取仓库自带的 `.claude/skills`？ | 默认关闭，可按项目开启（只读目标分支版本） |
| Q17 | Web 控制台的前端技术栈与登录方式（D18）？ | Vite + React + TypeScript；GitLab OAuth 登录 |
| Q18 | 代码索引能否以仓库身份（git 地址或 project_id）为 key，而不是按路径？ | 首版靠固定工作区路径规避；后续向 codebase-memory 提需求或贡献实现 |
