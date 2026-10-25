# 本地交付边界与证据索引

本轮交付是准备材料，不是提交、PR、合并或部署。未修改应用源码，未连接目标环境。实际升级与验收使用 [交付就绪计划](delivery-readiness.md)。

## 三个基准

| 基准 | 标识 | 用途 |
|---|---|---|
| Git HEAD | `352a6dfc41c8489d417f0b50298c944fd5d6263b` | 已有提交；不代表当前完整 MVP |
| 修复前工作树 | 350 文件，SHA256 `4d57290baabccfe9157a1867667e72e7434fe2d7229ec8d4ca59f03922cd7917` | 区分此前 MVP 工作与 #16–#27 修复；已逐文件复核 |
| 最终代码复审差异 | SHA256 `4fc401142b75cdccb87b0a5e032dee4ff57cd1ae86ff21d53965fa31c7136b0f` | 相对修复前基线的 126 个路径，两个领域独立复审 PASS |

最后一次代码复审后有 5 份文档澄清，并已单独复审 PASS。归档的 `current/` 是归档时磁盘快照，包含这些澄清及部分本轮交付准备；不能把其哈希冒充为较早的代码复审哈希。修复前基线也不证明与远程旧镜像一致。

## 路径边界

以 HEAD、修复前基线和固定复审路径表三方比较，归档时得到：

| 分类 | 路径数 | 含义 |
|---|---:|---|
| `preexisting-only` | 126 | 此前 MVP 已变化，本轮代码复审未修改 |
| `mixed` | 75 | 此前 MVP 与本轮修复均修改，不能整文件按单一归属暂存 |
| `repair-only` | 51 | 固定复审差异中存在，此前相对 HEAD 未变化 |
| `unchanged` | 131 | 不在上述两个改动集合内 |
| `preparation-only` | 1 | 归档时已生成的 `docs/delivery-readiness.md` |

原有 MVP 为 126 + 75 = **201** 个路径；本轮修复为 75 + 51 = **126** 个路径。路径数包含删除和生成产物，不等于现存文件数。完整列表和每条 HEAD/baseline/current 哈希在归档的 `delivery-inventory.json`。

`.gitignore` 和 `docs/operations.md` 原已属于前两组，本轮又分别加了归档忽略规则，以及导航和回退出站隔离要求，机器清单用 `preparation_touched` 标识。本文和 `docs/simulation-report.md` 在快照之后编写；上表计数是快照时状态，后续文档不回写不可变的 `current/` 快照。

## 建议的后续交付单元

这是依赖顺序，不是已创建的提交或 PR。不能使用 `git add .`，也不要只凭文件名把混合文件整体归到一个 Issue。

| 顺序 | 单元 | 主要范围与依赖 |
|---|---|---|
| A | 既有 MVP 基础 | 原有 #2–#8、#10、#11、#14 的工作区/上下文/反馈/保留期、部署包装、CI/许可证及控制台；迁移 `0004`–`0006` 等 |
| B | 本轮修复集成 | #16–#27；依赖 A 中尚未提交的 API、队列、发布、用户/session 和界面基础 |
| C | 交付准备 | 本文、就绪计划、本机模拟报告、运维导航/回退隔离要求和 `.gitignore` 的本地归档排除规则 |

本轮 B 的相关领域如下。它们用于审查分组，不保证可各自独立编译：

- #16/#18/#19/#23：`internal/app` 执行/恢复/发布和 SQLite 代次映射。
- #17/#22/#25/#26/#27：`internal/review`、`internal/agent`、`internal/llm`、`pkg/dag` 及 app 集成。
- #20/#21：用户和项目授权存储、迁移 `0007`、server/CLI/UI。
- #24：迁移 `0008`、发布存储与恢复、HTTP/SSE、总览及详情。
- 文档和 `web/dist`：跟随实际合并后的源代码重新生成或核对，不独立选用旧 bundle。

未来获得 Git 写入授权后，应在隔离工作树重建 A，再叠加 B，逐个候选单元执行相应检查。当前全量通过只证明整合后的工作树，**不证明 A 或 B 的任意拆分点通过**。固定 `final-review.diff` 含本机路径，归档用于审查与恢复参考，不是可直接对 HEAD 执行的移植补丁。

## 持久证据归档

仓库内本地目录：`artifacts/delivery/2026-10-24-review-repairs/`。根 `.gitignore` 已排除 `/artifacts/delivery/`；目录权限 0700、归档文件 0600。它不在待提交文件中，但仍只是本机副本，不是异机备份或不可篡改存储。

| 内容 | 用途 |
|---|---|
| `baseline/`、`evidence/baseline-manifest.json` | 保留全部 350 个修复前文件及其原始哈希 |
| `current/`、`delivery-inventory.json` | 归档时磁盘快照与三方路径归属；不含 `.git`、真实 `.env`、数据库或运行工作区 |
| `evidence/final-review.diff`、`final-review.json` | 原有代码复审绑定的精确差异与标识 |
| `evidence/final-*.log`、`.exit`、`final-go.cover` | 独立全量验证日志、退出码和覆盖率 |
| `evidence/07-queue-repeat.*` | 队列相关 28 个测试重复 race 20 次的记录 |
| `evidence/browser-verification.md`、3 张 PNG | 1440/390 视口及权限/发布状态检查；API/EventSource 是 fixture |
| `evidence/issue-*-evidence.md`、`issue-evidence-receipts.json` | 12 个 Issue 的证据评论与读回确认链接 |
| `manifest.json`、`credential-scan.json` | 初始 790 个归档文件的 SHA256/大小及有限敏感信息检查 |
| `review-summary.md`、`supplement-manifest.json` | 根据子 agent 最终报告整理的复审摘要及后续材料校验表 |
| `checks/simulation/`、`docs/simulation-report.md` | 本机 HTTP 隔离 11 项和旧库升级/回滚演示 6 项；合成数据，不是真实环境验收 |

敏感信息检查覆盖常见 token 前缀、URL 凭据、私钥标记与路径排除。24 处命中已核对为测试假值；截图为模拟账号。该检查不能证明不存在所有类型的秘密。归档不应未经再次审查上传到公网，真实备份和密钥应放在部署方的安全存储中。

## 已有验证与未做事项

独立检查：Go build/vet/全量 race 通过，覆盖率 **77.36%**；许可证 65 模块通过；web **155/155** 测试、覆盖率门槛与 build 通过。两组代码复审和文档增量复审均 PASS，无阻塞项。

统一远端证据入口：[Issue #20 验证评论](https://github.com/LeXwDeX/Code-Review-For-Gitlab-CE/issues/20#issuecomment-6369328377)。#16–#27 各自保留独立回归和限制说明，全部仍 OPEN。本文不替代 Issue 工作账本。

本轮只重验归档完整性、相关迁移测试及交付文档，不重复无改动的 Go/Web 全量测试。已完成本机隔离模拟（不同项目隔离、旧库升级、回滚风险演示），但**没有**远端 CI、新镜像、真实 OAuth/GitLab/provider 验收或目标环境升级/回退演练；这些仍是下一阶段的明确门槛。
