# 变更记录

格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [1.3.0] - 未发布

### 升级前必读

- **升级前让在途评审结束或取消；首次启动会补漏评审**：回滚到升级前快照时，旧版本会续跑快照时仍在途的评审，可能覆盖升级后已发布的 GitLab 汇总和状态评论。升级后的首次启动会运行启动补漏扫描，把窗口内从未评审的 MR 入队评审（按默认上限最多 20 次，产生模型用量）；不希望如此时，升级这次启动前设置 `gitlab.catch_up.enabled: false`。
- **全局只读迁移**：迁移 `0027` 不可逆。升级前按运维文档做一致性备份；回退需恢复升级前快照和匹配的旧版镜像。
- **模型存储与配置校验**：迁移 `0026` 不可逆，升级前按运维文档做一致性备份，回退需同时恢复旧版镜像和数据。所有配置 provider 的 `max_tokens` 必须为正数，名称不能含 `:`；temperature 为 0–2，thinking budget 和 context window 不能为负。旧配置不符合这些要求时会阻止启动。挂载的旧配置不会自动更新，须按供应方支持的上限填写 `max_tokens`。
- **模型参数校验与审计**：`providers.*.reasoning.effort` 只接受空值、`low`、`medium`、`high` 和 `max`，非法值会阻止启动，请在升级前检查配置。协议不会发送的 effort 或 thinking budget 会在启动时告警；调用详情新增实际发送参数，旧记录显示为旧记录。
- **LLM 出站规则**：默认拒绝链路本地、未指定、组播和受限广播地址，允许内网及 loopback 地址。升级后指向被拒地址的 provider 调用将失败，但不会因此阻止进程启动。可通过 `llm.egress.deny_cidrs` 和 `llm.egress.allow_cidrs` 调整，拒绝规则优先；重启后生效。为防止代理绕过地址校验，LLM 连接不再使用 `HTTP_PROXY` / `HTTPS_PROXY` 及其小写形式；依赖代理访问模型的部署升级前须提供可直接连接的模型网关，否则调用会失败。
- **用户管理 API 变更**：更新用户时必须提交用户列表返回的 `revision`；旧客户端仅发送 `disabled` 的请求需要升级。迁移 `0025` 不可逆，升级前按运维文档备份数据库及其关联数据。
- **行为变更：只有一个评审档位 `default`，评审图固定为「探索 → 四路并行评审 → 反证 → 裁决 → 报告」**（[#253](https://github.com/LeXwDeX/Tribunal/issues/253)）。`explore`（只读探索）之后，`code-review`（挂载内置 code-review Skill）、`review`、`reasoner`、`validate` 四路并行；`gather` 归一化后由 `verify.N` 逐条反证 P0/P1，`adjudicate` 规则裁决，`synthesize` 由代码渲染报告。`onboarding` 档位仍存在，只用于项目接入，不是评审档位。没有档位选择，也没有回到旧评审图的配置开关。
- **模型调用更多，成本预期更高**：每次评审的 LLM agent 节点从原来的一两个增加到多个。具体增幅以后另行测量，此处不给数字；按实际用量核对 `budget.max_tokens`。`budget.deadline` 默认值由 20m 改为 30m。
- **未终态工作流不会在新版本下续跑**：新增提示词 `explore.md`、`review.md`、`validate.md`，`_platform_rules.md` 升到 v5，`prompts.TaskRenderVersion` 由 4 改为 5。评审档位和提示词身份已变化，升级前创建的未终态工作流在启动恢复时被取消，不会在新身份下续跑、重试或发布；升级前核对未完成评审和待发布结果，需要时等待其结束或接受取消后重新触发。
- **已有的 `reasoner` 块绑定开始生效**：reasoner 一路现在以块 `reasoner` 运行，管理员此前登记在该块上的 MCP 工具和 Skill 绑定会开始作用于它；升级前核对这些绑定。
- **挂载的配置文件需要手工补充 `explore` 与 `validate` 的工具授权**：镜像不会改写已挂载的配置文件。要让这两路使用 codebase-memory 工具，把 `explore` 和 `validate` 加入 `default_extensions.mcp_servers[].tools`（见 `config/server.example.yaml`）；否则它们只带内置的 `read_file` / `grep` 运行。
- **配置需要修改**：`config.Load` 不会因旧配置失败，但旧档位和旧字段会被移除、迁移或忽略，每项在启动日志产生一行 WARN `configuration was migrated or ignored: <字段…>`（只含字段名，不含值）。升级操作：
  1. 把 `profiles.default` 中的 `propose` 改为 `provider`、`lanes.code_review.provider`、`lanes.review.provider`；`reasoner.provider` 改为 `lanes.reasoner.provider`；`challenge.provider: cross` 改为明确的 `challenge.provider`。
  2. 删除 `default`、`onboarding` 以外的档位，并清理项目档位、`settings.profile_by_label`、仓库 `.gitlab-review.yml` 的 `profile` 和评论命令中对它们的引用。遗留的旧档位名在评审时按 `default` 处理；但项目注册或更新为其他档位返回 400 `unknown profile`，`/review <名称>` 的名称不是 `default` 时命令被忽略；保存 `settings.profile_by_label` 时，值不是 `default` 的映射（包括 `onboarding` 和已移除的档位）被丢弃而不是被拒绝，遗留设置仍可编辑。`onboarding` 在这些位置都不被接受为评审档位。
  3. 启动后检查日志没有 `configuration was migrated or ignored` 告警。迁移告警在配置校验之前输出，校验失败时同样可见。`plan.max_packages` 高于 `min(17, (budget.max_nodes − 9) / 2)` 时会被降到该上界并告警；`budget.deadline` 低于 30m 也会告警。
  4. 回到旧评审图只能回退发布：恢复升级前的镜像和备份的配置，没有开关。
  逐项迁移规则见[配置设计 · 评审档位](docs/design/06-config.md#评审档位profiles)，操作步骤见[部署与运维 · 单一评审档位](docs/operations.md#单一评审档位升级)。

- **未终态工作流不会在新任务布局下续跑**（[#300](https://github.com/LeXwDeX/Tribunal/issues/300)）：`prompts.TaskRenderVersion` 在 1.3.0 中由 5 改为 7（#300 改为 6，#345 再改为 7）。升级前由更早版本创建的未终态工作流在启动恢复时被取消，不会在新布局或覆盖语义下续跑、重试或发布，过期的未发布结果也不会被发布；需要时等待其结束或接受取消后重新触发。

### 已验证范围（#294，本地、CI 与真实实例）

- 每个 Issue 的自建 CI `go` / `web` / `docker` 在合并前对 PR head 通过，覆盖率门槛 70%；本地 `go build ./...`、`go vet ./...`、scoped `go test`、staticcheck 与前端测试、`check:styles`、`build` 通过。
- 2026-11-05：v1.2.0 数据卷原地升级（迁移至 `0028`）、2 用户/2 项目矩阵、回退演练通过。
- 2026-11-06 第 1–5 轮：开发/测试环境、固定 sandbox 项目、真实 LLM provider 的真实实例验收。覆盖：错误 master key 拒绝启动并给出脱敏错误；多路并行下反复 SIGKILL 后同一 workflow 完成并发布；运行中改模型并重启后按 `profile_hash_changed` 取消且 GitLab 状态同步；用户模型与节点覆盖的调用审计与参数。
- 2026-11-06 升级夹具（隔离实例，v1.2.0 → `main` `92f6895` 候选）：迁移 `0025`–`0028` 应用；升级前 6 个终态工作流升级后状态、结论和发布情况不变；被 SIGKILL 中断的工作流因配置/提示词变化被取代并重建，新工作流完成且发布与 GitLab 状态一致；无覆盖时默认 `profile_hash` 与 v1.2.0 基线相等；回退演练（备份恢复后启动 v1.2.0）终态工作流可读。
- 发布一致性：发布前、发布中崩溃后只发布一次、无重复讨论；重启后新 head 的 workflow 约 6 秒创建；停机期间推送的 head 由启动补漏评审且不重复入队。
- 评审质量与安全：被忽略文件不再抑制其他文件的行内定位；近似重复项仅在展示层折叠，相邻的不同问题分别发布，无定位汇总条目大幅减少；机器人评论不再触发 GitLab quick action。
- 界面：评审档位编辑页和评审列表在 1440 / 390 宽度下做过人工检查，未附截图文件。
- 尚未完成：生产人工 Review（发布第 3 段）、候选 commit 上的 GitHub Actions（第 2 段）、tag 与 Release、#366；详见 `docs/delivery-readiness.md`。

### 已知限制

- 修改模型会重建 client 和信号量，旧 client 继续服务在途工作流，同一模型 ID 的实际并发会短时叠加新旧两份上限。
- 不支持 masterkey 轮换；软删除保留模型元数据并清空当前行密文，不保证擦除 WAL、快照或备份。
- 去重与折叠基于启发式：措辞改写后的重复可能保留，极少数情况下相隔 1–3 行、描述较泛的不同问题可能被合并（[#360](https://github.com/LeXwDeX/Tribunal/issues/360)、[#352](https://github.com/LeXwDeX/Tribunal/issues/352)、[#361](https://github.com/LeXwDeX/Tribunal/issues/361)）。
- 同一行、来自不同 lane 的同一问题可能各发一条讨论；规则模式汇总计数含被折叠项（[#366](https://github.com/LeXwDeX/Tribunal/issues/366)，修复中）。
- 启动补漏不处理停机期间被重新打开且 head 未变的 MR。
- 回退到 v1.2.0 必须恢复升级前快照；迁移 `0025` 及之后不可逆。

### 新增

- **启动后追赶停机期间错过的 MR head**（[#362](https://github.com/LeXwDeX/Tribunal/issues/362)，延续 [#294](https://github.com/LeXwDeX/Tribunal/issues/294)）：容器停机期间送达的 MR webhook 失败后，该 head 此前永远不会被评审。启动恢复完成后，对每个已启用且 token 可用的项目列出打开的 MR，若其当前 head 没有评审控制记录，则经与 webhook 相同的过滤规则（Draft、标签、目标分支、仓库配置）入队一次评审，触发方式为 `catch_up`（控制台显示「启动补评」）。已有该 head 控制记录（已评审、排队中、入队后被过滤或被取消）的 MR 不重复入队；每个（项目、MR、head）最多补评一次。新增 `gitlab.catch_up`（`enabled`、`max_per_project`、`max_total`、`max_age`，默认开启、10、20、24h；前两者限制入队的评审数，已评审的 MR 不占用额度；只考虑 `max_age` 内更新过的 MR；整体最长 2 分钟，连续 3 个项目失败即停止），升级后首次启动也会按此上限补评，不希望触发时先设 `enabled: false`；仅在启动时读取，修改需重启；每入队一个 MR 输出 INFO 日志 `catch-up enqueued review`，扫描完成后输出 `catch-up scan finished`（`projects`、`mrs_checked`、`enqueued`）。无数据库迁移，`catch_up` 是触发方式枚举的追加值。入队与评审控制记录的比较在同一事务内完成，列出 MR 之后到达的 webhook 不会被覆盖；`catch_up` 沿用 `mr_update` 的增量范围规则。同时在运维文档明确恢复取消的取消原因与发布原因的对应关系。
- **评审详情暴露 `usage_interruptions`，模型卡片参数改为可读摘要**（[#346](https://github.com/LeXwDeX/Tribunal/issues/346)，延续 [#294](https://github.com/LeXwDeX/Tribunal/issues/294)）：`GET /api/v1/reviews/{wf_id}`、图节点和节点详情新增可选整数 `usage_interruptions`（增量字段，为 0 时省略；顶层为单个节点的最大恢复重启次数），由事件日志投影得出，不新增迁移；此前 #325 引入的该计数只存在于内部视图，API 并不返回。评审详情用量面板新增「用量不完整」徽标（`usage_incomplete` 为真时）和「单节点最多中断恢复 N 次」（计数大于 0 时）。控制台模型卡片不再显示原始 JSON，改为键值摘要，只列出已设置的参数。`docs/design/03-http-api.md` 补充：草稿测试与创建一样要求正的 `params.max_tokens` 与 `params.max_concurrency`，创建与修改成功返回 `{id, effective}` 而非完整模型。
- **控制台档位页：节点参数覆盖、来源与同模型提示**（[#339](https://github.com/LeXwDeX/Tribunal/issues/339)，延续 [#293](https://github.com/LeXwDeX/Tribunal/issues/293)）：「模型与档位」页逐项显示 7 个节点的生效模型、来源（覆盖/继承覆盖默认/配置/配置默认/派生）、配置值和生效参数；管理员可设置模型与 `max_tokens`、`temperature`、`reasoning.effort`、`thinking.budget_tokens`（留空继承），校验错误显示在对应输入旁，修订冲突提示刷新，可恢复为配置；多个节点投票身份相同时提示「这些节点会被计为同一模型」。非管理员只读。
- **评审档位节点参数覆盖、来源标注与投票身份**（[#334](https://github.com/LeXwDeX/Tribunal/issues/334)，延续 [#291](https://github.com/LeXwDeX/Tribunal/issues/291)）：七个节点（`default`、`explore`、`lanes.code_review`、`lanes.review`、`lanes.reasoner`、`lanes.validate`、`challenge`）可覆盖 `max_tokens`、`temperature`、`reasoning.effort`、`thinking.budget_tokens`，校验规则与模型参数相同，节点参数逐字段叠加在 `default` 参数之上，作为请求值注入，不创建独立 client，并发与限流不变。`GET /api/v1/profiles/default` 按节点返回 `model`、`source`（`override` / `override_default` / `config` / `config_default` / `derived`）、`configured`、`configured_source`、`override`、`params`、`param_source`、`effective_params` 和 `vote_identity`（相同的节点在投票中算同一个模型）；`GET /api/v1/profiles` 列表与详情一致，`DELETE /api/v1/profiles/default/overrides` 返回完整视图。覆盖中的模型 ID 不存在、参数非法或键不在白名单内返回 400，字段路径如 `overrides.default.model`。

### 变更

- **`default` 覆盖优先于配置文件中的节点 provider**（[#334](https://github.com/LeXwDeX/Tribunal/issues/334)）：解析顺序为节点覆盖、`default` 覆盖、配置中的节点 provider、配置中的档位 provider、派生回退。已设置 `default` 覆盖且配置文件里有节点 provider 的部署，升级后这些节点会改用 `default` 覆盖的模型；清除覆盖即恢复配置行为。
- **模型调用审计新增字段**（[#334](https://github.com/LeXwDeX/Tribunal/issues/334)）：审计 JSON 增加可选的 `model_id` 和 `param_source`，`llm_calls` 表结构不变；档位哈希仅在设置了参数覆盖时才包含参数，无覆盖时哈希不变。
- **评审列表包含未创建工作流的评审**（[#331](https://github.com/LeXwDeX/Tribunal/issues/331)）：`GET /api/v1/reviews` 现在也返回在创建工作流之前结束的评审（`kind: "status_only"`，原因为 `empty diff`、`no reviewable files` 或 `snapshot omitted files`，同时写入 GitLab 状态评论的 `详情：` 行）；工作流项保持已有的 `kind: "review"`，其余字段不变。控制台以不可点击行显示这类评审。`next_cursor` 可能是 `m1.` 开头的复合值，客户端须将 cursor 视为不透明字符串。
- **用户管理页与全局视图**（[#297](https://github.com/LeXwDeX/Tribunal/issues/297)）：用户列表显示角色来源、全局模式与授权数；管理抽屉支持提升/降级管理员、开关全局模式、按用户增删项目授权；配置来源和自我降级被禁用。项目列表标记“只读（全局）”，评审页增加“全部评审/我的项目”切换，页头显示全局模式标识。
- **评审档位节点覆盖界面**（[#293](https://github.com/LeXwDeX/Tribunal/issues/293)）：模型页的「评审档位」区块显示 7 个节点的生效模型和来源，管理员可在 Drawer 中选择模型或恢复为配置；unavailable 模型不可选，保存带 revision。参数覆盖表单与同模型提示尚未实现。
- **模型管理界面**（[#292](https://github.com/LeXwDeX/Tribunal/issues/292)）：模型页改为 `/models` 数据源，支持配置模型只读展示与用户模型新增、编辑、删除、草稿测试；API key 三态且只存在抽屉局部状态；同名模型显示短 ID；被引用删除、revision 冲突和出站拒绝有专门提示。概览页健康状态改用 `/models`。
- **评审档位模型覆盖**（[#291](https://github.com/LeXwDeX/Tribunal/issues/291)）：新增 `review_profile_overrides` 与模型引用表，可按 7 个节点覆盖 `default` 的 provider；覆盖在 `profileFor` 中逐工作流合并，不修改共享配置。当前仅实现 provider 覆盖与 API，参数覆盖、来源标注和 `/profiles` 生效值仍待补齐。
- **工作流模型快照**（[#299](https://github.com/LeXwDeX/Tribunal/issues/299)）：工作流启动时固定所用 client，进程内 Refresh 不影响在途工作流；跨重启恢复仍按当前配置重算执行身份，不一致时取消并记录 `execution_identity_changed`；被引用用户模型的变化由档位哈希承担（见 #327，恢复日志的 cause 为 `profile_hash_changed`），不属于执行身份。无用户模型时 profile hash 与旧版一致。
- **用户模型目录与运行时刷新**（[#289](https://github.com/LeXwDeX/Tribunal/issues/289)）：配置模型与数据库用户模型合并到运行时目录，用户模型无需重启即可被新工作流使用；单条坏记录隔离为 unavailable，不影响启动和其他模型。修改模型会重建 client 和信号量，旧 client 继续服务在途工作流，因此同一模型 ID 的实际并发会短时叠加新旧两份上限。用户模型的成本按保存的价格计算，MR 评论署名显示 display_name。
- **模型管理 API**（[#290](https://github.com/LeXwDeX/Tribunal/issues/290)）：新增 `/api/v1/models` 列表、详情、创建、修改、删除和测试端点，全部要求平台管理员；配置模型只读。写操作后刷新运行时目录，响应 `effective: "new_workflows"`。草稿测试端点暂返回 501，待后续补齐。
- **全局只读**：平台管理员可显式为用户开启或关闭所有项目及评审的只读权限。新项目自动可见，项目管理仍需单独授权；禁用用户期间不可见，重新启用后保留设置。变更立即生效并写入平台审计日志。
- **平台角色管理**：平台管理员可为已登录的 GitLab 用户授予或撤销管理员角色，并查看用户的项目授权；配置指定的管理员不能降级或禁用。角色变更在下一次请求生效。
- **评审档位与图固定**：`profiles.default` 的配置收敛为 `provider`（所有节点默认）、`explore.provider`、`lanes.{code_review,review,reasoner,validate}.provider`、`challenge.provider`（逐条反证）、`plan.package_tokens` / `plan.max_packages` 和 `budget`；`provider` 为空时依次取 `lanes.code_review.provider`、`challenge.provider`。plan 固定为 code 模式，P0/P1 逐条挑战，不确定的挑战结果标记为「存疑」后发布，报告由代码生成，评审图不在汇报阶段扩展。`Load` 把 `plan.max_packages` 降到 `min(17, (budget.max_nodes − 9) / 2)`（至少 1），配置值高于上界时告警，未配置时静默降低；构建评审图时仍要求 `plan.max_packages × 2 + 2 ≤ max_fan_out`（默认 36），超出则拒绝。既没有 `provider` 也没有 `lanes.code_review.provider` 时构建评审图失败，字段为 `provider`。
- **旧字段迁移或忽略**：`propose` 中第一个 `code-review` / `review` 条目分别迁移为 `lanes.code_review.provider` / `lanes.review.provider`，其余 `propose` 条目忽略；`reasoner.provider` 迁移为 `lanes.reasoner.provider`，`reasoner.when` 单独告警并忽略（reasoner 一路始终运行）；`challenge.provider: cross` 改用 `challenge.fallback` 作为挑战者，没有 fallback 时告警并由档位 `provider` 挑战；显式的新字段优先于迁移值（告警为「…is deprecated and was ignored: <新字段> is set」），迁移得到的 provider 仍被校验，错误指向新字段。`plan.mode`（非 code）、`plan.provider`、`consensus`、`challenge.only`（不等于 P0、P1）、非 `cross` 时的 `challenge.fallback`、`on_uncertain`（非 mark）、`synthesize`、`budget.max_extends`、`default` 下的 `agent` 被忽略并告警。
- **探索与四路评审**：`explore` 是可选的只读 agent 节点（超时 5 分钟），输出事实、推断和未知项；其摘要（不超过 6000 字符）写入每路评审节点的 params，作为待核实的线索。探索失败时四路评审不带摘要运行，报告给出告警。`explore`、`validate` 和 `reasoner` 与 `code-review`、`review` 一样在代码拥有的 `tool_gaps` 字段记录 MCP 绑定缺口，绑定工具缺失时评审不会得到干净的 PASS。每个包一个 `code-review` 和一个 `review`，每个 MR 一个 `reasoner` 和一个 `validate`。
- **validate 通道不运行测试**：`validate` 对应 opencode 的 verify 角色，证据只有 GitLab pipeline 状态与失败 job 日志片段（`source=pipeline`）和只读读码（`source=code`），输出 PASS / FAIL / BLOCKED 与逐项检查。`BLOCKED`（如没有 CI；`BLOCKED` 的检查里不得出现 `FAIL`，有 `FAIL` 检查时状态必须是 `FAIL`）只在报告中给出「验证通道 BLOCKED：<原因>」告警，不降为 INCOMPLETE，没有 CI 的项目可以 PASS；`FAIL` 在增量过滤之后必须能由该节点仍在评审范围内的 P0/P1 finding 解释，否则 gather 加入一个 `review_blocking` 信号，结论为 INCOMPLETE 而不是 PASS（后来被 `verify.N` 反证的 P1 仍算作代码证据）；与本次变更无关的既有失败记为 `PRE_EXISTING`，不产生 finding。
- **四路评审任一路缺失使结论为 INCOMPLETE**：四路评审节点都是可选节点，某一路失败、被取消或被跳过时，其他路的 findings 仍会发布，缺失的一路写入证据缺口「lane X did not complete」，结论不会是 PASS。`review` 为 BLOCKING 且没有 finding、证据不足同样给出对应的未解决信号；`reasoner` 的 critical / notable / informational 归一化为 P1 / P2 / info。只缺少或未跑完 pipeline 本身不算 `review` 的「证据不足」。
- **Prompt 与 MCP 授权**：新增可绑定块 `explore` 和 `validate`，可绑定块共八个。示例配置和 Docker 配置的 codebase-memory 默认授权为 `explore` 11 项只读工具、`validate` 与 `verify` 各 `get_code_snippet`、`search_graph`、`trace_path`、`search_code`。新的 agent 节点使用同一个 agent 循环，因此最后一轮强制提交（[#255](https://github.com/LeXwDeX/Tribunal/issues/255)）和提交提示轮的不合法工具调用恢复（[#269](https://github.com/LeXwDeX/Tribunal/issues/269)）同样生效。
- **大 MR 评审不再截断 diff**（[#300](https://github.com/LeXwDeX/Tribunal/issues/300)）：评审 prompt 先给出每个变更文件的索引，再在任务预算内完整内联 diff；放不下的文件整文件不内联，由新增的 `list_changes` 和 `read_diff` 工具按 hunk 读取。`read_file` 和 `grep` 新增 `rev=head|base`。覆盖判定要求未内联文件的所有 hunk 都经 `read_diff` 读过，节点输出记录 `diff_reads`；没读完时按证据缺口处理，不会给出干净的 PASS。单文件 16000 字符和单包 120000 字符的截断都已取消。大 MR 会因此增加读取工具调用，评审耗时和 token 用量可能上升。
- **空 diff 评审不再 PASS**（[#329](https://github.com/LeXwDeX/Tribunal/issues/329)）：空 diff 的 MR 返回 INCOMPLETE，摘要说明“没有可评审的变更”，启用 commit status 时写 failed；全部变更被 ignore 的既有 PASS 行为不变。控制台可见性由 [#331](https://github.com/LeXwDeX/Tribunal/issues/331) 跟踪。
- **控制台**：「模型与档位」页只把 `default` 显示为「评审档位」，按固定的八步拓扑（探索、提出、挑战、裁决、汇报）列出各步骤的 provider，`onboarding` 单独标为「项目接入」；项目注册、项目设置和手动触发评审对话框不再有档位选择，项目列表不再有档位列。执行阶段轨道里 `explore` 和 `plan` 归入「规划」，`code-review`、`review`、`reasoner` 和 `validate` 归入「提出」；「项目接入」一行在 onboarding 没有 provider 时显示「未配置」。
- **模型草稿测试**（[#333](https://github.com/LeXwDeX/Tribunal/issues/333)）：`POST /api/v1/models:test` 不再固定返回 501，可在保存前测试草稿；校验同创建，出站规则同用户模型，不保存数据、不刷新目录、不产生计费记录。编辑已有用户模型时可传 `id` 并省略 `api_key`，服务端使用已保存的凭据，且仅当协议和端点（scheme、host、port）未变时才复用，否则要求重新填写 `api_key`。成功响应为 `{ok, model, latency_ms}`。

### 修复

- **无法定位到行的问题不再重复列出已发布的问题**（[#361](https://github.com/LeXwDeX/Tribunal/issues/361)）：reasoner 常输出没有路径的 finding，与已发布的行内问题描述同一件事，摘要的「无法定位到行的问题」因此冗长（验收评审 18 条中现折叠 10 条、继续列出 8 条：2 条确为不同问题，其余 6 条标题措辞与行内条目差异较大，保守地继续列出）。渲染摘要时，没有路径或位置不可用、且已决定发布的非存疑条目，若与某条已列出的行内问题或更早列出的无法定位条目指向同一问题（标题中的函数/参数等标识符在对方标题或正文出现，且标题内容或正文词集相似；已知路径不同、候选比对方更严重、或严重级别相差 2 级以上均不合并；无法定位条目不会折叠到级别更低的条目下；对方标题没有标识符时，要求标题内容与正文词集同时相似；标题内容采用比行内折叠更严格的规则：先去掉停用词再取中文二元组（`未处理` 不再产生 `未处`/`理文` 片段），英文词排除标识符及限定名（`TokenBucket.allow`）的各部分，且共享内容须至少 2 个并覆盖候选标题的一定比例（同函数不同问题，如 `parse_amount` 的负数与空字符串，不再折叠）；行内折叠（#352）沿用原有的标题内容规则，与此规则分开，行为不变），不再单独列出，改为在对应条目下以「相关描述」引用（行内条目的子项最多 5 条，其余显示「另有 N 条」）。已发出的行内讨论正文与发布键不变；存储的 finding、票数、裁决和 verdict 不变；摘要头部的严重级别计数、分节数量与 commit status 描述只计实际列出的条目。与任何已发布条目都不相似的无路径问题仍然列出。
- **同 category 相邻行的不同问题不再被裁决前去重吞并**（[#360](https://github.com/LeXwDeX/Tribunal/issues/360)）：原规则只看同 category 且行号差 ≤3，不看内容，MR 验收中 `:13` 的独立缺陷（3 个 lane 报告）被并入 `:11` 的另一个问题组而从未单独发布。现在行号相同仍直接合并；相差 1–3 行时，只有标题与正文内容也相似（同一问题的模型行号偏差）才合并，票数仍累计到真正的同一问题上。同一 lane（同一节点同一尝试）列出的两条 finding 永不合并，避免单个 lane 行号漂到另一问题所在行时再次吞并。已知局限：措辞被大幅改写且相差 1–3 行的同一问题可能保持分开（票数被拆散，可能各自进入挑战）；共用通用措辞的相邻行不同问题仍可能被合并；不同 lane 的同行同 category 仍无条件合并。
- **被忽略文件（如 `poetry.lock`）不再让整个 MR 的行内讨论全部降级为摘要**（[#357](https://github.com/LeXwDeX/Tribunal/issues/357)）：此前只要有一个文件被 ignore，所有文件的行内定位和发现路径归属都被视为不可证明，真实 GitLab 上「代码文件 + `poetry.lock`」的 MR 得到 0 条行内讨论。现在只扣留被忽略/被省略的路径本身，以及与它们存在重命名关联的文件（旧路径等于被忽略/被省略路径的已纳入文件，或其路径是被忽略/被省略文件旧路径的已纳入文件）；行内定位与发现归属（共识身份、发布键）使用同一规则。材料快照新增可选字段 `ignored_old_paths`、`path_alias_info` 和省略项的 `old_path`；兼容性：纯增量，旧快照没有这些字段，仍按原先保守方式处理，不需要迁移。以下情形仍扣留全部路径：`Truncated` 但未记录省略路径、renamed/copied 文件缺少 `old_path`、省略原因为 `invalid_path` / `missing_rename_path`、被忽略的重命名缺少一侧路径、旧版本生成的快照。回退：本版本写入的快照被旧版本二进制读取时退化为全部歧义，发布键可能不同，崩溃后的讨论采纳可能无法匹配。
- **同一问题的近重复 finding 在发布时折叠为一条讨论，增量评审警告只列本次文件**（[#352](https://github.com/LeXwDeX/Tribunal/issues/352)）：此前各 lane 对同一问题的分类稍有不同，就会发布多条几乎相同的讨论。投票、共识和挑战所用的去重规则不在此改动范围（同分类、行号相差不超过 3 行，相差 1–3 行时另需内容相似，见 [#360](https://github.com/LeXwDeX/Tribunal/issues/360)）。发布时新增只改变展示的折叠步骤：已决定发布（`publish`、`mark`、`downgrade`）的组，在同一文件同一侧、两组最高级别相差不到 2 级、同分类且行号相差不超过 6 行或不同分类但行号相同，并且标题含相同的非通用标识符、标题共享至少 2 个中文二元组或英文普通词、标题加正文词集相似时，折叠为级别最高的那一组；其余组在讨论和汇总条目的「相关描述」中列出级别、分类、标题和正文摘要（最多 5 条，其余显示「另有 N 条」）。折叠只影响发出的评论和汇总计数：存储的 finding（decision、严重级别、票数、挑战结果）和统计都不变，被折叠的组不单独发帖也不保存发布键，反馈对应展示组；被驳回、丢弃的组和存疑组不参与与已确认组的折叠。该文件本身被忽略、被省略或对应多个变更时不折叠，MR 中其他被忽略或省略的文件不影响折叠。展示组沿用自己的发布键。增量评审的 `no textual changes to review` 警告只列增量文件集内的路径。
- **安全修复：发布正文中首字节为 `/` 的行不再被 GitLab 当作 quick action 执行**（[#353](https://github.com/LeXwDeX/Tribunal/issues/353)）：GitLab 会以机器人账号执行评论里首字节为 `/` 的行（实测 `/title` 会修改 MR 标题，`/close`、`/merge`、`/approve` 同理），模型输出或仓库内容可借此越权。现在所有发往 GitLab 的正文（行内讨论、摘要、失败摘要、命令回复等）把这类行的 `/` 改写为 `&#47;`，显示仍是 `/`；仅第 0 列、顶层且已闭合的围栏代码块保持原样，列表项内的围栏、HTML 注释或 HTML 块之后的围栏、未闭合围栏都不被信任；有前导空白的行、行内代码、引用不受影响，处理幂等。升级后，仅当旧版本已发布的行内讨论正文含此类行时，其正文哈希与新渲染不同，该问题会保留在摘要中而不再更新旧讨论；摘要会在下次发布时被更新为已处理的正文。
- **崩溃恢复后采纳已发出的行内讨论**（[#350](https://github.com/LeXwDeX/Tribunal/issues/350)）：此前若在行内讨论已发出、意图已记录远端 ID，但发布记录和讨论计数尚未写入时崩溃，重启后虽不会重复发送，却始终不采纳该讨论，问题被列入「无法定位到行的问题」并带「历史讨论内容与当前内容不同或无法确认」提示，`publications.discussions` 也会少计。现在新的行内讨论正文末尾带隐藏标记行 `<!-- gitlab-ce-review:workflow id=<工作流ID> -->`；恢复时，意图已有远端 ID（或经唯一的发现标记找回 ID）则重新读取该讨论，仅当作者是当前机器人用户、且正文（忽略首尾空白和 CRLF）与现在为该发现渲染的、含本工作流标记的正文完全一致时才采纳，写入发布记录（计数正确），摘要中仍按行内讨论显示。读取失败时发布保持可重试（状态仍为待处理/未知），不会重复发送。内容已变、其他工作流、其他作者或缺少标记时，沿用原有保守行为。兼容性：旧版本发出的讨论没有工作流标记，仍走保守路径、不会被采纳，不提供迁移。
- **进程被强杀后重启，被中断的任务不再等租约到期才继续**（[#351](https://github.com/LeXwDeX/Tribunal/issues/351)）：此前同一 MR 的新 head 要等被中断的 `review.run` 任务租约（默认 5 分钟）过期才能开始。现在服务取得数据目录锁并打开数据库后，在 worker 开始领取前，把仍处于运行中的任务立即释放为可领取（沿用租约过期的重试与尝试次数语义，已有排队后继任务的则取消），并记录 INFO 日志 `reclaimed stale job leases`。仅服务启动路径执行，嵌入方不受影响。
- **空文件、纯改名和删除空文件不再使评审 INCOMPLETE**（[#345](https://github.com/LeXwDeX/Tribunal/issues/345)）：状态为 `added`/`deleted`/`renamed` 且 diff 为空的文件没有文本内容可评审，不再计为覆盖缺口，路径列入评审警告「no textual changes to review」，变更索引标注「无文本变更」。二进制文件不受影响：GitLab 对其返回 `Binary files … differ` 文本（2026-11-06 在测试实例实测），按原有路径处理；`modified` 且 diff 为空（可能是仅模式变更）仍按覆盖缺口处理；`too_large`/`collapsed` 仍记为省略文件和覆盖缺口。计划阶段的文件列表同样标注，agent 规划遗漏这类文件不再算覆盖缺口，`list_changes` 对其显示 `inlined=n/a`。
- **启动恢复取消当前代次的评审后，状态评论不再停在「评审运行中」**（[#341](https://github.com/LeXwDeX/Tribunal/issues/341)）：恢复时因 `profile_hash_changed`、`execution_identity_changed`、`config_changed` 等原因取消、且仍是 MR 当前代次的工作流，现在会把评审状态置为「评审已取消」，详情含 `cancelled on recovery: <原因>` 并提示重新触发，同时更新 GitLab 状态评论；发布记录的跳过原因同为该文本，不再是 `review superseded`。代次已被取代时不改动新代次的状态。
- **`default` 与 `lanes.code_review` 覆盖现在真正改变执行的模型**（[#334](https://github.com/LeXwDeX/Tribunal/issues/334)，[#327](https://github.com/LeXwDeX/Tribunal/issues/327) 评审中发现的缺陷）：此前这两项覆盖只改变显示，评审仍按配置执行。现在覆盖合并进未归一化的 `default` 配置后统一归一化一次，七个节点的覆盖都影响实际模型。
- **控制台新建的用户模型被档位覆盖引用后，评审不再整体失败**（[#327](https://github.com/LeXwDeX/Tribunal/issues/327)）：此前评审图校验用的 provider 白名单只在启动时计算一次；启动后创建的用户模型一旦被档位覆盖引用，每次评审都会在创建工作流前以 `review execution failed` 失败（永久性 `build review graph` 错误），重启后才恢复。现在每次评审都按当前模型目录校验，运行时新建或删除的模型无需重启即生效，已软删除的模型会被拒绝。
- **新增或修改用户模型不再导致在途工作流被取消**（[#327](https://github.com/LeXwDeX/Tribunal/issues/327)，修正 [#299](https://github.com/LeXwDeX/Tribunal/issues/299) 的哈希约定）：`reviewProfileHash` 此前包含启动时已有的全部用户模型 ID，新增任一用户模型并重启就会改变所有在途工作流的档位哈希，恢复时被当作已被取代而取消。现在参与哈希的 provider 列表只含配置 provider 名称（无用户模型、无覆盖时与 v1.2.0 一致），`user_models` 只含档位实际引用的用户模型的非密钥设置（协议、base_url、model、参数、价格，不含凭据）。因此新增或修改未被引用的用户模型不影响在途评审；修改被引用模型的设置后，被中断的工作流在恢复时按失败即关闭取消（`cause=profile_hash_changed`），进程内正在运行的工作流仍沿用其启动时快照。**升级提示**：在用户模型已存在时由 v1.3.0 预发布版本启动的工作流带有旧哈希，升级后恢复时会被取消，需重新触发评审；v1.2.0 的工作流不受影响。
- **评审永久失败时输出可定位的诊断日志**（[#327](https://github.com/LeXwDeX/Tribunal/issues/327)）：服务端记录 WARN `review run failed permanently`，含 `project_id`、`mr_iid`、`generation`、`error_class` 和 `stage`（`control`、`inputs`、`config`、`execution`、`prepare`、`graph`、`workflow` 之一）；不记录原始错误。
- **进程重启后带 token 预算的评审可以续跑**（[#325](https://github.com/LeXwDeX/Tribunal/issues/325)）：进程被 SIGKILL 或崩溃后，启动恢复会重启运行中的节点（attempt+1）并把该工作流的用量标记为不完整（`usage_incomplete=true`，API 返回；控制台自 #346 起才显示）。v1.3.0 的不完整用量预算策略曾使设置了 `max_tokens > 0` 的工作流在恢复后所有模型节点都在调用模型前以 `unsafe_model_response` 失败，被中断的评审无法续跑。现在内部视图新增 `usage_unreported`（不完整源于进程中断以外的原因：节点上报不完整、无效用量或计数溢出）和 `usage_interruptions`（任一单个节点的恢复重启最大次数，同一轮恢复重启多个节点只计 1 次，不同节点在不同轮被中断也不累加）；仅当 `usage_unreported` 为真或 `usage_interruptions` 超过 3（同一节点被中断超过 3 次）时才以 `unsafe_model_response` 阻断，否则工作流以已提交用量作为下限继续。已接受的风险：每次中断可能丢失当时所有运行中节点尚未提交的全部用量（每一轮和节点内重试），实际花费可能超过记录总量；同一节点被中断超过 3 次后预算检查重新 fail-closed，工作流期限仍限制运行时长。被取代 attempt 的迟到提交仍会被丢弃且不记用量。
- **恢复取消原因写入日志**：启动恢复取消工作流前记录一条 INFO 日志 `recovery cancelling workflow`，带 `workflow_id` 和稳定的 `cause`（如 `profile_hash_changed`、`execution_identity_changed`、`config_changed`、`diff_version_changed`、`control_not_current`、`workspace_identity_changed`、`dossier_invalid`）；该日志记录取消决定，工作流已处于终态时仍可能出现。恢复重跑后发现工作流以取消结束时另记 INFO `recovery observed cancelled workflow`，`cause` 为 `superseded_during_run` 或保存的取消原因（如 `deadline`，为空则 `unknown`）。任务重试路径（RunReview/Handle）取消被取代的工作流前记录 `review cancelling workflow`，`cause=control_not_current`。工作流中保存的取消原因和发布行为不变。原日志 `recovery skipped workflow with changed configuration`（含 `config_hash`）已由 `cause=config_changed` 取代。
- **已保存模型测试返回字段名**（[#333](https://github.com/LeXwDeX/Tribunal/issues/333)）：`POST /api/v1/models/{id}/test` 此前返回 Go 字段名 `Model` / `LatencyMS`；现在与草稿测试一致，返回 `ok`、`model`、`latency_ms`（成功时 `ok` 为 `true`）。旧版 `/providers` 测试端点不变。
- **模型创建与修改返回字段级校验详情**（[#333](https://github.com/LeXwDeX/Tribunal/issues/333)）：`POST /api/v1/models` 与 `PATCH /api/v1/models/{id}` 校验失败时返回 400 `validation_failed` 和逐字段 `details`，不再是无说明的 400。
- **主密钥不匹配时明确拒绝启动**（[#332](https://github.com/LeXwDeX/Tribunal/issues/332)）：配置的主密钥打不开库中已有密文时，服务记录一行 ERROR `master key does not match stored encrypted data; refusing to start`（只含环境变量名，不含密钥值）并退出，错误为 `initialize secrets: master key does not match stored encrypted data`，取代此前误导性的 `migrate MCP secrets: storage update failed`。这是有意拒绝：继续启动会用另一把密钥加密新凭据，使同一数据库混用两把密钥。仍不支持主密钥轮换。
- **用户模型凭据纳入主密钥校验证据（纵深防御）**（[#332](https://github.com/LeXwDeX/Tribunal/issues/332)）：校验依据顺序为 MCP 校验标记、项目 token/webhook 密文，再到用户模型 API key 密文（任一能打开即可）；有证据但都打不开时拒绝启动，且不写入校验标记。校验标记自迁移 0017 起在每次成功启动时写入，因此含模型凭据却无校验标记的库通常不会出现，此项主要覆盖校验标记缺失（如被手工删除）的情形，避免用错误密钥写入新校验标记。

## [1.2.0] - 2026-10-29

> v1.2.0 已发布（2026-11-05，tag `v1.2.0` 指向 `4e95f4f`，GitHub Release 为 Latest）。候选范围是 [v1.1.0...v1.2.0](https://github.com/LeXwDeX/Tribunal/compare/v1.1.0...v1.2.0)，包含 #143 与迁移 `0020`–`0024`；本节合并了原 `Unreleased` 条目。真实实例升级已验证 `0016`–`0024` 的应用、健康检查、内置扩展和 #143 授权迁移；`0020`/`0022` 带历史数据回填、完整 2 用户／2 项目矩阵和升级后新真实评审仍未做。

### 升级前必读

- **先做一致性备份，本版本新增的九个迁移都不可回退**：新增迁移 `0016_dossier_mr_title`、`0017_mcp_config_seal`、`0018_dossier_project`、`0019_job_defer_window`、`0020_review_command_replay`、`0021_repository_skill_authorizations`、`0022_discussion_intents`、`0023_repository_skill_snapshot_sha256`、`0024_workflow_usage_incomplete`，都没有 down migration。`0016` 只对 `workflow_dossiers` 追加 `mr_title TEXT NOT NULL DEFAULT ''`，不改写已有行，也不回填旧评审。回退只能恢复升级前的数据快照，不能只换回 v1.1.0 镜像。升级前按 [SQLite 一致性备份与恢复](docs/operations.md#7-sqlite-一致性备份与恢复) 核对生效路径和实际挂载，停止服务后备份数据库、存在时的 WAL/SHM 及全部工作区，另存配置、镜像 digest 和 `REVIEW_MASTER_KEY`。只有默认配置和单一数据挂载符合第 7 节前提时才能复制整个 `/app/data`；自定义布局须分别备份。恢复到匹配布局的新空位置，不覆盖旧数据，不混入旧 WAL；旧版本读取新 schema 不受支持。步骤见 [从 v1.1.0 升级到 v1.2.0](docs/operations.md#从-v110-升级到-v120)。
- **MCP 配置加密，迁移不可回退**：新增 `0017_mcp_config_seal`。启动时使用原 `REVIEW_MASTER_KEY`，在事务中加密所有已存 MCP 配置，清空旧明文列并写入密钥校验标记；错误密钥、损坏的 MCP 配置或写入失败会阻止启动。升级前做一致性备份；回退必须恢复升级前的快照，不能让旧程序读写已迁移的库。当前列清空不代表旧 WAL、空闲页、快照或备份中的明文已擦除。步骤及首次密钥校验边界见 [MCP 配置加密升级](docs/operations.md#mcp-配置加密升级v120)。
- **dossier 归属迁移不可回退**：新增 `0018_dossier_project`，只按已关联 workflow 回填旧记录归属，未知孤儿保持未知；不改快照内容。升级前做一致性备份，回退须恢复升级前快照。详见 [dossier 归属迁移](docs/operations.md#dossier-归属迁移v120)。
- **ignore 保护范围扩大**：有效 ignore 非空时，仓库读取和证据检查遵守同一规则；无法证明符合规则的 MCP 工具及全量图谱索引不再执行，无来源证明的历史上下文不再注入。默认 ignore 非空，因此默认图谱能力会降级；ignore 策略下 MCP 工具与全量图谱索引不执行，属于安全降级，不影响评审结论（不记 `tool_gaps`，PASS 不会因此变为 INCOMPLETE）；仅索引身份无法证明（且 ignore 为空）时，实际启用并绑定的工具仍记为覆盖缺口。管理员绑定、历史上下文和旧索引保留。缺少版本化策略的旧工作流不续跑或重发，需重新触发评审；详见 [ignore 安全边界](docs/operations.md#ignore-安全边界)。
- **日志字段改名，影响检索和告警**：生产日志不再记录原始错误文本。原来的 `error` 字段（原始错误）改为 `error_class`（脱敏类别，如 `timeout`、`canceled`、`internal`、`mr_closed`）；任务结算日志（`review job settled without retry`、`workspace job settled without retry`）里原来的 `reason` 字段也改名为 `error_class`。依赖 `error` 或任务结算日志 `reason` 的日志检索、面板和告警需要调整。评审筛选、发布失败、扩展绑定跳过等业务 `reason` 字段仍然保留。新增 `scripts/logcheck`，`go test ./...` 会拒绝在 logger 调用里记录 `err` 或 `err.Error()`。
- **数据目录独占锁**：服务启动时在 SQLite 文件所在目录对 `tribunal.lock` 加 `flock` 独占锁，第二个实例误用同一数据目录或数据卷时会记录带锁文件路径的错误日志并以退出码 1 退出。两个实例不能共用数据目录。锁随进程退出由内核释放，锁文件残留不代表仍被锁，不要手工删除。锁只在 Linux、macOS 和 BSD 上生效；Windows 等没有 `flock` 的平台只创建锁文件、不加锁，不在支持范围内。跨主机共享的 NFS 等网络文件系统不受保护，不要把数据目录放在网络文件系统上。
- **`bootstrap_admins` 用户名写法弃用**：用户名条目仍可用，但启动时每个条目记一条弃用告警，含条目序号 `index` 和绑定状态 `binding=none` / `binding=unavailable` 或替代值 `replacement=id:<N>`，不含用户名。计划在 v2.0.0 移除。迁移步骤见 [部署与运维](docs/operations.md#bootstrap_admins-用户名写法弃用与迁移)。

### 新增

- **按 provider 恢复最后一轮强制提交**：provider 新增可选 `forced_tool_choice`（`auto` / `supported` / `unsupported`）。支持强制选择的 provider 最后一轮同时收到 `tool_choice=submit_result` 和提交提示；`auto` 只对未开 thinking 的 Anthropic 强制，OpenAI 兼容网关（含 DeepSeek thinking）默认只发提示。强制选择被上游 400 拒绝时，同一轮改用提示重发；重发成功才在本进程内不再对该 provider 尝试。审计请求元数据新增 `forced_tool_choice` 标志。未配置时只有未开 thinking 的 Anthropic provider 行为改变（恢复 #255 之前的强制提交）；已有评审的执行身份不变（[#259](https://github.com/LeXwDeX/Tribunal/issues/259)）。
- **agent 上下文压缩**：provider 新增可选 `context_window`（填可用于输入的 token 数）。agent 节点估算输入达到该值的 85% 时，把较早的成功工具输出替换为「已省略」占位，保留最近 3 轮完整输出和工具调用配对，不额外调用模型；仍接近上限时依次保留更少轮次。压缩后仍接近上限时提前要求模型提交结果，本轮不再执行其他工具。onboarding 另在工作流 token 预算剩余不足最近最大单轮用量的 4 倍时提前要求提交，其他节点的预算耗尽处理不变。LLM 调用审计新增 `submit_prompt` 与 `compacted_tool_results`。未配置 `context_window` 时不压缩，已有评审的执行身份不变（[#260](https://github.com/LeXwDeX/Tribunal/issues/260)）。
- **锁等待指标**：新增 `GET /api/v1/system/lock-stats`（平台管理员），返回 SQLite 连接等待计数与耗时、连接池状态，以及工作区内容锁、生命周期锁和 mirror 锁的累计等待与单次最长等待。`scripts/loadtest.sh` 的 stderr 汇总追加 `sqlite_wait_*` 和 `workspace_wait_*` 字段。等待计数与耗时从进程启动累计，连接池的 `in_use`/`idle` 是当前值；不改变锁语义（[#235](https://github.com/LeXwDeX/Tribunal/issues/235)）。
- **评审列表返回 MR 标题**：`GET /api/v1/reviews` 每项增加 `mr_title`，控制台的评审列表和总览显示标题。标题在保存评审 dossier 时写入新列 `workflow_dossiers.mr_title`（迁移 `0016_dossier_mr_title`，只追加列），列表按页一次批量读取，不请求 GitLab。升级前已有的评审没有该列的值，接口省略 `mr_title`，控制台继续显示 `!iid`（[#101](https://github.com/LeXwDeX/Tribunal/issues/101)）。

### 变更

- **控制台设计令牌对齐**：补全深色导航、实心块与头像的共享设计令牌，导航计数底色改用与现行主色一致的配色，头像渐变改为低饱和配色，指标值区固定高度以便对齐，Toast 层级回到规范值，卡片标题字重及页面与顶栏水平内边距统一。只影响控制台样式，不改变接口或评审行为；零计数和暗色开关旋钮保留原有的可读对比度（[#242](https://github.com/LeXwDeX/Tribunal/issues/242)）。
- **Webhook 存储失败改返回 503**：查项目、写投递记录或入队失败时，`POST /webhooks/gitlab` 由 200 改为 503，响应体仍是 `{"ignored":"storage_error","request_id":...}`，日志带脱敏的失败类别。不要假定失败的投递会自动重试；GitLab 收到 503 时会在 hook 的「Recent events」里记为失败。能否用 Resend request 手动重发及重发是否保留事件 UUID 取决于 GitLab 版本和实例配置；满足条件时同一事件 UUID 会被正常受理，详见 [Webhook 失败与恢复](docs/operations.md#webhook-失败与恢复)。GitLab 连续收到失败响应时可能自动停用 hook，需要关注 GitLab 端的 hook 状态。输入错误、未登记项目、重复投递等情况仍返回 200（[#99](https://github.com/LeXwDeX/Tribunal/issues/99)）。
- **全局会话上限优先保留管理员**：全局 4096 个会话的兜底上限触发时，先淘汰非管理员的最旧会话，只有剩下的会话全部属于管理员时才淘汰管理员的最旧会话。角色按当前的 `bootstrap_admins` 判断：服务启动时会按配置刷新已存用户的角色，会话被使用时也会刷新，所以改了配置后，不需要用户重新登录。每个用户 10 个会话的上限不变，无需迁移（[#102](https://github.com/LeXwDeX/Tribunal/issues/102)）。
- **CI 接入静态分析与漏洞扫描**：go job 新增固定版本的 `staticcheck` 和 `govulncheck`，并清理了现有告警。`go.mod` 增加 `toolchain go1.26.10`，本地开发和 CI 不再用 go1.26.0 构建（该版本有 31 个可达的标准库漏洞）；发布镜像本来就是 1.26.10，不受影响（[#106](https://github.com/LeXwDeX/Tribunal/issues/106)）。
- **日志不再记录原始错误文本**：工作区、评审副作用、运行时恢复、清理、发布状态、bootstrap 管理员绑定和启动恢复等日志不再输出 `error`（原始错误），改为 `error_class` 脱敏类别并保留项目、MR、工作流等标识字段；任务「settled without retry」日志的 `reason` 字段也改为 `error_class`（如 `mr_closed`、`review_superseded`、`stale_publication`、`permanent_job`）。评审筛选、发布失败、扩展绑定跳过等业务 `reason` 字段保持不变。新增 `scripts/logcheck`，`go test ./...` 会拒绝在 logger 调用里记录 `err` 或 `err.Error()`（[#107](https://github.com/LeXwDeX/Tribunal/issues/107)）。
- **启动时独占数据目录**：服务在打开 SQLite 之前对数据库所在目录的 `tribunal.lock`（默认容器配置下为 `/app/data/tribunal.lock`）加 `flock` 独占锁。第二个实例误用同一数据目录或数据卷时，记录带锁文件路径的错误日志并以退出码 1 退出，不再与第一个实例同时跑恢复、worker 或发布。v1.1.0 已知限制中「多个服务进程共用一个数据库时仍可能重复发布」改由启动锁防护；平台与网络文件系统限制见上方升级前必读（[#104](https://github.com/LeXwDeX/Tribunal/issues/104)）。
- **自定义扩展创建关闭，内置扩展始终启用**：控制台不再显示上传 Skill、登记服务器目录、新增 MCP 服务器或导入 MCP 配置；`POST /api/v1/skills`、`POST /api/v1/mcp-servers`、`POST /api/v1/mcp-servers/import` 返回 403。已有自定义 MCP 可测试、编辑、删除和启停；已有自定义 Skill 可启停和删除。内置 `code-review` Skill 和 `codebase-memory` MCP 始终启用，禁用请求返回 409；启动同步会把历史禁用状态恢复为启用。Skill 脚本仍不执行；非空 ignore 下评审不执行 MCP 工具的安全边界不变（[#193](https://github.com/LeXwDeX/Tribunal/issues/193)）。
- **并发压测脚本**：新增 `scripts/loadtest.sh`，对已部署实例并发触发评审，并汇总完成/失败/取消计数、失败率、p50/p95 执行耗时、墙钟时间、吞吐、tokens 与成本；只触发评审，不创建 GitLab 项目或 MR。脚本在触发前记录每个 MR 的评审 ID 基线，只统计本次新产生的评审；项目列表按 `next_cursor` 分页，GET 对瞬时错误有限重试，每个请求有超时。默认不修改项目授权；需要时显式设置 `GRANT_MANAGE=1`。需要平台管理员 token 和项目所属用户 token，运行说明见[并发压测](docs/operations.md#11-并发压测)（[#194](https://github.com/LeXwDeX/Tribunal/issues/194)、[#210](https://github.com/LeXwDeX/Tribunal/issues/210)）。
- **可访问性修正**：暗色主题的强调色对比度从 4.1:1 提升到 5.0:1；执行图节点按钮的可访问名称直接取自可见文本，其中包含 provider 和重试次数，不再因附加标签与可见文字不一致。亮色主题和 390 宽度页面检查通过（[#195](https://github.com/LeXwDeX/Tribunal/issues/195)）。
- **CI 失败日志证据**：verify 节点读取失败 job 的有限日志片段，每个评审最多 3 个 job，单个日志最多 64 KiB，渲染到 prompt 时每个 job 保留头尾各约 2 KiB；片段会截断并脱敏常见 token、私钥、URL 凭据和 CI 变量。读取失败时评审继续，不伪造证据；成功和允许失败的 job 不拉取。非空 ignore policy 下不保存日志片段（默认 ignore 非空，因此默认不注入），避免日志绕过 ignore 边界。日志片段会改变 dossier 内容和发布指纹（[#192](https://github.com/LeXwDeX/Tribunal/issues/192)）。
- **总览告警与统计说明**：总览按统计、告警、运行中和最近评审排列，空状态更紧凑；告警标题旁列出检查范围，统计说明明确裁决口径和 provider 价格配置要求，零成本不能证明已配置价格（[#146](https://github.com/LeXwDeX/Tribunal/issues/146)）。
- **仓库 Skill 授权与运行时**：新增不可回退迁移 `0021_repository_skill_authorizations`，建立独立授权表；追加不可回退迁移 `0023_repository_skill_snapshot_sha256`，非空授权同时记录管理员核对的完整目标 commit SHA 和过滤后 Skill 快照的 SHA-256。快照按相对 `.claude/skills` 的路径字节排序，逐文件包含路径长度、路径、内容长度和内容；空文件、二进制和名称变更都参与计算，错误信息不输出文件内容。授权写入必须重新发现同一目标 commit 的过滤结果并核对调用者提供的目标 SHA 与快照 hash；ignore 放宽、内容或名称变化会返回冲突且不覆盖旧记录。已有块绑定不会自动转成仓库 Skill 授权；目标分支更新不会使旧授权覆盖新 commit。`0021` 迁移保留的非空授权没有快照 hash，不能用于执行；管理员仍可读取并撤销，须重新发现后授权。仓库 Skill 的发现和授权接口已经注册，评审运行时只接受当前目标分支 tip 等于授权 commit、且过滤快照 hash 一致的授权；分支移动、快照变化或授权撤销都会 fail closed，需重新授权后才能执行或重发。项目设置“允许仓库内 Skill”开启但没有任何非空授权时不会加载仓库 Skill。升级前备份与未发布草案的兼容限制见[仓库 Skill 授权迁移](docs/operations.md#仓库-skill-授权迁移v120)（[#163](https://github.com/LeXwDeX/Tribunal/issues/163)）。
- **默认扩展授权收窄**：内置 `code-review` Skill 只绑定 `code-review` 块。示例配置和 Docker 配置中的 codebase-memory 默认授权改为：`verify` 只授予 `get_code_snippet`、`search_graph`、`trace_path`、`search_code`，`onboarding` 只授予 `get_architecture`、`get_graph_schema`；`code-review` 和 `review` 保留 11 个只读工具；`reasoner` 和 `synthesize` 没有默认图谱授权。默认不授予写工具、`detect_changes` 或 `compare_graphs`。升级保留管理员改过的绑定和解绑结果；数据库平台行按块优先于文件 `blocks`，只有内容等于 `block:<block>` 快照且每个条目都能由默认授权台账证明的行才释放。行上有自建授权或台账不可证明时文件块不生效，迁移步骤见 [默认扩展授权迁移](docs/operations.md#默认扩展授权迁移)。镜像升级不会改写宿主机挂载的配置文件，旧 `default_extensions` 会继续生效；升级前要备份并对照实际挂载配置、`config/server.docker.yaml` 和 `config/server.example.yaml` 核对矩阵，保留管理员自定义项。内置扩展的启用状态由 #193 改为始终启用；配置或绑定变更需用 `--force-recreate` 重启生效（[#143](https://github.com/LeXwDeX/Tribunal/issues/143)）。
- **恢复默认块绑定**：平台管理员可以预览并恢复单个块或全部块的内置扩展授权。确认后保留自建扩展授权、项目级绑定和内置扩展启用状态；恢复会移除选中块上已登记内置扩展的授权，文件声明块返回 `skipped`，不删除平台行，也没有删除绑定 API。块绑定的远端变更与本地草稿冲突时，控制台会暂停保存并要求处理冲突。恢复结果在重启服务后生效（[#143](https://github.com/LeXwDeX/Tribunal/issues/143)）。
- **块绑定写入强制版本校验**：`GET /api/v1/block-bindings` 的每个已有绑定返回 `revision`，`PUT /api/v1/block-bindings/{block}` 必须带 `expected_revision`（已有行使用读取到的版本，没有行用 `absent`）。缺失或格式错误返回 `400`；过期或另一个客户端先创建返回 `409`，不覆盖新值。比较和写入在同一 SQLite 事务内，所有块绑定写入与恢复默认通过该事务串行；项目范围先取得 workspace 读锁，与项目删除串行化。旧脚本和未刷新的控制台会被拒绝；升级时需刷新控制台并按 [块绑定写入版本校验迁移](docs/operations.md#块绑定写入版本校验迁移v120) 更新脚本。平台 scope 的 `absent` 写入会创建覆盖文件 `blocks` 的数据库行，保存前必须确认。没有引入事件版本：内容相同的 ABA 写入保持同一 `revision`（[#160](https://github.com/LeXwDeX/Tribunal/issues/160)）。
- **Docker 多 Provider 配置**：`config/server.docker.yaml` 增加默认注释的 `secondary` provider 与档位引用示例，`compose.yaml` 允许可选的 `LLM_SECONDARY_PROTOCOL`、`LLM_SECONDARY_BASE_URL`、`LLM_SECONDARY_MODEL`、`LLM_SECONDARY_API_KEY`，`.env.example` 增加空默认值。默认仍只有 `primary`，onboarding、propose 和 challenge 不变。启用步骤、challenge 与 verify 的关系、第三个 provider 的 Compose override、测试费用和局限见[多 Provider 配置](docs/operations.md#多-provider-配置)；新增 provider 需 `--force-recreate`，不是热加载，且会改变评审执行配置，启动恢复会取消身份不匹配的未终态工作流；移除 secondary 前须清理所有档位引用，待发布结果以发布校验为准（[#142](https://github.com/LeXwDeX/Tribunal/issues/142)）。

### 修复

- **工作区失败给出固定、可操作的分类**：`GET /api/v1/projects/{id}/workspace` 及项目详情/列表中 `workspace` 摘要的 `last_error` 现在只会是一组固定脱敏值，覆盖 GitLab（`gitlab: project token rejected (401)`、`gitlab: access denied (403)`、`gitlab: project or file not found (404)`、`gitlab: rate limited (429)`、`gitlab: unavailable`、`gitlab: request rejected`、`gitlab: project identity changed`）、git clone/fetch（`git: authentication failed`、`git: access denied`、`git: repository not found`、`git: branch not found`、`git: network error`、`git: operation failed`）、工作区（`workspace: operation timed out` / `operation canceled`）和 onboarding（`onboarding: profile not configured` 及模型 provider 的凭据被拒、限流、不可用、超时、调用失败）。控制台在错误码前显示中文提示。git 类别按 stderr 文本分类，stderr 不入库、不记日志，git 子进程固定为 C locale 以保证分类稳定。兼容性：无迁移、无配置变更；库中旧的原始错误文本读取时仍统一返回 `workspace operation failed`。([#279](https://github.com/LeXwDeX/Tribunal/issues/279))
- **日志 `error_class` 区分 GitLab/git 与模型 provider**：`workspace operation failed` 新增 `gitlab_auth`、`gitlab_forbidden`、`gitlab_not_found`、`gitlab_rate_limited`、`gitlab_unavailable`、`gitlab_rejected`、`gitlab_project_changed`、`git_auth`、`git_forbidden`、`git_repository_not_found`、`git_ref_not_found`、`git_network`、`git_failed`，缺少 onboarding profile 记为 `config`；GitLab 或模型 provider 返回 HTTP 状态时另带 `upstream_status`。`upstream_auth`、`rate_limited`、`network`、`config`、`overloaded`、`context_too_long` 现在只指模型 provider。此前 token 被撤销导致的 GitLab 401 记为 `internal` 且无状态，依赖该分类的检索或告警需更新。日志仍不含 token、URL、响应体和 stderr。([#279](https://github.com/LeXwDeX/Tribunal/issues/279))
- **Token 角色不足时提前提示**：`POST /api/v1/projects/validate` 响应新增 `warnings`（数组，始终存在，可为空），元素为 `{code, message}`；token 的项目角色低于 Maintainer 时返回 `webhook_requires_maintainer`，因为评审和克隆在 Reporter/Developer 下可用，但 GitLab 项目 hooks API 需要 Maintainer，自动启用 Webhook 会失败。`PUT /api/v1/projects/{id}/token` 的响应（项目对象）同样新增 `warnings`。控制台在注册对话框和「更新访问 Key」对话框的「校验连接」后显示该提示。([#279](https://github.com/LeXwDeX/Tribunal/issues/279))
- **轮换 token 时校验成员角色（行为变更）**：`PUT /api/v1/projects/{id}/token` 现在与 validate 一样检查成员角色，低于 Reporter 的 token 返回 `400 validation_failed`（字段 `access_level`）；此前轮换不检查角色。使用低权限 token 轮换的脚本需改用 Reporter 及以上的 token。([#279](https://github.com/LeXwDeX/Tribunal/issues/279))
- **Webhook 启用/重建/停用失败给出原因**：`POST/DELETE /api/v1/projects/{id}/webhook`、重建以及 CLI `--auto-webhook` 注册失败时，错误码和 HTTP 状态仍为 `gitlab_rejected` / 422（保持兼容）；GitLab 返回 401 时 `details` 带 `{field: "token"}`，403 时带 `{field: "access_level"}` 并说明管理项目 Webhook 需要 Maintainer。项目的 `webhook_error` 追加后缀，如 `GitLab webhook <op> failed (status 401): project token rejected` 或 `(status 403): Maintainer role required`。控制台显示中文说明：401 提示更新失效的 Key，403 提示改用 Maintainer 的 Key，或由 Maintainer 在 GitLab 手动管理 Webhook。([#279](https://github.com/LeXwDeX/Tribunal/issues/279))
- **传输重试后保留轮次耗时观测**：agent 节点因可重试的传输错误重试时，新尝试沿用此前观测到的最慢一轮耗时；剩余时间不足时，重试的第一次请求就要求模型提交结果，不再从头探索直到超时。重试次数、退避和错误分类不变（[#261](https://github.com/LeXwDeX/Tribunal/issues/261)）。
- **onboarding 在 thinking mode 网关下不再因强制 `tool_choice` 失败**：agent 循环最后一轮不再发送 `tool_choice=submit_result`，改为在该次请求末尾追加一条一次性提示（不写入 transcript），要求立即调用 `submit_result`；原先 400 后改用 auto 重试的兜底请求没有任何提交提示，模型继续调用其他工具，循环到顶返回「agent finished without submit_result」。节点剩余时间低于保留量（已观测最慢一轮的 2 倍，且不少于开始时剩余时间的 1/8）时提前发出同一提示；最后一轮只执行 `submit_result`。`workspace.onboard` 的轮次上限由 30 提高到 90，评审、规划、核验、综合等其他节点仍为 30。提高上限不保证完成，时间、token 和上下文预算仍可能先到期。`submit_result` 的 schema、校验、重试和错误分类不变（[#255](https://github.com/LeXwDeX/Tribunal/issues/255)）。
- **提交提示轮的不合法工具调用不再直接终止运行**：已追加提交提示（`context`、`deadline`、`budget`，非最后一轮）的那一轮，若工具调用的参数或标识不合法，循环丢弃被拒绝的调用，追加一条只含类别名的固定反馈，让模型重新提交 `submit_result`；每次运行最多恢复两次，其余情形（协议异常类别、恢复用尽后再次不合法、普通轮、最后一轮、非 tool_use 的 stop reason）仍以 `unsafe_model_response` 结束。LLM 调用审计响应元数据新增可选 `invalid_tool_call`（固定类别）和 `invalid_tool_input_bytes`（参数字节数），不记录工具名和参数。实测这类损坏主要是 thinking 网关下模型在长字符串里漏转义双引号，因此提交提示和反馈都要求按严格 JSON 转义（或改用「」）。onboarding 的预算提前提交阈值同时为两次重提交多留两轮（[#269](https://github.com/LeXwDeX/Tribunal/issues/269)）。
- **兼容要求角色严格交替的 OpenAI 兼容网关**：`openai-chat` adapter 现在把相邻的纯文本 user 消息合并为一条（与 `anthropic` adapter 一致），#255 的最后一轮提示不再在 `MaxTurns=1` 或补问之后产生 `user → user`。provider 新增可选字段 `strict_role_alternation`（默认 `false`，仅 `openai-chat` 生效）：开启后，紧跟 tool 结果的提示改为附加在最后一条 tool 消息末尾、不可信数据包裹，并标明来自系统，请求中不再出现 `tool → user`。默认关闭时，没有相邻 user 文本的请求形状不变；`openai-responses` 与 `anthropic` 忽略该字段。修改该字段会改变评审执行配置，需重启；见 [严格角色交替](docs/design/06-config.md#严格角色交替strict_role_alternation)（[#262](https://github.com/LeXwDeX/Tribunal/issues/262)）。
- **项目校验结果与详情卡片的字段排版**：只读校验结果和项目详情等「字段 / 值」列表改为每行一个标签和一个值。标签列按内容收缩、最多占 40% 宽度，值列可收缩并自动换行；长身份字符串、长项目路径和多个 scope 不再把列撑宽、溢出面板，也不再因为相邻字段换行留下大段空白。窄屏仍是标签在上、值在下的单列布局。只影响控制台样式（[#251](https://github.com/LeXwDeX/Tribunal/issues/251)）。
- **模型与档位页的预算与 provider 字段**：档位预算里的 `deadline` 原来直接显示接口返回的纳秒整数（如 `1200000000000`），现在显示为可读时长（如 `20m`）；接口返回非数字值时原样显示，API 不变。providers 接口从不返回 API key 环境变量名，provider 卡片却总是显示一行「API key 环境变量 不可用」，容易被误认为没有配置密钥，现已去掉这一行。只影响控制台显示（[#271](https://github.com/LeXwDeX/Tribunal/issues/271)）。
- **档位拓扑不再显示空的 fallback 括号**：接口对未配置的 `fallback` / `provider` 返回空字符串，控制台原来只判断类型，导致 default 档位的「挑战」步骤显示成 `primary（fallback ）`，onboarding 档位只显示孤立的 `（fallback ）`；现在空或纯空白的值视为未配置，fallback 为空时只显示 provider，provider 为空时显示「未配置 provider」，fallback 非空时仍显示 `primary（fallback secondary）`，其他步骤的字段按同样规则处理。API 不变，只影响控制台显示（[#273](https://github.com/LeXwDeX/Tribunal/issues/273)）。
- **未登录不提示会话过期**：服务端 401 区分无 session cookie（`unauthenticated`）和已有会话失效（`session_expired`）；前端只在 `session_expired` 或登录后的请求返回 401 时显示“会话已过期，请重新登录。”，首次访问只显示登录按钮（[#153](https://github.com/LeXwDeX/Tribunal/issues/153)）。
- **Onboarding 排障信息**：工作区失败日志的 `error_class` 区分 `upstream_auth`（401/403）、`rate_limited`（429）、`network`、`config` 和上游过载/上下文超长，并在有 HTTP 响应时记录脱敏的 `upstream_status`；`last_error` 保持固定脱敏文案。`tribunalctl` 的全局、子命令和 action 均支持 `-h`/`--help` 并以 0 退出；裸 `help` 只在期待子命令的位置生效，叶子命令的位置参数仍按原规则校验。ignore 阻断索引时控制台显示信息提示，不再当作最近错误。（[#132](https://github.com/LeXwDeX/Tribunal/issues/132)）。
- **Onboarding 重试入口**：新增 `tribunalctl projects retry-onboarding <项目 ID>` 和平台管理员端点 `POST /api/v1/admin/projects/{id}/onboarding/retry`。重试走管理 CLI 和平台管理员权限，不需要项目 `manage` 授权或浏览器会话；项目侧 `workspace/refresh` 的 HTTP 权限不变。任务仍为 queued 时重复执行复用同一任务；任务执行或失败后可再次创建，旧失败记录保留（[#132](https://github.com/LeXwDeX/Tribunal/issues/132)）。
- **OAuth 未启用时的登录页**：新增公开接口 `GET /auth/status` 返回 `oauth_enabled`；未配置 OAuth 时控制台登录页隐藏登录按钮并提示管理员配置 `auth.oauth.client_id_env` 和 `auth.oauth.client_secret_env`，不再把 503 JSON 暴露给浏览器用户。浏览器导航的登录失败以 303 跳回控制台并显示固定错误码对应文案；已有会话时以一次性提示展示，不会静默丢弃。API 客户端仍收到原有 JSON 错误（`Accept` 明确拒绝 `text/html` 时也不跳转）；服务启动日志会说明 OAuth 是否启用及缺失的变量名，不打印变量值（[#131](https://github.com/LeXwDeX/Tribunal/issues/131)）。
- **节点失败错误类别**：`pkg/dag` 支持可选的错误分类接口；LLM 鉴权失败、限流、沙箱拒绝和 panic 在节点失败事件里记录固定类别，panic 只记 `panic`，不记录 panic 值或原始错误文本（[#127](https://github.com/LeXwDeX/Tribunal/issues/127)）。
- **Defer 有界**：任务连续因执行权争用被延后时记录首次争用时间；超过 15 分钟按失败结算，不再无限延长 `max_attempts`。新增迁移 `0019_job_defer_window`（只追加 `jobs.defer_since`，不可回退），完成、失败和替换任务时清空窗口（[#127](https://github.com/LeXwDeX/Tribunal/issues/127)）。
- **项目残留运维出口**：新增 `tribunalctl projects residues` 只读检查和 `tribunalctl projects residues clean --yes` 清理命令。只清理能证明属于未登记项目的规范目录，符号链接、非规范名称和未知归属保留并报告；服务端同时要求 `confirm: true`（[#127](https://github.com/LeXwDeX/Tribunal/issues/127)）。
- **0018 存量孤儿清理**：`project_id=0` 且无 workflow/job 引用的 dossier 按 `storage.job_retention` 结算为 skipped，保留审计行；未到保留期或仍有引用的记录保留（[#127](https://github.com/LeXwDeX/Tribunal/issues/127)）。
- **MCP 配置合并竞态**：`****` 占位符恢复移到存储层事务内，用事务中读取的当前配置合并；并发管理员更新不会把对方刚写入的密钥回滚成占位符（[#127](https://github.com/LeXwDeX/Tribunal/issues/127)）。
- **首次密封提示**：数据库中没有可验证的项目或 MCP 密文时，启动日志记录一条 Warn，说明当前主密钥未经验证；只含环境变量名，不含密钥值（[#127](https://github.com/LeXwDeX/Tribunal/issues/127)）。
- **ignore 规则规范化**：`/secret/**`、`./secret/**` 去掉前导 `/`、`./`；`vendor/` 匹配任意层级目录；`!` 否定和 `[...]` 字符类在保存项目设置时返回 400，平台配置在配置检查时拒绝，不再静默失效。旧配置中的不支持写法会让评审以配置错误永久失败；底层匹配器按“全部忽略”兜底。`IgnorePolicyVersion` 升到 2，旧策略的未完成工作流不会续跑，需要重新触发评审。规则说明见 [ignore 安全边界](docs/operations.md#ignore-安全边界)。
- **非法 `.gitlab-review.yml` 永久失败**：解析失败或包含不支持的 ignore 写法时，评审不重试，并在 MR 中发布一条说明评论；修正文件后重新触发。启动恢复会终结对应工作流，不会按重试上限反复执行；修正后重新触发评审。
- **工作区状态自动刷新**：工作区处于 `new`、`refreshing` 或 `onboarding` 时每 2 秒轮询状态。提交刷新任务后最多跟踪 5 分钟，按 `last_refresh_at` 变化或 `error` 判定任务结束，避免任务尚未开始或两次轮询之间完成时的旧 `ready` 影响结果；完成后取消未完成的文档请求并重新拉取当前上下文文档和历史列表，不再需要手动刷新页面（[#130](https://github.com/LeXwDeX/Tribunal/issues/130)）。
- **项目详情页头部布局**：状态徽章移到标题旁，和操作按钮分开；标签栏不再显示多余的竖向滚动条，窄屏保留横向滚动。只调整布局，不改变状态或操作行为（[#129](https://github.com/LeXwDeX/Tribunal/issues/129)）。
- **手动任务授权竞态**：触发评审、刷新工作区、取消及重发评审时，在项目生命周期锁内重新核对当前用户的项目 manage 授权，避免旧请求借用同 ID 新登记的项目。该检查不保证撤销已经通过复核的请求；其他项目写接口不属于此修复范围。
- **摘要评论重试**：创建结果不确定时，重试前回读本 bot 的摘要标记；回读或身份确认失败时停止创建，减少重复评论。回读与创建不是原子操作，跨工作流并发或 GitLab 列表延迟仍可能产生重复；不会自动删除已有重复评论。
- **评审孤儿和取消恢复**：项目删除清理已知归属的孤儿 dossier；周期清理将无 workflow 且任务已终结或被保留策略移除的已知归属 reservation 标记为 skipped 并保留审计数据。活跃任务和未知归属记录保留。取消后的待发布记录可在没有 publisher 时本地收敛，不发送远程写入。
- **项目删除生命周期**：本地项目删除先检查清理目标，再关闭项目 MCP 会话并提交数据库删除，最后清理项目工作区和受管 Skill。数据库已删除但文件清理失败时返回部分完成错误，不再报告删除成功。重新登记前拒绝接管任何残留；处理步骤见[项目删除后的残留核对](docs/operations.md#项目删除后的残留核对)。状态、反应、候选评审和发布任务在共享锁内复核原任务或工作流，避免已删除的任务作用于同 ID 的新登记。锁保护限于共享同一工作区管理器的单个受管运行时，不保证数据库、文件和 GitLab webhook 原子一致。
- **多目标分支工作区来源校验**：MR 评审按目标分支 tip 核对 context 和图谱索引，工作区锁只用于并发控制。索引不可用时，实际启用的 MCP 绑定产生覆盖缺口；缺少来源证明的历史快照不会续跑或重发，需要重新评审。升级前自动生成且缺少安全工作流证明的 context 保留显示但不注入评审；锁定文档可由管理员解锁重新生成，或检查后显式手工保存。历史 context 与 dossier 不会被自动改写。
- **GitLab 项目改名后的抓取身份**：工作区、评审和反馈抓取前按已登记的数字项目 ID 查询并校验当前项目，使用当前 HTTP 仓库地址；无法确认身份或地址时停止抓取，不回退旧路径。同项目镜像的地址更新和抓取在本地串行执行。数据库保存的路径和控制台显示暂不迁移，可能仍显示旧路径。
- **评审重试材料一致性**：身份校验通过的续跑复用持久化 dossier 和已完成节点，不混入新采集的 MR、pipeline 或上下文。配置、提示词、档位、目标分支、diff SHA 或工作区身份变化时保留旧记录并重建；缺少必要身份信息的启动恢复不猜测复用。
- **恢复与队列争用**：其他恢复者持有任务时延后领取，不标记成功、不消耗剩余失败预算。领取次数仍单调递增以拒绝旧租约，并同步补回此次争用的预算；真实执行失败仍受重试上限约束。
- **工作区刷新归属检查**：刷新前验证 base 属于当前项目的镜像；未知非空目录保持原样并拒绝刷新，避免 Git 上溯后覆盖父仓库内容。
- **ignore 跨输入隔离**：重命名 diff 检查旧、新路径，目录祖先与符号链接目标也受检查；固定仓库文档按来源过滤，初始化及刷新上下文使用相同策略。非空策略下 git log 只返回提交 hash 和时间，不返回提交说明。新工作流保存有效策略，恢复和发布校验策略；明确指向 ignored 路径的模型结果不会作为行内或汇总评论发布，拒绝时仍保留已消耗的用量。
- **前端 CI 门禁**：构建后检查 `web/dist` 与提交内容一致，包括修改、删除和新增未跟踪文件，避免 Go 嵌入过期产物。按 Vitest 4 当前实测基线提高覆盖率阈值至 statements 74%、branches 72%、functions 71%、lines 78%。
- **节点失败错误保密**：新节点失败事件使用固定错误消息并保留失败类别和用量；节点详情及评审问题接口隐藏历史错误及可归因的验证失败诊断，恢复汇总和发布时也使用安全视图。历史事件、旧数据库及备份中的原文不会自动清除，回退旧版可能重新暴露。
- **多个受管 Skill**：同名 Skill 目录在评审沙箱中分配独立别名，避免根目录重名导致评审无法运行；内置工具定义公开可用根名和默认根。
- **API token 签发竞态**：签发时在事务内复核浏览器会话仍属于当前用户且未撤销或过期，避免已失效的会话继续签发 token。
- **MCP 配置保密**：数据库中的完整配置使用绑定作用域、项目 ID 和服务器 ID 的密文保存，包括内置配置刷新和批量导入；未配置密钥时拒绝写入。API 的列表、创建、更新和导入响应统一返回 `config: {}`，不再回显部分脱敏配置。仅改名或启停时省略 `config`；替换配置需提交完整原始模板，旧客户端 env/header 的 `****` 占位符恢复规则保留。
- **工作区错误脱敏**：新写入的工作区错误改为固定消息；工作区详情、项目详情和列表对历史错误也只返回固定消息，不暴露原文。索引降级仍保留 `index: ` 前缀；依赖完整错误文本的客户端需调整。旧数据库和备份中的历史原文不会自动清除，回退旧版可能重新暴露。
- **本地烟测与注册清理**：本地烟测不再覆盖调用者的部署密钥，前端构建只继承 PATH 并使用临时 HOME；已有部署的 session key 故障处理改为恢复原值。注册响应删除后核验链接和原路径，失败时保留路径和文件描述符供安全核对。
- **README 前端构建**：启动、构建和验证示例在运行 npm 前清除列出的密钥并核验清除结果；如果密钥仍存在，则停止执行，避免传给 npm。
- **项目注册文档**：注册和清理前检查相关 shell 变量能否清除；遇到 readonly 或清理失败时停止，保留私有响应并提示安全处理，避免误报清理成功。README 授权示例改为只查询用户，变更操作按运维指南逐次确认。
- **注册路径与密钥示例**：注册要求受控私有目录的绝对路径，清理前核对本次文件路径和身份，避免切换目录或路径被改时误删。本地启动复用安全注入的已保存密钥，不再每次重新生成；生成步骤不向终端输出密钥。
- **MCP 停机竞态**：关闭时等待连接结果发布后再关闭会话，避免连接刚完成时停机漏关会话并卡住。并发及后续关闭调用等待同一次停机完成，并返回相同的脱敏结果。
- **运行时关闭错误**：运行时资源关闭失败时记录 `runtime close failed` 错误日志及脱敏的 `error_class`；不输出错误原文，也不改变服务退出码。
- **Docker 冒烟资源隔离**：使用 Docker 分配的数据卷标识隔离运行，并在清理前核对容器、网络、卷和镜像的归属。归属无法确认时保留资源并报告失败，不再直接清理整个 Compose 项目。
- **Docker 冒烟启动清理**：取得临时目录路径后，如果日志初始化失败或收到 SIGINT/SIGTERM，会尝试清理本次临时目录。无法取得目录路径时不会猜测删除位置，并提示可能有残留。清理失败时会告警，并保留原有错误或信号退出码。
- **停机资源收尾**：停机超时后不再立即关闭数据库或释放数据目录锁；恢复任务、后台任务、worker 和已登记 HTTP 请求全部退出后，才关闭数据库并释放锁。仍有使用者时交给后台收尾，服务仍按现有停机时限返回；panic 路径同样等待这些使用者。
- **执行图画布留白**：执行图画布以原画布高度为基准，上下内边距各增加约 2.5%，总高度约增加 5%。节点、标签和连线保持原有相对位置与水平坐标；底部留白增大，减少横向滚动条遮挡节点的风险（[#141](https://github.com/LeXwDeX/Tribunal/issues/141)）。
- **Findings 表格对齐**：评审详情页的级别、标题与路径、证据链和人工四列按同一行中线垂直对齐；长标题继续换行，降级级别和展开内容仍限制在表格滚动容器内，其他页面的表格样式不变（[#140](https://github.com/LeXwDeX/Tribunal/issues/140)）。
- **扩展作用域项目选择**：平台级扩展不再显示项目标签和选择框；项目级扩展仍可选择项目（[#144](https://github.com/LeXwDeX/Tribunal/issues/144)）。
- **项目概览布局**：基本信息与工作区摘要在宽屏并排、窄屏纵向排列；最近评审移至下方全宽显示，表格按内容自然增高，窄屏时仅表格内部横向滚动（[#145](https://github.com/LeXwDeX/Tribunal/issues/145)）。
- **工作区操作**：默认显示刷新按钮，将重建索引和重新生成上下文收进手动操作，并区分推送触发的自动刷新与手动刷新的行为。ignore 策略阻止索引时默认展开手动操作，保留重建入口以便修改配置后重试。手动重新生成会调用模型并可能产生费用；执行时 AGENTS 若已锁定，生成成功后保存未生效的新版本，当前文档和上下文 SHA 不变。推送触发的自动刷新仍跳过锁定的上下文（[#147](https://github.com/LeXwDeX/Tribunal/issues/147)）。
- **Provider 模型预检**：配置校验和 `--check-config` 拒绝空或全为空白的 provider model，避免检查通过后在注册 provider 时启动失败；空 `base_url` 仍可使用适配器默认地址（#142）。
- **GitLab 评审评论排版**：汇总评论先显示结论和严重级别计数，再列出行内问题的简要索引；无法定位的条目和存疑条目使用分级标题。存疑条目在汇总中保留折叠证据，即使复用没有证据的旧行内讨论也可查看；行内存疑讨论也保留证据。正文、标题和提示中的提及、引用与自动链接按字面显示，路径空格与代码中的 HTML 注释不再丢失或变形。异常围栏不会吞掉后续问题和建议；缩进围栏会移到顶层，原列表可能分成两段（[#170](https://github.com/LeXwDeX/Tribunal/issues/170)）。
- **diff 定位与限额修正**：评审任务为重命名显示旧侧和新侧路径；重命名链中同一路径属于不同变更时，问题保留在摘要，不尝试行内定位或合并投票，评审标为 INCOMPLETE。摘要标明旧侧和新侧。被忽略或省略的变更没有记录旧侧路径，因此也会保守地把问题留在摘要并分别计票；ignore 本身不产生证据缺口，也不改变 PASS。旧侧与新侧的问题分别计票，避免因增删行而错误合并；即使实际对应，也可能保留为两个问题。增量评审保留可能属于范围内文件的歧义问题。GitLab 未提供完整重命名路径时按覆盖缺口处理，ignore 规则仍先过滤。`max_diff_lines` 也限制首个文件；hunk 内以 `+++`/`---` 开头的内容行计入增删行数。若限额导致没有可评审文件且启用了 commit status，INCOMPLETE 会给当前 head 写入 failed 状态；纯 ignore 过滤保持原行为。任务渲染身份更新后，旧身份的待恢复或待发布工作流不会续用，需重新触发评审；已有行内讨论保留，旧侧发布键变化或重命名后重新评审可能创建新讨论（[#162](https://github.com/LeXwDeX/Tribunal/issues/162)）。
- **行内讨论发布键兼容性**：旧版发布键不区分 diff 侧，无法安全判断历史讨论属于旧侧还是新侧。新版两侧均使用带版本和侧别的发布键，不复用旧版键，也不删除历史发布记录或讨论；升级后首次重新评审可能为已有问题创建重复讨论。新版同侧的后续重试继续复用新版键（[#162](https://github.com/LeXwDeX/Tribunal/issues/162)）。
- **评审状态与重试一致性**：删除项目时清理该项目的 onboarding 重试任务，避免重新登记后执行旧任务；新的评审 generation 不再继承上一代的 attempt。第二次汇总会采用已有问题的扩展核验结果，并阻止有效反证被模型输出覆盖（[#161](https://github.com/LeXwDeX/Tribunal/issues/161)）。
- **MR 状态评论收敛**：新评审代次的排队状态不再被上一代 PASS 隐藏；新代次不再继承旧代次的尝试计数，同代同次的状态顺序仍由持久化目标约束。状态任务按 MR 串行，并定期复核已成功应用的评论，以修复超时或重启后才到达 GitLab 的旧写入，无需重新调用模型。GitLab 不提供条件写入，旧状态仍可能短暂出现；多个同 bot 状态标记不会自动删除，须人工核对。终止错误会停止该版本重试，后续生命周期事件可能生成新版本；否则修复权限后需重新触发评审。本分支与 [#181](https://github.com/LeXwDeX/Tribunal/pull/181) 合并后只保留一份尝试计数实现；收敛条件及终止错误处理见[状态评论恢复](docs/operations.md#状态评论恢复)（[#182](https://github.com/LeXwDeX/Tribunal/issues/182)）。
- **仓库 Skill 来源证明**：新评审记录受管 `repo:` Skill 能力、目标分支、目标 commit、授权版本和过滤快照 hash；该标记不证明管理员配置的其他 Skill 目录的实际内容来源。缺少来源记录、记录格式未知或记录了不受支持的仓库 Skill 能力的历史工作流不会续跑或重发；普通旧评审也可能受影响，需重新触发评审。旧 dossier、节点结果、findings 和人工标注保留。仅凭当前 `allow_repo_skills` 值或旧版本号不能推断历史来源。仓库 Skill 运行时和接口已启用，但恢复与发布仍会重新校验授权和快照（[#163](https://github.com/LeXwDeX/Tribunal/issues/163)）。
- **讨论与发布恢复**：外发门禁拒绝旧令牌时不再产生无法确认的新讨论意图；真实的不确定结果仍保持待处理。恢复按页处理取消和已完成的工作流，避免未知讨论阻塞其他项目；评审状态与反应按实际发布结果恢复，手动重发后的状态也会同步。
- **行内讨论的不确定结果**：发送 GitLab 讨论前先以局部 SQLite `FULL` 同步记录持久化意图；响应丢失、列表出错或暂时找不到标记时保持待处理，不自动或通过手动重发再次创建。仅在同一项目和 MR 中找到唯一的机器人首条标记，或取得有效创建响应后确认讨论。删除项目再注册时，旧远端 ID 必须重新核验，不能直接作为新项目的发布结果。多个匹配、创建前崩溃或原机器人身份不可用时需要人工核查；不会自动删除远端讨论。迁移 `0022_discussion_intents` 不可回退，备份、旧记录范围和恢复限制见[行内讨论意图迁移](docs/operations.md#行内讨论意图迁移v120)。
- **项目策略变更与在途传输隔离**：项目设置、启用状态、删除或仓库 Skill 授权变更会使该项目旧的评审发送令牌失效，并取消和等待正在执行的模型、MCP 和 GitLab 发布调用。等待传输结束最多 5 秒，也受请求截止时间限制；超时则不提交该策略写入，但旧令牌仍失效。其他项目不受这次等待影响。已经被远端接收的数据不能撤回，目标分支推送也不会被阻止。仓库 Skill 接口和运行时已启用；恢复、发布和重发都会重新校验授权版本与过滤快照，撤销后不再发送仓库 Skill 评审结果（[#163](https://github.com/LeXwDeX/Tribunal/issues/163)）。
- **历史行内讨论内容校验**：跨工作流复用讨论前，核对身份及当前渲染内容与保存的正文哈希；不一致或缺少哈希时，将当前问题保留在摘要并提示，不修改历史讨论。身份无法确认时仍按发布标识冲突处理。仅从持久化意图恢复的讨论只能确认远端 ID，不能证明所属工作流或内容；即使重试同一工作流也将当前问题保留在摘要并提示，不为其补写发布记录。已有同工作流记录缺少正文哈希也会降级。内容一致的讨论仍属于旧工作流，新工作流的反馈不会自动归属；正文哈希只记录本服务写入的内容，不验证 GitLab 上的站外修改。
- **模型工具调用安全边界**：截断、拒绝或尚未完成的模型响应不再执行工具或接受 `submit_result`；缺失、非对象或格式错误的工具参数不再伪装成 `{}`。同一轮只要存在非法调用就拒绝整批，并保留已知用量。工具执行错误和提交校验失败反馈改为固定文案，避免回显参数；模型可按原有上限重试。合法的空对象、多工具调用及推理内容回传保持支持（[#178](https://github.com/LeXwDeX/Tribunal/issues/178)）。
- **LLM 审计安全与格式**：新请求和响应只写安全元数据，不写提示词、自由文本、工具参数/输出、推理内容、错误原文及上游响应 ID；每个非空 payload 有 `_format=audit_metadata_v1`。agent 因不安全响应拒绝的调用在审计统计中记为 `unsafe_model_response`，不改变重试策略。调用列表使用本地 `id`，不再返回新旧行顶层 `request_id`。历史行不删除、不迁移；manage 用户读取历史 payload 仍可能看到敏感内容。未标记历史 payload 可能是原始消息、早期元数据或截断摘要，客户端须分别处理并保留 unknown 分支；详见[审计格式](docs/design/03-http-api.md#节点调用审计格式)。
- **用量完整性标记**：新增迁移 `0024_workflow_usage_incomplete`，仅为 workflow 和节点追加标记列；无 down migration。工作流和节点 API 暴露 `usage_incomplete`，历史 false 只表示未发现缺失，不证明历史用量完整。新事件可能使旧二进制严格解码失败；升级前备份并核对回退兼容性，不支持只替换镜像降级。`0020` 已在 `cf29656` 合并，`0021`–`0023` 随 PR #180 合并；升级前仍须重新核验迁移顺序及对这些表的影响，详见[运维说明](docs/operations.md#用量完整性与审计升级v120)。
- **Chat 响应用量校验**：空 choices 的 Chat 响应若含非法用量，按不安全响应处理且不重试，保留可确认的用量；缺失用量仍标记为不完整并沿用原重试行为。
- **问题发布标识冲突**：同一评审内的问题或历史讨论的发布标识冲突时，不能安全复用的行内问题保留在摘要并提示原因；历史讨论不删除。反馈归属查询失败或命中多个问题时暂停该讨论的采集并记录告警，缺少匹配时仍保留原始反馈，不用空归属覆盖已有归属（#162）。
- **保留大评论 ID 精度**：新入队的评审和取消任务不再将 JSON 中的大整数评论 ID 舍入。升级前已被舍入的任务数据无法自动恢复（[#159](https://github.com/LeXwDeX/Tribunal/issues/159)）。
- **同一 MR 的发布串行**：同一 `(project_id, mr_iid)` 的总结评论（包括空评审和失败说明）与 commit status 写入按评审代次串行。每个写入点前重新校验 control 的 generation 和 head；新代次已接管或 MR head 已变化时走既有 stale 路径，旧代次不再覆盖新代次的总结评论或 commit status。不同 MR 仍可并行发布（[#157](https://github.com/LeXwDeX/Tribunal/issues/157)）。
- **超长 MR 描述不再挤掉 diff**：评审任务先保留任务头和 diff，再为 MR 上下文单独预算。上下文超预算时在 prompt 中写入截断标记，并在评审结果的 warnings 中说明哪些上下文未完整包含；描述、pipeline、issue 不再能挤掉任务指令或 diff。MR 描述另有 8000 字符独立上限，截断时在 prompt 中标记。扩展评审节点的实际 prompt 也纳入二次汇总的覆盖检查。覆盖检查按生成任务时记录的 diff 区段位置逐文件核对（含中和后的内容），文件或包预算导致 diff 缺失或部分截断时记为未解决证据，汇总不能给出 PASS；提前出现的相同 diff 文本不视为覆盖。任务布局版本并入 prompt bundle hash，因此升级后旧的待恢复、待重试或待发布工作流会被判 stale；没有 `prompt_hash` 的旧工作流不再发布。短描述和完整 diff 的评审不受影响（[#158](https://github.com/LeXwDeX/Tribunal/issues/158)）。
- **`/review` 命令重放保护**：已认证的 `/review` 和 `/review cancel` 按 `(项目, MR, 来源评论, 命令)` 幂等。首次入队与身份记录在同一事务提交，已证明受理的重放不改变 generation、取消状态、队列或状态/表情。升级时一次性回填可由匹配下游 job 或符合条件的 workflow/dossier 证明受理的旧命令；reaction 不证明受理或命令种类。旧命令 payload 或仅存 reaction 标识的旧 note 若缺乏独立受理证明，会保守隔离，而非当成重复命令或自动执行；这也可能拦截旧评论上从未执行的命令。有权限的作者会收到提示，核对当前状态后可用新评论发出新命令；管理员可查询隔离记录。旧下游数据可能舍入大于等于 `2^53` 的 note ID，可能导致隔离误报或漏报；所有可识别历史和证明丢失时，新 UUID 重发仍可能被当成新命令。须限制旧事件重发并核对状态。新增迁移 `0020_review_command_replay`，没有 down migration；回退须恢复升级前快照。证据规则、隔离查询和升级步骤见 [命令重放幂等迁移](docs/operations.md#命令重放幂等迁移v120)（[#159](https://github.com/LeXwDeX/Tribunal/issues/159)）。
- **会话密钥预检**：`--check-config` 和实际启动都在准备存储前校验 session key；无效值不再通过配置检查，也不会先触发 SQLite 迁移。新旧环境变量的优先级不变；未配置时仍使用临时 key，实际启动会告警（#164）。
- **摘要评论恢复**：已绑定的摘要评论更新返回 404 时，先查找本 bot 的摘要标记，再复用或新建评论并更新绑定。旧评论仍在列表中、身份或列表不可用时不新建；写入前仍复核已存代次及非空 head，但不能消除在途请求竞态（#164）。
- **Webhook 请求体读取超时**：读取超时返回 `503 {"ignored":"request_timeout","request_id":...}`，不再假报 200。GitLab 可能因连续失败临时或永久停用 hook；不要假定失败投递会自动重试，运维恢复与版本限制见 [Webhook 失败与恢复](docs/operations.md#webhook-失败与恢复)（#164）。
- **Anthropic 用量**：输入 token 现在包含未缓存输入、缓存读取和缓存创建各一次，预算随之使用完整输入；缓存字段仍是明细。成本按配置的单一输入费率估算，不是供应方精确账单，已保存的历史用量不回填（#164）。
- **注册项目后注册人看不到项目**：此前通过控制台注册项目时不给注册人写入项目授权，项目列表为空，再次注册返回 `409 project is already registered`。现在由用户会话或用户创建的 API token 调用 `POST /api/v1/projects` 时，注册人自动获得该项目的 `manage` 授权，项目、工作区、onboarding 任务和授权在同一个 SQLite 事务内写入，授权失败则整体回滚；静态 bootstrap bearer token 注册行为不变，不写授权。409 消息追加系统页授权提示，控制台注册弹窗显示提示和「前往系统页授权」按钮。升级前已注册且注册人没有授权的项目不会自动补授权，需平台管理员补授权，步骤见 [存量项目补授权](docs/operations.md#存量项目补授权)（[#277](https://github.com/LeXwDeX/Tribunal/issues/277)）。

### 弃用

- **`bootstrap_admins` 用户名写法弃用**：`auth.bootstrap_admins` 里的 GitLab 用户名条目仍然可用，但启动时每个条目记一条弃用告警。告警含条目序号 `index` 和绑定状态 `binding=none` / `binding=unavailable` 或替代值 `replacement=id:<N>`，不含用户名。示例配置只保留 `id:` 写法。计划在下一个大版本（v2.0.0）移除用户名写法，迁移步骤见 [部署与运维](docs/operations.md#bootstrap_admins-用户名写法弃用与迁移)（[#103](https://github.com/LeXwDeX/Tribunal/issues/103)）。

### 已解决

- v1.1.0 已知限制「评审列表接口不返回 MR 标题」：见上方新增条目（[#101](https://github.com/LeXwDeX/Tribunal/issues/101)）。
- v1.1.0 已知限制「Webhook 存储失败返回 200」：改为返回 503；失败投递能否在 GitLab 中手动重发取决于版本和实例配置，见 [Webhook 失败与恢复](docs/operations.md#webhook-失败与恢复)（[#99](https://github.com/LeXwDeX/Tribunal/issues/99)）。
- v1.1.0 已知限制「多个服务进程共用一个数据库时仍可能重复发布」：同一数据目录的第二个进程现在无法启动（[#104](https://github.com/LeXwDeX/Tribunal/issues/104)）。
- v1.1.0 已知限制「全局会话上限淘汰不区分角色」：现在优先保留管理员会话（[#102](https://github.com/LeXwDeX/Tribunal/issues/102)）。

### 验证范围

- **历史状态（`96e8081`）**：截至 `main` `96e8081`，GitHub Actions 的 go、web、docker 三个 job 因账户计费/额度限制没有启动，job 注解说明了原因。这是当时的历史记录，不能代表后续候选。
- **真实实例验收已完成；当前范围已扩大**。[#108](https://github.com/LeXwDeX/Tribunal/issues/108) 已于 2026-10-29 在隔离真实实例执行：v1.0.0 → v1.1.0 → `a113e08` 原地升级、真实评审、停机续跑、Webhook、503 重发、MCP 调用与多项目隔离均通过；`bootstrap_admins` 用户名告警首轮未复现，经 [#151](https://github.com/LeXwDeX/Tribunal/issues/151) 排查为验证窗口未覆盖启动，已在真实实例复现告警。详见[交付就绪](docs/delivery-readiness.md#v120-验证状态)。这次真实实例验收覆盖 `a113e08` 的 `0016`–`0019`；后续已补做从 `a113e08` 到 `2621c44` 的升级，应用 `0020`–`0024` 并验证 #143 授权迁移；`0020`/`0022` 带历史数据回填、完整 2×2 矩阵和升级后新真实评审仍未验证，详见[交付就绪](docs/delivery-readiness.md#v120-真实实例升级验收-20261103)。CI 以最终 tag commit 的 GitHub Actions 结果为准。
- **本地预检（引用交接记录，不是本次发布重新核验）**：`main` `86c3a668` 对应的 PR #112 合并前 head 在本地通过 `go build`、`go vet`、`scripts/logcheck`、staticcheck v0.8.2、govulncheck v1.9.0、`go mod verify`、licensecheck、licensebundle `-check`、`go test -race -timeout 40m ./...`，语句覆盖率 77.90%；PR #128 合并前 head `eb1e277` 通过同样的全量检查（含前端 `npm ci`、`npm test -- --coverage`、`npm run build` 和 `web/dist` 一致性），语句覆盖率 79.07%。来源见 [PR #112 评论](https://github.com/LeXwDeX/Tribunal/pull/112#issuecomment-6432897860) 和 [PR #128 评论](https://github.com/LeXwDeX/Tribunal/pull/128)；它们不代表 CI 已启动，也不覆盖之后未提交的改动。详见 [交付就绪](docs/delivery-readiness.md#v120-验证状态)。
- **部署核对（引用交接记录）**：2026-10-29 在隔离真实实例的临时目录按运维文档从零部署 `main` `96e8081`，`docker compose config --quiet`、`build`、`--check-config`、`up --wait` 通过；healthz/readyz 和控制台返回 200，未启用 OAuth 时 `/auth/login` 返回 503；容器以 uid 10001 运行，`/app/data` 为 0700，启动锁生效；内置 Skill 和 codebase-memory 已登记，MCP 配置 API 返回 `{}`，只暴露 13 个只读工具；`scripts/docker-smoke.sh` 通过且无残留。该部署不是 GitLab 升级验收，也不覆盖 [#108](https://github.com/LeXwDeX/Tribunal/issues/108) 的真实数据升级场景。

### 已知限制

- 启动锁只在 Linux、macOS 和 BSD 上生效；Windows 没有 `flock`，只创建锁文件、不加锁。跨主机共享的 NFS 等网络文件系统不受 `flock` 保护，仍可能重复发布；当前部署形态为单实例。
- `bootstrap_admins` 的用户名写法仍是首次出现时绑定（TOFU），会产生弃用告警，计划在 v2.0.0 移除；在此之前建议改用 `id:<GitLab 用户 ID>`。
- 迁移 `0016` 不回填旧评审的 MR 标题。升级前已保存的评审在列表里仍显示 `!iid`，新保存的评审才有标题。

## [1.1.0] - 2026-10-26

v1.0.0 之后的第一个版本：产品改名为 Tribunal，修复 1.0 复审发现的问题，默认内置代码图谱和评审方法 Skill，控制台整体换成 v2 界面。

### 升级前必读

从 v1.0.0 升级前，先读 [部署与运维 · 从 v1.0.0 升级](docs/operations.md#从-v100-升级到-v110)，其中有几处无法回退：

- **先备份**：新增迁移 `0013_admin_bindings`、`0014_extension_defaults`、`0015_secret_migration`，并且启动时会把已存项目凭据改写成新的加密格式（[#64](https://github.com/LeXwDeX/Tribunal/pull/64)、[#76](https://github.com/LeXwDeX/Tribunal/pull/76)）。v1.0.0 读不了新格式。要回退，只能恢复升级前的数据备份，不能只换回旧镜像。
- **不要更换 `REVIEW_MASTER_KEY`**：迁移要用原主密钥解开旧密文。
- **数据卷不改名**：Compose 项目名 `crc-review` 和数据卷 `crc-review_data` 保持原名，继续挂载原卷即可保留数据（[#32](https://github.com/LeXwDeX/Tribunal/pull/32)）。
- **管理员配置**：`bootstrap_admins` 推荐改成 `id:<GitLab 用户 ID>`。旧的用户名写法仍然可用，会在第一次以管理员身份出现时绑定到当时的用户 ID（[#40](https://github.com/LeXwDeX/Tribunal/pull/40)）。

### 改名与兼容

- 产品、Go module、镜像和 CLI 改名为 Tribunal：镜像 `tribunal:*`，CLI `tribunalctl`，旧路径 `/app/reviewctl` 保留为兼容符号链接（[#32](https://github.com/LeXwDeX/Tribunal/pull/32)）。
- 环境变量主名改为 `TRIBUNAL_SESSION_KEY`、`TRIBUNAL_PUBLIC_URL`、`TRIBUNAL_HTTP_PORT`。旧的 `CRC_*` 名仍然有效，新旧同时设置时新名优先，旧名会记一次弃用日志。

### 新增

- **默认内置评审方法 Skill**：`code-review`，改编自 mattpocock/skills（MIT），已改成无需交互的版本，默认挂到 `code-review` 和 `review` 块。可以禁用，不能删除，随平台升级更新（[#45](https://github.com/LeXwDeX/Tribunal/pull/45)）。
- **默认内置代码图谱**：Docker 镜像内置 codebase-memory-mcp 0.12.1（MIT），下载包校验 SHA-256。默认登记为只读的平台 MCP 服务器 `codebase-memory`，按块授权只读工具，各项目的索引互相隔离。二进制不可用时评审照常完成，但带告警，原本的 PASS 改判 INCOMPLETE（[#46](https://github.com/LeXwDeX/Tribunal/pull/46)）。评审 prompt 补充说明：图谱反映的是 base 版本，改动以 worktree 为准（[#48](https://github.com/LeXwDeX/Tribunal/pull/48)）。
- **配置项 `default_extensions`**：用于声明内置 Skill 和 MCP 及其默认块绑定。管理员改过的绑定、YAML `blocks` 中声明的块，升级时都不会被改写。
- **独立的 Webhook 配置面板**：项目设置里可以单独启用、重建、停用 GitLab Webhook（[#65](https://github.com/LeXwDeX/Tribunal/pull/65)）。
- **`internal/errclass`**：5xx 日志和 job 失败记录带上脱敏后的失败类别（[#60](https://github.com/LeXwDeX/Tribunal/pull/60)）。

### 变更

- **v2 控制台**：
  - 导航分组、顶栏面包屑、登录页左右分栏（[#63](https://github.com/LeXwDeX/Tribunal/pull/63)）；
  - 总览和评审列表（[#67](https://github.com/LeXwDeX/Tribunal/pull/67)）；
  - 评审详情、执行图和节点抽屉（[#69](https://github.com/LeXwDeX/Tribunal/pull/69)）；
  - 项目列表、项目详情和扩展页（[#74](https://github.com/LeXwDeX/Tribunal/pull/74)）；
  - 模型、统计和系统页（[#66](https://github.com/LeXwDeX/Tribunal/pull/66)）；
  - 公共视觉基础：tokens 和共享组件（[#34](https://github.com/LeXwDeX/Tribunal/pull/34)）；新增 `ButtonLink`、`Toolbar`（[#42](https://github.com/LeXwDeX/Tribunal/pull/42)）。
- **注册只负责登记和初始化**：控制台注册不再创建 Webhook，Webhook 改在项目设置里配置。服务端的 `auto_webhook` 字段和 CLI 的 `--auto-webhook` 保持兼容（[#61](https://github.com/LeXwDeX/Tribunal/pull/61)）。
- **评审输出更精简**：引入报告门槛和评论写法规则，总结限 1–3 句（[#52](https://github.com/LeXwDeX/Tribunal/pull/52)）。prompt bundle hash 随之变化。
- **会话数量**：每个用户最多保留 10 个浏览器会话，超出时淘汰该用户自己最旧的会话；全局上限 4096 只作兜底（[#40](https://github.com/LeXwDeX/Tribunal/pull/40)）。
- **内部重构**：`internal/app/review_service.go` 按职责拆成 6 个文件，逻辑不变（[#73](https://github.com/LeXwDeX/Tribunal/pull/73)）。

### 修复

- **安全**：
  - 平台管理员改为按 GitLab 用户 ID 判定，杜绝改名接管（[#40](https://github.com/LeXwDeX/Tribunal/pull/40)）。
  - 项目 token 和 webhook secret 的密文绑定项目与字段作为 AAD，互换密文会解密失败（[#64](https://github.com/LeXwDeX/Tribunal/pull/64)）。
  - 启动时的密文迁移按字段分别进行，一个字段损坏不会拖累另一个（[#76](https://github.com/LeXwDeX/Tribunal/pull/76)）。
  - 密文迁移全部完成后写入完成标记（迁移 `0015`），之后启动不再接受旧格式密文，防止把旧备份中的密文写回数据库后被重新绑定。标记写入后若出现旧格式字段，该字段保持原样并告警，需要重新设置对应凭据（[#96](https://github.com/LeXwDeX/Tribunal/pull/96)）。
  - 没有写权限的用户更新 Skill、更新/删除/测试 MCP 服务器时，资源存在与否得到相同的 403，不再能借此探测 ID 是否存在（[#96](https://github.com/LeXwDeX/Tribunal/pull/96)）。
- **评审结论**：块绑定的 MCP 工具只有部分可用时（服务器没有公布，或工具带写入注解被跳过），可用工具照常提供，缺失的每个工具记一条 `tool_gaps`，原本的 PASS 改判 INCOMPLETE（[#94](https://github.com/LeXwDeX/Tribunal/pull/94)）。
- **发布**：同一进程内同一工作流的发布互斥，启动恢复与 worker 不会重复创建行内讨论；停机时先等启动恢复结束，再关闭运行时和数据库（[#94](https://github.com/LeXwDeX/Tribunal/pull/94)）。
- **默认扩展**：
  - YAML 声明了某个块、而数据库中该块绑定仍是默认值写入且未被管理员改过时，以 YAML 为准，启动时移除默认写入的那一行；管理员改过的绑定仍以控制台为准。
  - 默认绑定与其账本记录在一个事务内写入，中途失败不会把块误判为「管理员已修改」。
  - 内置 Skill 刷新失败只记告警并继续启动，不再导致服务起不来（[#95](https://github.com/LeXwDeX/Tribunal/pull/95)）。
- **停机**：
  - 正常停机不再永久取消进行中的评审，重启后会续跑。
  - 心跳出现临时错误时不再取消 job。
  - 打开的 SSE 连接不再拖住停机（[#41](https://github.com/LeXwDeX/Tribunal/pull/41)）。
- **内部超时**：不再返回空的 200，改为返回 503（[#60](https://github.com/LeXwDeX/Tribunal/pull/60)）。
- **DAG 调度**：运行已经判定出错后，不再重新占用并启动同一个节点，避免多一次提交或多执行一次节点（[#80](https://github.com/LeXwDeX/Tribunal/pull/80)）。
- **MCP 批量导入**：改为在一个事务内完成，遇到内置服务器同名时一条都不写（[#50](https://github.com/LeXwDeX/Tribunal/pull/50)、[#71](https://github.com/LeXwDeX/Tribunal/pull/71)）。
- **扩展页**：作用域切换时布局不再跳动，Tabs 始终显示（[#42](https://github.com/LeXwDeX/Tribunal/pull/42)）。
- **评审列表**：列表接口不返回 MR 标题，各列表行不再显示「未命名 MR」，改为显示 `!iid`；「成本」列读取接口实际返回的字段，不再一直是「—」；评审详情面包屑显示项目路径（[#84](https://github.com/LeXwDeX/Tribunal/pull/84)）。列表接口每项新增 `findings_summary` 和 `nodes`（按页批量查询），列表的「P0 · P1 · P2」列和总览的阶段条开始显示（[#93](https://github.com/LeXwDeX/Tribunal/pull/93)）。

### 验证范围

- CI（go、web、docker 三项，其中 docker 含 Compose 冒烟测试）在每个 PR 和 main 上都已通过；Go 语句覆盖率保持在 70% 以上。
- Docker 冒烟逐个确认内置 codebase-memory-mcp 公布了默认配置绑定的全部只读工具（[#86](https://github.com/LeXwDeX/Tribunal/pull/86)）。
- **没有**在真实 GitLab 实例上验收。v1.0.0 的真实实例验收（f143 快照）不覆盖本版本的改动。上线前建议按 [交付就绪](docs/delivery-readiness.md#v110-验证状态) 列出的项目做一次升级验收。

### 已知限制

- Webhook 处理时如果存储出错，仍然返回 200 加 `ignored: storage_error`，GitLab 不会重试，需要监控日志后手动重放。
- 评审列表接口不返回 MR 标题，列表行显示 `!iid`，标题只在详情页显示。
- 发布互斥只在单个进程内生效，多个服务进程共用一个数据库时仍可能重复发布；当前部署形态为单实例。
- 全局 4096 个会话的兜底上限触发时，淘汰最旧的会话，不区分用户角色。
- `bootstrap_admins` 的用户名写法是首次出现时绑定（TOFU），推荐直接使用 `id:<GitLab 用户 ID>`。

## [1.0.0] - 2026-10-25

首个版本（GitHub Pre-release），交付 M0–M6 范围：webhook 与持久化队列、DAG 调度、主 agent 编排、对抗核验与裁决、GitLab 发布、三种 LLM 协议、工作区、MCP 与 Skill、OAuth、项目授权、Web 控制台和统计。详见 [Release v1.0.0](https://github.com/LeXwDeX/Tribunal/releases/tag/v1.0.0)。

[1.2.0]: https://github.com/LeXwDeX/Tribunal/compare/v1.1.0...v1.2.0
[1.1.0]: https://github.com/LeXwDeX/Tribunal/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/LeXwDeX/Tribunal/releases/tag/v1.0.0
