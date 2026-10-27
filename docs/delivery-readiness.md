# 交付就绪、升级/回退与验收记录

> 本文分三部分：
>
> 1. [当前版本（v1.1.x）验证状态](#当前版本v11x验证状态)：当前版本实际做过哪些验证、还缺哪些。
> 2. 升级、授权、验收矩阵与回退计划：与版本无关的操作模板，从[首次升级前记录](#首次升级前记录模板)到[放行与归档](#放行与归档)。
> 3. [v1.0.0 历史验收记录（f143，仅供追溯）](#v100-历史验收记录f143仅供追溯)：v1.0.0 在隔离测试实例上的验收结果，**不能**用来证明 v1.1.x 的改动。
>
> 本文不复制完整日志，也不把测试实例结果扩大为生产验收。

<a id="v110-验证状态"></a>

## 当前版本（v1.1.x）验证状态

变更清单见 [CHANGELOG](../CHANGELOG.md)，发布页见 [v1.1.0 Release](https://github.com/LeXwDeX/Tribunal/releases/tag/v1.1.0)。本节只记录 v1.1.x 实际做过的验证。

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

**未在真实实例上验证**（上线前建议补做）

- [ ] 用 v1.0.0 的数据卷原地升级：迁移 13、14、15 已应用；密文迁移后已有项目还能 clone、评论和发布；OAuth 会话保留。
- [ ] 内置 codebase-memory-mcp 在真实评审中被模型实际调用，并且只能看到本项目的索引。
- [ ] 内置 `code-review` Skill 被评审块加载，评审输出符合新的报告门槛。
- [ ] 停机时有评审正在运行，重启后能续跑并完成发布。
- [ ] 用新的 Webhook 面板启用、重建、停用 GitLab Webhook，并确认 GitLab 端的 hook 与之一致。
- [ ] 在真实数据下浏览 v2 控制台的各个页面。

**v1.1.0 的已知限制**：webhook 处理时如果存储出错，返回 200 加 `ignored: storage_error`，GitLab 不会重试。main 已改为返回 503，失败投递可在 GitLab 中重发（[#99](https://github.com/LeXwDeX/Tribunal/issues/99)、[#100](https://github.com/LeXwDeX/Tribunal/pull/100)），随下一个版本发布，见 CHANGELOG 的 Unreleased 段。

### 迁移与回退边界

- 迁移实现：`internal/store/sqlite/sqlite.go` 的 `migrate()`；迁移文件为 `0001`、`0003`–`0015`，共 14 个（没有 `0002`）。确认迁移结果时按已知版本集合核对，不把最大版本号当数量。
- 每个未应用迁移单独开事务；没有 down migration。启动时只跳过 `schema_migrations` 已记录的版本；数据库中已记录、但不在当前嵌入文件列表的高版本会被忽略而不报错。
- `migration_rollback_test` 只覆盖 `0004` 单文件在“建表完成、记录版本前”失败时的事务回滚及之后重试；不覆盖整批升级失败、跨版本回滚或降级。
- `0007` 给 `users` 增加 `disabled`，并新建空的 `project_grants`；升级不会给任何用户创建 grant。
- `0008` 增加 `publication_error`；`0009` 增加 webhook 状态/事件/错误，并把已有 `hook_id` 的行标为 `unknown`；`0010` 增加 deferred review candidate 序号；`0011` 增加状态 note 与逐 note 表情的持久化表；`0012` 增加 webhook 归属和时间戳，已有 `hook_id` 的行视为本服务所有。
- v1.1.0 新增 `0013_admin_bindings`、`0014_extension_defaults`、`0015_secret_migration`；启动时还会把已存项目凭据改写为新的加密格式，v1.0.0 读不了新格式。升级步骤见 [从 v1.0.0 升级到 v1.1.0](operations.md#从-v100-升级到-v110)。
- 旧二进制可能打开已被新版本迁移的 schema，但它没有新权限逻辑，也读不了新格式密文，不能据此把“能启动”当作可安全回退。回退只能恢复升级前的数据备份。

## 首次升级前记录（模板）

不假设旧测试机仍保持历史磁盘状态；以下字段必须在目标机现场读取，未知时保持空白。

| 字段 | 实际值 |
|---|---|
| 执行时间 / 操作人 / 变更单 |  |
| 旧镜像 tag@digest |  |
| 新镜像 tag@digest |  |
| 实际 Compose 项目名 |  |
| 数据卷名称 / 容器内挂载点 |  |
| 配置文件与 secret 注入方式 |  |
| `data`、数据库、workspaces 大小 |  |
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
3. 停止旧服务后备份整个 `/app/data`，同时备份实际配置和 secret；不要把真实密钥写入仓库、本文或证据目录。
4. 保留旧镜像、旧卷和升级前快照，直到新实例验收完成；不要让两个实例共享同一 SQLite 卷。
5. 启动新实例会执行迁移、启动 worker、运行 recover/publish pending 和 cleanup，因此断网副本验证必须阻断对 GitLab、OAuth 和模型 provider 的出站访问，并使用占位配置；不得把真实库副本直接接到真实外部服务。
6. 恢复或离线验证使用新空卷，不把备份覆盖到旧数据目录，不混入旧 WAL。

## 升级操作（计划）

1. 完成前置记录和备份校验，确认旧镜像与备份均可恢复。
2. 隔离入口，停止旧服务，保存升级前全量快照。
3. 复制快照到隔离环境，启动新镜像；阻断外联，观察迁移、worker、recover 和 cleanup。确认迁移记录为 14 个已知版本（1、3–15），不把最大版本号当数量。
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

本地用户 ID 即 GitLab user id；项目 ID 使用 `projects all` 返回的数字 ID。只通过管理面授权，不直接写 SQL。以下是四个独立用法示例，不要顺序执行；按批准矩阵选择一次操作，示例 ID 不代表真实授权对象。

```bash
docker compose exec -T review /app/tribunalctl projects grants list 42
docker compose exec -T review /app/tribunalctl projects grants set 42 17 read
docker compose exec -T review /app/tribunalctl projects grants set 42 17 manage
docker compose exec -T review /app/tribunalctl projects grants delete 42 17
```

- `projects grants list <项目 ID>` 只请求单页，默认 50 条、上限 200 条，CLI 不会自动翻页；超过 50 条时用管理 API 的 `GET /api/v1/admin/projects/{id}/grants?cursor=...` 继续读取。
- 每次 `set` 后重新 `list` 核对，再让用户用自己的 session/API token 验证业务读取。
- 授权矩阵先由人工批准；禁止用直接 SQL 插入 `project_grants` 代替审批和审计。

## 真实验收矩阵

使用 2 个用户、2 个项目，建议记为 U1/U2、P1/P2。本矩阵是每次升级都要用的模板；v1.1.x 尚无真实实例结果。v1.0.0（f143）测试实例的结果见 [v1.0.0 历史验收矩阵结果](#v100-验收矩阵结果)。隔离外部调用会产生真实模型用量，需事先获得授权。「自动化覆盖」列是用 fake store、fake GitLab 和 fake provider 跑的本地回归测试（`internal/server`、`internal/app`），只证明代码路径的语义，不替代真实实例验收。

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
2. 新建空卷，恢复升级前全量数据，准备匹配的旧镜像、旧配置和原 masterkey；不覆盖旧卷，不混入新 WAL。**启动旧实例之前必须阻断 GitLab/OAuth/provider 出站访问**，并确认入站维护隔离生效。不能先启动再补防火墙规则，也不能把停用 webhook 当作阻止后台发布。
3. 不使用 down SQL，不删除 `schema_migrations` 行来绕过旧二进制检查，不只回退镜像而保留已迁移数据库。
4. 先在出站阻断下启动旧实例，按同样的隔离要求处理未知的旧版 worker/recover/publish 行为；不假设旧版提供“仅启动管理面”开关。保留不可变的原始备份，因为副本启动也可能改写任务状态。核对旧数据、镜像 digest、queued/running jobs 和 pending publications，并逐项比对 GitLab 已有外部副作用；核对完成且获得授权前，不解除出站限制。
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

> 本节是 v1.0.0 发布前在隔离测试实例上的验收记录，对应 [PR #30](https://github.com/LeXwDeX/Tribunal/pull/30) 和 [v1.0.0 Release](https://github.com/LeXwDeX/Tribunal/releases/tag/v1.0.0)。它不覆盖 v1.1.x 的任何改动；当前版本的状态见[当前版本（v1.1.x）验证状态](#当前版本v11x验证状态)。本节内容保持当时的事实，不随后续版本更新。

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
