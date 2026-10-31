import { useEffect, useState, type ReactNode } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { listProjects } from '../api/api'
import type { Project } from '../api/types'
import { ExtensionsManager } from '../features/extensions/ExtensionsManager'
import { useCanWrite } from '../components/RoleContext'
import {
  Button, EmptyState, ErrorState, LoadingState, PageHeader, Segmented, Toolbar, ToolbarField,
} from '../components/ui'
import { useLoadMoreFocus, usePaginated } from '../lib/hooks'
import ui from '../styles/ui.module.css'

const validTabs = ['mcp', 'skills', 'bindings']

export function ExtensionsPage() {
  const { tab: rawTab } = useParams()
  const navigate = useNavigate()
  const isAdmin = useCanWrite()
  const tab = rawTab && validTabs.includes(rawTab) ? rawTab : 'mcp'
  // Platform extension reads are admin-only. Other users start in project
  // scope so the page never issues a platform query they cannot run.
  const [scope, setScope] = useState<'platform' | 'project'>(isAdmin ? 'platform' : 'project')
  const [projectId, setProjectId] = useState('')

  const projects = usePaginated<Project>(['projects', 'extensions'], (cursor) => listProjects({ limit: 100 }, cursor), 'projects')
  const projectFocus = useLoadMoreFocus(projects.hasNextPage)

  useEffect(() => {
    if (scope !== 'project' || projectId || !projects.items.length) return
    setProjectId(String(projects.items[0].id))
  }, [scope, projectId, projects.items])

  const projectOptions = projects.items

  // Project-scope states render under the tabs so the toolbar and tabs never move.
  let projectPlaceholder: ReactNode
  if (projects.isError && projectOptions.length === 0) projectPlaceholder = <ErrorState error={projects.error} onRetry={projects.retry} title="项目列表加载失败" />
  else if (projects.isPending) projectPlaceholder = <LoadingState label="正在读取项目…" />
  else if (projectOptions.length === 0 && projects.hasNextPage) projectPlaceholder = <EmptyState title="尚未加载到项目" description="继续加载更多项目。" />
  else if (projectOptions.length === 0) projectPlaceholder = <EmptyState title="还没有项目" description="先注册项目，再维护项目级扩展。" />
  else projectPlaceholder = <EmptyState title="请选择项目" description="项目级 MCP、Skill 和块绑定需要指定项目。" />
  // A failed refresh keeps the cached projects selectable but must stay retryable.
  const projectNotice = scope === 'project' && projects.isError && projectOptions.length > 0
    ? <ErrorState error={projects.error} onRetry={projects.retry} title={projects.isFetchNextPageError ? '更多项目加载失败' : '项目列表刷新失败'} />
    : null

  return (
    <main className={ui.page}>
      <div className={ui.pageStack}>
        <PageHeader
          title="扩展"
          description="MCP 与 Skill 的格式兼容 Claude Code。所有工具对评审 agent 只读，且按块授权。"
        />
        <Toolbar label="扩展作用域与项目">
          <Segmented
            label="扩展作用域"
            options={isAdmin ? [{ value: 'platform', label: '平台级' }, { value: 'project', label: '项目级' }] : [{ value: 'project', label: '项目级' }]}
            value={scope}
            onChange={(next) => {
              setScope(next === 'platform' ? 'platform' : 'project')
              if (next === 'platform') setProjectId('')
            }}
          />
          {/* The project selector stays mounted in platform scope so the toolbar keeps its size. */}
          <ToolbarField label="项目">
            <select
              ref={projectFocus.selectRef}
              className="select"
              name="project_id"
              value={projectId}
              onChange={(event) => setProjectId(event.target.value)}
              aria-label="扩展作用域项目"
              disabled={scope !== 'project' || projectOptions.length === 0}
            >
              {/* Project scope auto-selects the first project, so the placeholder is not selectable then. */}
              <option value="" disabled={scope === 'project' && projectOptions.length > 0}>{scope === 'platform' ? '平台级无需选择项目' : '选择项目'}</option>
              {projectOptions.map((project) => <option key={project.id} value={project.id}>{project.path_with_namespace}</option>)}
            </select>
          </ToolbarField>
          {scope === 'project' && projects.hasNextPage ? (
            <Button variant="secondary" onClick={projects.fetchNextPage} loading={projects.isFetchingNextPage} onFocus={projectFocus.onFocus} onBlur={projectFocus.onBlur}>
              {projects.isFetchNextPageError ? '重试加载更多项目' : '加载更多项目'}
            </Button>
          ) : null}
        </Toolbar>

        <ExtensionsManager
          key={`${scope}-${projectId}`}
          scope={scope}
          projectId={scope === 'project' && projectId ? Number(projectId) : undefined}
          canManage={scope === 'project' && projectOptions.find((project) => String(project.id) === projectId)?.access === 'manage'}
          tab={tab}
          onTabChange={(next) => navigate(`/extensions/${next}`)}
          projects={projectOptions}
          placeholder={projectPlaceholder}
          notice={projectNotice}
        />
      </div>
    </main>
  )
}
