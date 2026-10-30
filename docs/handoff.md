# Tribunal 交接（2026-10-30 · 换机器后从这里开始）

> 这是给下一位 agent（或换机器后的自己）的唯一入口。新机器 `git clone` 之后只读这一份就能开工。
> 仓库是私有的，这里记录了内网地址和沙箱信息，但不含任何密钥或 token。

## 0. 启动提示词（在新机器上直接粘贴）

```text
这是 Tribunal 仓库。先读 AGENTS.md，再完整读 docs/handoff.md，按其中第 1 节准备环境、第 2 节的工作方式执行。
目标：按 docs/handoff.md 第 4 节的顺序处理待办 Issue，从 #157 开始；每个 Issue 走 TDD → 本地 CI（33.110）→ PR → squash 合并的完整流程。
全部自动推进，不要逐步问我；只有权限被拦或需要我本人操作（打 tag、发 Release、放行被拦的命令）时才停下来说明。
结束时更新 docs/handoff.md（通过 PR 入库），让下一次只读它就能继续。
```

## 1. 新机器环境准备

1. **克隆**：`gh repo clone LeXwDeX/Tribunal && cd Tribunal`。当前 `main` 至少包含 `4839248`（#143）和本交接的 PR（#165）。
2. **工具**：Go（`go.mod` 的 toolchain 是 1.26.10，旧机器用 1.27.3 也能跑）、Node + npm（`web/`）、Bash、Python 3、能跑 `-race` 的 C 编译器、`gh`（已登录、对仓库有写权限）、`ssh root@192.168.33.110` 免密（跑本地 CI 用）。Docker 本机可以没有，Docker 冒烟在 33.110 上做。
3. **SpecGit**：仓库要求每个提交带 `Issue: #N` trailer，并由 SpecGit 的本地 checkpoint 把关。`.specgit.yaml` 和 SpecGit Skill 是本地文件，不在仓库里，新 clone 要重新接入：
   - 安装共享 CLI（见 <https://github.com/LeXwDeX/SpecGit/blob/main/docs/agent-install.md>），确认 `specgit --human --version` ≥ 2.17.1。
   - 仓库根目录 `specgit init --manual-observe --dry-run`，确认后去掉 `--dry-run` 执行。原声明的取值：`language: zh`、`remote: origin`、`validation.labels: off`、`agent.native_auto_merge: false`、`agent.close_issues_after_merge: false`，与默认不同就用 `specgit init --help` 里的参数改过来。
   - 读 `.agents/skills/specgit-native/SKILL.md`。
   - 常用：`specgit issue <编号> --branch <分支>` 认领 Issue 并建 checkpoint；`specgit pr` 建草稿 PR；`specgit pr --ready`；合并后 `specgit pr --status` 把 checkpoint 记为已交付。
   - 踩过的坑：主检出里残留旧分支的 checkpoint 时，`specgit issue` 会报 `prepared_blocked`，按提示用独立 worktree（`git worktree add -b <分支> ../tribunal-<N> main`，进去先 `specgit init --config-file <主检出的 .specgit.yaml 绝对路径> --manual-observe`）。`--reviewed-candidates` 的 digest 要用带 `--body-file` 的 `--inspect` 重新取，否则报 `concurrent_edit`。
4. **33.110 CI runner**：`scp root@192.168.33.110:/DATA/AppData/tribunal-ci/client.sh /tmp/`，以后在仓库根目录 `bash /tmp/client.sh [rev]`。详见第 5 节。

## 2. 工作方式（用户的长期授权与要求）

这些原本记在旧机器的 agent 记忆里，新机器没有，以这里为准：

- **全部自动推进，不要再问**。自行决策，推进到合并。交付物是干净的 main、清晰的 Issue 计划、和这份交接文档。
- **PR 创建与 squash 合并已获长期授权**，不用等用户批准。前提：同步最新 main 后的 head 在本地 CI 上 PASS。不包括：仓库设置、分支保护、打 tag、发 Release。
- **GitHub Actions 一律跳过**：因为计费限制，go/web/docker 三个 check 会在 2–3 秒内 FAILURE（注解是 payments failed / spending limit），这是预期，不等、不修、不作为合并依据。文档里不能写成“GitHub CI 通过”，`docs/delivery-readiness.md` 的 GitHub Actions 一项保持未勾选。
- **合并门槛是本地 CI**：本机或 33.110 跑 AGENTS.md 的全量检查，Docker 冒烟只能在 33.110。把 run 目录、commit、覆盖率评论到 PR。
- **33.110 上的 Docker 可以随便用**：起实例、建镜像、做复现都不用事先确认，用完清理自己建的东西。
- **不设定时任务**：不建 cron、timer、定时唤醒，只做一次性操作。
- **TDD**：先写失败测试，再实现，再跑完整的受影响测试集；修 bug 必须带修复前会失败的回归测试。
- **子 agent 分工**：大范围搜索、读长日志用便宜模型；规格已定的实现交给 implementer；设计、复审、最终验收由主会话做。子 agent 结论要抽查。
- 权限分类器拦下的命令（例如 `gh issue create`、批量 `git push --delete`）不要绕过，记下来交给用户放行。

## 3. 当前状态

- `main`：`4839248`（#143 已合并）之后加上本交接的 PR（#165）。没有进行中的分支或 PR。
- **v1.2.0 仍未打 tag、未发 Release**，由维护者处理。CHANGELOG 有 `[1.2.0] - 2026-10-29` 版本段。#143 已在 main 上：如果 tag 打在 `4839248` 或之后，#143 的 CHANGELOG 条目应归入 `[1.2.0]`；如果打在 `e2b3628`，留在 `Unreleased`。动 CHANGELOG 前先确认 tag 的位置。
- 远端有一批已合并的旧分支可以删（`chore/v1.2.0-release-119`、`docs/readiness-108`、`fix/bootstrap-warning-151`、`fix/108-acceptance-docs`、`fix/153-auth-expiry-notice`、`fix/ignore-policy-126`、`fix/oauth-disabled-ui-131`、`fix/onboarding-diagnostics-132`、`fix/onboarding-retry-132`、`fix/project-detail-badge-129`、`fix/reliability-gaps-127`、`fix/workspace-auto-refresh-130`、`feat/143-default-extension-bindings`、`docs/handoff-repro`）。之前批量删除被权限分类器拦下，需要用户执行或放行。
- `chore/azure-ci-migration-138`（#138）在旧机器上有一个未提交的工作树，**新机器上拿不到那些改动**；#138 要从远端分支的现状重新评估。

## 4. 待办 Issue（全部未开始）

用户 2026-10-30 指示：这些 Issue 先不做，换机器后再开工。顺序如下。

### 4.1 2026-10-30 项目级审查发现（最优先）

审查对 `4839248` 做了只读综合审查；旧机器的会话复核了源码，4 个 P1 都成立。

| Issue | 级别 | 内容 | 主要位置 |
|---|---|---|---|
| #157 | P1 | 旧发布代次覆盖同一 MR 新代次的总结评论与 commit status | `internal/app/publisher.go`：`lockWorkflow` :568、`publish` :219/:244、summary :329、status :400-428、`publishFailureLocked` :486 |
| #158 | P1 | 超长 MR 描述把 diff 挤出 prompt，覆盖检查为 0 | `internal/review/prompt.go:94`；`nodes_agent.go` `reviewTask` :1146、`summaryForPrompt` :1297；`coverage.go:16-69,180` |
| #159 | P1 | `/review` 命令 job 重放清除之后的 `/cancel`（条件触发） | `internal/app/command_worker.go:223-273`；`internal/store/sqlite/review_state.go:269-285` |
| #160 | P1 | 扩展绑定分页未加载完时保存静默删除授权 | `web/src/features/extensions/ExtensionsManager.tsx:93-97,998-1021`；绑定 PUT 无版本校验 |
| #161 | P2 | retry key 未随项目删除；新 generation 继承旧 attempt；verify REFUTED 未被二次 synthesis 采用 | `sqlite/projects.go:338-351`、`app/workspace_service.go:217-221`；`sqlite/review_side_effects.go:86-158`、`review_state.go:232-245`；`review/nodes_agent.go:717-780`、`blocks.go:1276-1317` |
| #162 | P2 | 重命名链定位错文件；diff 行数统计与首文件上限 | `internal/gitlab/diff.go:42-57`；`internal/app/review_prepare.go:398-403,417-424,499-509` |
| #163 | P2 | AddMcp 跨项目提交草稿；项目只取前 100 项、总览空态；`allow_repo_skills` 未接线 | `ExtensionsManager.tsx:1387-1420`；`ExtensionsPage.tsx:24`、`ReviewsPage.tsx:69`、`OverviewPage.tsx:274-281,366-383`；`app/runtime.go:173`、`app/adapters.go:727` |
| #164 | P2 | check-config 顺序；已删除 summary note 发布永久失败；webhook 读体超时返回 200；Anthropic cache 用量少计 | `cmd/server/main.go:89-155,212-215`；`app/publisher.go:755-777`；`server/webhook.go:14-39`；`llm/anthropic.go:274-278`、`review/executor.go:832-844` |

行号以 `4839248` 为准。每个 Issue 正文都有 Why / Scope / Approach / Acceptance，开工先 `gh issue view <N>`。

**各 Issue 的额外要点**
- **#157**：同一 `(project_id, mr_iid)` 的发布要串行。在 workflow 锁之外加 MR 级锁，锁内读 control；每次写 summary、写 status 前重新校验 `control.Generation` 和 `HeadSHA`。`publishFailureLocked` 也要覆盖。加锁顺序固定为“workspace RLock → MR 锁 → workflow 锁”，避免死锁。
- **#158**：描述单独限额，截断时在 prompt 注明；给 diff 留足预算；覆盖检查基于 `BuildPrompt` 的实际输出。这是 prompt 改动，要 bump prompt 文件的版本注释，并更新 `prompts/prompts_test.go` 的 `embeddedBundleHash`。
- **#159**：命令按 `(project_id, mr_iid, note_id, command)` 幂等，记录与入队同一事务，重放直接返回 Duplicate、不 bump control。新表就加下一个编号的迁移（只能追加），并写进 CHANGELOG 和 `docs/operations.md`。
- **#160**：前端在保存前确保相关块已全量加载，否则禁止保存；服务端绑定写入加乐观并发校验，基线过期返回 409。UI 在 1440/390、亮/暗下检查。
- **#163**：打开 `allow_repo_skills` 前确认仓库 Skill 的信任模型（仓库内容不可信，只从目标分支读）与文档一致。
- **#164**：webhook 超时改 5xx 前，评估 GitLab 连续失败自动禁用 hook 的影响，保留 200 就写清原因。Anthropic 用量先确认预算和成本用哪个字段，只在 Anthropic 适配器或计费侧修，**不要**全局把 cache token 加进 `InputTokens`。

**审查里没开 Issue 的项**
- MCP HTTP JSON 响应无界读取（`internal/mcp/pool.go:480-488`、`tools.go:395-414`，SDK `streamable.go:2490`）；OAuth token POST 可能跟随 307/308 跨源（`internal/server/auth.go:80-83,389-403`、`cmd/server/main.go:227-236`）。都是条件性风险，未复现，确认后可合开一个安全加固 Issue。
- P3 级的若干条件性风险都未复现，不按缺陷处理，可在 #109 里记一笔。
- `docs/delivery-readiness.md:132` 说迁移是 1、3–19 共 18 个，`:181` 的升级步骤还写 1、3–18 共 17 个。下一个 docs 相关 PR 顺手改。

### 4.2 并行与先后

| 批次 | 可以并行 | 说明 |
|---|---|---|
| 第一批 | #157、#158、#160 | 文件不重叠 |
| 第二批 | #159、#162 | 不重叠 |
| 第三批 | #161（等 #158、#159 合并）、#163（等 #160 合并）、#164 | #158/#161 都改 `internal/review/`；#159/#161 都改 `review_state.go` |
| 之后 | UI 小项（第 4.4 节） | #144 要等 #163 合并 |

`ExtensionsManager.tsx` 被 #160、#163、#144 同时涉及，按 #160 → #163 → #144 串行。`web/dist` 有合并冲突时取 main 的版本后重建，不要手工合并。

### 4.3 复现测试存档（`docs/audit-repro-4839248/`）

审查时写的复现测试已存档入库。后缀加了 `.txt`，所以不会被 `go build`、`go vet`、staticcheck、Vitest 当成源码。文件名前缀是对应的 Issue 号。

- **含义**：`TestAudit*` / `TestAuditObserved*` 是“观察到错误行为就 PASS”，2026-10-30 在 `4839248` 上全部 PASS，说明缺陷还在。`TestAuditExpected*`（只在 `162-app_diff_limits_test.go.txt`）断言的是正确行为，现在 FAIL，可直接当 #162 的失败测试。
- **一次跑全部 Go 复现测试**（不改工作区，用 go overlay 映射进目标包）：
  ```bash
  python3 -I - "$PWD/docs/audit-repro-4839248" "$PWD" > /tmp/audit-overlay.json <<'PY'
  import json,sys
  A,R=sys.argv[1],sys.argv[2]
  m={"157-publisher_older_republish_test.go":"internal/app","158-161-review_prompt_truncation_and_verify_test.go":"internal/review","158-app_description_roundtrip_test.go":"internal/app","158-gitlab_description_test.go":"internal/gitlab","159-command_replay_test.go":"internal/app","161-retry_onboard_test.go":"internal/app","161-attempt_fence_store_test.go":"internal/store/sqlite","161-attempt_fence_worker_test.go":"internal/app","162-app_diff_limits_test.go":"internal/app","162-gitlab_rename_index_test.go":"internal/gitlab","163-repo_skill_wiring_test.go":"internal/app","164-anthropic_usage_app_test.go":"internal/app","164-anthropic_usage_review_test.go":"internal/review","164-check_config_session_key_test.go":"cmd/server"}
  print(json.dumps({"Replace":{f"{R}/{d}/zz_audit_{f.replace('-','_')}":f"{A}/{f}.txt" for f,d in m.items()}}))
  PY
  go test -count=1 -timeout 20m -overlay /tmp/audit-overlay.json -run '^TestAudit' -v ./internal/app ./internal/review ./internal/gitlab ./internal/store/sqlite ./cmd/server
  ```
  2026-10-30 的结果：20 个 PASS；4 个 FAIL，其中 3 个是 `TestAuditExpected*`（预期 FAIL），另一个 `TestAuditObservedRealGitRenameChain` 依赖审查机器上 `$HOME/rename` 的现成 git 夹具，新机器上一定失败。做 #162 时把它改成在 `t.TempDir()` 里现建 old/new 两棵树。
- **前端**：`160-163-extensions_audit.test.tsx.txt` 复制成 `web/tests/<名字>.test.tsx` 才能解析依赖（import 已改成相对 `web/tests/` 的路径），`cd web && npx vitest run tests/<名字>.test.tsx`，当时 4 个用例全部 PASS（缺陷仍在）。
- **转成正式回归测试时**：放进目标包，去掉 `Audit` 前缀起有意义的名字；Observed 类要**反转断言**为正确行为，确认修复前 FAIL、修复后 PASS；去掉只为观察的打印。存档本身不用改，#157–#164 全部关闭后可以整个目录删掉。

### 4.4 其余待办（审查之前就有）

| Issue | 内容 |
|---|---|
| #140 | Findings 表格：级别/标题/挑战链/人工标注列未上下对齐 |
| #141 | 执行图画布上下内边距增加 5% |
| #144 | 扩展页：平台级时隐藏「项目」下拉框（等 #163） |
| #145 | 项目概览页布局：信息卡在上、最近评审表格在下，表格不出现纵向滚动条 |
| #146 | 首页总览布局与文案：「需要处理」含义不清、统计卡片文字被截断（可与 #163 的总览空态一起看） |
| #147 | 工作区页：「重建索引」「重新生成上下文」收进手动操作折叠区，并说明自动触发机制 |
| #142 | 支持并文档化配置多个 LLM Provider（Docker 部署） |
| #138 | 开发 CI 迁移到 Azure Pipelines（见第 3 节，旧工作树的改动拿不到） |
| #123 | 自托管 runner 的 cgo 与 Docker 环境 |
| #109 | 路线图待决事项汇总，只记录，不排期 |

UI 小项每项都要重建并提交 `web/dist`，在 1440/390、亮/暗下检查。

### 4.5 总体路线

| 顺序 | 事项 |
|---|---|
| 0 | v1.2.0：维护者打 tag、发 Release（agent 不做）。 |
| 1 | P1：#157、#158、#159、#160 |
| 2 | P2：#161、#162、#163、#164 |
| 3 | 全部 P1 合并后，再做一次项目级复审，找同类问题：其他“先校验、后网络调用、再写入”的路径；其他整体截断后才做覆盖检查的地方 |
| 4 | UI 小项：#140、#141、#144、#145、#146、#147 |
| 5 | #142；#138、#123 |

## 5. 每个 Issue 的交付流程

1. `git switch main && git pull --ff-only`；`specgit issue <N> --branch fix/<N>-<短名>`（被旧 checkpoint 拦就用独立 worktree，见第 1 节）。
2. 把 `docs/audit-repro-4839248/` 里对应的测试转成正式回归测试（第 4.3 节），确认修复前 FAIL。
3. 实现修复；跑受影响包 `go test -race -timeout 40m ./internal/<pkg>/...`；改了前端就 `cd web && npm test && npm run build`，并提交 `web/dist`。
4. 提交带 `Issue: #N` trailer；用户可见的变化写进 CHANGELOG 的 `Unreleased`。
5. 同步最新 main，在 33.110 跑全量本地 CI，必须 `RESULT PASS`，覆盖率 ≥ 70%。
6. P1 修完后交给没看过实现的子 agent 独立复审（只给代码和 Issue，不给结论），没有新的 P1 才继续。
7. `specgit pr` → `specgit pr --ready`；PR 正文写修复、回归测试、CI run 目录和覆盖率，结尾加生成署名。`gh pr merge --squash --match-head-commit <已验证 head>`，回读 Issue 已关闭，`specgit pr --status` 记为已交付。

## 6. 本地 CI 与 33.110 环境

**33.110 CI runner**
- 位置 `/DATA/AppData/tribunal-ci`（目录里的 README.md 有说明）；镜像 `tribunal-ci:runner`，缓存卷 `tribunal-ci_go`、`tribunal-ci_gobuild`、`tribunal-ci_npm`。
- 用法：在仓库根目录 `bash /tmp/client.sh [rev]`。它把提交打成 git bundle 送过去（仓库私有，runner 不存 GitHub 凭据），推送的临时 ref `tribunal-ci-run` 退出时会删除，有残留就手动删。
- 覆盖 ci.yml 的 web、go（`-race`，覆盖率 ≥ 70%）和 docker 冒烟三个 job；结果在 `/DATA/AppData/tribunal-ci/runs/<时间-sha>/summary.txt`，末行 `RESULT PASS/FAIL`。
- 这是 GitHub Actions 受计费限制期间的过渡方案。Actions 恢复（或 #138 完成）后按 README 的拆除步骤删掉，只删自己的镜像、卷和目录。

**33.110 上的其他实例**
- `tribunal-regress`（`/DATA/AppData/tribunal-regress`，端口 18420）：镜像 `tribunal-regress:v1.2.0-a113e08`，配置是 `id:` 写法。沙箱项目 1334，hook 113。`.project-token` 2026-11-30 过期。适合在真实 GitLab 上验证 #157（commit status 最终显示）和 #164（webhook 重试）。
- `tribunal-acceptance-108`（18430）和 `tribunal-acceptance-108-session`：#108 验收已结束。不再需要时 `docker compose down`，按需删数据卷，并删掉 GitLab 上对应的 hook、token 和 OAuth 应用（项目 1335、1336）。
- `/opt/tribunal-test`（18410）没有动过。旧镜像（`audit-*`、`main`）和 `src-audit*.tgz` 可以清理。
- 部署注意：端口只绑 `127.0.0.1`；镜像 tag 固定为 `tribunal:local`，多个检出会互相覆盖；compose 没配日志轮转；**改了配置或 `.env` 后用 `docker compose up -d --force-recreate`**，只看新容器的启动日志。

**GitLab 沙箱**：[suntao/tribunal-sandbox](https://git.ycgame.com/suntao/tribunal-sandbox)（id 1334）。MR !1 有故意埋的 bug，MR !2 是干净改动，MR !3 是 #108 验收用的。project access token id 213（token 本身不在仓库里）。

## 7. 开发约定速记

完整规则见 AGENTS.md，这里只列最容易忘的：

- 一个目标对应一个 Issue 和一个 PR，每个提交带 `Issue: #N` trailer，squash 合并。
- `go test` 统一加 `-timeout 40m`；日志里禁止出现原始 `err`，用 `errclass`。
- 改 prompt 要 bump 版本注释并更新 `embeddedBundleHash`。迁移只能追加，没有 down 迁移。
- 默认扩展可禁用不可删除，升级时刷新，但不能覆盖管理员改过的绑定。
- 前端只用 CSS Modules 和 `web/src/styles/tokens.css`，用 `web/src/components/ui.tsx` 的共享组件；每次 web 改动都重建并提交 `web/dist`。
- 交接文档本身也走 Issue + PR 入库，不要再放在 `.git/info/exclude` 里的本地文件。
