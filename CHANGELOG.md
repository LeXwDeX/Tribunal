# 变更记录

格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### 变更

- **Webhook 存储失败改返回 503**：查项目、写投递记录或入队失败时，`POST /webhooks/gitlab` 由 200 改为 503，响应体仍是 `{"ignored":"storage_error","request_id":...}`，日志带脱敏的失败类别。GitLab 不会自动重试失败的投递，但会在 hook 的「Recent events」里记为失败，可以用 Resend request 重发，同一事件 UUID 会被正常受理。GitLab 连续收到失败响应时可能自动停用 hook，需要关注 GitLab 端的 hook 状态。输入错误、未登记项目、重复投递等情况仍返回 200（[#99](https://github.com/LeXwDeX/Tribunal/issues/99)）。
- **CI 接入静态分析与漏洞扫描**：go job 新增固定版本的 `staticcheck` 和 `govulncheck`，并清理了现有告警。`go.mod` 增加 `toolchain go1.26.10`，本地开发和 CI 不再用 go1.26.0 构建（该版本有 31 个可达的标准库漏洞）；发布镜像本来就是 1.26.10，不受影响（[#106](https://github.com/LeXwDeX/Tribunal/issues/106)）。
- **日志不再记录原始错误文本**：工作区、评审副作用、运行时恢复、清理、发布状态、bootstrap 管理员绑定和启动恢复等日志不再输出 `error`（原始错误），改为 `error_class` 脱敏类别并保留项目、MR、工作流等标识字段；任务「settled without retry」日志的 `reason` 字段也改为 `error_class`（如 `mr_closed`、`review_superseded`、`permanent_job`）。依赖原 `error`/`reason` 字段的日志检索需要调整。新增 `scripts/logcheck`，`go test ./...` 会拒绝在 logger 调用里记录 `err` 或 `err.Error()`（[#107](https://github.com/LeXwDeX/Tribunal/issues/107)）。
- **启动时独占数据目录**：服务在打开 SQLite 之前对数据库所在目录的 `tribunal.lock`（容器内 `/app/data/tribunal.lock`）加 `flock` 独占锁。第二个实例误用同一数据目录或数据卷时，记录带锁文件路径的错误日志并以退出码 1 退出，不再与第一个实例同时跑恢复、worker 或发布。锁随进程退出释放，崩溃后无需清理。v1.1.0 已知限制中「多个服务进程共用一个数据库时仍可能重复发布」改由启动锁防护。锁只在 Linux、macOS、BSD 上生效，Windows 不支持；网络文件系统上的数据目录不受保护（[#104](https://github.com/LeXwDeX/Tribunal/issues/104)）。

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
