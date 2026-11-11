# 07 Web 控制台

> 平台内部使用的管理控制台。页面原型见设计画布「GitLab CE 评审平台原型」；本文定义页面、每页用到的 API 和前端技术选型。

> 实现状态：控制台已实现，构建产物由 Go 服务 embed。评审详情返回来自存储 dossier 的 `mr_title`、当前用户的项目 `access`、独立于执行状态的 `publication_state`/`publication_error`，以及仅含 `success`/`failed` 的 `publications.commit_status`（旧发布键没有状态时为 unknown，界面显示不可用）。`INCOMPLETE` 与 BLOCKING、PASS 分开显示；BLOCKING 或 INCOMPLETE 的 `commit_status=failed` 表示评审结论，不表示发布失败。项目详情返回工作区摘要和最多 20 条最近 review，项目列表对平台管理员返回全部已登记项目，对其他用户按 grant 或 `global_read` 过滤，并返回 `access`、`access_source`、`workspace` 与 `last_review`。统计页展示总体/项目/provider/块的运行统计与 UTC 日趋势。人工标注在评审详情维护，项目 `manage` 可写、`read` 只读；`quality_by_provider` 展示提出方准确率和挑战方误杀率，`stats.feedback` 的原始计数与 provider/角色归因仍单独展示，不代替人工结论。
>
> 以下字段/页面存在降级，界面用占位符显示：
> - 工作区 `index_status` 按已存储 SHA 显示 `ready` / `stale` / `unknown`；这是检查点状态，不是实时索引健康证明，历史 SHA 的限制见 [HTTP API](03-http-api.md)。
> - 统计页、总览和项目详情将 `stats.runs` 统一标为「运行次数」，注明包含评审和 onboarding。`blocking_ratio` 的分母仍是全部 workflow，onboarding 会稀释该比例；不能解释为仅评审的阻断比例。
>
> 其他状态：项目设置写接口（`PATCH /projects/{id}`、`PUT .../token`、`POST .../webhook`）已实现，平台配置视图仍为只读。2026-10-23 的当前构建在隔离 Chrome、假 API、1440px/390px 和亮暗主题下完成全主路由及项目/扩展子页布局检查，共 79 个状态；表格、绑定矩阵及执行图在容器内滚动。人工标注、统计趋势、键盘展开、日期边界和缺失/坏数据另有交互检查，分页重试和日期极值有回归测试。该检查不代表真实登录、实体手机或所有业务写入的端到端验收。

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

过滤：项目、状态、结论、档位、触发方式、时间。列：MR、项目、档位、状态、结论、P0/P1/P2 数、用时、时间。后端响应中的 `cost_micros` 已在 v1.4.0 移除。

未创建评审任务的结果（API 中 `kind: "status_only"`，如 MR diff 为空）也出现在此列表以及概览、项目页的最近评审表中：该行不可点击，显示 MR、项目、短 head SHA、原因行（如「原因：没有可评审的变更（empty diff）」）、中性徽标「未创建评审任务」和结论徽标。这类行没有评审详情页。

### 3.3 评审详情（核心页面）

| 区块 | 内容 | API |
|---|---|---|
| 头部 | MR 标题与链接、head sha、档位、触发方式、状态、结论、项目 `access`；操作按 `manage` 显示取消、重新发布、在 GitLab 中打开 | `GET /reviews/{id}` |
| DAG 视图 | 按层排列节点，颜色表示状态，点击打开节点抽屉 | `GET /reviews/{id}/graph` + SSE |
| Findings | 表格：级别、标题、文件:行、来源（节点 / provider）、挑战结果、处置、处置理由；可按处置和级别过滤；展开看正文、证据和人工标注 | `GET /reviews/{id}/findings`、`PUT .../findings/{finding_id}/annotation` |
| 发布 | `publication_state`、脱敏的 `publication_error`、汇总评论链接、行内讨论数量、commit status；failed 可重试，pending 等待发布完成（启动恢复或手动重试），skipped 显示原因且不重发 | 详情中的 `publication_state`、`publication_error`、`publications` |
| 节点抽屉 | 参数、输出 JSON、错误、每轮 LLM 调用（用量、耗时、stop reason，可展开请求和响应） | `GET /reviews/{id}/nodes/{node}`、`.../calls` |

人工标注显示已保存结论、操作者和时间。项目 `manage` 选择有效、无效、未确定或未标注后点击「保存标注」，`read` 只读；保存中禁用操作，失败保留旧结论并提供重试。成功后刷新当前评审的 finding 缓存和统计缓存，保留筛选。旧响应没有标注字段时显示未标注。标注不改变发布结果或评审结论，不提供历史记录或并发冲突提示。

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

- **MCP 服务器**：列表（作用域、名称、命令、状态、工具数）；已有自定义服务器的编辑、删除、启停；「测试」按钮列出工具。平台管理员还会看到「新增 MCP 服务器」和「粘贴 Claude Code 配置导入」：
    - 新增对话框支持 stdio（command、args、env）和 http（url、headers），可设置创建后启用，也可从单个服务器的 JSON 粘贴填入表单；保存前可先执行草稿测试，测试不写库。
    - 导入对话框粘贴 Claude Code `mcpServers` 文档，显示解析出的服务器数量和名称，可逐项或全部执行草稿测试；任一项后端校验失败时整批不写入，错误只显示字段路径与固定类别，不回显配置值。
    - 测试结果面板显示连接状态、延迟、工具总数与截断状态，以及每个工具的名称、描述、只读/破坏性/可绑定注解；失败按 `invalid_config`、`connect_timeout`、`connect_failed`、`handshake_failed`、`list_tools_timeout`、`list_tools_failed`、`too_many_tools`、`cleanup_failed`、`test_busy` 等固定类别显示并提供重试。
    - 测试发现的工具以 `mcp__<server>__<tool>` 出现在块绑定矩阵；`bindable=false` 的列标注「不可绑定」，新增授权被禁用，已绑定的工具仍可取消。停用服务器的测试结果只展示，不进入矩阵。会话内测试结果不持久化，切换 scope 后清空。
- **Skill**：列表（作用域、名称、描述、来源目录）；已有自定义 Skill 的启停和删除。平台管理员可「上传 Skill zip」或「登记服务器目录」：上传入口使用 UI-KIT 按钮触发隐藏文件输入，不支持浏览器原生文件按钮外观；zip 必须包含 `SKILL.md`，服务端限制归档大小、解压总量和条目数。目录登记要求服务端绝对路径，并在对话框中明确提示会读取服务端文件系统路径、把内容复制到受管目录，源目录不会被修改或删除；这是高级选项，默认推荐 zip。成功后列表刷新并显示名称、描述和来源目录；同名或非法 manifest 的 400/409 错误保持对话框可重试。内置 Skill 始终启用，不提供替换或删除入口。
- **块绑定**：矩阵视图，行是块（explore / code-review / review / reasoner / validate / verify / synthesize / onboarding），列是可用的 MCP 工具和 Skill，勾选即绑定。项目 scope 的读取需要该项目 grant 或 `global_read`，平台管理员隐式可读；任何写入都仍要求平台管理员，项目 scope 不再要求逐项目 `manage` grant。矩阵中的工具来自已保存绑定、当前会话的服务器测试结果和 API 返回的 `server.tools`；测试结果显示的 `bindable=false` 工具阻止新增授权，已绑定的工具仍可取消；停用服务器的测试结果不进入矩阵。

后端立即生效：MCP、Skill 和块绑定的保存成功后，下一次评审在本进程内使用新配置；平台 scope 影响全部项目，项目 scope 只影响当前项目；恢复默认响应中的 `restart_required` 为 `false`，恢复默认的预览文案只在为 true 时显示重启提示。仅本进程内即时生效，多实例部署的其他进程不会收到失效通知。控制台 Skill 启停的旧提示「重启服务后生效」随 [#401](https://github.com/LeXwDeX/Tribunal/issues/401) 移除；本 Issue 不改 web / `web/dist`。配置文件中的扩展声明仍需重启，控制台不提供文件编辑。

### 3.7 模型与档位

- 用户管理：用户列表显示角色来源、全局模式、项目授权数；管理抽屉可提升/降级管理员、开关全局模式、增删用户维度项目授权。新增项目授权默认 `manage`（可读、可写、可删除），也可改为 `read` 只读共享；平台管理员的项目数据访问不依赖这些授权。配置来源与自我降级不可操作。全局用户在页头显示标识，项目列表标记“只读（全局）”，评审页可切换“全部评审/我的项目”。
- 模型列表：显示名、来源标签（配置/用户）、协议、上游 model、base_url host、参数摘要、API key 状态、可用性与 unavailable 原因、被引用节点、近 24 小时调用数/错误率/P50 延迟；同名模型显示短 ID。参数摘要对显式 `0` / `false` 给出语义标签：`max_tokens=0` 显示 `0（模型默认，不发送）`，`context_window=0` 显示 `0（未知）`，`max_concurrency=0` 显示 `0（不限制本地并发）`，`rpm=0` 显示 `0（不限制）`，`strict_role_alternation=false` 显示 `false（关闭）`；当该字段的 `param_sources` 为 `default` 时这些零值不生效，不再显示对应标签，改由下方「全局默认」继承行展示实际生效值（#413）。不再显示价格摘要，表单也不提交 `price`（成本字段彻底下线见 #407/#408）。配置模型仍不能通过 `/api/v1/models` 修改；参数摘要来自 `GET /api/v1/config` 的 `config.providers` 文件生效值，卡片显示逐字段来源徽标（内置默认/基础配置/overlay）、YAML 字段路径 `providers.<名称>` 和重启生效说明，平台管理员可通过「编辑配置」抽屉写回 overlay（见下）。非平台管理员的配置模型卡片显示「只读：仅平台管理员可通过配置文件（overlay）编辑。」，不显示编辑/清除入口，也不显示「配置文件快照不可用」这类管理员故障文案。
- 「全局默认参数」面板（#413）：位于「模型」区块之前，只对平台管理员渲染，模型或档位 Drawer 打开时隐藏，避免与编辑表单重复；隐藏期间未保存的表单草稿提升到页面状态保留，关闭 Drawer 后继续编辑。面板显示 `revision`，按限流与窗口（max_tokens、context_window、max_concurrency、rpm）、采样（temperature、forced_tool_choice、strict_role_alternation）、推理与能力（reasoning.effort、thinking.type、thinking.budget_tokens、responses_params、allow_reasoning_effort_none、preserve_reasoning_content、output_config_effort）分组，使用 UI-KIT `Field`/`FieldHint`/`NumberField`/`Select`/`Checkbox`/`Note`，字段说明在控件下方，布尔控件的服务端字段错误显示在对应控件下方；`params.price` 不出现。「保存全局默认」整体 PUT 当前表单的值与 revision；「重置」经确认后按当前 revision DELETE 全部默认值，尚无默认参数或 `revision=0` 时禁用。非指针字段的 0、false 或留空不提交，表示未设置；`temperature` 是显式值，填 0 会覆盖默认；`reasoning.effort: none` 在前端要求同时勾选 `allow_reasoning_effort_none`。400 的 `details[].field` 显示在对应控件下方，跨模型不兼容的 `models.<id>.<字段>` 与模型名显示在表单级错误中；409 `stale_revision`（保存或重置）显示提示与「重新加载」，点击后清除旧错误并重新拉取。面板说明明确只对 `user:` 模型生效，`config:` 模型的参数由配置文件负责，并提示保存时会对现有 `user:` 模型做协议交叉校验，不兼容组合会被拒绝，避免模型变为不可用。用户模型卡片在自身参数下方单独列出「全局默认」继承项（来自 `param_sources=default` 与 `effective_params`），配置模型卡片不显示该行。
- 「评审档位」区块顶部是「全局默认」卡片（`default` slot），显示生效模型（显示名与短 ID）、来源、配置值和生效参数（`effective_params`）及参数来源；下方 6 个可配置节点（explore、四路 lanes、challenge）逐项显示生效模型、来源标签（覆盖/全局默认/配置/配置默认/派生，以 API 返回的 `source` 为准）和生效参数/参数来源。多个节点的 `vote_identity` 相同时提示「这些节点会被计为同一模型」并列出节点（含全局默认）。
- 管理员在 Drawer 中编辑两层：顶部「全局默认」卡片可设置模型与 `max_tokens`、`temperature`、`reasoning.effort`（含 `max`）、`thinking.budget_tokens`、`thinking.type`；下方 6 个节点用相同控件逐项覆盖。每个输入下方显示继承值与来源：未填写时显示「继承值 … · 来源 模型/全局默认」，已填写时显示「覆盖中」及清除后的继承值；修改全局默认会立即更新节点下方的继承提示。`thinking` 覆盖按对象整体生效：本层只要填写任一子字段，未填写的子字段不继承，界面提示整体覆盖；只有本层没有 thinking 覆盖时才显示继承的整个 thinking 对象。不能解析模型目录时继承值显示「未知」，不把已清除的旧值或节点自身的覆盖当成继承值。不可用模型禁用并显示原因。
- 保存时只重建并提交实际填写了值的覆盖项（整体 PUT overrides 文档的协议不变），没有填写任何值的节点不会出现在文档中，已保存的覆盖会原样回填并保留；「清除<节点>覆盖」或「清除全局默认覆盖」清空表单后保存即恢复继承；「恢复为配置」调用 DELETE 清除全部覆盖。保存和恢复都带当前 revision：400 的 `details[].field` 路径显示在对应输入旁，错误与 409 刷新提示显示在全部字段集下方（409 提供重新加载）。非管理员只读，不渲染写控件。
- 管理员可通过 Drawer 新增或编辑用户模型；表单按连接（显示名、协议、base_url、上游模型、API key 三态）、采样（temperature、forced_tool_choice、strict_role_alternation）、推理（reasoning.effort、thinking.type、thinking.budget_tokens、preserve_reasoning_content）、限流（max_tokens、max_concurrency、context_window、rpm）分组，使用 UI-KIT 的 `Select`、`NumberField`、`Checkbox`、`FieldHint`，`ApiError.details` 的字段路径显示在对应控件下方。API key 三态（保持不变/替换/清除）只存在抽屉局部状态，关闭即清空；保存前测试调用草稿测试端点，编辑且 key 不变时不发送 key。预设入口只在新建模型时显示，编辑已有模型不显示，避免绕过预览/冲突流程。页面从 `GET /api/v1/model-presets` 拉取 DeepSeek 预设，按钮为「DeepSeek V4 Pro 最大能力」「DeepSeek Flash 最大能力」；加载中显示加载态，非 404 失败显示可重试提示，404 视为旧服务端并静默隐藏预设区。点击只把预设的显示名、协议、base_url、上游模型和参数填入本地表单，不立即写库，用户可继续修改，保存仍走原有创建或整记录 PATCH，前端不硬编码预设数值；预设未定义的 `temperature` 保留表单已有值，`thinking` 为 null 时清空表单中的 thinking 配置。`max_tokens` 留空表示模型默认（不发送；DeepSeek 默认 8K/64K/128K，不等于最大输出），anthropic 留空时前端直接报错；新建模型 `max_concurrency` 默认留空（不再默认 1），填 0 时显示「不创建本地并发信号量，不限制本地并发」警告。anthropic 的 `thinking.budget_tokens` 为 0 或留空表示不设置，启用时必须不小于 1024 且小于已填的 `max_tokens`；后端 `ValidateProviderConfig` 对 anthropic 下 1–1023 的预算返回字段 `thinking.budget_tokens` 错误；`budget_tokens` 填 0 时按未设置处理、不提交该键。表单与卡片都不再出现 `price`，编辑保存时从参数中移除历史 `price`。删除被引用模型显示 409 引用位置；revision 冲突提示重新加载；出站被拒显示专门说明。配置模型仍只读，模型级参数不能在 `/api/v1/models` 上修改，改用「编辑配置模型」抽屉写回 overlay。本页数据只允许平台管理员读取；其他用户打开会看到权限错误，写入口隐藏。
- 「编辑配置模型」抽屉（#412）：平台管理员在 `config:` 模型卡片点「编辑配置」打开，按连接（协议、base_url、上游模型、只读 API key 状态）、采样（temperature、forced_tool_choice、strict_role_alternation）、推理与能力（reasoning.effort、thinking.type、thinking.budget_tokens、responses_params、allow_reasoning_effort_none、preserve_reasoning_content、output_config_effort）和限流与窗口（max_tokens、context_window、max_concurrency、rpm）分组；每个字段显示来源徽标（内置默认/基础配置/overlay）。`base_url` 允许为空，表示沿用协议适配器默认端点；仅非空时要求绝对 http(s) URL，且不含凭据、查询或片段，表单不再要求必填。`reasoning.effort` 选项包含 `none`，选择 `none` 时必须同时开启 `allow_reasoning_effort_none`（`anthropic` 不接受 `none`）。`max_concurrency` 与 `rpm` 的显式 0 在输入框中显示 0，`max_tokens` 与 `context_window` 的 0 显示为空（模型默认/未知）；字段说明注明 0 或留空都表示不限制，填写 0 会显式写入 overlay。保存只提交实际变化的 `providers.<名称>.*` 叶子并带 `base_revision`、`overlay_revision`；成功且文件变化时按写响应 `overlay_revision` 以约 1 秒间隔轮询 `GET /api/v1/config`，直到快照追上；期间卡片提示「配置快照应用中，稍后可再次编辑」并禁用「编辑配置」，追上后恢复常规 5 秒刷新并显示「已保存到 overlay，待重启生效。重启服务可能取消在途评审。」。没有变化时不写文件并显示「没有需要保存的更改」。抽屉打开期间若 `base_revision` 或 `overlay_revision` 变化（外部编辑或重载），表单重建并提示「配置已被他人修改，已重新加载。」，避免静默丢弃草稿。「重置为 base」经确认后删除 overlay 中整个 `providers.<名称>` 覆盖（该 provider 只存在于 overlay 时，重启后会移除该模型）；仍被配置中的 `profiles`（含 `profiles.default`）或 0028 节点覆盖引用时返回 409 `provider_in_use`，抽屉列出引用者，不修改数据库或档位。卡片与抽屉显示 0028 节点覆盖：节点自身的模型覆盖或参数覆盖按节点名列出（如 `review`），仅参数覆盖（`source` 仍为 `config`、`param_source=override`）也会列出；默认级参数覆盖（`param_source=override_default`，或 `default` 节点自身的覆盖）单列为 `default`，不在继承节点重复。「在评审档位中清除」打开既有「编辑评审档位」抽屉。API key 只显示 `api_key_configured` 的「API key 已配置（只读）」/「API key 未配置（只读）」，不提供 `api_key_env` 编辑或凭据回显。仅平台管理员渲染「编辑配置」与「在评审档位中清除」，服务端同样拒绝非管理员。overlay 不热替换：本 PR 不扫描 active workflow，删除仍被在途评审使用的 provider 不会返回 409，重启后未终态评审可能以 `profile_hash_changed` 取消；`409 provider_in_use` 只覆盖配置中的 `profiles`（含 `profiles.default`）与迁移 0028 的存储引用，不覆盖运行中的工作流。
- 评审档位：只显示 `default`，标题为「评审档位」，按固定的八步拓扑列出各步骤及其 provider：探索（`explore`）、提出（`code-review`、`review`、`reasoner`、`validate`）、挑战（`verify`）、裁决（`adjudicate`）、汇报（`synthesize`），并展示预算。`onboarding` 单独标为「项目接入」，不是评审档位。
- 手动触发评审的对话框也没有档位选择；评审详情的 DAG 阶段轨道中，`explore` 和 `plan` 归入「规划」，`code-review`、`review`、`reasoner` 和 `validate` 归入「提出」。「模型与档位」页的「项目接入」一行在 onboarding 没有配置 provider 时显示「未配置」。

### 3.8 统计与反馈

后端统计字段已移除成本；控制台不再展示成本卡片、趋势与表格列。
- 目标视图：按项目、按 provider、按块的运行次数、耗时趋势，以及按 provider 的提出方准确率/挑战方误杀率。统计对平台管理员覆盖全部已登记项目；其他用户只覆盖自己有 grant 或 `global_read` 可见的项目。
- 当前页面已展示总体及按项目、provider、块的运行数、平均/P95 耗时、有效耗时样本数与 UTC 日趋势。项目统计工作流；provider/块统计已启动节点，每个当前节点行只计一次，不按 LLM 调用轮次或重试次数累加。无 provider 的代码节点只进入块统计。
- 趋势以可横向滚动的日期表格展示；每个项目/provider/块可展开自己的每日明细。筛选和日桶都基于 workflow 创建时间，日桶采用 UTC；只返回有记录的日，不补零。节点指标仅表示最新一次尝试的投影，不代表完整重试历史。字段和样本口径见 [HTTP API](03-http-api.md#45-系统)。旧响应缺字段显示不可用，已知空数组单独显示空状态，非法趋势不显示部分合计。
- 运行次数包含评审和 onboarding；`overturned_ratio` 标为「规则丢弃比例」，是规则裁决 `drop` 的占比，不是「被挑战推翻」或模型准确率。
- 反馈原始计数：`stats.feedback.total` 与 `by_signal`，按项目和 `observed_at` 的 `[from, to)` 筛选；日期控件把结束日映射到次日本地午夜，选择同一天覆盖完整一天。空范围显示已知为零，缺失字段显示不可用。
- 按 provider 与角色归因：`stats.feedback.by_provider` 每项为 `{provider, role, total, by_signal}`，`role` 为 `proposer`/`challenger`。提出方取 `findings.origin_provider`，挑战方取 `challenge_node` 的 provider；仅统计 `feedback.finding_id` 能精确匹配 finding 且 publication 一致的 discussion，summary/unlinked 不计入 `by_provider`，但仍计入原始 `total`/`by_signal`。同一 signal 可同时计入两个角色，两个角色合计不是反馈总数。
- 人工质量面板按提出方、挑战方分表展示 `quality_by_provider`：提出方有效/明确人工结论总数，挑战方被反驳且人工有效/被反驳且已有明确人工结论总数；另列挑战推翻数/提出总数。每个比例同时显示分子与分母，零分母显示「无样本」。未标注和未确定不进人工分母；被丢弃条目也可标注。缺失字段、已知空数组和非法数据分别显示不可用、空状态和错误，不显示部分或伪造的百分比。具体统计口径见 [HTTP API](03-http-api.md#45-系统)。

### 3.9 系统

- 标签：API Token、平台配置（只读）、用户与账号、项目授权；平台配置、用户与账号、项目授权只对平台管理员显示。
- 平台配置（只读）标签顶部新增「配置来源与待重启」面板。面板约每 5 秒轮询 `GET /api/v1/config/status` 与 `GET /api/v1/config`，显示 applied / 待重启徽标、base 与 overlay 的短 revision（overlay 缺失时显示「未挂载」）、`last_error`（`role="alert"`）和 `warnings`（`role="status"`）。
- 面板以表格列出文件生效的标量配置项：路径、值、来源徽标（内置默认/基础配置/overlay）。数组和映射只通过其标量叶子显示；本页不提供编辑。后端在 [#411](https://github.com/LeXwDeX/Tribunal/issues/411) 后已提供 `PATCH /api/v1/config/overlay` 写 API，但系统页仍只读，不渲染写控件；配置模型的编辑入口在「模型与档位」页（[#412](https://github.com/LeXwDeX/Tribunal/issues/412)），本页不提供。
- revision 变化时，前端让 settings、providers、profiles、models 和配置快照相关查询失效并重新拉取。
- 面板说明文字：文件生效值按“内置默认 < `--config` < overlay”合并，不含数据库覆盖；运行中的服务仍使用启动快照，标记为“待重启”的字段需要重启后生效。

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
| `DatePicker` | 值格式 `YYYY-MM-DD`，按本地日历日处理；支持 `min`/`max`、手动输入、清空、Esc 与点外关闭。不得用 `new Date('YYYY-MM-DD')`（按 UTC 解析会使日期偏移）。模态（`Dialog`/`Drawer`）内暂不支持 DatePicker 弹层，与 `useModalFocus` 的焦点陷阱冲突，后续 Issue 支持。 |
| `NumberField` / `SearchField` / `Textarea` | 保留原生可访问语义（`spinbutton`、`searchbox`、文本域），隐藏浏览器默认 spinner/清除叉；`Textarea` 传 `code` 使用代码变体。 |
| `Tooltip` | hover 与 focus 显示、Esc 关闭，通过 `aria-describedby` 关联；不使用 DOM `title`。 |
| `Disclosure` | 保留 `<details>` / `<summary>` 语义，隐藏原生 marker，使用自定义 chevron。 |

例外：移动端原生日期滚轮、滚动条、密码管理器、textarea resize、焦点与选区仍保留浏览器行为；这些原语不新增 npm 依赖。
