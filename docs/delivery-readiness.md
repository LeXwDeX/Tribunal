# 交付就绪、升级/回退与验收记录

> 本文先列出 v1.2.0 候选批次（[#128](https://github.com/LeXwDeX/Tribunal/pull/128)）的本地修复验证，再分三部分记录版本与交付状态：
>
> 1. [v1.2.0（待发布）验证状态](#v120-验证状态)：v1.2.0 实际做过哪些验证、还有哪些没做。
> 2. 升级、授权、验收矩阵与回退计划：与版本无关的操作模板，从[首次升级前记录](#首次升级前记录模板)到[放行与归档](#放行与归档)。
> 3. [v1.0.0 历史验收记录（f143，仅供追溯）](#v100-历史验收记录f143仅供追溯)：v1.0.0 在隔离测试实例上的验收结果，**不能**用来证明后续版本的改动。
>
> 本文不复制完整日志，也不把测试实例结果扩大为生产验收。

## v1.2.0 候选批次本地修复验证（PR #128）

- 2026-10-28，在 `main` `e7c59ab` 加本地未提交改动的工作区，MCP 配置加密、旧库回填、错误密钥拒绝、事务回滚和项目坏字段容错已通过 `go test -race -count=1 -timeout=15m ./internal/secret ./internal/store/sqlite ./internal/app ./internal/server ./cmd/server`；`go build ./...`、`go vet ./...` 通过。此前无密钥创建/导入及明文存储回归已实测失败，修复后通过。
- 本批新增 `0017_mcp_config_seal`，API 响应统一隐藏完整配置。升级前备份、原主密钥、回退及历史明文残留边界见 [MCP 配置加密升级](operations.md#mcp-配置加密升级v120)。
- 补充通过真实临时 SQLite、app service 和 HTTP handler 的 MCP 生命周期集成测试，以及 `go test -race -count=1 -timeout=10m ./scripts`。前端 `npm test -- --coverage` 通过 40 个文件、288 项测试，语句覆盖率 74.68%；`npm run build` 已刷新 `web/dist`，随后 `go build ./...` 通过。
- 扩展页使用本地静态产物和合成 API，在 1440/390 宽度、亮/暗主题下检查：页面无横向滚动，窄屏表格在自身容器滚动；改名与停用的请求只含 `name`、`enabled`，未回写隐藏的 `config`。从桌面切换到窄屏时，原导航焦点触发了 `aria-hidden` 警告；本批未修改共享导航，也未完成全站无障碍检查。
- 2026-10-28，审查 1–20 项已分别完成本地修复及独立终审。项目删除的旧状态任务跨登记、取消恢复最多处理 150 条，以及上下文生成复用错误索引等复审发现，均补充失败回归后修复。此处的逐项通过不代替最终跨模块终审。
- 最终本地 Go 检查：`go test -race -count=1 -timeout=40m -coverprofile=<临时文件> ./...` 全部通过；`coveragecheck -min 70` 为 **79.05%（20503/25936）**。`go build ./...`、`go vet ./...`、Staticcheck v0.8.2、`go mod verify`、licensecheck 和 licensebundle `-check` 通过。govulncheck v1.9.0 报告 `No vulnerabilities found`；该扫描使用公共漏洞库，不是离线扫描或不存在所有安全问题的证明。
- 上下文及 token 页面使用独立 Chrome 155 headless 和本地合成 API 验证。1440/390 宽度、亮/暗主题下均无页面横向溢出；保存上下文的实际请求不含旧 `locked` 值，保留并发管理员锁定。token 请求关闭重开仍防重，迟到明文不回填；停止等待及卸载的 abort 不承诺服务端回滚。隔离前端构建与当前 `web/dist` 字节一致；前述 288 项测试及覆盖率仍通过。部分操作采用 DOM 事件，不是完整无障碍审计。
- 本批还新增 `0018_dossier_project`，将可信项目归属独立存入 dossier；历史未知孤儿保守保留。取消后 pending 的发布状态仅在本地收敛，不调用 GitLab。升级与保留边界见 [dossier 归属迁移](operations.md#dossier-归属迁移v120)。
- 最终跨模块独立终审为 **PASS**，覆盖来源隔离、密封与启动迁移、重试与队列、删除与清理、取消恢复、摘要对账及 UI 的直接交互。复审提出的配置解析失败文档不一致已修正。此结论不证明代码不存在其他未知缺陷。
- 最终逻辑对抗复核提出慢请求的授权跨登记窗口；已补充真实临时 SQLite、HTTP handler 和 Delete/Register 回归，并在四个 dispatch 入口的项目锁内重新校验当前授权。修复后重新执行上述全仓 race、覆盖率、build、vet 和 Staticcheck，检查通过；补丁独立终审 **PASS**，针对该反例的逻辑复核为 **CLEAN**。其他在途写请求及复核后撤权的边界见[项目授权](operations.md#项目授权用户停用和凭据撤销)，不承诺全站即时撤销。
- **部署验收仍未完成**：本机没有 Docker CLI，未运行实际 Compose/镜像冒烟；未使用真实 GitLab、模型、凭据或生产数据库，也未提交或触发本候选的 CI。以上是本地工作区证据，不是发布放行。

<a id="v120-验证状态"></a>

## v1.2.0（待发布）验证状态

v1.2.0 尚未发布：版本说明已合并到 `main`，尚未打 tag，也未发布 Release。变更清单见 [CHANGELOG](../CHANGELOG.md) 的 `[1.2.0] - 2026-10-29` 段；候选范围可用 [v1.1.0...main](https://github.com/LeXwDeX/Tribunal/compare/v1.1.0...main) 核对。本节最初以 `main` `96e8081` 为发布候选基线，真实实例升级验收在 `main` `a113e08` 上执行；另列后续工作区的本地检查。发布前必须补齐未完成项。

**状态汇总（GitHub Actions 仍未通过）**

- [ ] **GitHub Actions**：截至 `main` `96e8081`，CI 有 workflow run 记录，但 go、web、docker 三个 job 因账户计费/额度限制没有启动，job 注解说明了原因。v1.2.0 期间的 PR 在 CI job 未启动时按交接约定用本地全量检查放行，并把结果评论到 PR；本文件不逐 PR 重述，[PR #112 评论](https://github.com/LeXwDeX/Tribunal/pull/112#issuecomment-6432897860) 是一个已核实的例子。没有成功执行的 CI job 结果，不能把本地预检当作 CI 结果。
- [x] **真实 GitLab 实例验收**：[#108](https://github.com/LeXwDeX/Tribunal/issues/108) 已于 2026-10-29 在 `main` `a113e08` 上执行；剩余 5 项全部通过；其中 `bootstrap_admins` 弃用告警首轮未复现，经 [#151](https://github.com/LeXwDeX/Tribunal/issues/151) 排查后在真实实例复现。详见[真实实例升级验收（#108）](#v120-真实实例升级验收)。GitHub Actions 仍未成功执行，不能把本项勾选当作 CI 通过；v1.0.0 的 f143 结果不覆盖本版本，见[历史验收记录](#v100-历史验收记录f143仅供追溯)。

<a id="v120-真实实例升级验收"></a>

**真实实例升级验收（[#108](https://github.com/LeXwDeX/Tribunal/issues/108)，2026-10-29，`main` `a113e08`）**

环境是 33.110 的隔离回归实例 `tribunal-regress`，连接测试用 GitLab 实例和测试项目，不是生产环境。升级前做了一致性备份并保留到验收结束。逐项证据（时间、workflow ID、hook ID、GitLab event ID）见 [#108 评论](https://github.com/LeXwDeX/Tribunal/issues/108)，本文不重复。

已通过：

- 升级：镜像从已有数据卷原地升级到 `a113e08`，`0019_job_defer_window` 已应用，项目、dossier 和发布记录保留，healthz/readyz 200。数据卷升级前的 schema 已含 `0016`，所以实际验证的是 `0017`–`0019` 的升级。
- v1.0.0 数据卷原地升级：在 33.110 新建隔离实例 `tribunal-acceptance-108`，从 `v1.0.0` tag 构建并启动，注册项目 1335、跑真实评审并发布；升级到 `v1.1.0` 后日志显示 `stored secrets migrated projects:1`，`0013`–`0015` 已应用，项目仍可 clone、评审、发布；再升级到 `a113e08`，`0016`–`0019` 已应用。
- 旧评审显示 `!iid`：v1.0.0/v1.1.0 产生的 dossier `mr_title=''`，控制台显示 `!1`；升级后的新评审 `mr_title='feat: add withdraw'`，列表显示标题（#101）。
- codebase-memory-mcp 实际调用：临时将验收实例 `review_defaults.ignore` 置空并重建索引，再用测试用项目 `custom_rules` 引导评审先调用图谱工具；按 `message.parts[].type == "tool_call"` 统计，项目 1335 实际调用 `list_projects`、`get_architecture`，项目 1336 调用 `list_projects`、`get_architecture`、`search_graph`。引导只用于验收，不改变产品默认行为。
- 多项目 MCP 隔离：注册第二个项目 1336 并跑真实评审；按 workflow 分别统计请求和响应，1335 的 workflow 中 `tribunal-acceptance-108` 出现 6 次、`tribunal-acceptance-108b` 出现 0 次；1336 中 `108b` 出现 5 次、`108` 出现 0 次。
- OAuth 登录；容器重启后会话保留；另用 v1.1.0 会话跨版本升级到 `a113e08`，同一浏览器 cookie 访问 `/api/v1/me` 仍返回 200。
- 升级后真实评审：MR !2 结论 `PASS`，MR !1、MR !3 结论 `BLOCKING`，均已发布到 GitLab。
- 停机续跑：评审运行中对容器发 SIGKILL，重启后 workflow 完成并发布（MR !3、MR !1）。
- Webhook 面板：停用、启用、外部删除后重建，GitLab 端 hook 状态与面板一致。
- 存储故障：SQLite 被独占锁定期间 webhook 返回 503，GitLab Recent events 记录失败；Resend 后返回 200 并被受理（#99）。
- 启动锁：同一数据卷启动第二个实例，以退出码 1 拒绝启动，第一个实例健康（#104）。
- 日志：故障日志使用 `error_class`，检查时段内没有 `error` 字段、token 或密码（#107）。
- 内置 `code-review` Skill：MR !3 的 LLM 调用均加载了该 Skill；4 条 findings 为 P1 concurrency、P2 logic、P1 concurrency、P1 standards，全部发布，汇总与明细一致，baseline smell 没有被单独升级为 BLOCKING。
- MR 标题：新评审的 dossier 都有 `mr_title`，列表显示标题（#101 新评审部分）。
- v2 控制台：在真实数据下检查 1440 和 390 宽度、亮色和暗色；总览、评审、项目工作区、扩展、模型与档位、统计与反馈、系统页面均无横向滚动（390 下逐页 `scrollWidth=390`）。
- 用户名写法的 `bootstrap_admins` 弃用告警（#103）：首轮两次尝试未观察到告警，另开 [#151](https://github.com/LeXwDeX/Tribunal/issues/151)。排查发现：按配置备份文件的修改时间，两次尝试中用户名写法只在宿主机文件里保留 10–30 秒，现存容器均在配置恢复为 `id:` 写法之后创建，验证窗口没有覆盖任何一次启动。2026-10-29 在 `tribunal-regress`（镜像 `a113e08`）把配置改为 `bootstrap_admins: ["suntao"]` 后执行 `docker compose up -d --force-recreate`，新容器启动日志在 `storage opened` 之后出现 `auth.bootstrap_admins username entry is deprecated ...`，带 `index=0`、`binding=none`，不含用户名；随后恢复原配置。`cmd/server` 新增从 YAML 加载并展开 `${VAR}` 的启动回归测试。

**历史本地预检（引用 PR #112 交接记录）**

- `main` `86c3a668` 对应的 PR #112 合并前 head 在本地通过：`go build ./...`、`go vet ./...`、`scripts/logcheck`、staticcheck v0.8.2、govulncheck v1.9.0、`go mod verify`、licensecheck、licensebundle `-check`、`go test -race -timeout 40m ./...`，Go 语句覆盖率 77.90%。
- 记录位置：[PR #112 评论](https://github.com/LeXwDeX/Tribunal/pull/112#issuecomment-6432897860)。该结果表明当时工作树通过本地预检，不代表 CI job 成功执行，也不覆盖之后未提交的改动。

**后续工作区本地检查（不属于 PR #112 或 CI）**

- 在 `main` `86c3a668` 加未提交改动的工作区，2026-10-27 复跑 `go test -race -count=1 -timeout=150s -run '^TestRepositoryLogsUseErrorClass$' -v ./scripts/logcheck` 并通过。这是一次针对日志检查的本地测试，不代表工作区完整验证或发布验收。
- 2026-10-28 针对停机资源收尾改动（`cmd/server/main.go`、`cmd/server/main_test.go`、`internal/app/runtime.go`、`internal/app/runtime_test.go`）运行 `go build ./...`、`go vet ./...`、staticcheck v0.8.2、govulncheck v1.9.0、`go test -race -timeout=40m ./...`，语句覆盖率 78.20%（18976/24266），并通过 `go mod verify`、licensecheck、licensebundle `-check` 和 `git diff --check`。独立 review 为 PASS，对抗推演确认 panic 路径的 handler/worker 屏障闭合。该结果只覆盖当前未提交工作区，不代表 CI 或真实实例验收；govulncheck 在本机读取了公共 Go 漏洞库元信息，不是离线扫描。
- 2026-10-28 在 PR #128 合并前 head `eb1e277` 运行 AGENTS.md 规定的全量检查，全部通过：`npm ci`、`npm test -- --coverage`、`npm run build`、`web/dist` 一致性、`go build ./...`、`go vet ./...`、staticcheck v0.8.2、govulncheck v1.9.0、`go test -race -timeout=40m ./...`、`coveragecheck` **79.07%**、`go mod verify`、licensecheck、licensebundle `-check`。GitHub Actions 未启动；该结果是本地预检，不是 CI 结果。来源见 [PR #128 评论](https://github.com/LeXwDeX/Tribunal/pull/128)。
- 2026-10-29 在 33.110 的临时目录按运维文档从零部署 `main` `96e8081`：`docker compose config --quiet`、`build`、`--check-config`、`up --wait` 通过；healthz/readyz 和控制台返回 200，未启用 OAuth 时 `/auth/login` 返回 503；容器以 uid 10001 运行，`/app/data` 为 0700，启动锁生效；内置 Skill 和 codebase-memory 已登记，MCP 配置 API 返回 `{}`，只暴露 13 个只读工具；`scripts/docker-smoke.sh` 通过且无残留。核对后已拆除该临时部署，不影响 `tribunal-regress` 和 `/opt/tribunal-test`。这是全新部署核对，不是 [#108](https://github.com/LeXwDeX/Tribunal/issues/108) 要求的真实数据升级验收。
- [#120](https://github.com/LeXwDeX/Tribunal/issues/120) 要求覆盖率不下降；尚未用可比的前后测量证明覆盖率严格不下降。PR #112 的 77.90% 和 PR #128 的 79.07% 不是同一候选上的前后对比，不证明后续工作区的覆盖率没有下降。

**v1.2.0 改动的自动化覆盖（测试代码存在；实际运行结果见上述记录）**

- 迁移文件 `0016_dossier_mr_title` 只追加列；接口层测试覆盖旧评审省略 `mr_title`、新 dossier 写入标题。
- 日志字段：`scripts/logcheck` 扫描非测试代码，禁止 logger 调用记录 `err` 或 `err.Error()`。
- 数据目录锁：独占锁在释放前阻止第二个持有者；启动时锁冲突以退出码 1 拒绝启动。
- `bootstrap_admins` 用户名写法：启动告警含序号，已绑定时给出 `replacement=id:<N>`，未绑定或读取失败时给出 `binding=none` 或 `binding=unavailable`，不含用户名；`id:` 写法不产生此告警。
- 会话淘汰保留管理员；webhook 存储失败返回 503。
- 这些测试只证明代码路径的语义，不替代真实实例验收；上述后续工作区检查只覆盖所运行的测试，不代表本节所有测试或当前发布候选已完整验证。

**升级边界**

- 迁移 `0016` 只对 `workflow_dossiers` 追加 `mr_title TEXT NOT NULL DEFAULT ''`，不改写已有行，不回填旧评审；迁移没有 down migration。升级前必须做一致性备份，回退只能恢复该备份，不能只回退镜像。
- 迁移 `0017_mcp_config_seal` 在启动时把已存 MCP 配置加密到新列并清空旧明文列；错误密钥、损坏配置或写入失败会阻止启动。回退只能恢复升级前快照；当前列清空不保证历史 WAL、空闲页、快照或备份中的明文已擦除。详见 [MCP 配置加密升级](operations.md#mcp-配置加密升级v120)。
- 迁移 `0018_dossier_project` 只按已关联 workflow 回填 dossier 项目归属，未知孤儿保持未知，不改快照内容。回退须恢复升级前快照。详见 [dossier 归属迁移](operations.md#dossier-归属迁移v120)。
- 完整边界见[迁移与回退边界](#迁移与回退边界)，操作步骤见 [从 v1.1.0 升级到 v1.2.0](operations.md#从-v110-升级到-v120)。
- ignore 保护范围扩大：默认 ignore 非空时，MCP 工具和全量图谱索引不执行，属于安全降级，不记 `tool_gaps`，也不改变 PASS。缺少版本化策略的旧工作流不续跑或重发。详见 [ignore 安全边界](operations.md#ignore-安全边界)。
- 启动锁要求同一服务实例独占数据目录；平台和网络文件系统限制见 [部署与运维](operations.md#1-镜像内容与边界)。
- 日志 `error` 和任务结算日志的 `reason` 已改名为 `error_class`；按旧字段配置的检索和告警需要调整。

<a id="v110-验证状态"></a>
<a id="当前版本v11x验证状态"></a>

## v1.1.x 验证状态（已发布）

变更清单见 [CHANGELOG](../CHANGELOG.md)，发布页见 [v1.1.0 Release](https://github.com/LeXwDeX/Tribunal/releases/tag/v1.1.0)。本节只记录 v1.1.x 实际做过的验证；待发布版本的状态见 [v1.2.0 验证状态](#v120-验证状态)。

**已完成**

- 每个 PR 和 main 上的 CI 均已通过：go 包括 vet、race 测试和覆盖率门槛，web 包括测试、覆盖率阈值和构建，docker 包括镜像构建和 Compose 冒烟。
- Compose 冒烟（`scripts/docker-smoke.sh`）覆盖以下内容：
  - 镜像构建；
  - `docker compose config`；
  - 服务就绪；
  - 内置 codebase-memory-mcp 的版本、wrapper 和许可证文本；
  - 在隔离 HOME 中用 CLI 建索引，并确认另一个 HOME 看不到这份索引；
  - 默认服务器已登记，测试结果只列出只读工具。
- 各 PR 的单元和集成测试覆盖：
  - 停机后续跑、心跳遇到临时错误不取消；
  - 管理员 ID 绑定与改名防护、每用户会话上限；
  - 密文 AAD 与启动迁移，包括幂等、并发修改和单个字段损坏；
  - 默认扩展的登记、升级同步和禁用保持；
  - MCP 导入的事务性；
  - v2 各页面的布局与可访问性断言。
- v2 界面在 1440 和 390 宽度、亮色和暗色下，用 mock 数据截图并与原型对照过。

**未在真实实例上验证**（上线前建议补做；v1.2.0 候选上的真实实例结果见[真实实例升级验收（#108）](#v120-真实实例升级验收)，不回填为 v1.1.x 的结论）

- [ ] 用 v1.0.0 的数据卷原地升级：迁移 13、14、15 已应用；密文迁移后已有项目还能 clone、评论和发布；OAuth 会话保留。
- [ ] 内置 codebase-memory-mcp 在真实评审中被模型实际调用，并且只能看到本项目的索引。
- [ ] 内置 `code-review` Skill 被评审块加载，评审输出符合新的报告门槛。
- [ ] 停机时有评审正在运行，重启后能续跑并完成发布。
- [ ] 用新的 Webhook 面板启用、重建、停用 GitLab Webhook，并确认 GitLab 端的 hook 与之一致。
- [ ] 在真实数据下浏览 v2 控制台的各个页面。

**v1.1.0 的已知限制**：webhook 处理时如果存储出错，返回 200 加 `ignored: storage_error`，GitLab 不会重试。该限制已在 `main` 修复（[#99](https://github.com/LeXwDeX/Tribunal/issues/99)、[#100](https://github.com/LeXwDeX/Tribunal/pull/100)），随 v1.2.0 发布（待发布），见 CHANGELOG 的 `[1.2.0]` 段。

## 迁移与回退边界

- 迁移实现：`internal/store/sqlite/sqlite.go` 的 `migrate()`；当前工作区迁移文件为 `0001`、`0003`–`0020`，共 19 个（没有 `0002`）。其中 `0016`–`0019` 属于 v1.2.0，`0020` 属于包含 #159 的后续版本；v1.2.0 升级基线仍为 `0001`、`0003`–`0019`，共 18 个。确认迁移结果时按已知版本集合核对，不把最大版本号当数量。
- 每个未应用迁移单独开事务；没有 down migration。启动时只跳过 `schema_migrations` 已记录的版本；数据库中已记录、但不在当前嵌入文件列表的高版本会被忽略而不报错。
- `migration_rollback_test` 只覆盖 `0004` 单文件在“建表完成、记录版本前”失败时的事务回滚及之后重试；不覆盖整批升级失败、跨版本回滚或降级。
- `0007` 给 `users` 增加 `disabled`，并新建空的 `project_grants`；升级不会给任何用户创建 grant。
- `0008` 增加 `publication_error`；`0009` 增加 webhook 状态/事件/错误，并把已有 `hook_id` 的行标为 `unknown`；`0010` 增加 deferred review candidate 序号；`0011` 增加状态 note 与逐 note 表情的持久化表；`0012` 增加 webhook 归属和时间戳，已有 `hook_id` 的行视为本服务所有。
- v1.1.0 新增 `0013_admin_bindings`、`0014_extension_defaults`、`0015_secret_migration`；启动时还会把已存项目凭据改写为新的加密格式，v1.0.0 读不了新格式。升级步骤见 [从 v1.0.0 升级到 v1.1.0](operations.md#从-v100-升级到-v110)。
- v1.2.0 新增 `0016_dossier_mr_title`，只对 `workflow_dossiers` 执行 `ALTER TABLE ... ADD COLUMN mr_title TEXT NOT NULL DEFAULT ''`。它不改写已有行，不回填旧评审，也不改变既有索引；升级前保存的 dossier 标题为空。
- v1.2.0 新增 `0017_mcp_config_seal`，追加 MCP 密文列和密钥校验表。启动时的数据回填另开事务，失败保留旧配置；成功后旧程序不再能读取这些配置。当前列清空不保证历史磁盘页、WAL 或备份中的明文已擦除。
- v1.2.0 新增 `0018_dossier_project`，只根据已关联 workflow 回填 dossier 项目归属，不改压缩快照内容。无关联证明的历史孤儿保持未知，不猜测删除。两项迁移均没有 down migration。
- 包含 #159 的后续版本只新增 `0020_review_command_replay`。启动时在独立事务回填可证明的旧命令，隔离由旧命令 payload 或 reaction 标识但无法证明的旧 note，并与唯一回填标记一起提交；reaction 不证明受理或命令种类，保守隔离可能拦截实际上从未执行的命令。schema 迁移完成不代表回填完成。没有可识别历史的旧命令，以及 reaction 中已舍入的大 note ID，仍可能在新 UUID 重发时漏过隔离；操作和回退限制见[命令重放幂等迁移](operations.md#命令重放幂等迁移后续版本)。
- 旧二进制可能打开已被新版本迁移的 schema，但它没有新权限逻辑，也读不了新格式密文，不能据此把“能启动”当作可安全回退。v1.2.0 迁移之后旧二进制读取新 schema 不受支持；回退只能恢复升级前的一致性备份。
- v1.2.0 升级前的一致性备份：核对生效的 `storage.dsn`、`workspace.root` 和全部实际挂载；停止旧服务后备份数据库、可能存在的 `-wal`/`-shm` 和所有工作区，并另存配置、镜像 digest 与 `REVIEW_MASTER_KEY`。只有符合默认路径和挂载前提时才能复制整个 `/app/data`；自定义路径、分离卷或额外/重叠挂载须按实际布局分别备份。运行中只复制数据库文件不是一致备份。恢复到匹配布局的新空位置，不覆盖旧数据、不混入旧 WAL，否则数据库与工作区可能来自不同时间点。操作步骤见 [SQLite 一致性备份与恢复](operations.md#7-sqlite-一致性备份与恢复)。

## 首次升级前记录（模板）

不假设旧测试机仍保持历史磁盘状态；以下字段必须在目标机现场读取，未知时保持空白。

| 字段 | 实际值 |
|---|---|
| 执行时间 / 操作人 / 变更单 |  |
| 旧镜像 tag@digest |  |
| 新镜像 tag@digest |  |
| 实际 Compose 项目名 |  |
| 全部数据挂载来源 / 容器内挂载点及覆盖关系 |  |
| 生效 `storage.dsn` 解析后的数据库路径 / `workspace.root`（不记录 secret） |  |
| 配置文件与 secret 注入方式 |  |
| 数据库及各工作区大小 |  |
| Docker 主机可用磁盘 |  |
| 备份目录 / 校验和 |  |
| SQLite `PRAGMA integrity_check` |  |
| 旧镜像可用性（本地/可拉取） |  |
| masterkey 是否仍在原安全位置（只记存在性） |  |

升级前必须同时满足：旧镜像可用、备份完整且已校验、主密钥可用、入口已隔离、目标机资源足够。任一不满足即停止，不进入迁移。

Tribunal 升级注意：镜像 tag 改为 `tribunal:*`；CLI 使用 `/app/tribunalctl`，旧 `/app/reviewctl` 兼容符号链接仍保留。环境变量主名 `TRIBUNAL_SESSION_KEY`、`TRIBUNAL_PUBLIC_URL`、`TRIBUNAL_HTTP_PORT` 优先，旧 `CRC_*` 名仍兼容。复用原 session key 的同一个值，不要重新生成。

## 入口隔离与备份

1. 暂停 GitLab webhook 或按项目禁用 hook，阻止人工触发、定时触发和 API 触发；如不能暂停，先在反向代理阻断 webhook 入口并记录 GitLab 侧可能的重试。
2. 记录旧实例停止时的 queued/running jobs、running/pending workflows 和 pending publications。新实例启动会由 worker/recover 接管并可能重放这些任务，不能假设队列为空；先人工核对 GitLab 已有评论/状态，避免重复副作用。
3. 按[第 7 节](operations.md#7-sqlite-一致性备份与恢复)先核对生效路径与全部挂载，停止旧服务并备份、核验数据库和所有工作区，同时备份实际配置和 secret；仅默认布局可使用其中的单目录命令。保持旧服务停止，不要把真实密钥写入仓库、本文或证据目录。
4. 保留旧镜像、旧卷和升级前快照，直到新实例验收完成；不要让两个实例共享同一 SQLite 卷。
5. 启动新实例会执行迁移、启动 worker、运行 recover/publish pending 和 cleanup，因此断网副本验证必须阻断对 GitLab、OAuth 和模型 provider 的出站访问，并使用占位配置；不得把真实库副本直接接到真实外部服务。
6. 恢复或离线验证使用匹配实际布局的新空卷或空目录，分别恢复并核验各数据部分；不把备份覆盖到旧数据目录，不混入旧 WAL。

## 升级操作（计划）

1. 完成前置记录和备份校验，确认旧镜像与备份均可恢复。
2. 隔离入口，停止旧服务，保存升级前全量快照。
3. 复制快照到隔离环境，启动新镜像；阻断外联，观察迁移、worker、recover 和 cleanup。按 v1.2.0 升级基线确认迁移记录为 18 个已知版本（`0001`、`0003`–`0019`；不含后续版本的 `0020`），并核对 `0017_mcp_config_seal` 和 `0018_dossier_project`，不把最大版本号当数量。
4. 隔离验证通过且已获部署授权后，在目标测试数据卷启动新镜像；等待 `/readyz` 200，检查日志和迁移错误。
5. 先完成阶段 A 管理面检查，再执行阶段 B 授权；授权完成前不向普通用户开放业务入口。
6. 按验收矩阵逐项记录；任何跨项目访问、误 PASS、数据损坏或迁移失败都触发回退评估。
7. 隔离外部调用、提交、推送、合并与 Release 都需要相应授权；验收不修改其他共享服务或生产数据。

## 两阶段授权

### 阶段 A：只开管理面

升级后 `project_grants` 为空，已有用户默认没有任何项目访问权。平台 `admin` 标签本身只表示管理面角色，`bootstrap` Bearer 的用户 ID 为 0，没有 grant，不能读取项目业务数据；`projects list` 对 bootstrap 通常为空，不能据此判断项目不存在。

先用管理员凭据列出本地用户和全部已登记项目，结合人工批准矩阵确定“本地用户 ID × 项目 ID × read/manage”：

```bash
docker compose exec -T review /app/tribunalctl users list
docker compose exec -T review /app/tribunalctl projects all
```

- `users list` 与 `projects all` 会自动跟随 `next_cursor` 汇总全部页。
- 未登录过 OAuth 的 GitLab 用户不在本地 `users` 表，无法预先授权；先让其完成一次登录。

### 阶段 B：按数字 ID 授权并验证

本地用户 ID 即 GitLab user id；项目 ID 使用 `projects all` 返回的数字 ID。按批准矩阵先向用户确认实际项目 ID、目标 GitLab 用户 ID 和本次唯一动作，再使用[部署与运维中的受保护单次授权步骤](operations.md#项目授权用户停用和凭据撤销)。每次都重新设置确认过的 ID 和动作，不沿用旧值；该步骤校验输入，只执行一个授权动作并列出结果。只通过管理面授权，不直接写 SQL。

- `projects grants list <项目 ID>` 只请求单页，默认 50 条、上限 200 条，CLI 不会自动翻页；超过 50 条时用管理 API 的 `GET /api/v1/admin/projects/{id}/grants?cursor=...` 继续读取。
- 每次 `set` 后重新 `list` 核对，再让用户用自己的 session/API token 验证业务读取。
- 授权矩阵先由人工批准；禁止用直接 SQL 插入 `project_grants` 代替审批和审计。

## 真实验收矩阵

使用 2 个用户、2 个项目，建议记为 U1/U2、P1/P2。本矩阵是每次升级都要用的模板；v1.1.x 和 v1.2.0 均尚无真实实例结果。v1.0.0（f143）测试实例的结果见 [v1.0.0 历史验收矩阵结果](#v100-验收矩阵结果)。隔离外部调用会产生真实模型用量，需事先获得授权。「自动化覆盖」列是用 fake store、fake GitLab 和 fake provider 跑的本地回归测试（`internal/server`、`internal/app`），只证明代码路径的语义，不替代真实实例验收。

| 场景 | 操作 | 预期 | 自动化覆盖 |
|---|---|---|---|
| 管理面 | bootstrap 调 `users list`、`projects all` | 成功；项目业务接口仍不可读 | `TestBootstrapBearerIsManagementPlaneOnly`、`TestAdminAccessEndpoints` |
| 仅管理面 | bootstrap 读 P1 项目详情、指定评审、项目统计 | 404，不泄露存在性 | `TestBootstrapBearerIsManagementPlaneOnly` |
| 跨项目 | U1 仅授权 P1，读取 P2 | 404；列表不出现 P2 | `TestProjectAccessIsolationTwoUsersTwoProjects` |
| 只读边界 | U1 对 P1 执行 manage 写操作 | 403 | `TestProjectWritePermissions`、`TestViewerCannotUseUnsafeMethods` |
| 管理授权 | U2 对 P2 有 manage，执行允许的项目写操作 | 成功 | `TestProjectWebhookControlRequiresManageGrant`、`TestExtensionScopeAuthorization` |
| 宿主配置 | U2 用 manage 创建项目级 MCP/Skill/块绑定 | 403；宿主执行配置仍需平台管理员 + manage | `TestAcceptanceManageGrantCannotConfigureHostExtensions` |
| 会话/Token | 同一用户分别用当前 session 和 API token 读取 | 授权结果一致 | `TestAcceptanceSessionAndAPITokenAuthorizeAlike` |
| CSRF | 浏览器会话写操作缺少/带有效 CSRF token | 403 / 200 | `TestCSRFRequiredForUnsafeSessionRequests` |
| 撤销/禁用 | `users revoke` 后旧 token 失效；`users disable` 后 session/token 失效且 OAuth 被拒 | 旧凭据不可用 | `TestAdminRevocationInvalidatesOldCredentials`、`TestDisabledUserCannotLoginOrReuseCredentials` |
| 重新登录 | `enable` 后重新 OAuth；不恢复旧 token | 登录成功、旧凭据仍无效、grant 保留 | `TestAcceptanceReloginKeepsGrantWithoutOldCredentials` |
| 升级会话 | 在线升级后使用原浏览器会话 | 会话仍有效 | `TestSessionSurvivesRestartWithSameKey` |
| webhook 归属 | 启用/停用/重新启用/重建，并保留外部同 URL hook | 只操作持久化自有 hook；外部 hook 保留 | `TestCreateWebhookDoesNotAdoptMatchingURLHook`、`TestDisableWebhookDoesNotDeleteUnrecordedHook`、`TestDeleteProjectRemovesOnlyRecordedHook` |
| SSE | 打开已授权评审流，随后撤 grant 或禁用用户 | 下一轮询关闭，不继续泄露 | `TestReviewStreamClosesOnGrantRevocationAndDisabledUser` |
| 统计 | 有 read 的用户读取授权项目统计；bootstrap 读项目统计 | 仅授权范围可见；bootstrap 404 | `TestProjectAccessIsolationTwoUsersTwoProjects`、`TestBootstrapBearerIsManagementPlaneOnly` |
| Payload | read 用户请求 `include_payload=true` | 403；manage 才可读 | `TestProjectAccessIsolationTwoUsersTwoProjects` |
| 扩展 | read 用户列项目级扩展；访问平台级扩展 | 项目级按 grant；平台级 403 | `TestExtensionScopeAuthorization` |
| 正常发布 | 完成评审并发布 | `published`，汇总评论/状态与 MR 一致 | `TestReviewRunPublishesSummaryOnce`、`TestPublisherRecordsCommitStatusState` |
| 部分失败 | 模拟一项发布失败、其他项成功 | 可重试失败保持 `pending`；永久失败为 `failed`；不重复副作用 | `TestPublisherPartialFailureDoesNotRepeatSideEffects`、`TestPublisherPermanentFailureMarksFailed` |
| 重启恢复 | 发布 pending 或运行中断后重启 | recover 继续，状态最终一致 | `TestReviewRecoverRunsPendingAndPublishes`、`TestReviewShutdownResumesOnStartupRecovery` |
| stale | 发布前改变 MR head/base | `skipped` 并记录原因，不发布旧结果 | `TestPublisherStalePublicationStoresSkippedReason`、`TestPublisherBaseChangeMarksStaleSkipped` |
| INCOMPLETE | 评审结果为 INCOMPLETE | 明确显示 INCOMPLETE，不得写 PASS 或绿灯状态 | `TestReviewIncompleteVerdictPersistsAndNeverReportsSuccess`、`TestAcceptanceIncompleteReviewIsNeverShownAsPass` |

## 回退触发与操作计划

以下为 0 容忍触发，出现任一项即停止放行并评估回退：

- 任何跨项目读取或写入。
- 任何误 PASS，或 INCOMPLETE 被显示为通过。
- 数据损坏、SQLite integrity 失败、workspaces 与数据库不一致。
- 迁移失败、迁移记录不完整，或启动后 schema 与预期不符。

回退操作计划（本文不执行生产回退）：

1. 停止新实例并隔离入口；保存升级后新数据的只读事故快照，保留新卷、旧卷、旧镜像和日志。
2. 按记录的实际布局新建空卷或空目录，确认挂载覆盖关系后分别恢复并核验升级前的数据库和全部工作区，准备匹配的旧镜像、旧配置和原 masterkey；不覆盖旧数据，不混入新 WAL。**启动恢复副本之前必须阻断 GitLab/OAuth/provider 出站访问**，并确认入站维护隔离生效。不能先启动再补防火墙规则，也不能把停用 webhook 当作阻止后台发布。
3. 不使用 down SQL，不删除 `schema_migrations` 行来绕过旧二进制检查，不只回退镜像而保留已迁移数据库。
4. 保持旧服务停止，先在出站阻断下启动旧镜像的恢复副本，按同样的隔离要求处理未知的旧版 worker/recover/publish 行为；不假设旧版提供“仅启动管理面”开关。保留不可变的原始备份，因为副本启动也可能改写任务状态。核对旧数据、镜像 digest、queued/running jobs 和 pending publications，并逐项比对 GitLab 已有外部副作用；核对完成且获得授权前，不解除出站限制。
5. 数据库能打开只允许继续受控管理面/只读核对，不能据此开放普通业务；恢复访问必须满足下述安全边界。旧镜像不能满足项目隔离要求时维持维护状态，转向安全版本修复，不以恢复可用性覆盖授权要求。

### 回退后的安全边界

- 升级后新增的 grant、禁用、撤销、反馈等写入不在旧快照中，回退会丢失；需要重新审批和补录，不能盲目合并。
- 旧快照中的 session/token 可能随数据库恢复而复活。旧代码没有项目隔离，可能让用户看到超出范围的数据。
- 因此回退后保持维护状态：阻断 OAuth 登录、所有用户 token 及既有浏览器 session 的业务请求，仅保留受控运维/管理面通道，不直接对普通用户重新开放。只有在恢复撤销语义或部署安全修复版本后，才可重新开放业务访问。
- 维护入口默认全拒：对普通网络阻断 `/auth/*`、业务 `/api/v1/*`、SSE 和控制台页面；只通过运维独占的回环/受控隧道开放核对所需接口，使用 bootstrap Bearer。该通道拒绝浏览器 Cookie 和个人 API token，包括旧库中的 admin 会话；具体代理/网络规则必须在目标环境验证，不能依赖旧应用实现新 grant 策略。
- GitLab 外部 note、commit status 等副作用不会随数据库恢复自动回滚。恢复发布前必须人工核对已发布内容和状态，避免重复评论、重复状态或错误覆盖。

## 放行与归档

### 放行条件

每次在目标环境升级时逐项填写；下列勾选框针对目标环境，不由本文预先勾选。

- [ ] 前置记录完整，旧镜像和新镜像 digest 可追溯。
- [ ] 全量备份、配置/secret 备份和 masterkey 均可用，恢复演练成功。
- [ ] 入口隔离完成，GitLab 重试和 pending 发布已有人工核对方案。
- [ ] 断网副本完成迁移、worker、recover 和 cleanup 验证。
- [ ] 两阶段授权完成，2 用户/2 项目矩阵全部通过。
- [ ] publication 正常、部分失败、重启恢复、stale、INCOMPLETE 均有记录。
- [ ] 没有未处置的跨项目访问、误 PASS、数据损坏或迁移失败。

### 归档

- 归档内容建议按证据类别保存：前置记录、镜像 digest、备份校验、迁移/就绪日志、授权矩阵、验收结果、回退决策。
- 不得把真实密钥、token、数据库副本或含真实凭据的日志放入仓库。
- 本地证据目录不纳入仓库（根 `.gitignore` 排除）；需要对外引用的证据摘要写进对应 PR 或 Issue。

## v1.0.0 历史验收记录（f143，仅供追溯）

> 本节是 v1.0.0 发布前在隔离测试实例上的验收记录，对应 [PR #30](https://github.com/LeXwDeX/Tribunal/pull/30) 和 [v1.0.0 Release](https://github.com/LeXwDeX/Tribunal/releases/tag/v1.0.0)。它不覆盖后续版本的任何改动；待发布版本的状态见 [v1.2.0 验证状态](#v120-验证状态)。本节内容保持当时的事实，不随后续版本更新。

### 结论

- 目标：v1 M0–M6，对应 PR #30。
- 已验证运行时候选/源码快照 `f143e7a7835e1e99345b1df9b289bb499e3c67416598d30e6184a558374eb0ac`（402 文件）的检查通过：`go build ./...`、`go vet ./...`、`go test -race -count=1 ./...`；语句覆盖率 77.09%（17665/22916）；`go mod verify`、65 个模块的 `licensecheck`、许可证 bundle `-check`；Web 27 个测试文件、160 个测试及 `npm run build`；Responses race 20 次通过，`dist` 未变。更早的 `27c9b1c`（77.11%）仅历史。
- 实际镜像 `crc-v1-acceptance:candidate-f143e7a`（ID `sha256:1a20aa96065a24986990de94d0e2e6466df04a379ed2e6b50f076271ff52574e`）构建并通过配置检查；该 tag 是历史验收标识，刻意保留。
- 旧快照的本地 review ACCEPT 不覆盖新的 Responses header。较早的独立代码评审未发现 code blocker，但因缺少 f143 运行时 manifest 与 `12a62d1` 文档 manifest/source-equivalence 而记为 evidence BLOCKING；该缺口已关闭。最终独立 review（`ses_fe5e8e8d28a2Mm41JlrHzyCxwp`）对 `12a62d1` 文档树给出 code/documentation PASS、0 blockers；`12a62d1` 相对 f143 只有 6 个 Markdown 文档不同，运行时输入未变。
- 连续两次实际无诊断代理 full deep 均为 0 call errors、0 failed nodes；MCP 成功/调用分别为 10/10 和 10/11（第二次一次模型工具错误已纠正），不能声称所有工具调用成功。两次均 BLOCKING/published；BLOCKING 是合成授权回归的预期结论，不是验收失败。实际镜像、离线恢复和在线替换均 PASS。

### PR #30 回读

- [x] PR #30 已于 2026-10-25T06:18:27Z squash 合并，合并 commit `73465ffde9e9a53b4301b08dff7ea4cf58810c4e`；PR 最终 head `0871450e6060962bcda1d251687bb1ef291832e1` 与合并 commit 上的 go、web、docker 三项 CI 均为 success。
- [x] [v1.0.0 Release](https://github.com/LeXwDeX/Tribunal/releases/tag/v1.0.0) 已发布（pre-release），tag `v1.0.0` 指向上述合并 commit。

### 隔离测试实例结果

- 管理面：bootstrap 可访问 admin 路由（200），项目业务路由返回 404；viewer 仅见项目 1332，跨项目 1331 的项目/评审/SSE/统计均 404，admin users 403。
- OAuth/项目授权：管理员用户 199 对项目 1331 有 `manage`，viewer 用户 200 对项目 1332 有 `read`；CSRF 缺失返回 403、有效 token 返回 200；跨项目读取返回 404；viewer 写操作返回 403；既有两个 OAuth 会话跨升级保留。
- SSE/撤权：撤 grant 后已有 SSE 在 1986ms 内关闭；撤权后读取 404，恢复 grant 后读取 200。
- Webhook：真实启用并发幂等；停用、重新启用和重建都保留外部同 URL hook；归属只按持久化的自有 hook ID 判断，不做 URL 匹配。
- GitLab：真实 Note Hook、状态 note 与表情反应、commit status，以及应用新增行（added line）行内评论发布成功。最终回读显示 `ai-review` status 52895/52896 在 `bae78e9980348c621cc0e1132e21b1656d44f212` 上为 failed；命令源 note 100746/100748 为 x、无 eyes，summary 100734 与 status note 100733 各一条。不同随机运行的 finding 集合和行内 diff key 可能不同，不能据此声称所有运行只有一条行内评论；幂等重放/重复发布结果以 PR #30 记录为准。
- 离线恢复与在线升级：应用了当时的全部迁移（1、3–12，共 11 个）；SQLite integrity、用户/授权/会话/任务/工作流/工作区保留。
- Deep：两次均使用 `deepseek-flash` 与 `gpt-6-astra`，两家 provider 均实际调用 MCP，完成 plan/fan-out/cross-verify/synthesis。
- Responses header：adapter 每次 Generate 发送请求级随机不透明 `X-Session-Id`，仅 Responses，无新配置/依赖/400 重试。它是网关兼容措施，不是 OpenAI 标准、会话认证或幂等保证；已部署网关内部根因未证实，缓存/握手成本可能存在。
- UI：f143 隔离实例在 1440 管理设置页和 390 viewer 设置页完成实际截图与 DOM 读取。390 视口为 390×844、scrollWidth 390，无横向溢出；viewer 页仅显示只读说明，无管理按钮。这不等同物理设备或完整 WCAG 验收。
- 范围外：生产部署、容量与计费认证、物理设备/完整 WCAG、Postgres、其他协议/其他 provider 和所有 MCP HTTP backend 不属于 v1 隔离验收范围。
- 上述结果只覆盖该隔离测试实例。

### v1.0.0 验收矩阵结果

按[真实验收矩阵](#真实验收矩阵)的场景记录；空白表示该测试实例未覆盖。

| 场景 | f143 测试实例结果 |
|---|---|
| 管理面 | 200 admin 路由；404 项目业务路由 |
| 仅管理面 | 已覆盖 |
| 跨项目 | 已覆盖 |
| 只读边界 | viewer 写操作返回 403 |
| 管理授权 | 管理员用户 199/manage 项目 1331 |
| 宿主配置 |  |
| 会话/Token |  |
| CSRF | 已覆盖 |
| 撤销/禁用 |  |
| 重新登录 |  |
| 升级会话 | 已覆盖；两个既有 OAuth 会话跨升级保留 |
| webhook 归属 | 已覆盖 |
| SSE | 已覆盖；撤 grant 后 1986ms 内关闭，读取 404，恢复 grant 后 200 |
| 统计 |  |
| Payload |  |
| 扩展 |  |
| 正常发布 | 两次 deep 均 BLOCKING/published；commit status failed；应用新增行行内评论存在 |
| 部分失败 |  |
| 重启恢复 |  |
| stale |  |
| INCOMPLETE |  |

完整 2 用户/2 项目矩阵仍有空白；该测试实例结果不授权向生产用户开放测试实例。

### 证据位置

- 交付证据、原始响应和最终回读以 [PR #30](https://github.com/LeXwDeX/Tribunal/pull/30) 为准。
- 当时的执行日志、运行时快照 manifest、`12a62d1` 文档快照 manifest 与 source-equivalence、f143 截图与 DOM 记录、GitLab 回读 JSON 只保存在当时执行环境的本地证据目录，未纳入仓库；本文不再引用这些本地路径。
