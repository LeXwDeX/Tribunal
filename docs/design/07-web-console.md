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
| 近 7 天 | 运行次数（含评审与 onboarding）、BLOCKING 比例、发布条目数、规则丢弃比例、成本、平均/P95 耗时 | `GET /stats` |
| 需要处理 | 工作区异常、token 30 天内过期、发布失败，或带 `publication_error` 的 pending 重试；普通 pending 不告警；provider 连续错误只对平台管理员加载 | `GET /projects`、`GET /providers`、`GET /reviews?publication_state=failed`、`GET /reviews?publication_state=pending` |
| 最近评审 | 最近 20 条 | `GET /reviews?limit=20` |

### 3.2 评审列表

过滤：项目、状态、结论、档位、触发方式、时间。列：MR、项目、档位、状态、结论、P0/P1/P2 数、用时、成本、时间。

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

「保存并初始化」才登记项目、加密保存 Key 并入队后台初始化，可能产生模型费用；注册人自动成为该项目的 `manage` 持有者。高级设置保留分支、跳过 Draft 和自动 webhook，没有档位选择（项目固定使用 `default`）；自动 webhook 默认关闭。Key 仅保存在对话框局部状态，不写入 URL、查询缓存或浏览器持久存储；关闭对话框会清除 Key 和校验结果。

### 3.5 项目详情

| 标签 | 内容 | API |
|---|---|---|
| 概览 | 基本信息、最近评审、本项目统计 | `GET /projects/{id}`、`GET /stats?project_id=` |
| 工作区与上下文 | 工作区状态、base sha、索引状态、最近刷新；操作：刷新、重建索引、重新生成上下文；上下文文档查看与编辑、锁定开关、历史版本 | `/workspace`、`/context-docs` |
| 设置 | 项目 `manage` 可编辑项目设置（没有档位选择）、更新访问 Key、重建 webhook；`read` 只读。`POST /projects/validate` 已认证用户可调用，普通调用方的 token 需能识别出该项目至少 Reporter 的成员（平台管理员免检）；项目 `manage`（平台管理员隐式具备）保存 Key 时直接 `PUT .../token`，由后端校验凭据 | `PATCH /projects/{id}`、`POST /projects/validate`、`PUT .../token` |
| 扩展绑定 | 项目 `read` 可查看项目级 MCP、Skill 和块绑定；写入宿主执行配置仍仅平台管理员，项目 scope 不再要求逐项目 `manage` grant（管理员隐式具备） | `/mcp-servers`、`/skills`、`/block-bindings` |

平台管理员更新访问 Key 时可先做只读校验，返回的项目 ID 必须与当前项目一致；项目 `manage` 不要求调用 validate 端点，直接保存时由服务端校验凭据。该操作只更新平台保存的凭据，不在 GitLab 创建或撤销 token，也不自动初始化或触发评审。

### 3.6 扩展

- **MCP 服务器**：列表（作用域、名称、命令、状态、工具数）；已有自定义服务器的编辑、删除、启停；「测试」按钮列出工具。新增、导入和保存前草稿测试的后端 API 已恢复（仅平台管理员，项目 scope 需项目已登记）；控制台的新增/导入表单与草稿测试界面另行交付，当前页面仍只管理已有服务器。
- **Skill**：列表（作用域、名称、描述、来源目录）；已有自定义 Skill 的启停和删除。zip 上传和目录登记的后端 API 已恢复（仅平台管理员，默认推荐 zip，目录登记为高级选项）；控制台的上传/登记界面另行交付，当前页面仍只管理已有 Skill；内置项始终启用。
- **块绑定**：矩阵视图，行是块（explore / code-review / review / reasoner / validate / verify / synthesize / onboarding），列是可用的 MCP 工具和 Skill，勾选即绑定。项目 scope 的读取需要该项目 grant 或 `global_read`，平台管理员隐式可读；任何写入都仍要求平台管理员，项目 scope 不再要求逐项目 `manage` grant。

后端立即生效：MCP、Skill 和块绑定的保存成功后，下一次评审在本进程内使用新配置；平台 scope 影响全部项目，项目 scope 只影响当前项目；恢复默认响应中的 `restart_required` 为 `false`，恢复默认的预览文案只在为 true 时显示重启提示。仅本进程内即时生效，多实例部署的其他进程不会收到失效通知。控制台 Skill 启停的旧提示「重启服务后生效」随 [#401](https://github.com/LeXwDeX/Tribunal/issues/401) 移除；本 Issue 不改 web / `web/dist`。配置文件中的扩展声明仍需重启，控制台不提供文件编辑。

### 3.7 模型与档位

- 用户管理：用户列表显示角色来源、全局模式、项目授权数；管理抽屉可提升/降级管理员、开关全局模式、增删用户维度项目授权。新增项目授权默认 `manage`（可读、可写、可删除），也可改为 `read` 只读共享；平台管理员的项目数据访问不依赖这些授权。配置来源与自我降级不可操作。全局用户在页头显示标识，项目列表标记“只读（全局）”，评审页可切换“全部评审/我的项目”。
- 模型列表：显示名、来源标签（配置/用户）、协议、上游 model、base_url host、参数摘要、API key 状态、可用性与 unavailable 原因、被引用节点、近 24 小时调用数/错误率/P50 延迟；同名模型显示短 ID。配置模型只读。
- 「评审档位」区块逐项显示 7 个可配置节点（default、explore、四路 lanes、challenge）的生效模型（显示名与短 ID）、来源标签（覆盖/继承覆盖默认/配置/配置默认/派生，以 API 返回的 `source` 为准）、配置值和生效参数（`effective_params`）。多个节点的 `vote_identity` 相同时提示「这些节点会被计为同一模型」并列出节点。管理员在 Drawer 中为每个节点选择模型（不可用模型禁用并显示原因）并设置 `max_tokens`、`temperature`、`reasoning.effort`（含 `max`）、`thinking.budget_tokens`，留空表示继承；「清除某项覆盖」只清空该节点表单，保存时整体 PUT 不含该节点；「恢复为配置」调用 DELETE 清除全部覆盖。保存和恢复都带当前 revision：400 的 `details[].field` 路径显示在对应输入旁，409 提示刷新并提供重新加载。非管理员只读，不渲染写控件。
- 管理员可通过 Drawer 新增或编辑用户模型；API key 三态（保持不变/替换/清除），只存在抽屉局部状态，关闭即清空；保存前测试调用草稿测试端点，编辑且 key 不变时不发送 key。删除被引用模型显示 409 引用位置；revision 冲突提示重新加载；出站被拒显示专门说明。本页数据只允许平台管理员读取；其他用户打开会看到权限错误，写入口隐藏。
- 评审档位：只显示 `default`，标题为「评审档位」，按固定的八步拓扑列出各步骤及其 provider：探索（`explore`）、提出（`code-review`、`review`、`reasoner`、`validate`）、挑战（`verify`）、裁决（`adjudicate`）、汇报（`synthesize`），并展示预算。`onboarding` 单独标为「项目接入」，不是评审档位。
- 手动触发评审的对话框也没有档位选择；评审详情的 DAG 阶段轨道中，`explore` 和 `plan` 归入「规划」，`code-review`、`review`、`reasoner` 和 `validate` 归入「提出」。「模型与档位」页的「项目接入」一行在 onboarding 没有配置 provider 时显示「未配置」。

### 3.8 统计与反馈

- 目标视图：按项目、按 provider、按块的运行次数、成本、耗时趋势，以及按 provider 的提出方准确率/挑战方误杀率。统计对平台管理员覆盖全部已登记项目；其他用户只覆盖自己有 grant 或 `global_read` 可见的项目。
- 当前页面已展示总体及按项目、provider、块的运行数、成本、平均/P95 耗时、有效耗时样本数与 UTC 日趋势。项目统计工作流；provider/块统计已启动节点，每个当前节点行只计一次，不按 LLM 调用轮次或重试次数累加。无 provider 的代码节点只进入块统计。
- 趋势以可横向滚动的日期表格展示；每个项目/provider/块可展开自己的每日明细。筛选和日桶都基于 workflow 创建时间，日桶采用 UTC；只返回有记录的日，不补零。节点指标仅表示最新一次尝试的投影，不代表完整重试历史。字段、样本和成本口径见 [HTTP API](03-http-api.md#45-系统)。旧响应缺字段显示不可用，已知空数组单独显示空状态，非法趋势不显示部分合计。
- 运行次数包含评审和 onboarding；`overturned_ratio` 标为「规则丢弃比例」，是规则裁决 `drop` 的占比，不是「被挑战推翻」或模型准确率。
- 反馈原始计数：`stats.feedback.total` 与 `by_signal`，按项目和 `observed_at` 的 `[from, to)` 筛选；日期控件把结束日映射到次日本地午夜，选择同一天覆盖完整一天。空范围显示已知为零，缺失字段显示不可用。
- 按 provider 与角色归因：`stats.feedback.by_provider` 每项为 `{provider, role, total, by_signal}`，`role` 为 `proposer`/`challenger`。提出方取 `findings.origin_provider`，挑战方取 `challenge_node` 的 provider；仅统计 `feedback.finding_id` 能精确匹配 finding 且 publication 一致的 discussion，summary/unlinked 不计入 `by_provider`，但仍计入原始 `total`/`by_signal`。同一 signal 可同时计入两个角色，两个角色合计不是反馈总数。
- 人工质量面板按提出方、挑战方分表展示 `quality_by_provider`：提出方有效/明确人工结论总数，挑战方被反驳且人工有效/被反驳且已有明确人工结论总数；另列挑战推翻数/提出总数。每个比例同时显示分子与分母，零分母显示「无样本」。未标注和未确定不进人工分母；被丢弃条目也可标注。缺失字段、已知空数组和非法数据分别显示不可用、空状态和错误，不显示部分或伪造的百分比。具体统计口径见 [HTTP API](03-http-api.md#45-系统)。

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
