# 本机隔离模拟报告

> 2026-10-24。状态：PASS。范围是本机二进制 + 临时 SQLite + 合成数据。**不是**真实 OAuth、GitLab、模型 provider 或 Docker 部署验收。

## 方法

1. 用当前工作树构建 `cmd/server`，以 `config/server.example.yaml` + 本地覆盖启动；OAuth 关闭、无模型调用、无外部 GitLab 请求。
2. 首次启动创建 schema 后停止，写入两个用户、两个项目、`read`/`manage` 授权、API token、评审、dossier 与统计行。
3. 通过真实 HTTP 认证/授权路径执行 11 组断言。
4. 另用修复前 350 文件基线构建旧二进制，创建旧 schema（迁移 1/3/4/5/6），写入旧数据，再换当前二进制升级；随后把迁移后的数据库副本交给旧二进制，演示回滚风险。

启动时 `/readyz` 通过；所有进程均以 SIGTERM 正常停止。日志存放在证据目录，未出现 panic，也未出现模拟 token 明文。

## 结果

### HTTP 隔离与生命周期（11/11 PASS）

| 场景 | 结果 |
|---|---|
| 匿名请求 | 401 |
| bootstrap 管理面 | 用户/项目身份可管理；业务项目、评审、统计均为空或 404 |
| U1（P1 `read`） | 仅见 P1；P2 详情/评审/统计 404；写入 403；payload 403 |
| U2（P2 `manage`） | 仅见 P2；P1 404；设置写入 200；payload 200 |
| 普通平台管理员 | 管理面可用，但业务数据仍需 grant |
| 项目/评审/统计 | 列表、详情、统计与过滤均未跨项目泄漏 |
| grant | 新增后立即生效；删除后立即 404 |
| 禁用/启用 | 禁用使旧 token 401；启用**不恢复**旧 token |
| 新 token | 启用后重新签发可用；撤销全部凭据后 401 |
| 过期 token | 401 |
| 其他用户 | 不受影响 |

发现的一个测试假设错误：禁用用户会事务撤销其 token，因此启用不会让旧凭据复活。实现符合 #21 验收，模拟断言已改为验证更强语义。

### 旧库升级与回退演示（6/6 PASS）

| 场景 | 结果 |
|---|---|
| 迁移 | 由旧版 5 个版本升级为 `1,3,4,5,6,7,8`，共 7 条记录 |
| 新列/表 | `users.disabled`、`workflow_dossiers.publication_error`、`project_grants` 存在 |
| 旧数据 | 用户、项目、评审、finding、dossier 行数保留 |
| 隐式授权 | 升级后旧用户和 bootstrap 均无项目访问权 |
| 显式授权 | 授予后旧项目、评审和发布状态可读 |
| 回滚演示 | 旧二进制可打开迁移后数据库，但旧用户可直接看到两个项目 |

最后一项**不是**支持的降级路径，而是证明仅换回旧镜像不安全：旧代码没有项目隔离。回退必须恢复升级前全量数据，并保持维护/受限访问，不能只回退二进制。

## 证据

`artifacts/delivery/2026-10-24-review-repairs/checks/simulation/` 包含：

- `result.json`、`upgrade-result.json`：逐项 PASS/FAIL 与耗时。
- `run.mjs`、`run-upgrade.mjs`、`seed.sql`：可复现脚本和合成数据定义。
- `server.log`、`new-server.log`、`old-server.log`、`rollback-old.log`：进程日志。
- `check-config*.log`、`old-schema.json`、`seed-check.json`：前置与 schema 证据。

临时数据库与二进制留在本机临时目录，未归档、未提交；归档只有结果、脚本和日志。

## 限制

- 全部 API 用户、项目和 token 由 SQLite 合成；未执行 OAuth 登录、GitLab webhook、模型评审或真实发布。
- 旧二进制来自修复前工作树基线，不等于测试机上的旧镜像；真实旧镜像行为仍需在目标环境核对。
- 未覆盖 SSE 撤权、真实发布失败重试/重启恢复、真实网络超时和 Docker 卷权限；这些仍依赖既有 Go 集成测试或后续环境验收。
- 本报告只证明本地模拟路径，不替代远端 CI、备份恢复演练、生产部署或安全评审。
