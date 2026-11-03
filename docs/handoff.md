# Tribunal 交接（2026-11-02）

> 这是给下一位 agent（或换机器后的自己）的唯一入口。新机器 `git clone` 之后只读这一份就能开工。
> 仓库是公开的，这里记录了内网地址和沙箱信息，但不含任何密钥或 token。

## 0. 启动提示词（在新机器上直接粘贴）

```text
这是 Tribunal 仓库。先读 AGENTS.md，再完整读 docs/handoff.md，按其中第 1 节准备环境、第 2 节的工作方式执行。
先看第 4 节剩余 Issue；没有明确新指令时，不要重做第 3 节已合并的 Issue。
每个 Issue 走 TDD → 本地检查 → 按 AGENTS.md 的 Development review workflow 完成审查（docs-only 用缩减链）→ PR → 最新 head 的 GitHub Actions 通过 → squash 合并。
全部自动推进，不要逐步问我；只有权限被拦或需要我本人操作（打 tag、发 Release、放行被拦的命令）时才停下来说明。
结束时更新 docs/handoff.md（通过 PR 入库），让下一次只读它就能继续。
```

## 1. 新机器环境准备

1. **克隆**：`gh repo clone LeXwDeX/Tribunal && cd Tribunal`。当前 `main` 为 `ca53b34`（#220 已合并）。
2. **工具**：Go（`go.mod` 的 toolchain 是 1.26.10）、Node + npm（`web/`）、Bash、Python 3、能跑 `-race` 的 C 编译器、`gh`（已登录、对仓库有写权限）。本机可以没有 Docker；没有 Docker 时 Compose/冒烟必须记为未验证。
3. **SpecGit**：仓库要求每个提交带 `Issue: #N` trailer，并由 SpecGit 的本地 checkpoint 把关。`.specgit.yaml` 和 SpecGit Skill 是本地文件，不在仓库里，新 clone 要重新接入：
   - 安装共享 CLI（见 <https://github.com/LeXwDeX/SpecGit/blob/main/docs/agent-install.md>），确认 `specgit --human --version` ≥ 2.17.1。
   - 仓库根目录 `specgit init --manual-observe --dry-run`，确认后去掉 `--dry-run` 执行。原声明的取值：`language: zh`、`remote: origin`、`validation.labels: off`、`agent.native_auto_merge: false`、`agent.close_issues_after_merge: false`，与默认不同就用 `specgit init --help` 里的参数改过来。
   - 读 `.agents/skills/specgit-native/SKILL.md`。
   - 常用：`specgit issue <编号> --branch <分支>` 认领 Issue 并建 checkpoint；`specgit pr` 建草稿 PR；`specgit pr --ready`；合并后 `specgit pr --status` 把 checkpoint 记为已交付。
   - 踩过的坑：主检出里残留旧分支的 checkpoint 时，`specgit issue` 会报 `prepared_blocked`，按提示用独立 worktree（`git worktree add -b <分支> ../tribunal-<N> main`，进去先 `specgit init --config-file <主检出的 .specgit.yaml 绝对路径> --manual-observe`）。`--reviewed-candidates` 的 digest 要用带 `--body-file` 的 `--inspect` 重新取，否则报 `concurrent_edit`。`--body-file` 不要用 `/tmp`（符号链接会被判 `unsafe_path`），用 `$TMPDIR/opencode/` 下的真实路径。
4. **CI**：GitHub Actions 的 hosted runner 已可用，`go`/`web`/`docker` 三个 job 会实际运行。合并前必须在**最新 main 同步后的 head** 上等这三项全部 success；旧 head 的通过结果不算。查看用 `gh run list` / `gh pr view <N> --json statusCheckRollup`，不要只依赖 PR 页面缓存。
5. **33.110**：已恢复可用，提供真实的 Docker 与 GitLab 部署验证环境（`tribunal-smoke-1102`，端口 18500）。它仍不参与 CI 合并门禁；CI 只用真实 GitHub Actions，33.110 只作为额外证据，见第 6 节。

## 2. 工作方式（用户的长期授权与要求）

- **全部自动推进，不要再问**。自行决策，推进到合并。交付物是干净的 main、清晰的 Issue 计划和这份交接文档。
- **PR 创建与 squash 合并已获长期授权**，不用等用户批准。前提：同步最新 main 后的 head 在 GitHub Actions 上 `go`/`web`/`docker` 全部通过，并完成 AGENTS.md 的 Development review workflow。不包括：仓库设置、分支保护、打 tag、发 Release。
- **合并门槛是当前 head 的 CI 加审查流程**，不是旧 head、不是本地单次运行、也不是 PR 的 `mergeStateStatus` 缓存。
- **PR 逐个合并的已知成本**：每个 PR 都改 `CHANGELOG.md`，因此每次合并后其余打开的 PR 会变成 `DIRTY/CONFLICTING`。处理方式固定：
  1. 在各 worktree `git merge origin/main`（不要 force-push）；
  2. `CHANGELOG.md` 保留双方条目、去重、修复粘连 bullet；
  3. `web/dist` 冲突时取 `main` 的版本，再在 `web/` 里 `npm ci --offline && npm test -- --coverage && npm run build` 重建，不要手改构建产物；
  4. 重新推送并等新 head 的 CI。
- **Docker**：本机没有 Docker 时，`docker compose config` 与 `scripts/docker-smoke.sh` 记为未验证；CI 的 docker job 通过可以作为合并依据，但不能写成“本机 Docker 已验证”。
- **TDD**：先写失败测试，再实现，再跑受影响测试集；修 bug 必须带修复前会失败的回归测试。
- **不要用 `--no-verify`**。commit/push 的 SpecGit guard 必须真实通过；被拒时修 checkpoint 或 trailer，不要跳过。
- **子 agent 分工**：大范围搜索、读长日志用便宜模型；规格已定的实现交给 implementer；设计、复审、最终验收由主会话做。正式审查按 AGENTS.md 的独立审核、3 方会审、对抗审核执行；子 agent 结论只是线索，必须逐条复核证据。
- 权限分类器拦下的命令不要绕过，记下来交给用户放行。

## 3. 当前状态

- `main`：`ca53b34`（#220 已合并）。
- 2026-10-30 审查列出的 P1/P2 与后续发现的 Issue 均已处理：#157、#158、#159、#160、#161、#162、#163、#164、#175、#176、#177、#178、#179、#180、#181、#182、#183、#184、#185、#186、#187。另有更早的 #140、#141、#142、#143、#144、#145、#146、#147、#170、#172、#174 同期合并。
- 2026-11-02 新增：PR #197（#196，镜像内 codebase-memory 验证）、#198（#192，verify 读取 CI 失败 job 日志）、#200（#199，交接文档）、#201（#195，暗色对比度）、#203（#202，成本函数修复）、#204（#194，压测脚本）、#205（#195，可访问名称）、#207/#209（交接文档）、#211（#210，压测脚本复审修复）、#213（#212，审查流程前缀指令）已合并；Issue #196、#192、#199、#202、#194、#206、#208、#210、#212 已关闭。
- 2026-11-03 新增：PR #217（#216，候选 CI 结论）、#219（#218，v1.2.0 范围扩大）、#220（#193，关闭自定义扩展创建并锁定内置扩展）已合并；Issue #216、#218、#193、#123 已关闭。
- #194 并发压测已实测：10 个项目并发 10/10 完成、0 失败，p50 22.24s、p95 27.23s；瓶颈是 provider `max_concurrency: 4` 排队。压测脚本随 PR #204 合并（`dc8fcde`），Issue #194 已关闭。
- #204 合并后按约定补做代码审核、3 方会审和对抗审核，发现压测脚本 P1：`read` 在无换行输入上触发 `set -e` 退出、缺少吞吐、`duration_ms` 文档口径、默认授权用户 59、轮询瞬时 HTTP 失败直接退出。修复随 PR #211 合并（`c279814`）；新增本地 mock 端到端测试，覆盖汇总、重试、取消、基线、默认不授权、admin 列表失败、grant 失败与授权目标用户，并接入 CI。
- 5 项目真实复测在修复 GitLab 项目 token 后通过：5/5 完成、0 失败，p50 27.4s、p95 31.9s，wall 58s，输入 348,429、输出 20,008 tokens，成本 116,085 micros。项目访问令牌 2026-12-03 到期，到期前需轮换。
- #195 自动化验收完成：33.110 总览页亮/暗 Lighthouse Accessibility 100；390/1440 宽度无横向滚动；键盘抽查通过。执行图 `label-content-name-mismatch` 由 PR #205 合并（`95e9e08`）修复，新镜像上 axe 0 违规；截图已生成但 GitHub 未登录，无法附到 Issue。
- **v1.2.0 仍未打 tag、未发 Release**，由维护者处理。范围已扩大到当前 `main`，包含 #143 与迁移 `0020`–`0024`；真实实例验收基线是 `a113e08`，只覆盖 `0016`–`0019`，`0020`–`0024` 与 #143 尚未在真实实例升级验收。CI 以最终 tag commit 的 GitHub Actions 结果为准。CHANGELOG 有 `[1.2.0] - 2026-10-29` 版本段。
- 迁移序号已用：`0016`–`0019`（v1.2.0 基线）、`0020`（#159）、`0021`–`0023`（#180）、`0024`（#179），均已合并并属于 v1.2.0。无 down migration；升级/回退必须按 `docs/operations.md` 备份并整体恢复，不能只换镜像。

## 4. 剩余 Issue

| Issue | 状态 | 说明 |
|---|---|---|
| #199 | 已关闭 | 交接文档已由 PR #200 合并；本次更新随本 PR 再入。 |
| #193 | 已关闭 | 关闭自定义 Skill/MCP 创建入口；内置 `code-review` 与 `codebase-memory` 锁定启用并启动恢复。原“执行内置 Skill 脚本”不再实施，随 PR #220 合并。 |
| #194 | 已关闭 | 10 项目并发已实测通过；`scripts/loadtest.sh` 随 PR #204 合并。复审发现脚本 P1，修复随 PR #211 合并（`c279814`）。SQLite/工作区锁等待未单独采集。 |
| #210 | 已关闭 | 压测脚本复审修复随 PR #211 合并（`c279814`）：汇总退出、吞吐、授权默认为零、HTTP 重试、基线 ID、统计与文档；`GRANT_MANAGE=1` 时从用户 token 的 `/me` 解析授权用户；执行图测试改为断言完整可访问名称。 |
| #212 | 已关闭 | 在 `AGENTS.md` 固化审查流程并删除过期 `prototype/v2` 条目，随 PR #213 合并。 |
| #195 | 已关闭 | 自动化审计、键盘抽查、390/1440 检查完成；`label-content-name-mismatch` 由 PR #205 合并修复，新镜像 axe 0 违规。截图未附，维护者豁免。 |
| #138 | 已关闭（not planned） | 内容是把开发 CI 迁移到 Azure Pipelines 并删除 `.github/workflows`。当前 GitHub Actions 的 hosted runner 已可用且是合并门槛；除非维护者明确要求迁移，否则保持现状。 |
| #123 | 已关闭（not planned） | 维护者决定不安排 self-hosted runner 验证；hosted runner 仍是默认门禁，自托管路径保留为手工可选补充。 |
| #109 | 跟踪中 | #192、#193、#194、#195、#196 已完成；Q1 暂不排期；Q18 上游改造与 Postgres 已记为不做；完整 WCAG 仍只完成主要页面自动化与键盘抽查。 |

审查中另有未开 Issue 的条件性风险，保留在 #109 的记录里，不按缺陷处理。

## 5. 每个 Issue 的交付流程

1. `git switch main && git pull --ff-only`；`specgit issue <N> --branch fix/<N>-<短名>`（被旧 checkpoint 拦就用独立 worktree，见第 1 节）。
2. 把对应的复现测试转成正式回归测试；确认修复前 FAIL。
3. 实现修复；跑受影响包 `go test -race -timeout 40m ./internal/<pkg>/...`；改了前端就 `cd web && npm ci --offline && npm test -- --coverage && npm run build`，并提交 `web/dist`。
4. 提交带 `Issue: #N` trailer；用户可见的变化写进 CHANGELOG 的 `Unreleased`。
5. 同步最新 main，推送后等当前 head 的 GitHub Actions `go`/`web`/`docker` 全部 success。
6. 按 AGENTS.md 的 Development review workflow 完成独立审核、3 方会审、对抗审核、裁决与证据检查（docs-only 用缩减链）；没有新的 P1 才继续。
7. `specgit pr` → `specgit pr --ready`；PR 正文写修复、回归测试、验证结果和未验证项。
8. `gh pr merge --squash --match-head-commit <已验证 head>`，回读 Issue 已关闭，`specgit pr --status` 记为已交付。

## 6. CI 与环境

**GitHub Actions（当前主路径）**
- 每个 push 和 PR 触发 `go`、`web`、`docker` 三个 job；合并前要求当前 head 三项全绿。
- 工作流在 `.github/workflows/ci.yml`，只读权限，不发布。

**33.110（已验证环境，不用于 CI）**
- SSH 已恢复。2026-11-02 使用 `/DATA/AppData/tribunal-smoke-1102`（端口 18500，镜像 `tribunal:smoke-1102-95e9e08`，commit `95e9e08`）完成真实部署验证；回退备份在 `.env.pre-95e9e08`。
- 验证内容：容器内 `codebase-memory-mcp` 0.12.1；MCP 测试返回 13 个只读工具；真实 MR !2 评审 PASS 并更新 summary note 101831。非空 ignore policy 下评审不注入 MCP 工具，属预期安全降级。
- 合并门禁仍只用真实 GitHub Actions；33.110 结果只作为额外证据，不能替代当前 head 的 GitHub Actions。
- 历史用法：`bash /tmp/client.sh [rev]`；结果在 `/DATA/AppData/tribunal-ci/runs/<时间-sha>/summary.txt`，末行 `RESULT PASS/FAIL`。

**GitLab 沙箱**：[suntao/tribunal-sandbox](https://git.ycgame.com/suntao/tribunal-sandbox)（id 1334）。MR !1 有故意埋的 bug，MR !2 是干净改动，MR !3 是 #108 验收用的。project access token id 213（token 本身不在仓库里）。

**33.110 上的其他实例（本次未使用，保留备查）**
- `tribunal-regress`（18420）：镜像 `tribunal-regress:v1.2.0-a113e08`，沙箱项目 1334，hook 113，`.project-token` 2026-11-30 过期。
- `tribunal-acceptance-108`（18430）与 `tribunal-acceptance-108-session`：#108 验收已结束，不再需要时可 `docker compose down` 并清理对应 hook/token/OAuth 应用。
- `/opt/tribunal-test`（18410）没有动过。
- 部署注意：端口只绑 `127.0.0.1`；镜像 tag 固定为 `tribunal:local`，多个检出会互相覆盖；compose 没配日志轮转；改了配置或 `.env` 后用 `docker compose up -d --force-recreate`。

## 7. 未验证项与风险

- 本机没有 Docker：所有 PR 的 `docker compose config` 与 `scripts/docker-smoke.sh` 未在本地运行。GitHub Actions 的 docker job 是合并门禁并已实际通过，但它不等同于本机 Docker 验证。
- 真实 GitLab 与真实模型 provider 已在 33.110 完成一次端到端评审（MR !2，deepseek-flash，summary note 101831），但讨论恢复、仓库 Skill 授权、跨代状态收敛的远端时序仍未在真实服务上复现。
- MCP 工具在 33.110 的测试接口可用（13 个只读工具）；评审因非空 ignore policy 未注入 MCP，这是安全降级。无 ignore policy 的真实评审尚未验证。
- #192 的 CI 日志脱敏是启发式，不能保证覆盖任意 secret；非空 ignore policy 下 trace 不会进入 dossier。`TaskRenderVersion` 已升至 4，旧工作流的 `prompt_hash` 会失效，恢复时需重新触发评审。
- 真实 GitLab 对“取消后迟到写入”的请求排序没有验证；#182 的收敛承诺只在 `docs/operations.md` 列出的条件（服务持续运行、失败可重试、权限有效、远程操作最终结束）下成立。
- 仓库 Skill 的发现接口不感知具体 MR 的 ignored rename；授权快照与运行时完整 deny 集合不一致时会 fail closed，需要重新授权。
- UI 的 1440/390、亮/暗主题检查已补真实后端（33.110）的总览、评审详情、统计和系统页；仍不是物理设备与触屏验证。
- #194 的单次 10 项目并发结果是环境观测，不是容量承诺；SQLite 锁等待没有单独指标，后续高并发需补 WAL/锁统计。
- 2026-11-02 用 #210 脚本复测 5 项目时先遇到 `gitlab_rejected`（GitLab 401）：注册时使用的个人 PAT 已撤销。改用项目访问令牌并轮换 Tribunal 项目 token 后，5 项目通过（5/5，0 失败，p50 27.4s、p95 31.9s，wall 58s，348,429/20,008 tokens，116,085 micros）。项目令牌 2026-12-03 到期，到期前需轮换。
- #195 已关闭；截图未附到 Issue，由维护者豁免。
- GitLab 临时 PAT（id 219）与 Tribunal 临时用户 token 已在验收后撤销；33.110 的 `.env` 与 admin token 仍保留，未轮换。
- 一次浏览器验证的进程诊断曾把疑似 GitHub 令牌写入会话工具输出；用户确认该令牌早已撤销。仍未复核所有会话记录/备份的访问范围，后续不要再输出进程列表或环境变量。
- `web` 构建存在既有的 >500 kB chunk 警告；没有引入代码分割。

## 8. 开发约定速记

完整规则见 AGENTS.md，这里只列最容易忘的：

- 一个目标对应一个 Issue 和一个 PR，每个提交带 `Issue: #N` trailer，squash 合并。
- `go test` 统一加 `-timeout 40m`；日志里禁止出现原始 `err`，用 `errclass`。
- 改 prompt 要 bump 版本注释并更新 `embeddedBundleHash`；任务布局或覆盖语义变化还要升 `prompts.TaskRenderVersion`（#192 后为 `4`）。
- 迁移只能追加，没有 down 迁移；不要在已发布迁移上改 SQL。
- 内置扩展锁定启用，不可禁用或删除；已有自定义扩展仍可启停。升级时刷新内置扩展，但不能覆盖管理员改过的绑定。
- 前端只用 CSS Modules 和 `web/src/styles/tokens.css`，用 `web/src/components/ui.tsx` 的共享组件；每次 web 改动都重建并提交 `web/dist`。
- 交接文档本身也走 Issue + PR 入库，不要放在 `.git/info/exclude` 里的本地文件。
- 合并后回读 Issue 与 PR 状态；CI 通知/评论不等于合并完成。
- 合并门槛是当前 head 的 CI 加上 AGENTS.md 的 Development review workflow；CI 绿只是必要条件。
