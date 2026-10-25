# 详细设计

[ARCH.md](../../ARCH.md) 是设计基线，讲清楚「做什么、为什么」；本目录是实现层面的细化，讲清楚「怎么做」：接口签名、表结构、API、流程步骤和配置格式。

当前 M0–M6 主体功能已有本地实现，包括人工标注和质量指标。已验证运行时候选/源码快照 `f143e7a7835e1e99345b1df9b289bb499e3c67416598d30e6184a558374eb0ac`（402 文件）通过本地全量检查、实际镜像和连续两次无诊断代理 deep。较早的独立代码评审未发现 code blocker，但因缺少 f143 运行时 manifest 与 `12a62d1` 文档 manifest/source-equivalence 而记为 evidence BLOCKING；缺口关闭后，最终独立 review 对 `12a62d1` 文档树给出 code/documentation PASS、0 blockers。之后的文档澄清只关联 [PR #30](https://github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pull/30)，不构成新的 review；native CI、平台评审、合并和 Release 仍以 PR #30 回读为准。

已验证快照的隔离实例覆盖 OAuth/项目授权、CSRF、webhook 归属保护、GitLab Note Hook/状态 note/commit status/应用新增行行内评论、升级恢复、SSE 撤权关闭和 MCP 隔离。两次实际 deep 分别 0 call errors/0 failed nodes，MCP 成功/调用 10/10 与 10/11（第二次一次模型工具错误已纠正），两家 provider 均实际调用 MCP，结果均 BLOCKING/published。详见[项目 README](../../README.md#验证)和[交付就绪](../delivery-readiness.md)。实际启动方式和运行边界见 [部署与运维](../operations.md)。下文保留目标设计；实现与文档不同处标在各文档的「实现状态与偏差」中。

| 文档 | 内容 | 主要对应里程碑 |
|---|---|---|
| [01-modules.md](01-modules.md) | 目录到文件的划分、依赖方向、各包 Go 接口、通用约定、推荐依赖 | 全部 |
| [02-data-model.md](02-data-model.md) | 数据库 DDL、事件 payload、Postgres 差异、数据保留 | M0、M2 |
| [03-http-api.md](03-http-api.md) | webhook 处理规则、管理 API、MR 评论命令 | M0、M1、M6 |
| [04-dag-and-contracts.md](04-dag-and-contracts.md) | 节点种类、图模板、状态迁移表、调度算法、节点输出契约、裁决算法、评论渲染 | M2、M3、M4 |
| [05-flows.md](05-flows.md) | 评审、取消、命令、刷新、onboarding、崩溃恢复、发布幂等、反馈收集 | M1–M5 |
| [06-config.md](06-config.md) | 平台配置、MCP 配置、项目设置、仓库配置、环境变量 | M0 起 |
| [07-web-console.md](07-web-console.md) | 控制台技术选型、页面与所用 API | M6 |

## 实现状态与偏差（当前本地实现）

以下记录当前实现状态与已知偏差。文档正文保留目标契约，不因实现偏差删除。

- [01-modules.md](01-modules.md)：文件清单是设计目标。发布器实际在 `internal/app/publisher.go`；GitLab 发布与评论命令处理在 `internal/app` 层；`pkg/dag/validate.go` 的校验合并进 `pkg/dag/graph.go`。
- [02-data-model.md](02-data-model.md)：浏览器会话持久化在 `sessions`（cookie token 的 SHA-256，无新增迁移）；`context_docs` 保存追加版本，按 `(project, kind)` 维护唯一 active 版本；保留期定时清理已实现，删除整行而不只是清空 payload，`0005_retention_indexes.sql` 增加清理索引；`0006_finding_annotations.sql` 增加人工结论、操作者与时间，同 ID 替换保留标注；`0007_project_access.sql` 增加 `users.disabled` 和 `project_grants`，现有用户无隐式授权；`0008_publication_state.sql` 增加独立发布失败原因；`0009_project_webhook_state.sql` 增加 webhook 状态/事件/错误列；`0010_review_intent_seq.sql` 增加 deferred review candidate 的序号和屏障列；`0011_review_side_effects.sql` 增加持久化状态 note 与逐 note 表情；`0012_project_webhook_ownership.sql` 增加 webhook 归属和时间戳；`stats.feedback.by_provider` 仍只是原始信号归因。
- [03-http-api.md](03-http-api.md)：projects、api-tokens、mcp-servers、skills、block-bindings，以及管理面的 users/admin projects/grants 已使用游标分页；项目列表/详情补充 `access`、`workspace` 与 `last_review`/`recent_reviews`；评审详情补充 `access`、`mr_title`、`publication_state`/`publication_error` 和 `publications.commit_status`，SSE 同时传播发布状态；finding 标注 PUT 要求项目 `manage`，`quality_by_provider` 返回人工质量指标及分子/分母，`stats.feedback` 仍为原始计数；新增平台管理员用户停用、凭据撤销和 grant API；上下文文档保存追加 active 版本；webhook 启用、重建、停用接口按持久化 `hook_id` 判断归属，不按 URL 判断；`/review help` 和权限拒绝由 worker 回复 per-note 隐藏 marker，重试幂等；MCP/Skill/绑定的宿主配置写入要求平台管理员，项目 scope 还需 `manage`；SSE 每 2 秒轮询。
- [04-dag-and-contracts.md](04-dag-and-contracts.md)：I4 使用 `decision.finding_id` + `reason`；token 预算为 workflow 共享上限，逐请求检查并累计失败重试的已知 partial usage，并发/在途请求允许有界超支；token 预算耗尽后，标记为 `TokenFree` 的节点仍运行，其余未完成 required 节点失败、optional 节点跳过；gather 受 `max_nodes` 约束时按严重级别优先选择挑战目标，不会自动降级 plan；共识票和 cross challenger 按 endpoint + model 身份归一；无法解析身份的来源共用一个 unknown bucket，known 与 unknown 可各计一票、unknown 之间不增票，metadata 缺失时不能声称跨模型独立；synthesize 的预算/传输失败使用确定性规则兜底并保留 partial usage，不重试；上游证据缺口/覆盖缺口把最终 verdict 覆盖为 INCOMPLETE；CodeRef 在固定 head/base 快照校验；extend 契约增加 `new_finding_index`。
- [05-flows.md](05-flows.md)：全部 ignore 发布「无需评审」且不建 workflow，全部采集 omitted 也不建 workflow，只发布 INCOMPLETE 摘要（omitted 总数、原因计数和最多 4 条路径，其余只计数；正常 dossier 保留完整列表）；部分 omitted、提示词截断或完成 plan 后未分配的非 generated included 文件由 unresolved 传播为 INCOMPLETE；发布状态独立为 pending/published/skipped/failed，启动恢复 pending，取消/stale/关闭 failure note 记 skipped，失败可手动 republish；F3 的 help 和权限拒绝由 worker 回复，使用 per-note 隐藏 marker 幂等；受理 run/cancel 时入队事务记录源 note eyes，终态由状态 worker 协调，webhook 无网络且不添加 emoji；F8 仍从最近 14 天创建的已完成 workflow 中筛选，合并 MR 只采集 3 天；讨论只采集发布者首条 note，old-side、缺失行和取证失败不写 signal；按持久化 FindingID 精确匹配后归因 provider，summary/unlinked 不计入，同一 signal 可计入两个角色；原始信号不自动生成标注，人工指标独立计算。run cleanup 先查询保护集合，再清理超过年龄的 `runs/<id>` 和 `context/<sha>`；run 保护覆盖未终态 workflow、queued/running job、`publication_state=pending` dossier 和本进程注册 run，context 保护覆盖所有 DB revision base、当前 workspace base/context、未终态和待发布 workflow base，locked/非法 metadata fail closed；缺失 mirror 时只有满足年龄、未保护、未注册和路径检查的 run 才删除。
- [06-config.md](06-config.md)：`bootstrap_admins` 只决定平台 admin 标签，项目数据仍需 grant；`REVIEW_ADMIN_TOKEN` 仅平台管理面并由部署方轮换；仓库配置只在 worker 按目标分支合并；`allow_repo_config=true` 时 Draft 由 worker 最终判断，标签只追加排除，目标分支不可覆盖，仓库 `profile` 优先于隐式选择；`language` 单行最多 128 字符并放入 scoped 项目背景。示例配置缺 `providers` 和 `propose`，不能直接跑真实评审。MCP 示例改用 `scripts/codebase-memory-mcp.sh` 固定每个项目的 cache/runtime 目录；隔离仍需部署方安装上游二进制并重新验证，wrapper 不是文件系统沙箱。真实 MCP stdio 已测；每个 HTTP backend 的完整覆盖不在本次 v1 范围。
- [07-web-console.md](07-web-console.md)：项目写按钮由 API 返回的 `access=manage` 控制，平台 admin 没有 grant 时也不能写；普通用户自助管理个人 API token，用户停用、凭据撤销和项目授权仅平台管理员可见；评审详情显示独立发布状态/原因、INCOMPLETE 和 BLOCKING 的 commit status 区别，发布失败可重试，skipped 不重发；`workspace.index_status` 描述已存储索引检查点，不证明实时健康；项目设置的写接口已实现；统计页提供总体/项目/provider/块的运行统计和 UTC 日趋势，项目按工作流、provider/块按已启动节点计数；finding 展开行提供人工标注，质量面板显示提出方准确率、挑战方误杀率及样本数，原始反馈独立显示。`stats.runs` 包含评审与 onboarding，`blocking_ratio` 仍以全部 workflow 为分母。已验证快照的 `web/dist` 未变；f143 隔离实例在 1440 管理设置页和 390 viewer 设置页完成实际截图与 DOM 读取（浏览器传输于 2026-10-25T05:14Z 恢复）。390 视口为 390×844、scrollWidth 390，无横向溢出；viewer 页仅显示只读说明，无管理按钮。此前的截图工具超时和传输挂起是临时工具故障，不是产品失败；这不等同物理设备或完整 WCAG 验收。

OpenAI Responses adapter 在每次 Generate 时通过 SDK option 发送请求级随机不透明 `X-Session-Id`。这是网关兼容措施，不是 OpenAI 标准、会话认证或幂等保证；仅适用于 Responses 协议，不新增配置、依赖或 400 重试。已部署网关内部实现未证实，可能增加缓存/握手成本。

## 建议的实现顺序

以下是历史建议顺序；主体实现已落地，未完成项见上，实际文件位置以「实现状态与偏差」为准。

1. `internal/config`、`internal/store`（迁移 0001）、`internal/secret` —— 能启动、能连库。
2. `internal/server/webhook.go` + `internal/jobs` —— webhook 能入队、能去重。
3. `internal/gitlab`（含 `DiffIndex` 与单元测试）、`internal/repo` —— 能拿到 diff 和代码。
4. `internal/llm` 的一个 adapter + `internal/agent` 的工具循环 —— 能跑通一次 `code-review`。
5. `internal/review/render.go` + `gitlab/publish.go` —— 完成 M1 闭环。
6. `pkg/dag`（先写迁移表和测试，再写引擎）—— M2。
7. 其余块、裁决、对抗核验、工作区、扩展、控制台。

## 给 AI 编码助手的说明

- 以本目录为准生成代码；与 ARCH.md 冲突时以 ARCH.md 为准，并回头修正本目录。
- 每个包先写接口和测试，再写实现；`pkg/dag` 的迁移表和级联规则必须有表驱动测试。
- 不要引入本目录「推荐依赖」以外的库；需要时先补充到 01-modules.md §12 并检查许可证。
