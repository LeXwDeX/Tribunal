import { ApiError } from '../api/client'
import type { ProjectWarning } from '../api/types'

// Fixed workspace failure codes from the server, mapped to actionable hints.
// The code itself is kept in the text so operators can match logs.
const WORKSPACE_HINTS: Record<string, string> = {
  'workspace operation failed': '工作区操作失败，请查看服务日志中的 error_class 与 upstream_status',
  'gitlab: project token rejected (401)': 'GitLab 拒绝了项目访问 Key：Key 无效、已过期或已撤销，请在项目设置中更新访问 Key',
  'gitlab: access denied (403)': 'GitLab 拒绝访问项目：访问 Key 需要 Reporter 及以上角色，以及 api 和 read_repository scope',
  'gitlab: project or file not found (404)': 'GitLab 找不到项目：项目可能已删除或移动，或访问 Key 无权查看',
  'gitlab: rate limited (429)': 'GitLab 限流，请稍后重试',
  'gitlab: unavailable': '无法连接 GitLab，或 GitLab 返回服务器错误，请稍后重试',
  'gitlab: request rejected': 'GitLab 拒绝了请求，请检查访问 Key 和项目设置',
  'gitlab: project identity changed': '项目在 GitLab 中的路径或地址与注册信息不一致（可能已移动或重命名），请重新注册项目',
  'git: authentication failed': '克隆仓库时认证失败：访问 Key 无效或已撤销，请更新访问 Key',
  'git: access denied': '克隆仓库被拒绝：访问 Key 需要 read_repository scope 和 Reporter 及以上角色',
  'git: repository not found': '克隆时找不到仓库：项目可能已移动，或访问 Key 无权读取代码',
  'git: branch not found': '分支不存在：默认分支或推送的分支在仓库中找不到，请检查 GitLab 默认分支',
  'git: network error': '克隆仓库时网络失败，请检查服务到 GitLab 的网络后重试',
  'git: operation failed': 'git 操作失败，请查看服务日志中的 error_class',
  'workspace: operation timed out': '工作区操作超时，请稍后重试',
  'workspace: operation canceled': '工作区操作被取消（例如项目被删除或服务重启），请重新触发',
  'onboarding: profile not configured': '未配置 onboarding 档案，无法生成项目上下文，请联系管理员',
  'onboarding: model provider rejected credentials': '模型服务拒绝了凭据，请管理员检查模型配置中的 API Key',
  'onboarding: model provider rate limited': '模型服务限流，请稍后重试',
  'onboarding: model provider unavailable': '模型服务不可用或过载，请稍后重试',
  'onboarding: model call timed out': '模型调用超时，请稍后重试',
  'onboarding: model call failed': '模型调用失败，请查看服务日志中的 error_class',
  // The workspace indexer refuses to index by policy. This is a safe
  // configuration outcome, not a failure.
  'index: blocked by ignore policy': '已按 ignore 策略跳过索引，这是安全配置，不是故障。修改项目 ignore 配置后可重建索引',
}

// NODE_REASON_HINTS translates the durable DAG failure classes into an
// actionable sentence. An unknown class is shown verbatim so operators can
// match the server logs instead of losing the evidence.
const NODE_REASON_HINTS: Record<string, string> = {
  required_node_failed: '必需的前置节点失败，该节点未执行，评审范围因此不完整。',
  budget_exhausted: '工作流 token 预算耗尽，该节点未执行。',
  queue_timeout: '节点排队超时，未开始执行。',
  timeout: '节点执行超时。',
  executor_error: '节点执行失败。',
  sandbox_denied: '证据沙箱或 ignore 策略拒绝了访问路径，这是安全配置。',
  invalid_output: '模型输出未通过校验，节点结果未被采用。',
  rate_limited: '模型服务限流，节点未完成。',
  auth: '模型服务拒绝了凭据，节点未完成。',
  unavailable: '模型服务不可用，节点未完成。',
  panic: '节点执行器异常，节点未完成。',
  canceled: '评审被取消，节点未完成。',
}

export function describeNodeReason(reason: string | undefined, status?: string): string | null {
  if (reason) {
    const hint = NODE_REASON_HINTS[reason]
    return hint ? `${hint}（${reason}）` : `节点未完成（原因代码：${reason}）。`
  }
  if (status === 'skipped') return '该节点已跳过，评审范围不完整。'
  if (status === 'failed') return '节点执行失败，服务端没有返回原因代码。'
  return null
}

export function unresolvedKindLabel(kind: string): string {
  if (kind === 'evidence_gap') return '证据缺口'
  if (kind === 'review_blocking') return '评审阻断'
  return '未解决信号'
}

export function describeWorkspaceError(message: string): string {
  const hint = WORKSPACE_HINTS[message]
  return hint ? `${hint}（${message}）` : message
}

export const MAINTAINER_WARNING = '该 Key 的项目角色低于 Maintainer：评审可以正常运行，但自动启用 Webhook 需要 Maintainer。请改用 Maintainer 角色的 Key，或在 GitLab 中手动添加 Webhook。'
const TOKEN_REJECTED_HINT = 'GitLab 拒绝了访问 Key（401）：Key 无效、已过期或已撤销，请先更新访问 Key。'
const MAINTAINER_REQUIRED_HINT = 'GitLab 拒绝管理 Webhook（403）：需要 Maintainer 角色。请改用 Maintainer 角色的访问 Key，或由 Maintainer 在 GitLab 中手动管理 Webhook。'

export function tokenWarningText(warnings: ProjectWarning[] | undefined): string | null {
  return warnings?.some((warning) => warning.code === 'webhook_requires_maintainer') ? MAINTAINER_WARNING : null
}

// webhookFailureHint explains a failed enable/rebuild call from its details.
export function webhookFailureHint(error: unknown): string | null {
  if (!(error instanceof ApiError) || error.code !== 'gitlab_rejected') return null
  const fields = (error.details || []).map((detail) => detail.field)
  if (fields.includes('token')) return TOKEN_REJECTED_HINT
  if (fields.includes('access_level')) return MAINTAINER_REQUIRED_HINT
  return null
}

// webhookStoredErrorHint explains the stored webhook_error of a project.
export function webhookStoredErrorHint(message: string): string | null {
  if (message.endsWith(': project token rejected')) return TOKEN_REJECTED_HINT
  if (message.endsWith(': Maintainer role required')) return MAINTAINER_REQUIRED_HINT
  return null
}
