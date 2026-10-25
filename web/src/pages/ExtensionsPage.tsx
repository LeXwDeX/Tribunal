import { useEffect, useMemo, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { listProjects } from '../api/api'
import { ExtensionsManager } from '../features/extensions/ExtensionsManager'
import { useCanWrite } from '../components/RoleContext'
import { EmptyState, ErrorState, LoadingState, PageHeader } from '../components/ui'
import ui from '../styles/ui.module.css'
import styles from './pages.module.css'

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

  const projects = useQuery({ queryKey: ['projects', 'extensions'], queryFn: () => listProjects({ limit: 100 }) })

  useEffect(() => {
    if (scope !== 'project' || projectId || !projects.data?.projects?.length) return
    setProjectId(String(projects.data.projects[0].id))
  }, [scope, projectId, projects.data])

  const projectOptions = useMemo(() => projects.data?.projects || [], [projects.data])

  return (
    <main className={ui.page}>
      <div className={ui.pageStack}>
        <PageHeader
          title="扩展"
          description="MCP 与 Skill 的格式兼容 Claude Code。所有工具对评审 agent 只读，且按块授权。"
        />
        <div className={styles.scopeBar}>
          <div className={styles.filterChips} role="group" aria-label="扩展作用域">
            {isAdmin ? (
              <button type="button" className={[styles.chip, scope === 'platform' ? styles.chipActive : ''].filter(Boolean).join(' ')} aria-pressed={scope === 'platform'} onClick={() => { setScope('platform'); setProjectId('') }}>平台级</button>
            ) : null}
            <button type="button" className={[styles.chip, scope === 'project' ? styles.chipActive : ''].filter(Boolean).join(' ')} aria-pressed={scope === 'project'} onClick={() => setScope('project')}>项目级</button>
          </div>
          {scope === 'project' ? (
            <label className={styles.field}>
              <span className="small">项目</span>
              <select
                className="select"
                name="project_id"
                value={projectId}
                onChange={(event) => setProjectId(event.target.value)}
                aria-label="扩展作用域项目"
                disabled={projects.isPending || projects.isError}
              >
                <option value="">选择项目</option>
                {projectOptions.map((project) => <option key={project.id} value={project.id}>{project.path_with_namespace}</option>)}
              </select>
            </label>
          ) : null}
          {scope === 'project' && projects.isError ? <ErrorState error={projects.error} onRetry={() => { void projects.refetch() }} title="项目列表加载失败" /> : null}
          {scope === 'project' && projects.isPending ? <LoadingState label="正在读取项目…" /> : null}
          {scope === 'project' && projects.isSuccess && projectOptions.length === 0 ? <EmptyState title="还没有项目" description="先注册项目，再维护项目级扩展。" /> : null}
        </div>

        {scope === 'project' && !projectId ? (
          <EmptyState title="请选择项目" description="项目级 MCP、Skill 和块绑定需要指定项目。" />
        ) : (
          <ExtensionsManager
            key={`${scope}-${projectId}`}
            scope={scope}
            projectId={scope === 'project' ? Number(projectId) : undefined}
            canManage={scope === 'project' && projectOptions.find((project) => String(project.id) === projectId)?.access === 'manage'}
            tab={tab}
            onTabChange={(next) => navigate(`/extensions/${next}`)}
            projects={projectOptions}
          />
        )}
      </div>
    </main>
  )
}
