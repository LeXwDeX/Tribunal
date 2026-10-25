import { useMemo, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { listProfiles, listProjects, listReviews } from '../api/api'
import type { Project, ReviewSummary } from '../api/types'
import { ReviewTable } from '../components/ReviewTable'
import { TriggerReviewDialog } from '../components/TriggerReviewDialog'
import { Button, EmptyState, ErrorState, LoadingState, PageHeader, Panel, useToast } from '../components/ui'
import { usePaginated } from '../lib/hooks'
import ui from '../styles/ui.module.css'
import styles from './pages.module.css'

const statuses = [
  ['', '全部状态'],
  ['pending', '排队中'],
  ['running', '运行中'],
  ['completed', '已完成'],
  ['failed', '失败'],
  ['cancelled', '已取消'],
]

const verdicts = [['', '全部结论'], ['PASS', 'PASS'], ['BLOCKING', 'BLOCKING'], ['INCOMPLETE', 'INCOMPLETE']]
const triggers = [
  ['', '全部触发方式'],
  ['mr_open', 'MR 打开'],
  ['mr_update', 'MR 更新'],
  ['command', '评论命令'],
  ['api', 'API 手动触发'],
  ['push', '推送'],
  ['register', '项目注册'],
]

export function ReviewsPage() {
  const [params, setParams] = useSearchParams()
  const toast = useToast()
  const queryClient = useQueryClient()
  const [triggerOpen, setTriggerOpen] = useState(false)

  const update = (name: string, value: string) => {
    const next = new URLSearchParams(params)
    if (value) next.set(name, value)
    else next.delete(name)
    setParams(next, { replace: true })
  }

  const projectId = params.get('project_id') || ''
  const status = params.get('status') || ''
  const verdict = params.get('verdict') || ''
  const profile = params.get('profile') || ''
  const trigger = params.get('trigger') || ''
  const sinceDate = params.get('since') ? params.get('since')!.slice(0, 10) : ''
  const since = sinceDate ? new Date(`${sinceDate}T00:00:00`).toISOString() : undefined

  const filtersKey = [projectId, status, verdict, profile, trigger, since]
  const reviews = usePaginated<ReviewSummary>(
    ['reviews', 'list', ...filtersKey],
    (cursor) => listReviews({
      project_id: projectId || undefined,
      status: status || undefined,
      verdict: verdict || undefined,
      profile: profile || undefined,
      trigger: trigger || undefined,
      since,
    }, cursor),
    'reviews',
  )
  const projects = useQuery({ queryKey: ['projects', 'filter'], queryFn: () => listProjects({ limit: 100 }) })
  const profiles = useQuery({ queryKey: ['profiles', 'filter'], queryFn: () => listProfiles() })

  const projectNames = useMemo(
    () => new Map((projects.data?.projects ?? []).map((item) => [item.id, item.path_with_namespace])),
    [projects.data],
  )
  const activeCount = [projectId, status, verdict, profile, trigger, sinceDate].filter(Boolean).length
  const hasFilters = activeCount > 0
  const projectsList: Project[] = projects.data?.projects ?? []
  const manageableProjects = projectsList.filter((project) => project.access === 'manage')
  const canTrigger = manageableProjects.length > 0

  const clear = () => setParams(new URLSearchParams(), { replace: true })

  const onTriggered = (jobId?: string) => {
    setTriggerOpen(false)
    toast.push('success', jobId ? `已创建评审任务 ${jobId}。` : '已创建评审任务。')
    void queryClient.invalidateQueries({ queryKey: ['reviews'] })
    void queryClient.invalidateQueries({ queryKey: ['stats'] })
  }

  return (
    <main className={ui.page}>
      <div className={ui.pageStack}>
        <PageHeader
          title="评审"
          description="按项目、状态、结论、档位、触发方式或起始时间筛选。表格在面板内横向滚动。"
          actions={canTrigger ? (
            <Button variant="primary" onClick={() => setTriggerOpen(true)} disabled={projects.isPending}>
              手动触发评审
            </Button>
          ) : undefined}
        />

        <Panel title="筛选" aside={hasFilters ? `${activeCount} 个条件` : '未筛选'}>
          <div className={styles.inlineFilters}>
            <label style={fieldStyle}>
              <span className="small">项目</span>
              <select className="select" name="project_id" value={projectId} onChange={(event) => update('project_id', event.target.value)} aria-label="按项目筛选">
                <option value="">全部项目</option>
                {projectsList.map((project) => (
                  <option key={project.id} value={project.id}>{project.path_with_namespace}</option>
                ))}
              </select>
            </label>
            <label style={fieldStyle}>
              <span className="small">状态</span>
              <select className="select" name="status" value={status} onChange={(event) => update('status', event.target.value)} aria-label="按状态筛选">
                {statuses.map(([value, label]) => <option key={value} value={value}>{label}</option>)}
              </select>
            </label>
            <label style={fieldStyle}>
              <span className="small">结论</span>
              <select className="select" name="verdict" value={verdict} onChange={(event) => update('verdict', event.target.value)} aria-label="按结论筛选">
                {verdicts.map(([value, label]) => <option key={value} value={value}>{label}</option>)}
              </select>
            </label>
            <label style={fieldStyle}>
              <span className="small">档位</span>
              <select className="select" name="profile" value={profile} onChange={(event) => update('profile', event.target.value)} aria-label="按档位筛选">
                <option value="">全部档位</option>
                {(profiles.data?.profiles || []).map((item) => <option key={item.name} value={item.name}>{item.name}</option>)}
              </select>
            </label>
            <label style={fieldStyle}>
              <span className="small">触发方式</span>
              <select className="select" name="trigger" value={trigger} onChange={(event) => update('trigger', event.target.value)} aria-label="按触发方式筛选">
                {triggers.map(([value, label]) => <option key={value} value={value}>{label}</option>)}
              </select>
            </label>
            <label style={fieldStyle}>
              <span className="small">起始日期</span>
              <input className="input" name="since" type="date" value={sinceDate} onChange={(event) => update('since', event.target.value)} aria-label="起始日期" />
            </label>
            <Button variant="secondary" onClick={clear} disabled={!hasFilters}>清除筛选</Button>
          </div>
          {projects.isError ? <span className="small muted">项目筛选列表加载失败，仍可查看全部项目。</span> : null}
          {profiles.isError ? <span className="small muted">档位筛选列表加载失败。</span> : null}
        </Panel>

        <Panel
          title="评审记录"
          aside={reviews.isSuccess ? `${reviews.items.length}${reviews.hasNextPage ? '+' : ''} 条` : undefined}
        >
          {reviews.isPending ? <LoadingState /> : null}
          {reviews.isError ? <ErrorState error={reviews.error} onRetry={reviews.retry} /> : null}
          {reviews.isSuccess && reviews.items.length === 0 ? (
            <EmptyState
              title={hasFilters ? '没有符合条件的评审' : '还没有评审记录'}
              description={hasFilters ? '调整筛选条件，或清除筛选查看全部记录。' : '注册项目并打开 MR，或手动触发一次评审。'}
              action={hasFilters ? <Button variant="secondary" onClick={clear}>清除筛选</Button> : undefined}
            />
          ) : null}
          {reviews.items.length ? <ReviewTable reviews={reviews.items} projectNames={projectNames} /> : null}
          {reviews.hasNextPage ? (
            <div className={styles.actionsRight}>
              <Button variant="secondary" onClick={reviews.fetchNextPage} loading={reviews.isFetchingNextPage}>加载更多</Button>
            </div>
          ) : null}
        </Panel>
      </div>

      {canTrigger ? (
        <TriggerReviewDialog
          open={triggerOpen}
          onClose={() => setTriggerOpen(false)}
          projects={manageableProjects}
          onTriggered={onTriggered}
        />
      ) : null}
    </main>
  )
}

const fieldStyle = {
  display: 'flex',
  flexDirection: 'column',
  gap: 6,
  minWidth: 0,
} as const
