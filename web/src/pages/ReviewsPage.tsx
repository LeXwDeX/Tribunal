import { useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { listProfiles, listProjects, listReviews } from '../api/api'
import type { Project, ReviewSummary } from '../api/types'
import { ReviewTable } from '../components/ReviewTable'
import { TriggerReviewDialog } from '../components/TriggerReviewDialog'
import {
  Button, EmptyState, ErrorState, LoadingState, PageHeader, Panel, Segmented, Toolbar, useToast,
} from '../components/ui'
import { useLoadMoreFocus, usePaginated } from '../lib/hooks'
import ui from '../styles/ui.module.css'
import reviewsStyles from './ReviewsPage.module.css'

const statuses = [
  { value: '', label: '全部' },
  { value: 'pending', label: '排队中' },
  { value: 'running', label: '运行中' },
  { value: 'completed', label: '已完成' },
  { value: 'failed', label: '失败' },
  { value: 'cancelled', label: '已取消' },
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
  const projects = usePaginated<Project>(['projects', 'filter'], (cursor) => listProjects({ limit: 100 }, cursor), 'projects')
  const projectFocus = useLoadMoreFocus(projects.hasNextPage)
  const profiles = useQuery({ queryKey: ['profiles', 'filter'], queryFn: () => listProfiles() })

  const projectNames = new Map(projects.items.map((item) => [item.id, item.path_with_namespace]))
  const activeCount = [projectId, status, verdict, profile, trigger, sinceDate].filter(Boolean).length
  const hasFilters = activeCount > 0
  const projectsList = projects.items
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
          description="所有有权限项目的评审记录，可按状态、项目、结论、档位、触发方式和起始日期筛选。"
          actions={canTrigger ? (
            <Button variant="primary" onClick={() => setTriggerOpen(true)} disabled={projects.isPending}>
              手动触发评审
            </Button>
          ) : undefined}
        />

        <Panel
          flush
          title="评审记录"
          aside={reviews.isSuccess ? `${reviews.items.length}${reviews.hasNextPage ? '+' : ''} 条` : undefined}
        >
          <Toolbar
            inset
            label="筛选评审"
            className={reviewsStyles.filters}
            actions={<Button variant="secondary" className={ui.smallButton} onClick={clear} disabled={!hasFilters}>清除筛选{hasFilters ? `（${activeCount}）` : ''}</Button>}
          >
            <div className={reviewsStyles.statusFilter}>
              <Segmented label="按状态筛选" options={statuses} value={status} onChange={(value) => update('status', value)} />
            </div>
            <select ref={projectFocus.selectRef} className="select" name="project_id" value={projectId} onChange={(event) => update('project_id', event.target.value)} aria-label="按项目筛选">
              <option value="">全部项目</option>
              {projectId && !projectsList.some((project) => String(project.id) === projectId) ? <option value={projectId} disabled>指定项目（未加载或不可访问）</option> : null}
              {projectsList.map((project) => (
                <option key={project.id} value={project.id}>{project.path_with_namespace}</option>
              ))}
            </select>
            {projects.hasNextPage ? (
              <Button variant="secondary" onClick={projects.fetchNextPage} loading={projects.isFetchingNextPage} onFocus={projectFocus.onFocus} onBlur={projectFocus.onBlur}>
                {projects.isFetchNextPageError ? '重试加载更多项目' : '加载更多项目'}
              </Button>
            ) : null}
            <select className="select" name="verdict" value={verdict} onChange={(event) => update('verdict', event.target.value)} aria-label="按结论筛选">
              {verdicts.map(([value, label]) => <option key={value} value={value}>{label}</option>)}
            </select>
            <select className="select" name="profile" value={profile} onChange={(event) => update('profile', event.target.value)} aria-label="按档位筛选">
              <option value="">全部档位</option>
              {(profiles.data?.profiles || []).map((item) => <option key={item.name} value={item.name}>{item.name}</option>)}
            </select>
            <select className="select" name="trigger" value={trigger} onChange={(event) => update('trigger', event.target.value)} aria-label="按触发方式筛选">
              {triggers.map(([value, label]) => <option key={value} value={value}>{label}</option>)}
            </select>
            <input className="input" name="since" type="date" value={sinceDate} onChange={(event) => update('since', event.target.value)} aria-label="起始日期" title="起始日期" />
          </Toolbar>
          {projects.isError || profiles.isError ? (
            <div className={reviewsStyles.notes}>
              {projects.isError ? (
                <>
                  <span className="small muted" role="alert">
                    {projects.isFetchNextPageError
                      ? projects.items.length ? '更多项目加载失败；已加载的项目仍可选择。' : '更多项目加载失败，请重试。'
                      : projects.items.length ? '项目列表刷新失败；已加载的项目与当前选择仍保留。' : '项目筛选列表加载失败，请重试。'}
                  </span>
                  {!projects.isFetchNextPageError ? (
                    <Button variant="secondary" onClick={projects.refetch} loading={projects.isFetching}>
                      {projects.items.length ? '重试刷新项目' : '重试加载项目'}
                    </Button>
                  ) : null}
                </>
              ) : null}
              {profiles.isError ? <span className="small muted">档位筛选列表加载失败。</span> : null}
            </div>
          ) : null}
          {reviews.isPending ? <div className={reviewsStyles.state}><LoadingState /></div> : null}
          {reviews.isError ? <div className={reviewsStyles.state}><ErrorState error={reviews.error} onRetry={reviews.retry} /></div> : null}
          {reviews.isSuccess && reviews.items.length === 0 ? (
            <div className={reviewsStyles.state}>
              <EmptyState
                title={hasFilters ? '没有符合条件的评审' : '还没有评审记录'}
                description={hasFilters ? '调整筛选条件，或清除筛选查看全部记录。' : '注册项目并打开 MR，或手动触发一次评审。'}
                action={hasFilters ? <Button variant="secondary" onClick={clear}>清除筛选</Button> : undefined}
              />
            </div>
          ) : null}
          {reviews.items.length ? <ReviewTable reviews={reviews.items} projectNames={projectNames} /> : null}
          {reviews.hasNextPage ? (
            <div className={ui.loadMore}>
              <Button variant="secondary" className={ui.smallButton} onClick={reviews.fetchNextPage} loading={reviews.isFetchingNextPage}>加载更多</Button>
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
