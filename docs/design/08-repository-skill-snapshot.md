# 08 仓库 Skill 快照语义

> 状态：已实现（2026-11-19）；对应 [Issue #545](https://github.com/LeXwDeX/Tribunal/issues/545)。实现偏差登记在 [README](README.md#实现状态与偏差当前本地实现)。
>
> 本文定义评审期仓库 Skill 快照哈希与 MR diff 派生 deny 的关系。读者是维护 #545 相关代码的开发者；「现状」均已对照源码核实，引用以「文件 + 函数名」给出。
>
> 符号：**R0** 是目标分支 tip 上 `.claude/skills/` 下仅按项目 ignore 策略过滤后的文件集（键为相对 `.claude/skills` 的路径）；**deny** 是由 MR diff 派生的路径集合（仓库相对路径）；**R = R0 \ deny**；hash(·) 是 `repo.RepositorySkillsSnapshotSHA256`。

---

## 1. 背景

- **授权路径（改动前）。** `internal/app/repo_skills_authorization.go` 的 `ReplaceRepositorySkillAuthorization` 经 `repo_skills_source.go` 的 `discoverRepositorySkills` 调用 `repo_skills_runtime.go` 的 `loadRepositorySkillSnapshot(..., deny=nil, ...)`：只按项目 ignore 策略过滤 `.claude/skills/` 下的文件得到 R0，并用 `repo.RepositorySkillsSnapshotSHA256` 计算哈希。授权行按 (project, target_branch, block) 保存 `TargetSHA`、`SnapshotSHA256` 和授权的 skill 名（`repo:<frontmatter name>`）。
- **运行路径（改动前）。** `review_service.go` 的 `RunReview` 用 MR diff 派生 deny：`repo_skills_source.go` 的 `repositorySkillDenyPaths` 取 ignore 命中的 diff 路径、rename 另一侧路径、被拒目录下的子路径，排序后存入 dossier 的 `RepositorySkillExcluded`。`resolveRepositorySkillRuntime` 以 ignore ∪ deny 过滤后计算哈希，要求它与每个非空授权的 `SnapshotSHA256` 相等，否则返回 `PermanentJob`，整单失败。
- **恢复与发布复核（改动前）。** `review_recovery.go` 的 `recoverWorkflow` 与 `repo_skills_runtime.go` 的 `Publisher.repositorySkillPublicationCurrent` 用 `repositorySkillDenyFromEvidence` 从 dossier 证据重建同一 deny，再比较哈希。
- **后果。** MR 只要把 `.claude/skills/` 下任何文件（包括未授权 skill 的文件、非 skill 文件）rename 进或移出 ignore 路径，运行期哈希就与授权哈希不等，整单永久失败；重新授权仍按 `deny=nil` 计算，无法修复。测试 `TestRepositorySkillRuntimeFailsClosedWhenIgnoredRenameChangesSnapshot` 固化了该行为（实现时已合并删除，见 §11），但被影响的 `hidden` 并未授权，授权的是 `repo:demo`。该测试的 fixture `repositoryRuntimeRenameFixture` 默认把授权哈希设为「已按 MR deny 过滤后」的集合，与生产授权流程不一致。
- **补充发现：PR #582 改的提示用户看不到。** `RunReview` 永久失败收尾把状态 detail 固定为 `reviewExecutionFailedReason`（`review execution failed`），`switch` 只区分 token 被拒、superseded / MR 已关闭和 `ErrInvalidRepoConfig`。`PermanentJob(errors.New(...))` 没有 `ErrorCode()`，`errclass.Of` 把它归为 `internal`，日志只记 `error_class`。因此 #582 改写的文案既不进 MR 状态评论，也不进日志。
- **deny 的目的。** 被 ignore 的文件内容（含 rename 同源的另一侧）不得进入模型。
- **关键事实。** 沙箱只挂载 head、base 与已绑定 skill 的根目录（`internal/review/executor.go` 的 `Executor.sandboxFor`；`callAgent` 传入的 `skillRoots` 来自 `skillBinding.Roots`），`load_skill` 只接受已绑定名。因此未授权 skill 的文件是否被 deny，不影响模型可见内容；现状多拦的那部分没有保护作用。

---

## 2. 目标与不变量

- (i) 模型只能看到管理员审过的内容。
- (ii) ignore 内容（含 rename 同源侧）不进模型。
- (iii) 发布、恢复时的能力复核仍然严格。
- (iv) 已有 dossier 与在途 workflow 升级兼容。

---

## 3. 方案比较

| 方案 | 做法 | (i)–(iv) | 结论 |
|---|---|---|---|
| A | 维持整单 fail-closed，只改提示与文档 | 四条都成立 | 不采用：与授权 skill 无关的 rename 仍使整单永久失败，管理员只能撤销整条分支授权 |
| **B** | 哈希只校验 R0（与授权同一过滤器）；deny 只用于 materialize（R = `R0 \ deny`）；deny 剔除了任一被授权 skill 目录下的文件时，该 MR fail-closed，否则继续 | 四条都成立 | **采用** |
| C | 哈希语义同 B；被授权 skill 受影响时从绑定中剔除并降级继续 | (iv) 不成立 | 不采用，见下 |

不采用 C 的原因：

1. provenance 需要新字段，旧二进制的 `RepositorySkillProvenance.UnmarshalJSON` 拒绝未知字段，必须升版本，执行身份摘要随之变化。
2. 更重要的是：deny 由 MR diff 派生，MR 作者可以把某个 skill 文件 rename 到 ignore 路径，借此摘掉评审自己的 skill 而仍得到一个完成的评审。B 下同样的手法只会让该 MR 的评审失败。

另有两条未列入比较的做法，均不采用：

- 把 MR 派生的 excluded 纳入授权：授权按 (project, target_branch, block) 记录，deny 按 MR 派生，不可行。
- 按 skill 摘要授权：需要迁移与 API 变更，而授权仍钉 `TargetSHA`，没有增益。

---

## 4. 详细设计

### 4.1 快照语义

`loadRepositorySkillSnapshot` 去掉 `deny` 参数，只返回目标 commit、R0 和 hash(R0)。授权、运行、恢复、发布复核都用 hash(R0) 与授权的 `SnapshotSHA256` 比较；`provenance.SnapshotSHA256` 记录的也是 hash(R0)。授权路径（`discoverRepositorySkills`）只是少传一个 `nil`，行为不变。

### 4.2 运行时文件集

`internal/repo` 导出 `FilterRepositorySkillFiles(files, deny)`，复用 `repo_skills.go` 的 `deniedRepositorySkillPath`（逐级前缀判定）。`files` 的键相对 `.claude/skills`，而 deny 接受仓库相对路径，所以实现要先给键加 `.claude/skills/` 前缀再判定。它必须与 `ReadRepositorySkills(ignore ∪ deny)` 对同一份目标 tree 给出相同结果（§6 有对应用例）。

运行时只 materialize R；R0 中不在 R 里的文件只参与哈希与 4.3 的检查，不落盘。

### 4.3 被授权目录剔除检查

`repo_skills_runtime.go` 新增 `repositorySkillRuntimeFiles(r0, deny, grantedNames)`，返回 R 或错误。`resolveRepositorySkillRuntime` 在 grant 与哈希校验通过之后、materialize 之前调用它。「被授权」取该分支所有非空 grant 的 skill 名并集。

`internal/skill` 新增 `RepositorySkillDirs(files) (map[string]string, error)`：skill 名 → 顶层目录，键与 `RepositoryCatalogue` 返回的 `Skill.Name` 同形（`repo:<frontmatter name>`），可直接用授权名查找。`RepositoryCatalogue` 改为基于它实现，保证只有一条解析路径（同一套 frontmatter 校验、`<dir>/SKILL.md` 判定、重名规则）。`RepositoryCatalogue` 还需要 description，实现时可由包内共用的解析函数同时承载两者。

规则（在 R0 上按 `SKILL.md` frontmatter 的 `name` 映射到顶层目录，不假设目录名等于 skill 名）：

| # | 情形 | 结果 |
|---|---|---|
| 1 | 被授权名在 R0 中找不到，或 R0 内 skill 重名 | 失败（防御性：hash(R0) 已匹配时不会出现）：普通 `PermanentJob`，沿用「…reauthorize repository skills」文案，不用新 sentinel |
| 2 | 被授权目录下任一文件（资源文件、嵌套 `SKILL.md`）被 deny 剔除 | 失败：`ErrRepositorySkillExcludedByMR` |
| 3 | deny 命中 `.claude` 或 `.claude/skills` 祖先：R 为空，被授权目录全被剔除 | 失败：同第 2 条 |
| 4 | deny 指向 R0 中不存在的路径（如 MR 新增文件） | 无剔除，继续 |
| 5 | 未授权 skill（或顶层非 skill 文件）被剔除 | 被拒文件不落盘；其余未授权文件留在临时目录，但不绑定、不挂载 |
| 6 | 有非空 grant 而 deny 为 nil | 失败（防御：运行路径总应提供 deny）：普通 `PermanentJob`（如 `repository skill filter is unavailable`），不用新 sentinel |

只有第 2、3 条是 MR 内容引起的，才使用 `ErrRepositorySkillExcludedByMR`；第 1、6 条属于不变量被破坏，保持普通永久失败，`error_class` 仍为既有类别。

实现步骤：

1. `RepositorySkillDirs(r0)`，出错则失败（第 1 条）。
2. `r := FilterRepositorySkillFiles(r0, deny)`。
3. 对每个被授权名：在映射中找不到则失败（第 1 条）；R0 中以 `<dir>/` 为前缀的每个键都必须仍在 `r` 中，否则失败（第 2、3 条）。
4. 返回 `r`。

### 4.4 恢复与发布复核

- 复核顺序：先比 revision、`TargetSHA`、hash(R0)（既有的 `repositorySkillProvenanceCurrent` 与哈希比较），再用 dossier 的 `RepositorySkillExcluded` 经 `repositorySkillDenyFromEvidence` 重建 deny，重做 4.3 检查。
- `RunReview` 与 `recoverWorkflow` 都经 `resolveReviewExecution` → `resolveRepositorySkillRuntime`，4.3 检查随之生效，无需单独接线。发布复核在 `repositorySkillPublicationCurrent` 中显式重做，丢弃返回的 R，只看是否出错。
- 发布复核需要被授权名：应取自已校验过 revision 的那次授权读取（让 `repositorySkillProvenanceCurrent` 把 grants 一并返回，或在其内完成检查），避免两次读取之间授权变化。
- 复核失败：发布侧沿既有 stale publication 路径（`repo_skills_provenance.go` 的 `rejectRepositorySkillPublicationLocked`，经 `repositorySkillPublicationError` 返回 `ErrStalePublication`），不发布任何内容；恢复侧沿既有 `recoveryCancel(..., "repository_skill_resolve_failed", ...)`；`RunReview` 续跑路径同样取消工作流。

### 4.5 错误分类与用户提示

- 新增 sentinel `ErrRepositorySkillExcludedByMR`，形式沿用 `internal/agent/tool.go` 的 `ErrUnsafeResponse`（零字段类型，实现 `Error()` 与 `ErrorCode()`）。`ErrorCode()` 返回 `repo_skill_excluded_by_mr`，符合 `errclass.Of` 要求的 `^[a-z][a-z0-9_]{0,31}$`。4.3 第 2、3 条返回 `PermanentJob(ErrRepositorySkillExcludedByMR)`，这样 `errclass.Of` 与 `jobSettleClass` 都直接给出该类别，日志里的 `error_class` 不再是 `internal` / `permanent_job`。错误文案不得出现「reauthorize」。
- `RunReview` 永久失败收尾的状态 detail `switch` 增加一个分支（示意），`state` 仍为 `failed`：

  ```go
  case errors.Is(err, ErrRepositorySkillExcludedByMR):
      detail = repositorySkillExcludedDetail
  ```

- detail 的约束：英文，与现有 detail 风格一致；不点名具体 skill；不超过 160 rune。MR 状态评论的「详情」列经 `sanitizeStatusDetail` 截断到 `reviewStatusDetailRunes`（160），存储侧上限为 500。示例（156 rune，措辞可调）：

  ```text
  this MR moves authorized repository skill files into or out of ignored paths; the review cannot use them. Rerun after merging or adjusting the authorization
  ```

- 该分支只写状态 detail，不另发 MR 评论（token 被拒、`ErrInvalidRepoConfig` 分支才额外发 note）。续跑路径（`workflow != nil` 且 dossier 使用仓库 Skill）仍走 `cancelRepositorySkillWorkflow`，不经过该 `switch`。
- hash(R0) 不等的分支改回 #582 之前的文案 `app: repository skill snapshot changed; reauthorize repository skills`。该分支此后对应的原因重新授权都能解决：授权后项目 ignore 策略变化；授权行没有有效快照哈希（迁移 `0023` 之前保存的授权，`resolveRepositorySkillRuntime` 对两者共用同一个判断）。#582 的「重新授权无法解决」移到新的剔除失败分支，由其专用 detail 与错误类别承载。

### 4.6 兼容与升级

- 已有的成功 dossier 必满足 hash(R0 \ deny) == `grant.SnapshotSHA256`，而 grant 哈希就是授权时的 hash(R0)，所以旧 `SnapshotSHA256` 与新算法一致。provenance 字段与 `Version` 不变，已绑定 skill 目录树哈希（`review_execution.go` 的 `reviewSkillTreeHash`）不变，执行身份摘要不变，在途 workflow 可以继续。
- 唯一例外：授权后 ignore 被放宽，且 MR deny 恰好补回差集。旧算法下通过，新算法下 hash(R0) 与授权哈希不等，fail-closed（该 workflow 恢复时被取消）；重新授权可以修复。
- 回滚旧二进制：新放行的 dossier（deny 剔除了未授权文件）会被旧逻辑判为快照变化，同样 fail-closed，不越权。

---

## 5. 代码改动清单

| 文件 | 改动 |
|---|---|
| `internal/repo/repo_skills.go` | 新增导出 `FilterRepositorySkillFiles`；更新 `ReadRepositorySkills` 注释（调用方只需传 ignore 策略，MR 派生排除由 `FilterRepositorySkillFiles` 负责） |
| `internal/skill/loader.go` | 新增 `RepositorySkillDirs`；`RepositoryCatalogue` 基于它实现 |
| `internal/app/repo_skills_runtime.go` | `loadRepositorySkillSnapshot` 去掉 `deny` 参数；`resolveRepositorySkillRuntime` 用 hash(R0) 比较、调用 `repositorySkillRuntimeFiles` 并只 materialize R、哈希不等分支改回「重新授权」文案；`repositorySkillProvenanceCurrent` 拆出 `repositorySkillProvenanceGrants` 供发布复核复用同一次授权读取；`repositorySkillPublicationCurrent` 比较 hash(R0) 并重做 4.3 检查；新增 `repositorySkillRuntimeFiles`、`repositorySkillGrantedNames` 与 sentinel |
| `internal/app/repo_skills_source.go` | `discoverRepositorySkills` 调用 `loadRepositorySkillSnapshot` 的签名随之变化（行为不变）；同步修正 `repositorySkillDenyPaths` 注释里「source filter」的说法 |
| `internal/app/review_service.go` | `RunReview` 永久失败收尾的状态 detail `switch` 增加新分支与 detail 常量 |
| `internal/app/review_recovery.go` | 无需改动：`recoverWorkflow` 只调用 `resolveReviewExecution` 与 `repositorySkillDenyFromEvidence`，两者签名不变，新检查经 `resolveRepositorySkillRuntime` 自动生效 |

无迁移、无 API 字段变化、无 prompt 变化。测试与文档改动见 §6、§7。

---

## 6. 测试计划

遵循 TDD（`AGENTS.md` §7）：先写失败测试，再实现，再跑范围内的测试集。

**先行的最便宜验证。** 先把下面「修改」一节里翻转后的用例在现有代码上跑一遍，确认它们失败，再动实现。

**修改**（`internal/app/repo_skills_runtime_test.go`）：

- `repositoryRuntimeRenameFixture`：默认授权哈希改为 R0（`demo` + `hidden`）。
- `TestRepositorySkillRuntimeFailsClosedWhenIgnoredRenameChangesSnapshot`：翻转为成功（建议改名）。断言 `provenance.SnapshotSHA256` 等于 hash(R0)，`RepositorySkillExcluded` 含 rename 两侧（`.claude/skills/hidden/SKILL.md` 与 `private/secret.txt`），`repo:hidden` 未进入 prompt。翻转后它与 `TestRepositorySkillRuntimeDenySetIncludesIgnoredRenames`（fixture 默认哈希改为 R0 后）的断言重叠，可合并为一个。
- `TestRepositorySkillRuntimeSnapshotFailureExplainsReauthorizationCannotResolve`：改为 deny 命中被授权的 `demo`（例如让 diff 中的 rename 目标落在 `.claude/skills/demo/` 下），断言错误满足 `errors.Is(err, ErrRepositorySkillExcludedByMR)`、`errclass.Of` 为 `repo_skill_excluded_by_mr`，且文案不建议重新授权。
- 其他断言快照不一致提示的用例改为期望「重新授权」文案：`TestRepositorySkillRuntimeRejectsTargetAndSnapshotMismatch` 的 snapshot 部分补断言含 `reauthorize repository skills`。
- 其余以「授权哈希 = deny 过滤后集合」构造的用例改用 R0：`TestRepositorySkillRetryRebuildsFreshDenyAfterEnablement`（自行计算授权哈希，且断言 `provenance.SnapshotSHA256`）；`TestRepositorySkillRuntimeDenySetIncludesIgnoredRenames` 与 `TestRepositorySkillRuntimeRecoveryAndRepublishUseStoredDenyEvidence` 经 fixture 默认值自动改用 R0。
- 现有以 nil deny 调用 `resolveReviewExecution` 的测试辅助（如 `testExecutionIdentity`）没有非空仓库授权，不会触发 4.3 第 6 条；改完后跑 `internal/app` 全量确认。

**新增**：

- `repositorySkillRuntimeFiles` 表驱动：4.3 每条规则一例，另加「顶层非 skill 文件被拒」与「多 block 授权取并集」。
- `FilterRepositorySkillFiles` 与 `ReadRepositorySkills(ignore ∪ deny)` 在真实 git fixture 上结果一致，沿用 `internal/app/repo_skills_boundary_test.go` 中 `TestRepositorySkillSnapshotUsesDossierIgnoreAndRenamePaths` 的 fixture（含 ignored 资源文件、rename 目标文件与反向 rename 的 `hidden/SKILL.md`）。
- `RepositorySkillDirs`（`internal/skill/loader_test.go`）：重名报错、无效 manifest 跳过、嵌套 `SKILL.md` 不算 skill。
- 发布复核：dossier 的 `RepositorySkillExcluded` 命中被授权目录时拒绝发布（`ErrStalePublication`，不发布任何 note）。
- 恢复：deny 只命中未授权 skill 的在途 workflow 可以续跑，`resolveReviewExecution` 的 `digest` 与传入空 deny（返回 false 的非 nil 函数）时相同。
- 剔除失败时的可观察值：MR 状态 detail 等于新常量（断言方式参照 `internal/app/review_invalid_config_test.go` 对 `status.Detail` 的断言），`errclass.Of(err)` 与 `jobSettleClass(err)` 均为 `repo_skill_excluded_by_mr`（`internal/app/errors_test.go` 的表驱动用例补一行）。
- provenance 与执行身份不变（4.6）：对 deny 不剔除任何被授权文件的 fixture，断言 provenance 各字段不变；建议以改动前现有代码的 `digest` 输出作为期望值写入用例，改动后必须保持不变。

---

## 7. 文档与发布说明

实现 PR 已同步：

- [`CHANGELOG.md`](../../CHANGELOG.md)：`[Unreleased]` 的 #545 条目已由 PR #582 的提示文案变更改写，写明行为变化：与被授权 Skill 无关的 rename 不再使整单失败；被授权 Skill 受 MR 影响成为单独的失败类别（`repo_skill_excluded_by_mr`）及其提示；哈希不等的提示改回建议重新授权。
- [`docs/operations.md`](../operations.md)：「仓库 Skill 授权迁移（v1.2.0）」一节已写明快照语义（哈希只按 ignore 策略过滤，与授权同口径；MR 派生排除只决定哪些文件进入评审）；「评审永久失败排查」一节已补充新的 `error_class` 与处置（调整 MR，使其不把已授权 Skill 的文件移入或移出 ignore 路径；或由管理员调整授权，之后重新触发评审）。
- 本文状态已改为已实现；实现偏差按 [README](README.md) 的约定登记在「实现状态与偏差」。

---

## 8. 验收

映射 Issue #545 的三条验收项：

| Issue 验收项 | 证据 |
|---|---|
| 增加覆盖该失败场景的回归测试（或文档核对） | 翻转后的原失败场景用例、被授权 skill 受影响的失败用例、恢复与发布复核用例（§6） |
| 自建 CI 与 GitHub 必需检查通过 | 自建 CI 报告 `RESULT PASS`，PR 描述写明被测 commit SHA；GitHub 必需检查通过（流程见 `AGENTS.md` §9）。该 PR 是行为变更，走 `AGENTS.md` §8 的完整评审链 |
| 不改变无关行为，文档 / CHANGELOG 按需更新 | provenance 字段、`Version`、已绑定目录树哈希、执行身份摘要不变的测试证据（4.6、§6）；`CHANGELOG.md` 与 `docs/operations.md` 已更新（§7） |

---

## 9. 未决问题

- **安全侧确认（已完成）。** 「运行时集合 ⊆ 授权集合」已获安全侧确认（2026-11-19），实现按方案 B 落地：仅 `R = R0 \ deny` 落盘并挂载；沙箱只挂载 head、base 与已绑定 Skill 的目录（`Executor.sandboxFor` ← `Binding.Roots`），未授权 Skill 的临时目录不在其中。已确认并接受：授权后放宽 ignore 且 MR deny 恰好补回差集时，旧 workflow 会收紧为失败（重新授权可修复）；未授权文件仍可能落盘但不挂载。实现评审重点是 Skill 名到目录的映射：`RepositorySkillDirs` 与 `RepositoryCatalogue` 共用 `repositorySkillManifests` 单一解析路径，重名或缺失即失败。
- **剔除失败的状态评论是否点名 skill。** 默认不点名，避免在 MR 公开评论中暴露授权配置。管理员可按日志中的 `error_class=repo_skill_excluded_by_mr` 定位（日志字段为 `project_id`、`mr_iid`、`generation`、`error_class`、`stage`，不含 skill 名）。
- **「旧二进制中断、新二进制恢复」的真实环境演练已列为验收（未执行）。** 缺隔离部署环境，按 `AGENTS.md` §7 记为 BLOCKED，见 [`docs/delivery-readiness.md`](../delivery-readiness.md)；不以单测代替。
- **可选加固（不在本 Issue 范围）：** 只 materialize 被授权目录。

---

## 10. 附：同期其他残留

与 #545 无关，可另开小 PR。

- **P2：`03-http-api.md` 未写 `max_files` 上限。** [`03-http-api.md`](03-http-api.md) 中 `PATCH /api/v1/config/overlay` 错误表的 `invalid_config` 行（「`max_files` / `max_diff_lines` 必须为正」处）没有写 `max_files` 的上限。应补充：本次请求直接写入的 `max_files` 必须在 1–200；从基础配置继承的超限值只归一为 200 并告警，不阻塞保存。这与 #588 / PR #591 的行为一致，见 [`06-config.md`](06-config.md)「控制台写回 overlay」一节。
- **P3：两个测试文件缺末尾换行。** `internal/config/review_defaults_588_test.go`、`internal/config/watch_588_test.go` 末尾缺换行，补一个换行即可（gofmt 会处理）。

---

## 11. 验证状态

本文的「现状」基于 `main` 的源码阅读与 Issue 核对，撰写时没有运行任何测试，也没有在现有代码上跑过 §6 里翻转后的用例。实现完成后，`go test ./internal/app ./internal/repo ./internal/skill` 全量通过（2026-11-19 复跑确认）。4.2 的等价性（`FilterRepositorySkillFiles` 与 `ReadRepositorySkills(ignore ∪ deny)` 结果一致），以及 4.6 的「provenance 与执行身份摘要不变」，均由 §6 的用例证明。

§6 建议「以改动前现有代码的 digest 输出作为期望值写入用例」一项未按字面量实现：仓库 Skill 授权的 revision 由存储层每次随机生成并进入 provenance 与执行身份，字面 digest 会随 fixture 变化；改为在同一 fixture 上断言排除未授权 Skill 前后 `resolveReviewExecution` 的 digest 相等，并逐项核对 provenance 与授权记录一致。该偏差登记在 [README](README.md#实现状态与偏差当前本地实现)。4.5 的 detail 常量已按示例文案定稿。§6 的用例按建议合并：原 `TestRepositorySkillRuntimeFailsClosedWhenIgnoredRenameChangesSnapshot` 的断言并入 `TestRepositorySkillRuntimeDenySetIncludesIgnoredRenames`，剔除失败用例命名为 `TestRepositorySkillRuntimeFailsClosedWhenAuthorizedSkillIsExcluded`。
