import { ApiError } from '../api/client'
import type { ProjectWarning } from '../api/types'

// Fixed workspace failure codes from the server, mapped to actionable hints.
// The code itself is kept in the text so operators can match logs.
const WORKSPACE_HINTS: Record<string, string> = {
  'workspace operation failed': '工作区操作失败，请查看服务日志中的 error_class 与 upstream_status',
  'gitlab: project token rejected (401)': 'GitLab 拒绝了项目访问 Key：Key 无效、已过期或已撤销，请在项目设置中更新访问 Key',
  'gitlab: access denied (403)': 'GitLab 拒绝访问项目：访问 Key 需要 Reporter 及以上角色，以及 api（或 read_api）和 read_repository scope',
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
}

export function describeWorkspaceError(message: string): string {
  const hint = WORKSPACE_HINTS[message]
  return hint ? `${hint}（${message}）` : message
}

export const MAINTAINER_WARNING = '该 Key 的项目角色低于 Maintainer：评审可以正常运行，但自动启用 Webhook 需要 Maintainer。请改用 Maintainer 角色的 Key，或在 GitLab 中手动添加 Webhook。'
const TOKEN_REJECTED_HINT = 'GitLab 拒绝了访问 Key（401）：Key 无效、已过期或已撤销，请先更新访问 Key。'
const MAINTAINER_REQUIRED_HINT = 'GitLab 拒绝管理 Webhook（403）：需要 Maintainer 角色。请改用 Maintainer 角色的访问 Key，或在 GitLab 中手动添加 Webhook。'

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
