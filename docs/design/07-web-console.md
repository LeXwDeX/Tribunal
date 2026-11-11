# 07 Web 控制台

> 平台内部使用的管理控制台。页面原型见设计画布「GitLab CE 评审平台原型」；本文定义页面、每页用到的 API 和前端技术选型。

> 实现状态：控制台已实现，构建产物由 Go 服务 embed。页面行为、字段和 API 见下文各节与 [HTTP API](03-http-api.md)。
>
> 以下字段/页面存在降级，界面用占位符显示：
> - 工作区 `index_status` 按已存储 SHA 显示 `ready` / `stale` / `unknown`；这是检查点状态，不是实时索引健康证明，历史 SHA 的限制见 [HTTP API](03-http-api.md)。
> - 统计页、总览和项目详情将 `stats.runs` 统一标为「运行次数」，注明含评审与 onboarding。`blocking_ratio` 的分母仍是全部 workflow，onboarding 会稀释该比例；不能解释为仅评审的阻断比例。
>
> 项目设置写接口（`PATCH /projects/{id}`、`PUT .../token`、`POST .../webhook`）已实现，平台配置视图仍为只读。

---

## 1. 技术选型（建议默认）

| 项 | 选择 | 理由 |
|---|---|---|
| 形态 | SPA，构建产物用 `embed.FS` 打进 Go 二进制 | 保持单二进制部署 |
| 框架 | Vite + React + TypeScript | 生态成熟，GPT 生成代码质量稳定 |
| 数据 | TanStack Query（请求缓存与轮询）、原生 `EventSource`（SSE） | 评审详情页需要实时刷新 |
| 路由 | TanStack Router 或 React Router | 二选一即可 |
| 样式 | CSS Modules + CSS 变量（亮色 / 暗色两套 token） | 依赖少，不绑定组件库 |
| DAG 视图 | 自己按拓扑分层布局（列 = 层，行 = 节点），SVG 画连线 | 节点数 ≤ 30，不需要图布局库 |
| 代码与 diff 展示 | `<pre>` + 行号；后续可换 Shiki | 首版够用 |
| API 类型 | 从 Go 结构体生成 OpenAPI，再生成 TS 类型（可选） | 前后端字段一致 |

目录：

```
web/
  index.html
  src/
    main.tsx  app.tsx  api/  pages/  components/  styles/
  vite.config.ts
```

开发时 Vite dev server 代理 `/api` 和 `/auth` 到 Go 服务；生产构建输出到 `web/dist`，由 `internal/server/web.go` embed。

---

## 2. 导航结构

```
总览
评审
项目
  └ 项目详情（概览 / 工作区与上下文 / 设置 / 扩展绑定）
扩展（MCP 服务器 / Skill / 块绑定）
模型与档位
统计与反馈
系统（API Token / 平台配置只读视图 / 用户与账号 / 项目授权）
```

`admin`/`viewer` 是平台标签，项目业务写操作由 API 返回的 `access` 控制：只有 `manage` 才显示按钮；平台管理员对全部已登记项目隐式拥有 `manage`，项目列表的来源列显示「管理员」。平台配置、用户与账号、项目授权标签只对平台管理员显示；模型与档位页面导航仍可见，但 provider/profile 数据只允许平台管理员读取，非管理员会看到权限错误。后端仍逐请求校验。已登录用户都能注册项目，但普通用户的访问 Key 必须能识别出该项目至少 Reporter 的成员，平台管理员免成员检查；已登录用户也都能在“系统 → API Token”自助管理自己的 token。

---

## 3. 页面

### 3.1 总览

| 区块 | 内容 | API |
|---|---|---|
| 运行中 | 正在运行的评审：项目、MR、当前阶段、已用时 | `GET /reviews?status=running` + SSE |
| 近 7 天 | 运行次数（含评审与 onboarding）、BLOCKING 比例、发布条目数、规则丢弃比例、平均/P95 耗时 | `GET /stats` |
| 需要处理 | 工作区异常、token 30 天内过期、发布失败，或带 `publication_error` 的 pending 重试；普通 pending 不告警；provider 连续错误只对平台管理员加载 | `GET /projects`、`GET /providers`、`GET /reviews?publication_state=failed`、`GET /reviews?publication_state=pending` |
| 最近评审 | 最近 20 条 | `GET /reviews?limit=20` |

### 3.2 评审列表

过滤：项目、状态、结论、档位、触发方式、时间。列：MR、项目、档位、状态、结论、P0/P1/P2 数、用时、时间。

未创建评审任务的结果（API 中 `kind: "status_only"`，如 MR diff 为空）也出现在此列表以及概览、项目页的最近评审表中：该行不可点击，显示 MR、项目、短 head SHA、原因行（如「原因：没有可评审的变更（empty diff）」）、中性徽标「未创建评审任务」和结论徽标。这类行没有评审详情页。

### 3.3 评审详情（核心页面）

| 区块 | 内容 | API |
|---|---|---|
| 头部 | MR 标题与链接、head sha、档位、触发方式、状态、结论、项目 `access`；操作按 `manage` 显示取消、重新发布、在 GitLab 中打开 | `GET /reviews/{id}` |
| DAG 视图 | 按层排列节点，颜色表示状态，点击打开节点抽屉 | `GET /reviews/{id}/graph` + SSE |
| Findings | 表格：级别、标题、文件:行、来源（节点 / provider）、挑战结果、处置、处置理由；可按处置和级别过滤；展开看正文、证据和人工标注 | `GET /reviews/{id}/findings`、`PUT .../findings/{finding_id}/annotation` |
| 发布 | `publication_state`、脱敏的 `publication_error`、汇总评论链接、行内讨论数量、commit status；failed 可重试，pending 等待发布完成（启动恢复或手动重试），skipped 显示原因且不重发 | 详情中的 `publication_state`、`publication_error`、`publications` |
| 节点抽屉 | 参数、输出 JSON、错误、每轮 LLM 调用（用量、耗时、stop reason，可展开请求和响应） | `GET /reviews/{id}/nodes/{node}`、`.../calls` |

`publications.commit_status` 仅含 `success`/`failed`；旧发布键没有状态时为 unknown，界面显示不可用。

人工标注显示已保存结论、操作者和时间。项目 `manage` 选择有效、无效、未确定或未标注后点击「保存标注」，`read` 只读；保存中禁用操作，失败保留旧结论并提供重试。成功后刷新当前评审的 finding 缓存和统计缓存，保留筛选。旧响应没有标注字段时显示未标注。被丢弃条目也可标注。标注不改变发布结果或评审结论，不提供历史记录或并发冲突提示。

### 3.4 项目列表与注册

列表列：项目路径、工作区状态、最近评审、token 过期时间、启用开关（评审档位只有 `default`，列表不再显示档位列）。平台管理员看到全部已登记项目，来源列显示「管理员」；其他用户只看到自己有 grant 或 `global_read` 可见的项目。已登录用户都可看到“注册项目”入口，普通用户填写的访问 Key 需能识别出该项目至少 Reporter 的成员，平台管理员免此检查；注册成功后注册人自动获得该项目 `manage`。

注册分两步：输入仓库完整网址、路径或 ID 和访问 Key，先执行只读「校验连接」，展示项目、身份、角色、scope、有效期和 Git HEAD，并明确「校验通过，尚未保存」。校验按 token 所有者在项目中的成员角色判定：普通用户需至少 Reporter，否则提示权限不足；平台管理员跳过成员检查，但仍需 token 有效、scope 正确且 HEAD 可读。修改地址或 Key 后校验失效，保存按钮再次禁用；校验通过前不能保存。

「保存并初始化」才登记项目、加密保存 Key 并入队后台初始化，会调用模型并消耗 token；注册人自动成为该项目的 `manage` 持有者。高级设置保留分支、跳过 Draft 和自动 webhook，没有档位选择（项目固定使用 `default`）；自动 webhook 默认关闭。Key 仅保存在对话框局部状态，不写入 URL、查询缓存或浏览器持久存储；关闭对话框会清除 Key 和校验结果。

### 3.5 项目详情

| 标签 | 内容 | API |
|---|---|---|
| 概览 | 基本信息、最近评审、本项目统计 | `GET /projects/{id}`、`GET /stats?project_id=` |
| 工作区与上下文 | 工作区状态、base sha、索引状态、最近刷新；操作：刷新、重建索引、重新生成上下文；上下文文档查看与编辑、锁定开关、历史版本 | `/workspace`、`/context-docs` |
| 设置 | 项目 `manage` 可编辑项目设置（没有档位选择）、更新访问 Key、重建 webhook；`read` 只读。`POST /projects/validate` 已认证用户可调用，普通调用方的 token 需能识别出该项目至少 Reporter 的成员（平台管理员免检）；项目 `manage`（平台管理员隐式具备）保存 Key 时直接 `PUT .../token`，由后端校验凭据 | `PATCH /projects/{id}`、`POST /projects/validate`、`PUT .../token` |
| 扩展绑定 | 项目 `read` 可查看项目级 MCP、Skill 和块绑定；写入宿主执行配置仍仅平台管理员，项目 scope 不再要求逐项目 `manage` grant（管理员隐式具备） | `/mcp-servers`、`/skills`、`/block-bindings` |

平台管理员更新访问 Key 时可先做只读校验，返回的项目 ID 必须与当前项目一致；项目 `manage` 不要求调用 validate 端点，直接保存时由服务端校验凭据。该操作只更新平台保存的凭据，不在 GitLab 创建或撤销 token，也不自动初始化或触发评审。

### 3.6 扩展

- **MCP 服务器**：列表（作用域、名称、命令、状态、工具数）；已有自定义服务器的编辑、删除、启停；「测试」列出工具。平台管理员可新增或粘贴 Claude Code 配置导入。
    - 新增支持 stdio（command、args、env）和 http（url、headers），可设置创建后启用或从单个服务器 JSON 填入表单；测试为草稿测试，不写库。
    - 导入粘贴 `mcpServers` 文档，可逐项或全部测试；任一项校验失败时整批不写入，错误只显示字段路径与固定类别，不回显配置值。
    - 测试结果显示连接状态、延迟、工具总数与截断状态，以及每个工具的名称、描述、只读/破坏性/可绑定注解；失败按固定类别显示并提供重试。
    - 测试发现的工具以 `mcp__<server>__<tool>` 进入块绑定矩阵；`bindable=false` 的列不可新增授权，已绑定工具仍可取消。停用服务器的测试结果只展示，不进入矩阵；会话内结果不持久化，切换 scope 后清空。
- **Skill**：列表（作用域、名称、描述、来源目录）；已有自定义 Skill 的启停和删除。平台管理员可上传 zip 或登记服务端目录。zip 必须包含 `SKILL.md`，服务端限制归档大小、解压总量和条目数。目录登记要求服务端绝对路径，读取服务端文件系统路径并复制到受管目录，源目录不会被修改或删除；这是高级选项，推荐 zip。成功后列表刷新；同名或非法 manifest 的 400/409 错误保持对话框可重试。内置 Skill 始终启用，不提供替换或删除入口。
- **块绑定**：矩阵视图，行是块（explore / code-review / review / reasoner / validate / verify / synthesize / onboarding），列是可用 MCP 工具和 Skill，勾选即绑定。项目 scope 读取需要该项目 grant 或 `global_read`；写入仍要求平台管理员。矩阵工具来自已保存绑定、当前会话测试结果和 API 返回的 `server.tools`；`bindable=false` 阻止新增授权，已绑定工具仍可取消。

后端立即生效：MCP、Skill 和块绑定保存成功后，下一次评审在本进程内使用新配置；平台 scope 影响全部项目，项目 scope 只影响当前项目；恢复默认的 `restart_required` 为 `false` 时不显示重启提示。多实例部署的其他进程不会收到失效通知。配置文件中的扩展声明仍需重启，控制台不提供文件编辑。

### 3.7 模型与档位

- 用户管理：用户列表显示角色来源、全局模式、项目授权数；管理抽屉可提升/降级管理员、开关全局模式、增删用户维度项目授权。新增项目授权默认 `manage`（可读、可写、可删除），也可改为 `read` 只读共享；平台管理员的项目数据访问不依赖这些授权。配置来源与自我降级不可操作。全局用户在页头显示标识，项目列表标记“只读（全局）”，评审页可切换“全部评审/我的项目”。
- 模型列表：显示名、来源标签（配置/用户）、协议、上游 model、base_url host、参数摘要、API key 状态、可用性与原因、被引用节点、近 24 小时调用数/错误率/P50 延迟；同名模型显示短 ID。参数摘要对显式 0/false 给出语义标签；当字段来源为全局默认时显示实际生效值，不再显示未生效的零值标签。配置模型卡片显示逐字段来源徽标、YAML 字段路径和重启生效说明，平台管理员可通过「编辑配置」抽屉写回 overlay（见下）。非平台管理员看到只读提示，不显示编辑入口或管理员故障文案。
- 「全局默认参数」面板：位于「模型」区块之前，只对平台管理员渲染，模型或档位 Drawer 打开时隐藏；隐藏期间未保存的草稿提升到页面状态保留。面板显示 `revision`，按参数分组编辑；「保存全局默认」整体 PUT 当前表单值与 revision；「重置」经确认后按当前 revision DELETE，尚无默认参数或 `revision=0` 时禁用。非指针字段的 0、false 或留空不提交；`temperature` 填 0 会覆盖默认；`reasoning.effort: none` 要求同时勾选 `allow_reasoning_effort_none`。400 的 `details[].field` 显示在对应控件下方，跨模型不兼容项显示在表单级错误中；409 `stale_revision` 显示提示与「重新加载」。面板说明明确只对 `user:` 模型生效，`config:` 模型由配置文件负责，并提示保存时会对现有 `user:` 模型做协议交叉校验。用户模型卡片在参数下方列出「全局默认」继承项。
- 「评审档位」区块顶部是「全局默认」卡片（`default` slot），显示生效模型（显示名与短 ID）、来源、配置值和生效参数（`effective_params`）及参数来源；下方 6 个可配置节点（explore、四路 lanes、challenge）逐项显示生效模型、来源标签（覆盖/全局默认/配置/配置默认/派生，以 API 返回的 `source` 为准）和生效参数/参数来源。多个节点的 `vote_identity` 相同时提示「这些节点会被计为同一模型」并列出节点（含全局默认）。
- 管理员在 Drawer 中编辑两层：顶部「全局默认」卡片可设置模型与 `max_tokens`、`temperature`、`reasoning.effort`（含 `max`）、`thinking.budget_tokens`、`thinking.type`；下方 6 个节点用相同控件逐项覆盖。每个输入下方显示继承值与来源：未填写时显示「继承值 … · 来源 模型/全局默认」，已填写时显示「覆盖中」及清除后的继承值；修改全局默认会立即更新节点下方的继承提示。`thinking` 覆盖按对象整体生效：本层只要填写任一子字段，未填写的子字段不继承，界面提示整体覆盖；只有本层没有 thinking 覆盖时才显示继承的整个 thinking 对象。不能解析模型目录时继承值显示「未知」，不把已清除的旧值或节点自身的覆盖当成继承值。不可用模型禁用并显示原因。
- 保存时只重建并提交实际填写了值的覆盖项（整体 PUT overrides 文档的协议不变），没有填写任何值的节点不会出现在文档中，已保存的覆盖会原样回填并保留；「清除<节点>覆盖」或「清除全局默认覆盖」清空表单后保存即恢复继承；「恢复为配置」调用 DELETE 清除全部覆盖。保存和恢复都带当前 revision：400 的 `details[].field` 路径显示在对应输入旁，错误与 409 刷新提示显示在全部字段集下方（409 提供重新加载）。非管理员只读，不渲染写控件。
- 管理员可通过 Drawer 新增或编辑用户模型；表单按连接、采样、推理和限流分组。API key 三态（保持不变/替换/清除）只存在抽屉局部状态，关闭即清空；保存前测试调用草稿测试端点，编辑且 key 不变时不发送 key。预设入口只在新建模型时显示，从 `GET /api/v1/model-presets` 拉取，点击只填入本地表单，不立即写库；404 视为旧服务端并静默隐藏，非 404 失败显示可重试提示。`max_tokens` 留空表示模型默认（DeepSeek 默认 8K/64K/128K，不是最大输出），anthropic 留空时前端直接报错；`max_concurrency` 新建时默认留空，填 0 显示不限制本地并发的警告。anthropic 的 `thinking.budget_tokens` 为 0 或留空表示不设置，启用时必须不小于 1024 且小于已填的 `max_tokens`。删除被引用模型显示 409 引用位置；revision 冲突提示重新加载；出站被拒显示专门说明。配置模型仍只读，模型级参数改用「编辑配置模型」抽屉写回 overlay。本页只允许平台管理员读取，其他用户看到权限错误，写入口隐藏。
- 「编辑配置模型」抽屉：平台管理员在 `config:` 模型卡片点「编辑配置」打开，按连接、采样、推理与能力、限流与窗口分组；每个字段显示来源徽标（内置默认/基础配置/overlay）。`base_url` 可为空，表示协议适配器默认端点；非空时要求绝对 http(s) URL，且不含凭据、查询或片段。`reasoning.effort: none` 要求同时开启 `allow_reasoning_effort_none`。`max_concurrency` 与 `rpm` 的显式 0 显示为 0，`max_tokens` 与 `context_window` 的 0 显示为空。保存只提交实际变化的 `providers.<名称>.*` 叶子并带 `base_revision`、`overlay_revision`；文件变化后按写响应轮询 `GET /api/v1/config` 直到快照追上，期间禁用重复编辑。无变化时不写文件。抽屉打开期间 revision 变化会重建表单并提示，避免静默丢弃草稿。「重置为 base」经确认后删除整个 `providers.<名称>` 覆盖；仍被 `profiles`（含 `profiles.default`）或 0028 节点覆盖引用时返回 409 `provider_in_use`，列出引用者，不修改数据库或档位。卡片与抽屉列出 0028 节点覆盖；「在评审档位中清除」打开既有档位抽屉。API key 只显示是否配置（只读），不回显凭据。仅平台管理员渲染写入口，服务端同样拒绝非管理员。overlay 不热替换，重启后未终态评审可能以 `profile_hash_changed` 取消。
- 评审档位：只显示 `default`，标题为「评审档位」，按固定的八步拓扑列出各步骤及其 provider：探索（`explore`）、提出（`code-review`、`review`、`reasoner`、`validate`）、挑战（`verify`）、裁决（`adjudicate`）、汇报（`synthesize`），并展示预算。`onboarding` 单独标为「项目接入」，不是评审档位。
- 手动触发评审的对话框也没有档位选择；评审详情的 DAG 阶段轨道中，`explore` 和 `plan` 归入「规划」，`code-review`、`review`、`reasoner` 和 `validate` 归入「提出」。「模型与档位」页的「项目接入」一行在 onboarding 没有配置 provider 时显示「未配置」。

### 3.8 统计与反馈

- 当前页面展示总体及按项目、provider、块的运行数、平均/P95 耗时、有效耗时样本数与 UTC 日趋势。项目统计工作流；provider/块统计已启动节点，每个当前节点行只计一次，不按 LLM 调用轮次或重试次数累加，无 provider 的代码节点只进入块统计。
- 趋势用可横向滚动的日期表格；每个项目/provider/块可展开每日明细。筛选和日桶按 workflow 创建时间，日桶用 UTC；只返回有记录的日，不补零。节点指标仅表示最新一次尝试，不代表完整重试历史。
- 运行次数含评审和 onboarding；`overturned_ratio` 标为「规则丢弃比例」，是规则裁决 `drop` 的占比，不是「被挑战推翻」或模型准确率。
- 旧响应缺字段显示不可用，已知空数组显示空状态，非法趋势不显示部分合计。
- 反馈原始计数、provider/角色归因、人工质量指标的定义与样本口径见 [HTTP API](03-http-api.md#45-系统)；界面只展示接口返回值，不显示部分或伪造的百分比。

### 3.9 系统

- 标签：API Token、平台配置（只读）、用户与账号、项目授权；平台配置、用户与账号、项目授权只对平台管理员显示。
- 平台配置（只读）标签顶部有「配置来源与待重启」面板，约每 5 秒轮询 `GET /api/v1/config/status` 与 `GET /api/v1/config`，显示 applied / 待重启徽标、base 与 overlay 的短 revision（overlay 缺失时显示「未挂载」）、`last_error`（`role="alert"`）和 `warnings`（`role="status"`）。
- 面板以表格列出文件生效的标量配置项：路径、值、来源徽标（内置默认/基础配置/overlay）。数组和映射只通过标量叶子显示；本页不提供编辑。配置模型的编辑入口在「模型与档位」页。
- revision 变化时，前端让 settings、providers、profiles、models 和配置快照相关查询失效并重新拉取。
- 文件生效值按内置默认、`--config`、overlay 依次合并，不含数据库覆盖；运行中的服务使用启动快照，待重启项在重启后生效。

---

## 4. 交互约定

- 所有破坏性操作（注销项目、删除 MCP、取消评审）二次确认。
- 长操作（注册、刷新、触发评审）返回 job ID，页面用 toast 提示并跳转到对应详情。
- 分页列表的「重试加载更多」复用失败请求的同一 cursor，保留已加载项；「刷新」重新加载已加载页并从第一页开始。
- 时间显示为相对时间，悬停显示绝对时间（本地时区）。
- 表格超出宽度时在容器内横向滚动；窄屏下侧边导航收起为顶部菜单。

### 4.1 说明文字位置

| 类型 | 组件 | 位置与关联 |
| --- | --- | --- |
| 字段说明 | `Field` 的 `hint` | 控件下方；id 为 `<htmlFor>-hint`，仅在包裹单个控件子元素时由 `Field` 自动并入 `aria-describedby`，多控件字段（如 select + input）需手动设置；错误在 hint 之后，保持 `role="alert"` |
| 页面说明 | `PageHeader` 的 `description` | 标题下方 |
| 面板说明 | `Panel` 的 `note`、`TableNote` | 内容之后，即面板底部；非 flush 用 `.panelNote`，flush 用 `.tableNote` |
| 工具栏状态 | `Toolbar` 的 `status` | `role="toolbar"` 元素之后，不放进 `actions`；传空字符串仍保留 live region 节点，需要可靠播报时不要用 `undefined` 卸载 |
| 操作说明 | `ActionHint` | 按钮组下方 |

`Panel` 的 `aside` 只放计数、状态、来源短标签、链接和操作，长说明不放在这里。警示与错误保持 `role="alert"` / `role="status"`，不降级为普通说明。

### 4.2 UI-KIT 表单与控制原语

`web/src/components/ui.tsx` 提供以下原语，新页面和新表单优先复用。

| 原语 | 约定 |
| --- | --- |
| `Select` | 单选 combobox；空值显示 `emptyLabel`，默认「继承」；空值语义为「全部」等其它含义时显式传入 `emptyLabel`。键盘、ARIA（`aria-activedescendant`）与点外关闭由原语负责。 |
| `Checkbox` | 视觉隐藏原生 `input`，保留 label 点击、Space 与 `focus-visible`。组件自带 label，不要再包在 `Field` 里，避免重复关联 label；需要说明文字时用 `FieldHint` 并手动设置 `aria-describedby`。 |
| `DatePicker` | 值格式 `YYYY-MM-DD`，按本地日历日处理；支持 `min`/`max`、手动输入、清空、Esc 与点外关闭。不得用 `new Date('YYYY-MM-DD')`（按 UTC 解析会使日期偏移）。模态（`Dialog`/`Drawer`）内弹层 portal 到最近的模态容器，其余场景仍 portal 到 `document.body`，无需页面传参。模态内日历处于 `aria-modal="true"` 子树中，焦点陷阱会包含日历控件：日历内按 Tab/Shift+Tab 会关闭日历并把焦点交回输入框，点击月份标题、星期行或弹层留白等不可聚焦区域不会关闭，只有焦点真正离开控件与弹层或点击真实外部区域才关闭；月份导航按钮保持焦点，disabled 关闭且焦点位于日历时，焦点不落到 `<body>`，极矮视口下弹层留在视口内、保留最小可用高度并内部滚动。限制：只识别本库的 `Dialog`/`Drawer`，自建模态容器收不到该能力，弹层回落到 `document.body`，可能与其焦点陷阱冲突；弹层为 `position: fixed`，模态容器或祖先使用 `transform`/`filter` 等会改变 fixed 包含块，导致定位偏移，因此应复用 UI-KIT 的 `Dialog`/`Drawer`。 |
| `NumberField` / `SearchField` / `Textarea` | 保留原生可访问语义（`spinbutton`、`searchbox`、文本域），隐藏浏览器默认 spinner/清除叉；`Textarea` 传 `code` 使用代码变体。 |
| `Tooltip` | hover 与 focus 显示、Esc 关闭，通过 `aria-describedby` 关联；不使用 DOM `title`。 |
| `Disclosure` | 保留 `<details>` / `<summary>` 语义，隐藏原生 marker，使用自定义 chevron。 |

例外：移动端原生日期滚轮、滚动条、密码管理器、textarea resize、焦点与选区仍保留浏览器行为；这些原语不新增 npm 依赖。
