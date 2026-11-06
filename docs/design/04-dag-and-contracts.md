# 04 DAG 引擎与节点契约

> 细化 [ARCH.md](../../ARCH.md) §5、§6。本文给出：节点种类、评审图模板、状态迁移表、调度算法、每个节点的输入输出契约、裁决算法和评论渲染格式。

---

## 1. 节点种类

节点执行器分两类：**agent 节点**（跑一次 agent 工具循环）和**代码节点**（纯 Go 逻辑，不调用 LLM）。两类对 `pkg/dag` 没有区别，都实现 `NodeExecutor`。

Tribunal 只有一个评审档位 `default`，评审图固定（见 §3.1）。`onboarding` 档位只驱动项目接入工作流，不是评审档位。

| kind | 类别 | 来源块 | 默认 required | 工具 | 输出契约 |
|---|---|---|---|---|---|
| `explore` | agent | `explore` | 否 | 只读工具 + 绑定的 MCP | `ExploreOut` |
| `plan` | 代码 | 主流程 | 是 | — | `PlanOut`（由代码按文件分包生成） |
| `code-review` | agent | `code-review` | 否 | 只读工具 + 绑定的 MCP / Skill | `CodeReviewOut` |
| `review` | agent | `review` | 否 | 只读工具 + 绑定的 MCP | `ReviewOut` |
| `reasoner` | agent | `reasoner` | 否 | 只读工具 + 绑定的 MCP（默认无） | `ReasonerOut` |
| `validate` | agent | `validate` | 否 | 只读工具 + 绑定的 MCP | `ValidateOut` |
| `gather` | 代码 | `adversarial` | 是 | — | `GatherOut`（并展开 verify 节点） |
| `verify` | agent | `adversarial` | 否 | 只读工具 + 绑定的 MCP | `VerifyOut` |
| `adjudicate` | 代码 | `adversarial` | 是 | — | `AdjudicateOut` |
| `synthesize` | 代码 | 主流程 | 是 | — | 代码拥有的 `SynthesisResult` |
| `onboarding` | agent | `onboarding` | 是 | 只读工具 + 绑定的 MCP / Skill | `OnboardingOut` |

- `plan` 不调用 LLM，依赖 `explore`（`explore` 是可选节点，失败不会级联，`plan` 照常运行）：按文件把变更切成若干包，每包估算 token 不超过 `plan.package_tokens`（默认 60k），同目录文件尽量放一起，包数不超过 `plan.max_packages`（默认 6）。
- `gather`、`adjudicate`、`synthesize` 是代码节点，评审报告由代码渲染，不调用模型。
- 四路评审节点（`code-review`、`review`、`reasoner`、`validate`）和 `explore` 都是可选节点；失败语义见 §5.4。
- 所有 agent 节点使用同一个 agent 循环，因此最后一轮强制提交（[#255](https://github.com/LeXwDeX/Tribunal/issues/255)）和提交提示轮的不合法工具调用恢复（[#269](https://github.com/LeXwDeX/Tribunal/issues/269)）对新节点同样生效。
- 引擎仍能解码历史工作流里记录的旧节点种类（`review.standards`、`review.intent`、`review.gate`、agent 规划与 agent 汇总等），用于查看既有评审；新评审不再生成它们。

---

## 2. 节点 ID 规则

| 节点 | ID |
|---|---|
| 探索 | `explore` |
| 规划 | `plan` |
| 每个包的 code-review | `cr.<pkg>` |
| 每个包的 review | `rv.<pkg>` |
| reasoner（每个 MR 一个） | `reasoner` |
| validate（每个 MR 一个） | `validate` |
| 汇聚 | `gather` |
| 挑战 | `verify.<finding_seq>` |
| 规则裁决 | `adjudicate` |
| 汇总 | `synthesize` |

`<pkg>` 由 plan 输出，只允许 `[a-z0-9_-]{1,32}`。

---

## 3. 图模板

### 3.1 `default` 档

```
explore ─▶ plan(code) ──expands──▶ cr.p1 ─┐
                                   rv.p1 ─┤
                                   cr.pN ─┤
                                   rv.pN ─┼──▶ gather ──expands──▶ verify.1 ─┐
                                   reasoner ┤                      verify.k ─┼──▶ adjudicate ──▶ synthesize
                                   validate ┘                               ─┘
```

- 初始图有 `explore`、`plan`、`gather`、`adjudicate`、`synthesize` 五个节点；`plan` 依赖 `explore`，`gather` 依赖 `plan`。展开 `plan` 时，新增的 `cr.*`、`rv.*`、`reasoner`、`validate` 都依赖 `plan`，并被加进 `gather` 的依赖（见 §5.3）。
- **探索**：`explore` 是可选的只读 agent 节点，超时 5 分钟。它的输出 `ExploreOut` 在展开 `plan` 时渲染成有界摘要（不超过 6000 字符，超出截断并标注），写入每路评审节点的 params（字段 `exploration`），因此节点任务仍是 params 与材料包的纯函数，重试和恢复不依赖读取兄弟节点。`explore` 未成功完成或输出不合法时，四路评审不带摘要运行，报告给出一条告警。
- **四路评审并行**：每个包一个 `cr.<pkg>`（挂载内置 code-review Skill）和一个 `rv.<pkg>`，每个 MR 一个 `reasoner` 和一个 `validate`。节点数为 `包数 × 2 + 2`，配置校验要求 `plan.max_packages × 2 + 2 ≤ max_fan_out`（`max_fan_out` 默认 36），展开时再按实际包数检查一次。
- **汇聚与挑战**：`gather` 把四路的 findings 归一化、去重（§7.1），只把 P0/P1 交给 `verify`；其余不挑战，直接进入 `adjudicate`。受 `max_nodes` 约束时，按严重级别优先选择挑战目标，超出节点容量的条目标记 `skipped` 并写 warning。不确定的挑战结果一律标记为「存疑」后发布。
- **汇报**：`synthesize` 是代码节点，直接渲染 `adjudicate` 的结果，不再调用 LLM，图也不会在汇报阶段追加节点。
- 单路评审节点失败不会终止其他路，见 §5.4。

### 3.2 onboarding 工作流

```
onboarding ─▶ (代码) 渲染并写入 context_docs
```

只有一个 agent 节点；建索引在工作流开始前由 `workspace_service` 用 CLI 完成，不进 DAG。

### 3.3 四路评审的职责

| 节点 | 范围 | 契约 | 说明 |
|---|---|---|---|
| `explore` | 整个 MR | `ExploreOut` | 只读探索，减少四路评审的不确定性；不报告缺陷，不给 PASS/BLOCKING |
| `cr.<pkg>` | 一个包 | `CodeReviewOut` | diff 加少量上下文，输出带行号的 findings，使用内置 code-review Skill |
| `rv.<pkg>` | 一个包 | `ReviewOut` | 独立终审：PASS/BLOCKING、`evidence_status`、P0/P1/P2 findings 与证据缺口。只缺少或未跑完 pipeline 本身不算「证据不足」 |
| `reasoner` | 整个 MR | `ReasonerOut` | 审 MR 描述、关联 issue 和设计文档的逻辑，不审 diff；严重级别 critical / notable / informational |
| `validate` | 整个 MR | `ValidateOut` | 对应 opencode 的 verify 角色：判断已有验证证据是否证明变更可用。**Tribunal 不运行测试或仓库脚本**；证据只有 GitLab pipeline 状态与失败 job 日志片段（`source=pipeline`）和只读读码（`source=code`） |

---

## 4. 状态迁移表

```go
var nodeTransitions = map[NodeStatus][]NodeStatus{
	NodePending: {NodeQueued, NodeRunning, NodeSkipped, NodeFailed},
	NodeQueued:  {NodeQueued, NodeRunning, NodePending, NodeSkipped, NodeFailed},
	NodeRunning: {NodeCompleted, NodeFailed, NodeAborted, NodePending, NodeSkipped},
	// 终态：不可再迁移
	NodeCompleted: {}, NodeFailed: {}, NodeAborted: {}, NodeSkipped: {},
}

var workflowTransitions = map[WorkflowStatus][]WorkflowStatus{
	WorkflowPending: {WorkflowRunning, WorkflowCancelled},
	WorkflowRunning: {WorkflowCompleted, WorkflowFailed, WorkflowCancelled},
	WorkflowCompleted: {}, WorkflowFailed: {}, WorkflowCancelled: {},
}
```

说明（相对原引擎的裁剪）：

- 去掉 `paused`、`stepping`，以及与之相关的迁移。
- `queued → queued`：崩溃恢复后的幂等重入。
- `queued → failed`：排队等待超过节点超时。
- `running → pending`：崩溃恢复时，运行中的节点重置为 pending 并 `attempt+1`。
- `pending → failed`：上游 required 节点失败导致不可满足。

非法迁移返回 `TerminalViolationError` 或 `InvalidTransitionError`，并且**不写事件**。

---

## 5. 调度

### 5.1 主循环

```
Run(wf):
  emit workflow.started（如果是 pending）
  loop:
    snapshot := project(events)
    if snapshot 已终态: return
    if deadline 到期: Cancel(wf, "deadline"); return
    for n in readyNodes(snapshot):          # 依赖全部终态、自身 pending
        if 有依赖为 skipped 且全部依赖都 skipped: emit node.skipped(deps_skipped); continue
        if 有 required 依赖 failed/aborted:   emit node.failed(unsatisfied); continue
        if n.Condition 为假:                  emit node.skipped(condition_false); continue
        emit node.queued
        go runNode(n)                         # 先拿全局许可再执行
    if 所有节点终态:
        if 存在 required 节点 failed: emit workflow.failed
        else: emit workflow.completed
        continue                            # 重读，确认没有并发 Extend 增加节点
    wait(节点完成通知 | ctx 取消 | 定时器)
```

### 5.2 执行单个节点

```
runNode(n):
  release := globalSem.Acquire(ctx)          # 默认 8
  defer release()
  attempt := n.attempt
  emit node.started(attempt)
  ctx, cancel := context.WithTimeout(wfCtx, n.Timeout - 已排队时长)
  out, err := executor.Execute(ctx, input(n))
  if 当前 attempt != attempt 或工作流已终态: 丢弃结果; return
  if token 预算已耗尽: 按 required / optional 写 budget_exhausted，保留可得用量
  else if err 或结果到达时节点期限已到: emit node.failed，保留可得用量
  else:
    if n.Expands:
      extension := expander.Expand(...)
      validate(extension, budget)             # 失败则写 node.failed，不先写 completed
      append(node.completed(out), workflow.extended(extension)) # 同一事务
    else:
      emit node.completed(out)
```

所有 `emit` 都走 `EventStore.Append(expectRev)`；遇到 `ErrStaleRevision` 时重新读取快照后再判断是否还需要写。内部提交看到 revision 前进时继续重试，不因兄弟节点正常提交耗尽固定重试次数；同一 revision 连续 4 次冲突则返回错误。重试保留 attempt 栅栏，不重新执行 executor 或 expander；非版本冲突的持久化错误返回 `Run`，不能静默丢失完成结果。调用方取消仍可终止重试，持久化取消另有 10 秒写入上限；公开 `Extend(expectRev)` 的调用方版本约束不变。

节点期限已到时，即使执行器无错误返回，也不提交结果或展开。错误有有效分类时保留该分类，否则使用 `timeout`；预算已耗尽时仍优先记录预算取消。失败和预算取消会保留该次返回的可得用量及 `usage_incomplete` 标志。WorkflowView 由事件投影三个标记：`usage_incomplete`（检测到任何缺失，含重启或中止）、`usage_unreported`（不完整源于进程中断以外的原因：节点上报不完整、无效用量或计数溢出）、`usage_interruptions`（任一单个节点的恢复重启最大次数，同一轮恢复重启多个节点只计 1 次，不同节点在不同轮被中断也不累加；NodeView 同名字段为该节点自身次数）。`max_tokens > 0` 时，仅当 `usage_unreported` 为真或 `usage_interruptions` 超过 3（同一节点被中断超过 3 次）才以 `unsafe_model_response` 阻断，否则以已提交用量为下限继续。超时、预算或工作流取消只会取消 context，不能强杀不协作的 Go 执行器。`Run` 在返回前同步等待已启动执行器退出，以保护工作区；执行器不退出时可能无限等待。工作流取消时立即持久化终态，并为当时仍在运行的节点标记 `usage_incomplete`（包括 `TokenFree` 节点）；已完成节点的用量保持不变，未启动节点不因取消而标记。迟到结果不会改写终态，其具体用量不能恢复。进程中断不持久化取消，恢复运行中的 attempt 时标记工作流用量不完整。

finalize 若在重读时发现新节点，返回主循环继续调度，而不是把未终态工作流当作成功结束。该路径与默认并发 8 下的完整评审流程均有回归测试。

### 5.3 展开时的依赖改写

`gather` 必须等所有动态生成的评审节点完成后才能运行。展开 `plan` 时，Expander 同时返回一条依赖改写：把新节点 ID 追加到 `gather.DependsOn`。引擎在 `workflow.extended` 事件中记录 `{nodes, dep_patches: [{node: "gather", add: [...]}]}`，投影时一起应用。

约束：只能给**仍为 pending** 的节点追加依赖；目标节点已开始则展开失败。

### 5.4 失败语义

| 情况 | 处理 |
|---|---|
| required 节点失败 | 下游全部 `failed(unsatisfied)`，工作流 `failed` |
| 可选节点失败 | 下游照常运行；该输入填占位 `{"_unavailable": true, "reason": "..."}` |
| 依赖全部 skipped | 节点 `skipped(deps_skipped)` |
| verify 节点失败 | 对应 finding 的挑战结果记为 `uncertain` |
| 工作流取消 | 运行中节点 `aborted`，未开始节点 `skipped(workflow_cancelled)` |
| 四路评审节点（`cr.*`、`rv.*`、`reasoner`、`validate`）失败、被取消或被跳过 | 它们都是可选节点，其余各路继续运行并照常发布各自的 findings。`gather` 为缺失的一路写入证据缺口「lane X did not complete」，最终结论为 `INCOMPLETE`，不会是 `PASS` |
| `explore` 未成功完成 | 四路评审不带探索摘要运行；报告给出一条告警，不产生证据缺口 |
| `explore` 被跳过 | 只会发生在 token 预算在它开始前已耗尽时；此时 `plan` 也被跳过，工作流在没有结论的情况下结束 |

### 5.5 预算校验

每次 `Create` / `Extend` 前检查：

- 节点总数 ≤ `budget.max_nodes`（默认 30）。
- 显式补派（`source=extend`）次数 ≤ `budget.max_extends`。评审档位固定为 0（报告阶段不扩展图）；plan 和 gather 的正常 `fan_out` 不消耗此额度。
- 累计 token 超过 `budget.max_tokens` 时（设计意图）：不再展开 verify（`gather` 只输出不挑战的结果），并在汇总里注明「预算不足，部分条目未经核验」。

当前实现的 token 预算语义：

- `pkg/dag` 在预算耗尽时终止未完成的非 `TokenFree` 节点：required 节点写 `failed(budget_exhausted)`，optional 节点写 `skipped(budget_exhausted)`。
- 标记为 `TokenFree` 的节点不会被引擎终止，仍会运行。当前构建把 `plan`（code 模式）、`gather`、`adjudicate`、`synthesize` 和 `synthesize.2` 标为 `TokenFree`；其他未完成节点按 required/optional 失败或跳过。
- `gather` 在预算耗尽时不展开 verify，把待挑战组标记为 `skipped(budget_exhausted)`，并写入 warning；`adjudicate` 固定按 `mark` 处理这些组（标「存疑」后发布），汇总会注明部分条目未核验。
- 引擎不会因为 plan 超出 token 预算而自动减少包或降级 plan。plan 仍按原包数展开，后续 LLM 节点按上面的规则失败或跳过。
- agent 循环在至少完成一轮后失败会返回 partial usage。agent 节点只对 transport 错误重试，每次重试携带上一次已知 partial usage 参与下一轮预算检查，最终节点 usage 也包含这些已知用量；上游未返回 usage 的请求仍无法精确计费，因此跨重试 token/成本只能按已知边界使用，不能当账单。
- workflow 的 `max_tokens` 是共享预算，按 `input_tokens + output_tokens` 计算。cache read/write 和 reasoning 在 usage 中分别表示，但不额外相加，避免与输入/输出重复计数。执行器在每次模型请求前检查已提交用量加当前 attempt 已知 partial usage；并发节点不预留额度，已发出的请求可能在检查后完成，因此允许有界超支，不是账单级硬上限；四路评审并行，超支幅度随并行节点数增加。探索摘要（不超过 6000 字符）与 diff 共用任务预算。

---

## 6. 节点输入输出契约

所有 agent 节点通过 `submit_result` 提交；下面的 Go 结构体用 `invopop/jsonschema` 生成 schema 交给模型，解码时 `DisallowUnknownFields` 并执行 `Validate()`。

### 6.1 公共类型

```go
type Severity string   // P0 | P1 | P2 | info
type Confidence string // high | medium | low

type FindingOut struct {
	Path       string     `json:"path"`
	Line       int        `json:"line,omitempty"`    // 0 = 无法定位到行
	Side       string     `json:"side,omitempty"`    // new（默认）| old
	Severity   Severity   `json:"severity"`
	Category   string     `json:"category"`          // correctness | security | concurrency | compat | performance | logic | maintainability
	Title      string     `json:"title"`             // ≤ 120 字符
	Body       string     `json:"body"`              // 结构化 Markdown：问题、触发条件、影响；代码用围栏或行内代码
	Suggestion string     `json:"suggestion,omitempty"` // 结构化 Markdown；代码或命令用围栏
	Evidence   string     `json:"evidence"`          // 引用的代码或依据
	Confidence Confidence `json:"confidence"`
}

type CodeRef struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Root      string `json:"root,omitempty"` // head | base
	Note      string `json:"note"`
}
```

生产裁决会针对固定 dossier 的 head/base 快照验证 CodeRef：路径必须存在、行号范围必须有效，`root` 为空时按 `head`；缺失快照或读取失败不能证明反驳，按 `UNCERTAIN` 处理。语法有效或 diff 外的合法代码位置都可能通过，但存在性检查不等于证明语义正确。

材料覆盖使用 `DiffSummary`：`files` 是进入材料快照的 included 文件数，不是已评审数；omitted 文件另有带原因和路径的记录，不能从 included 数或总数反推原因。`total` 是 ignore 规则前的变更文件总数；`ignored` 是用户规则排除，不算覆盖缺口；`omitted` 记录 `max_files`、`max_diff_lines`、`too_large`、`collapsed`、`invalid_path` 等缺口及路径。上述 omitted 总是进入 unresolved；完成 plan 后未被分配评审节点的非 generated included 文件、以及评审节点实际遇到的文件/包文本截断，也会进入 unresolved，最终不能无提示 PASS。正常 dossier 保存完整的 omitted 路径列表；只有不创建 workflow 的独立摘要把路径展示限制为最多 4 条，其余只计数。

### 6.2 各节点

```go
// plan：由代码按文件分包生成，不调用模型
type PlanOut struct {
	Risk        string    `json:"risk"`          // low | medium | high
	RiskReasons []string  `json:"risk_reasons"`
	Packages    []Package `json:"packages"`      // 1..max_packages
	RunReasoner bool      `json:"run_reasoner"`
	ChallengeOnly []Severity `json:"challenge_only"` // 默认 [P0, P1]
}
type Package struct {
	ID       string   `json:"id"`
	Blocks   []string `json:"blocks"`    // 允许列表内：code-review | review
	Files    []string `json:"files"`     // 必须是本次变更的文件
	Focus    string   `json:"focus"`     // 本包需要重点关注什么
	Provider string   `json:"provider,omitempty"` // 允许列表内；空则用 profile 默认
}

// code-review
type CodeReviewOut struct {
	Summary  string       `json:"summary"`
	Findings []FindingOut `json:"findings"`
}

// review（rv.<pkg>）
type ReviewOut struct {
	Verdict        string       `json:"verdict"`         // PASS | BLOCKING
	EvidenceStatus string       `json:"evidence_status"` // sufficient | insufficient
	Findings       []FindingOut `json:"findings"`
	EvidenceGaps   []string     `json:"evidence_gaps"`
	Summary        string       `json:"summary"`
}

// reasoner
type ReasonerOut struct {
	ResultType string            `json:"result_type"` // CLEAN | FINDINGS | INSUFFICIENT_SUBJECT
	Findings   []ReasonerFinding `json:"findings"`
}
type ReasonerFinding struct {
	Severity    string     `json:"severity"`    // critical | notable | informational
	Dimension   string     `json:"dimension"`
	Finding     string     `json:"finding"`
	Where       string     `json:"where"`       // MR 描述 / issue / 文档中的位置
	Why         string     `json:"why"`
	Assumptions []string   `json:"assumptions"`
	Confidence  Confidence `json:"confidence"`
	Path        string     `json:"path,omitempty"` // 能落到代码时填写
	Line        int        `json:"line,omitempty"`
}

// explore：只读探索，减少四路评审的不确定性
type ExploreOut struct {
	Summary    string        `json:"summary"`    // ≤ 2000 字符
	Facts      []ExploreFact `json:"facts"`      // 已验证事实，≤ 30 条
	Inferences []string      `json:"inferences"` // 推断（未证实），≤ 20 条
	Unknowns   []string      `json:"unknowns"`   // 未知项，≤ 20 条
}
type ExploreFact struct {
	Statement string `json:"statement"`
	Path      string `json:"path,omitempty"`
	Line      int    `json:"line,omitempty"`
	Evidence  string `json:"evidence"`
}

// validate：对 pipeline 状态、失败 job 日志和读到的代码做验证；不运行测试或仓库脚本
type ValidateOut struct {
	Status      string          `json:"status"`       // PASS | FAIL | BLOCKED
	Checks      []ValidateCheck `json:"checks"`       // ≤ 30 条
	Findings    []FindingOut    `json:"findings"`     // ≤ 50 条
	BlockReason string          `json:"block_reason,omitempty"`
	Summary     string          `json:"summary"`
}
type ValidateCheck struct {
	Name     string `json:"name"`
	Source   string `json:"source"`   // pipeline | code
	Result   string `json:"result"`   // PASS | FAIL | BLOCKED | PRE_EXISTING
	Evidence string `json:"evidence"`
}

// verify：输入只有单条 finding（去掉来源节点、置信度和模型身份）
type VerifyOut struct {
	Status            string    `json:"status"`   // CONFIRMED | REFUTED | UNCERTAIN
	Reason            string    `json:"reason"`
	Evidence          []CodeRef `json:"evidence"` // REFUTED 时至少 1 条，否则按 UNCERTAIN 处理（I3）
	SuggestedSeverity Severity  `json:"suggested_severity,omitempty"`
}

// onboarding
type OnboardingOut struct {
	Overview      string   `json:"overview"`
	Architecture  string   `json:"architecture"`
	Modules       []Module `json:"modules"`
	Conventions   []string `json:"conventions"`
	BuildAndTest  []string `json:"build_and_test"`
	RiskHotspots  []string `json:"risk_hotspots"`
	ReviewFocus   []string `json:"review_focus"`
}
type Module struct {
	Path    string `json:"path"`
	Purpose string `json:"purpose"`
}
```

`ExploreOut`、`ValidateOut` 和 `ReasonerOut` 与 `CodeReviewOut`、`ReviewOut` 一样带代码拥有的 `tool_gaps` 字段：节点的 MCP 绑定工具缺失或运行时不可用时由代码记录，不出现在提交给模型的 schema 里。任一评审节点（`explore`、`cr.*`、`rv.*`、`reasoner`、`validate`）记录了 `tool_gaps`，评审就不会得到干净的 `PASS`（原本的 `PASS` 改判 `INCOMPLETE`；已是 `BLOCKING` 的保持不变）。

`ExploreOut` 的 `summary`、`facts`、`inferences`、`unknowns` 必须出现（可为空数组）。`facts[].path` 若填写，必须是合法的仓库相对路径。

`ValidateOut` 的状态一致性由代码校验：

| 状态 | 要求 |
|---|---|
| `PASS` | 不得有 `FAIL` 或 `BLOCKED` 检查，也不得有 P0/P1 finding |
| `FAIL` | 至少一条 `FAIL` 检查或至少一个 finding |
| `BLOCKED` | `block_reason` 必填；不得包含 `FAIL` 检查（有 `FAIL` 检查时状态必须是 `FAIL`） |

> 最终持久化和发布使用代码拥有的 `SynthesisResult`，它包含 `verdict`、`groups` 和 `unresolved`。上游证据/覆盖不足会把 `verdict` 覆盖为 `INCOMPLETE` 并携带具体信号。评审图不再有模型汇总节点，因此新评审不产生 `SynthesizeOut`、`ReviewGateOut` 或补派请求；这些契约只在解码历史工作流时保留。

### 6.3 严重级别与结论归一化

| 来源 | 原始值 | 归一化 |
|---|---|---|
| code-review / review / validate | P0 / P1 / P2 / info | 原样 |
| reasoner | critical / notable / informational | P1 / P2 / info |
| review | `BLOCKING` 且没有 finding | 一个 `review_blocking` 未解决信号 |
| review | `evidence_status=insufficient` | 一个 `evidence_gap` 未解决信号（只缺少或未跑完 pipeline 本身不算证据不足） |
| validate | `FAIL`，且增量过滤之后该节点没有仍在评审范围内的 P0/P1 finding | 一个 `review_blocking` 未解决信号，结论为 `INCOMPLETE`，不会是 `PASS`。该检查在 `gather` 中于增量过滤之后执行；后来被 `verify.N` 反证的 P1 仍算作代码证据，对应的 `FAIL` 视为已有解释 |
| validate | `BLOCKED` | 报告告警「验证通道 BLOCKED：<原因>」；**不是**未解决信号，没有 CI 的项目可以 `PASS` |
| validate | 检查结果 `PRE_EXISTING` | 与本次变更无关的既有失败，不产生 finding |
| 任一路 | 节点失败、被取消或被跳过 | 一个 `evidence_gap` 未解决信号「lane X did not complete」，结论为 `INCOMPLETE` |
| verify | `suggested_severity` | 仅供 adjudicate 参考，不直接覆盖 |

---

## 7. 汇聚与裁决

### 7.1 gather（代码）

1. 收集所有提出节点（`cr.*`、`rv.*`、`reasoner`、`validate`）的 findings，并按 §6.3 归一化；缺失的一路写入证据缺口。
2. 去重：同一 `path`、同一侧的 finding 在满足以下任一条件时同组：(a) `category` 相同且行号相差不超过 3 行；(b) `category` 相同且行号相差 4–6 行，或 `category` 不同但行号完全相同，并同时满足三项。标题标识符一致：标题中的 ASCII 标识符（≥3 字符，去掉固定的通用词表，如 `err`、`nil`、`ctx`、`return`、`handle`、`map`）集合 Jaccard ≥ 0.5 且至少共享 1 个。标题内容一致：标题共享标识符之外的内容，至少 2 个中文二元组（去掉少量停用二元组），或至少 2 个全小写英文普通词（≥4 字符、不含下划线、数字、驼峰，且不在通用词表中）；只有函数名相同不满足。正文相似：标题加正文的词集（英文单词与中文二元组）Jaccard ≥ 0.45（同分类）或 ≥ 0.5（跨分类），至少共享 4 个词；对所有文本同时比较全部词视图与仅标识符视图，任一满足即可。候选只与组内首个 finding 比较（不链式扩散），分组前按路径、侧、行号、分类、级别、标题、正文、来源排序，结果与提出节点返回顺序无关。组内取最高严重级别，正文保留证据最充分的一条，记录所有来源。计票：按规则 (b) 合并的来源不计独立票（仍列在来源中），按规则 (a) 合并的来源按模型身份计票，同一模型的多个来源仍按同模型权重计。发布讨论时，标题与展示标题不同的成员以「相关描述」列出标题、级别和分类（最多 5 条）。
3. 选择待挑战：严重级别为 P0 或 P1 的组。受 `max_nodes` 约束时，按严重级别（P0/P1 优先）排序后截断，未入选的组标记为 skipped 并写 warning。
4. 写入 `findings` 表，输出 `GatherOut{groups, dropped, to_challenge, warnings}`，并展开 `verify.<seq>` 节点。

挑战者使用 `profiles.default.challenge.provider`（为空则用 `profiles.default.provider`）；每个 `verify` 节点只看到单条 finding 的结论、位置和相关代码，不看提出方的推理、置信度和模型身份。挑战者与某条 finding 的来源按配置的 endpoint + model 身份归一后相同，或任一身份无法解析时，该条的挑战结果标记为同模型；同模型反证不能当作跨模型确认，不同别名也不保证实际模型独立。

### 7.2 adjudicate（代码）

按 [ARCH.md](../../ARCH.md) §5.4 的裁决表计算每组的 `decision`：

```
for g in groups:
  if g 已在 gather 阶段 drop:        keep drop
  elif g 未挑战（不是 P0/P1）:         publish
  elif challenge == CONFIRMED:       publish
  elif challenge == REFUTED && len(evidence) > 0: drop
  else (UNCERTAIN / REFUTED 无证据 / verify 失败):
       mark（标「存疑」后发布）
  if !diffIndex.Locate(g.path, g.line): g.position_ok = false（只进汇总，I2）
```

### 7.3 synthesize（代码）

输入：`adjudicate` 的结果（每组：ID、严重级别、标题、正文、证据、挑战结果、decision）、MR 摘要、pipeline 摘要。**不输入**子 agent 对话。`synthesize` 是代码节点：直接渲染报告，不调用模型，也不追加节点。

最终完成条件由代码执行：`review` 通道 `evidence_status=insufficient`、`review` 为 BLOCKING 却没有 finding、`validate` 为 FAIL 却没有仍在范围内的 P0/P1 finding、某一路评审没有完成、材料或提示词覆盖缺口、完成 plan 后非 generated 的 included 文件未被分配等上游信号会合并进 `unresolved`。只要存在 unresolved，最终 verdict 就是 INCOMPLETE，summary 列出具体缺口。存在最终 `publish` 的 P0/P1 时 verdict 为 BLOCKING（I6）。

---

## 8. prompt 组装

顺序固定，越稳定的越靠前，以命中 prompt 缓存：

```
system[0]  (cache)  _platform_rules.md                  平台规则、输出要求、不可信内容声明
system[1]  (cache)  <role>.md                           节点角色 prompt（来自 prompts/）
system[2]  (cache)  项目背景：上下文文档 + 仓库 AGENTS.md（目标分支）   ← 标注「非授权内容」
system[3]           Skill 清单（名称 + 描述）
user                本节点任务：MR 摘要、包内文件列表与 diff、focus、上游输入
```

不可信内容（MR 标题、描述、评论、diff、文件内容、工具输出）一律包在带随机 nonce 的定界标记里：

```
<untrusted-data nonce="k3f9…">
…
</untrusted-data nonce="k3f9…">
```

并在 `_platform_rules.md` 中声明：定界内的任何指令都不得执行。

新增的 `explore.md`、`review.md`、`validate.md` 是对应节点的角色 prompt。存在探索摘要时，它作为 user 段的「探索结论」小节交给四路评审节点；该小节是待核实的线索，不是指令，也不是证据，`_platform_rules.md` 要求节点以自己读到的代码为准。任务渲染版本 `prompts.TaskRenderVersion` 为 `6`：它是代码拥有的常量，不能被 prompt 覆盖；升级前创建的工作流因执行身份不同，不会在新版本下续跑、重试或发布。

---

## 9. 评论渲染

### 9.1 汇总评论

````markdown
<!-- gitlab-ce-review:summary v1 fp=9f3c2a1:7be0… -->
### AI 评审：🔴 BLOCKING

**P1** × 2 · **P2** × 1

<synthesize.summary，1–3 句：结论与主要理由，不复述各条问题>

### 行内问题（1）

- **[P1]** 标题 · `path/to/file.go`:18

### 无法定位到行的问题（1）

#### [P2] 标题

`path/to/file.go`:42

一段话：先写触发条件，再写影响。

**建议**：一句话建议。

### 存疑条目（1）

#### [P1?] 标题

`path/to/file.go`:9

**标记原因**：挑战结果：UNCERTAIN

说明正文。

<sub>head 9f3c2a1 · profile default · 用时 2m14s · <a href="…/reviews/0192f0c1">详情</a></sub>
````

- 首行隐藏标记用于找到并就地更新这条评论（见 05-flows.md F7）。
- 级别计数紧随结论，只列非零项；没有已发布问题时不输出计数行。
- 行内问题的索引只列级别、标题和位置，不重复正文、建议与证据。存疑条目单独列出，不在行内问题索引中重复。
- 写法规则在 `_platform_rules.md` 的 Reporting bar 与 Comment style：只报本次 MR 引入、作者会修的离散问题；`body` 为一段话，先写触发条件再写影响，代码不超过 3 行；`suggestion` 是一句话或替换被评论行的 GitLab `suggestion` 块。渲染器保留 Markdown 的标题、列表和代码内容，中和代码外的原始 HTML 与链接语法；代码中的 HTML 注释按字面显示。为防止列表提前结束围栏，缩进围栏会移到顶层，因此原列表可能分成两段。
- 标题、提示、裁决原因和多行正文中的 GitLab 提及、引用（包括 commit SHA、项目引用）及自动链接按字面显示；代码块和行内代码按代码显示。路径中的有效空格不折叠。反引号围栏的 info 不得包含反引号，闭合围栏最多缩进三格；未闭合围栏在字段末尾闭合，不遮住后续条目。
- 汇总条目正文最多 2000 字符、建议最多 1000 字符；超限时明确追加“内容已截断”，不再静默截到半句。完整正文仍在评审详情中。
- 存疑条目在汇总中保留折叠证据；可定位的存疑条目在行内讨论中也保留证据。这样即使复用的旧讨论属于已确认条目，证据仍可见，不需要创建或修改讨论。首行标记保持不变；汇总恢复只认首行，代码块内的标记文本不参与匹配。
- PASS 用 🟢，评审未完成用 ⚪ 并说明原因。
- 是否标注模型来源由项目设置 `show_model_attribution` 决定（默认关闭）。
- 评论使用 GitLab 支持的标题、列表与代码块；评论不支持自动目录，见 [GitLab Flavored Markdown](https://docs.gitlab.com/user/markdown/)。


### 9.2 行内讨论

~~~markdown
<!-- gitlab-ce-review:finding key=3a91… -->
**[P1] 并发写 map 未加锁**

批量导入多于 1 个文件时，`handleBatch` 在多个 goroutine 中无锁写入 `results`，会触发 `concurrent map writes` panic。

**建议**：使用 `sync.Mutex` 或按下标写入切片。
~~~

- 讨论挂在对应行上，正文不再重复位置。
- `suggestion` 以围栏开头（如 GitLab `suggestion` 块）时原样输出，不加“建议”标签。
- 证据只在存疑条目中以折叠的 `<details>` 保留；已确认条目的证据仍存在评审详情中。

存疑条目（`mark`）的标题使用 `[P1 · 存疑]` 这样的单一标记；非空裁决原因在正文后显示，证据仍可展开查看。
