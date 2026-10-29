# 变更记录

格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## Unreleased

v1.2.0 尚未发布；冻结前的新改动仍记入下方 `[1.2.0]` 段，避免与待发布内容分叉。比较链接待打 tag 后按实际 tag 补充。

## [1.2.0] - 2026-10-29

> v1.2.0 尚未打 tag，也未发布 Release。本节的日期是版本说明的合并日，不是发布日；tag 和 Release 由维护者在 [#108](https://github.com/LeXwDeX/Tribunal/issues/108) 验收后决定。当前可核对的候选范围是 [v1.1.0...main](https://github.com/LeXwDeX/Tribunal/compare/v1.1.0...main)。打 tag 后补 `[1.2.0]: https://github.com/LeXwDeX/Tribunal/compare/v1.1.0...v1.2.0`，并回读确认链接可用。

### 升级前必读

- **先做一致性备份，三个迁移都不可回退**：新增迁移 `0016_dossier_mr_title`、`0017_mcp_config_seal`、`0018_dossier_project`，都没有 down migration。`0016` 只对 `workflow_dossiers` 追加 `mr_title TEXT NOT NULL DEFAULT ''`，不改写已有行，也不回填旧评审。回退只能恢复升级前的数据快照，不能只换回 v1.1.0 镜像。升级前按 [SQLite 一致性备份与恢复](docs/operations.md#7-sqlite-一致性备份与恢复) 核对生效路径和实际挂载，停止服务后备份数据库、存在时的 WAL/SHM 及全部工作区，另存配置、镜像 digest 和 `REVIEW_MASTER_KEY`。只有默认配置和单一数据挂载符合第 7 节前提时才能复制整个 `/app/data`；自定义布局须分别备份。恢复到匹配布局的新空位置，不覆盖旧数据，不混入旧 WAL；旧版本读取新 schema 不受支持。步骤见 [从 v1.1.0 升级到 v1.2.0](docs/operations.md#从-v110-升级到-v120)。
- **MCP 配置加密，迁移不可回退**：新增 `0017_mcp_config_seal`。启动时使用原 `REVIEW_MASTER_KEY`，在事务中加密所有已存 MCP 配置，清空旧明文列并写入密钥校验标记；错误密钥、损坏的 MCP 配置或写入失败会阻止启动。升级前做一致性备份；回退必须恢复升级前的快照，不能让旧程序读写已迁移的库。当前列清空不代表旧 WAL、空闲页、快照或备份中的明文已擦除。步骤及首次密钥校验边界见 [MCP 配置加密升级](docs/operations.md#mcp-配置加密升级v120)。
- **dossier 归属迁移不可回退**：新增 `0018_dossier_project`，只按已关联 workflow 回填旧记录归属，未知孤儿保持未知；不改快照内容。升级前做一致性备份，回退须恢复升级前快照。详见 [dossier 归属迁移](docs/operations.md#dossier-归属迁移v120)。
- **ignore 保护范围扩大**：有效 ignore 非空时，仓库读取和证据检查遵守同一规则；无法证明符合规则的 MCP 工具及全量图谱索引不再执行，无来源证明的历史上下文不再注入。默认 ignore 非空，因此默认图谱能力会降级；ignore 策略下 MCP 工具与全量图谱索引不执行，属于安全降级，不影响评审结论（不记 `tool_gaps`，PASS 不会因此变为 INCOMPLETE）；仅索引身份无法证明（且 ignore 为空）时，实际启用并绑定的工具仍记为覆盖缺口。管理员绑定、历史上下文和旧索引保留。缺少版本化策略的旧工作流不续跑或重发，需重新触发评审；详见 [ignore 安全边界](docs/operations.md#ignore-安全边界)。
- **日志字段改名，影响检索和告警**：生产日志不再记录原始错误文本。原来的 `error` 字段（原始错误）改为 `error_class`（脱敏类别，如 `timeout`、`canceled`、`internal`、`mr_closed`）；任务结算日志（`review job settled without retry`、`workspace job settled without retry`）里原来的 `reason` 字段也改名为 `error_class`。依赖 `error` 或任务结算日志 `reason` 的日志检索、面板和告警需要调整。评审筛选、发布失败、扩展绑定跳过等业务 `reason` 字段仍然保留。新增 `scripts/logcheck`，`go test ./...` 会拒绝在 logger 调用里记录 `err` 或 `err.Error()`。
- **数据目录独占锁**：服务启动时在 SQLite 文件所在目录对 `tribunal.lock` 加 `flock` 独占锁，第二个实例误用同一数据目录或数据卷时会记录带锁文件路径的错误日志并以退出码 1 退出。两个实例不能共用数据目录。锁随进程退出由内核释放，锁文件残留不代表仍被锁，不要手工删除。锁只在 Linux、macOS 和 BSD 上生效；Windows 等没有 `flock` 的平台只创建锁文件、不加锁，不在支持范围内。跨主机共享的 NFS 等网络文件系统不受保护，不要把数据目录放在网络文件系统上。
- **`bootstrap_admins` 用户名写法弃用**：用户名条目仍可用，但启动时每个条目记一条弃用告警，含条目序号 `index` 和绑定状态 `binding=none` / `binding=unavailable` 或替代值 `replacement=id:<N>`，不含用户名。计划在 v2.0.0 移除。迁移步骤见 [部署与运维](docs/operations.md#bootstrap_admins-用户名写法弃用与迁移)。

### 新增

- **评审列表返回 MR 标题**：`GET /api/v1/reviews` 每项增加 `mr_title`，控制台的评审列表和总览显示标题。标题在保存评审 dossier 时写入新列 `workflow_dossiers.mr_title`（迁移 `0016_dossier_mr_title`，只追加列），列表按页一次批量读取，不请求 GitLab。升级前已有的评审没有该列的值，接口省略 `mr_title`，控制台继续显示 `!iid`（[#101](https://github.com/LeXwDeX/Tribunal/issues/101)）。

### 变更

- **Webhook 存储失败改返回 503**：查项目、写投递记录或入队失败时，`POST /webhooks/gitlab` 由 200 改为 503，响应体仍是 `{"ignored":"storage_error","request_id":...}`，日志带脱敏的失败类别。GitLab 不会自动重试失败的投递，但会在 hook 的「Recent events」里记为失败，可以用 Resend request 重发，同一事件 UUID 会被正常受理。GitLab 连续收到失败响应时可能自动停用 hook，需要关注 GitLab 端的 hook 状态。输入错误、未登记项目、重复投递等情况仍返回 200（[#99](https://github.com/LeXwDeX/Tribunal/issues/99)）。
- **全局会话上限优先保留管理员**：全局 4096 个会话的兜底上限触发时，先淘汰非管理员的最旧会话，只有剩下的会话全部属于管理员时才淘汰管理员的最旧会话。角色按当前的 `bootstrap_admins` 判断：服务启动时会按配置刷新已存用户的角色，会话被使用时也会刷新，所以改了配置后，不需要用户重新登录。每个用户 10 个会话的上限不变，无需迁移（[#102](https://github.com/LeXwDeX/Tribunal/issues/102)）。
- **CI 接入静态分析与漏洞扫描**：go job 新增固定版本的 `staticcheck` 和 `govulncheck`，并清理了现有告警。`go.mod` 增加 `toolchain go1.26.10`，本地开发和 CI 不再用 go1.26.0 构建（该版本有 31 个可达的标准库漏洞）；发布镜像本来就是 1.26.10，不受影响（[#106](https://github.com/LeXwDeX/Tribunal/issues/106)）。
- **日志不再记录原始错误文本**：工作区、评审副作用、运行时恢复、清理、发布状态、bootstrap 管理员绑定和启动恢复等日志不再输出 `error`（原始错误），改为 `error_class` 脱敏类别并保留项目、MR、工作流等标识字段；任务「settled without retry」日志的 `reason` 字段也改为 `error_class`（如 `mr_closed`、`review_superseded`、`stale_publication`、`permanent_job`）。评审筛选、发布失败、扩展绑定跳过等业务 `reason` 字段保持不变。新增 `scripts/logcheck`，`go test ./...` 会拒绝在 logger 调用里记录 `err` 或 `err.Error()`（[#107](https://github.com/LeXwDeX/Tribunal/issues/107)）。
- **启动时独占数据目录**：服务在打开 SQLite 之前对数据库所在目录的 `tribunal.lock`（默认容器配置下为 `/app/data/tribunal.lock`）加 `flock` 独占锁。第二个实例误用同一数据目录或数据卷时，记录带锁文件路径的错误日志并以退出码 1 退出，不再与第一个实例同时跑恢复、worker 或发布。v1.1.0 已知限制中「多个服务进程共用一个数据库时仍可能重复发布」改由启动锁防护；平台与网络文件系统限制见上方升级前必读（[#104](https://github.com/LeXwDeX/Tribunal/issues/104)）。

### 修复

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

### 弃用

- **`bootstrap_admins` 用户名写法弃用**：`auth.bootstrap_admins` 里的 GitLab 用户名条目仍然可用，但启动时每个条目记一条弃用告警。告警含条目序号 `index` 和绑定状态 `binding=none` / `binding=unavailable` 或替代值 `replacement=id:<N>`，不含用户名。示例配置只保留 `id:` 写法。计划在下一个大版本（v2.0.0）移除用户名写法，迁移步骤见 [部署与运维](docs/operations.md#bootstrap_admins-用户名写法弃用与迁移)（[#103](https://github.com/LeXwDeX/Tribunal/issues/103)）。

### 已解决

- v1.1.0 已知限制「评审列表接口不返回 MR 标题」：见上方新增条目（[#101](https://github.com/LeXwDeX/Tribunal/issues/101)）。
- v1.1.0 已知限制「Webhook 存储失败返回 200」：改为返回 503，失败投递可在 GitLab 中重发（[#99](https://github.com/LeXwDeX/Tribunal/issues/99)）。
- v1.1.0 已知限制「多个服务进程共用一个数据库时仍可能重复发布」：同一数据目录的第二个进程现在无法启动（[#104](https://github.com/LeXwDeX/Tribunal/issues/104)）。
- v1.1.0 已知限制「全局会话上限淘汰不区分角色」：现在优先保留管理员会话（[#102](https://github.com/LeXwDeX/Tribunal/issues/102)）。

### 验证范围

- **未完成：CI**。截至 `main` `96e8081`，GitHub Actions 的 go、web、docker 三个 job 因账户计费/额度限制没有启动，job 注解说明了原因。不能声称本版本的 CI 已通过。
- **未完成：真实实例验收**。[#108](https://github.com/LeXwDeX/Tribunal/issues/108) 要求的 v1.1.x/v1.2.0 真实 GitLab 实例升级验收尚未执行，它是打 `v1.2.0` tag 的前置条件。v1.0.0 的 f143 结果不覆盖本版本。
- **本地预检（引用交接记录，不是本次发布重新核验）**：`main` `86c3a668` 对应的 PR #112 合并前 head 在本地通过 `go build`、`go vet`、`scripts/logcheck`、staticcheck v0.8.2、govulncheck v1.9.0、`go mod verify`、licensecheck、licensebundle `-check`、`go test -race -timeout 40m ./...`，语句覆盖率 77.90%；PR #128 合并前 head `eb1e277` 通过同样的全量检查（含前端 `npm ci`、`npm test -- --coverage`、`npm run build` 和 `web/dist` 一致性），语句覆盖率 79.07%。来源见 [PR #112 评论](https://github.com/LeXwDeX/Tribunal/pull/112#issuecomment-6432897860) 和 [PR #128 评论](https://github.com/LeXwDeX/Tribunal/pull/128)；它们不代表 CI 已启动，也不覆盖之后未提交的改动。详见 [交付就绪](docs/delivery-readiness.md#v120-验证状态)。
- **部署核对（引用交接记录）**：2026-10-29 在 33.110 的临时目录按运维文档从零部署 `main` `96e8081`，`docker compose config --quiet`、`build`、`--check-config`、`up --wait` 通过；healthz/readyz 和控制台返回 200，未启用 OAuth 时 `/auth/login` 返回 503；容器以 uid 10001 运行，`/app/data` 为 0700，启动锁生效；内置 Skill 和 codebase-memory 已登记，MCP 配置 API 返回 `{}`，只暴露 13 个只读工具；`scripts/docker-smoke.sh` 通过且无残留。该部署不是 GitLab 升级验收，也不覆盖 [#108](https://github.com/LeXwDeX/Tribunal/issues/108) 的真实数据升级场景。

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

[1.1.0]: https://github.com/LeXwDeX/Tribunal/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/LeXwDeX/Tribunal/releases/tag/v1.0.0
