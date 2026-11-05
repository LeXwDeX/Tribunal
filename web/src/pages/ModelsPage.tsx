import { useEffect, useId, useMemo, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useCanWrite } from '../components/RoleContext'
import {
  createModel, deleteModel, listModels, listProfiles, testModelDraft, updateModel,
} from '../api/api'
import { ApiError, errorMessage } from '../api/client'
import type { Model, ModelParams, ModelWrite, Profile, ProfileConfig, ProviderTestResult } from '../api/types'
import {
  Badge, Button, ConfirmDialog, Drawer, EmptyState, ErrorState, Field, LoadingState, PageHeader, Panel,
} from '../components/ui'
import { formatDuration, formatJson, formatNumber } from '../lib/format'
import ui from '../styles/ui.module.css'
import styles from './insights.module.css'

function fmt(value: number | undefined, suffix = ''): string {
  return typeof value === 'number' && Number.isFinite(value) ? `${formatNumber(value)}${suffix}` : '不可用'
}

function shortID(id: string): string {
  return id.length > 16 ? `${id.slice(0, 8)}…${id.slice(-4)}` : id
}

function hostOf(value: string): string {
  try {
    return new URL(value).host || value
  } catch {
    return value || '不可用'
  }
}

type KeyMode = 'keep' | 'replace' | 'clear'

function ModelCard({ model, canWrite, onEdit, onDelete }: {
  model: Model
  canWrite: boolean
  onEdit: () => void
  onDelete: () => void
}) {
  const stats = model.stats_24h
  const errorRate = typeof stats?.error_rate === 'number'
    ? `${(stats.error_rate <= 1 ? stats.error_rate * 100 : stats.error_rate).toFixed(2)}%`
    : '不可用'
  const p50 = typeof stats?.p50_latency_ms === 'number' ? formatDuration(stats.p50_latency_ms) : '不可用'
  return (
    <article className={`${ui.panel} ${styles.providerCard}`}>
      <div className={styles.providerHead}>
        <span className={styles.cardTitle}>{model.display_name}</span>
        <Badge tone={model.source === 'config' ? 'neutral' : 'info'} plain>{model.source === 'config' ? '配置' : '用户'}</Badge>
        <Badge tone={model.available ? 'ok' : 'danger'} plain>{model.available ? '可用' : '不可用'}</Badge>
        {model.read_only ? <Badge tone="neutral" plain>只读</Badge> : null}
      </div>
      <div className={styles.cardMeta}>
        <span>ID <span className="mono">{shortID(model.id)}</span></span>
        <span>协议 {model.protocol || '不可用'}</span>
      </div>
      <div className={styles.cardMeta}>
        <span>上游模型 <span className="mono">{model.model || '不可用'}</span></span>
        <span>base_url <code>{hostOf(model.base_url)}</code></span>
      </div>
      <div className={styles.cardMeta}>
        <span>API key {model.has_api_key ? '已配置' : '未配置'}</span>
        <span>参数 {formatJson(model.params)}</span>
      </div>
      {model.unavailable_reason ? <span className={styles.resultError}>不可用原因：{model.unavailable_reason}</span> : null}
      {model.referenced_by?.length ? <span className={styles.cardMeta}>引用：{model.referenced_by.join('、')}</span> : null}
      <ul className={styles.cardStats} aria-label={`${model.display_name} 近 24 小时`}>
        {[['24h 调用', fmt(stats?.calls)], ['错误率', errorRate], ['P50 延迟', p50]].map(([label, value]) => (
          <li key={label}><span>{label}</span><b className={value === '不可用' ? styles.statUnavailable : undefined}>{value}</b></li>
        ))}
      </ul>
      {canWrite && !model.read_only ? (
        <div className={styles.cardActions}>
          <Button variant="secondary" className={ui.smallButton} onClick={onEdit}>编辑</Button>
          <Button variant="danger" className={ui.smallButton} onClick={onDelete}>删除</Button>
        </div>
      ) : null}
    </article>
  )
}

function ModelDrawer({ open, model, onClose, onSaved }: {
  open: boolean
  model?: Model
  onClose: () => void
  onSaved: () => void
}) {
  const [displayName, setDisplayName] = useState('')
  const [protocol, setProtocol] = useState('openai-chat')
  const [baseURL, setBaseURL] = useState('')
  const [upstreamModel, setUpstreamModel] = useState('')
  const [maxTokens, setMaxTokens] = useState('')
  const [temperature, setTemperature] = useState('')
  const [effort, setEffort] = useState('')
  const [keyMode, setKeyMode] = useState<KeyMode>('keep')
  const [apiKey, setApiKey] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string>()
  const [test, setTest] = useState<ProviderTestResult>()
  const ids = { display: useId(), protocol: useId(), base: useId(), model: useId(), tokens: useId(), temp: useId(), effort: useId(), key: useId() }

  useEffect(() => {
    if (!open) {
      setApiKey('')
      setKeyMode('keep')
      setError(undefined)
      setTest(undefined)
      return
    }
    setDisplayName(model?.display_name ?? '')
    setProtocol(model?.protocol ?? 'openai-chat')
    setBaseURL(model?.base_url ?? '')
    setUpstreamModel(model?.model ?? '')
    setMaxTokens(model?.params?.max_tokens ? String(model.params.max_tokens) : '')
    setTemperature(model?.params?.temperature !== undefined ? String(model.params.temperature) : '')
    setEffort(model?.params?.reasoning?.effort ?? '')
    setApiKey('')
    setKeyMode('keep')
    setError(undefined)
    setTest(undefined)
  }, [open, model])

  const input = (): ModelWrite => {
    const params: ModelParams = {}
    if (maxTokens) params.max_tokens = Number(maxTokens)
    if (temperature) params.temperature = Number(temperature)
    if (effort) params.reasoning = { effort }
    const value: ModelWrite = {
      display_name: displayName.trim(), protocol, base_url: baseURL.trim(), model: upstreamModel.trim(), params,
    }
    if (keyMode === 'replace') value.api_key = apiKey
    if (keyMode === 'clear') value.api_key = ''
    if (model) {
      value.id = model.id
      value.revision = model.revision
    }
    return value
  }

  const save = async () => {
    setPending(true)
    setError(undefined)
    try {
      if (model) await updateModel(model.id, input())
      else await createModel(input())
      onSaved()
      onClose()
    } catch (err) {
      setError(err instanceof ApiError && err.code === 'stale_revision'
        ? '已被他人修改，请重新加载。'
        : errorMessage(err))
    } finally {
      setPending(false)
    }
  }

  const runTest = async () => {
    setPending(true)
    setError(undefined)
    setTest(undefined)
    try {
      setTest(await testModelDraft(input()))
    } catch (err) {
      setError(err instanceof ApiError && err.code === 'egress_denied'
        ? '出站地址被拒绝：请检查 base_url 或服务端出站规则。'
        : errorMessage(err))
    } finally {
      setPending(false)
    }
  }

  return (
    <Drawer
      open={open}
      onClose={onClose}
      title={model ? '编辑模型' : '新增模型'}
      description="API key 只保存在本次表单中；关闭后不会保留。"
      footer={(
        <>
          <Button variant="secondary" onClick={() => { void runTest() }} loading={pending}>保存前测试</Button>
          <Button variant="secondary" onClick={onClose}>取消</Button>
          <Button onClick={() => { void save() }} loading={pending}>{model ? '保存' : '创建'}</Button>
        </>
      )}
    >
      <Field label="显示名" htmlFor={ids.display}><input id={ids.display} className={ui.input} value={displayName} onChange={(event) => setDisplayName(event.target.value)} /></Field>
      <Field label="协议" htmlFor={ids.protocol}>
        <select id={ids.protocol} className={ui.input} value={protocol} onChange={(event) => setProtocol(event.target.value)}>
          <option value="openai-chat">openai-chat</option>
          <option value="openai-responses">openai-responses</option>
          <option value="anthropic">anthropic</option>
        </select>
      </Field>
      <Field label="base_url" htmlFor={ids.base}><input id={ids.base} className={ui.input} value={baseURL} onChange={(event) => setBaseURL(event.target.value)} /></Field>
      <Field label="上游模型" htmlFor={ids.model}><input id={ids.model} className={ui.input} value={upstreamModel} onChange={(event) => setUpstreamModel(event.target.value)} /></Field>
      <Field label="max_tokens" htmlFor={ids.tokens}><input id={ids.tokens} className={ui.input} inputMode="numeric" value={maxTokens} onChange={(event) => setMaxTokens(event.target.value)} /></Field>
      <Field label="temperature" htmlFor={ids.temp}><input id={ids.temp} className={ui.input} inputMode="decimal" value={temperature} onChange={(event) => setTemperature(event.target.value)} /></Field>
      <Field label="reasoning effort" htmlFor={ids.effort} hint="anthropic 不会发送 effort。">
        <select id={ids.effort} className={ui.input} value={effort} onChange={(event) => setEffort(event.target.value)}>
          <option value="">继承</option>
          <option value="low">low</option>
          <option value="medium">medium</option>
          <option value="high">high</option>
          <option value="max">max</option>
        </select>
      </Field>
      <Field label="API key" htmlFor={ids.key}>
        <select id={`${ids.key}-mode`} className={ui.input} value={keyMode} onChange={(event) => setKeyMode(event.target.value as KeyMode)} aria-label="API key 状态">
          <option value="keep">保持不变</option>
          <option value="replace">替换</option>
          <option value="clear">清除</option>
        </select>
        {keyMode === 'replace' ? <input id={ids.key} className={ui.input} type="password" value={apiKey} onChange={(event) => setApiKey(event.target.value)} /> : null}
      </Field>
      {model?.read_only ? <p className="small muted">由配置文件提供，修改需改配置并重启。</p> : null}
      {error ? <span className={styles.resultError}>{error}</span> : null}
      {test ? (
        <span className={test.ok === false ? styles.resultError : styles.resultOk}>
          {test.ok === true ? '测试成功。' : '测试失败。'} {test.model ? `模型：${test.model}。` : ''}
          {typeof test.latency_ms === 'number' ? `延迟：${formatDuration(test.latency_ms)}。` : ''}
        </span>
      ) : null}
    </Drawer>
  )
}

function asRecord(value: unknown): Record<string, unknown> | undefined {
  return value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : undefined
}

interface Step { stage: string; what: string; who: string }

const REVIEW_PROFILE = 'default'

function text(value: unknown): string | undefined {
  return typeof value === 'string' && value.trim() ? value : undefined
}

function profileSteps(config?: ProfileConfig): Step[] {
  if (!config) return []
  const base = text(config.provider)
  const lanes = asRecord(config.lanes)
  const who = (value: unknown) => text(value) ?? base ?? '未配置 provider'
  const lane = (name: string) => who(asRecord(lanes?.[name])?.provider)
  return [
    { stage: '探索', what: 'explore（只读探索）', who: who(asRecord(config.explore)?.provider) },
    { stage: '提出', what: 'code-review（挂载 code-review Skill）', who: lane('code_review') },
    { stage: '提出', what: 'review（独立终审）', who: lane('review') },
    { stage: '提出', what: 'reasoner（逻辑推演）', who: lane('reasoner') },
    { stage: '提出', what: 'validate（pipeline 与代码证据验证）', who: lane('validate') },
    { stage: '挑战', what: 'verify（逐条反证 P0/P1）', who: who(asRecord(config.challenge)?.provider) },
    { stage: '裁决', what: 'adjudicate', who: '代码' },
    { stage: '汇报', what: 'synthesize', who: '代码' },
  ]
}

function budgetLabel(budget?: Record<string, unknown>): string | undefined {
  if (!budget) return undefined
  const entries = Object.entries(budget)
  if (!entries.length) return undefined
  return `预算 ${entries.map(([key, value]) => `${key}: ${budgetValue(key, value)}`).join(' · ')}`
}

function budgetValue(key: string, value: unknown): string {
  if (key === 'deadline' && typeof value === 'number' && Number.isFinite(value) && value >= 0) {
    return formatDuration(value / 1_000_000)
  }
  return typeof value === 'object' ? JSON.stringify(value) : String(value)
}

function ProfilePanel({ profile }: { profile: Profile }) {
  const config = profile.config
  const steps = useMemo(() => profileSteps(config), [config])
  const budget = budgetLabel(asRecord(config?.budget))
  return (
    <Panel
      title={<><span className={styles.profileTitle}>{profile.name}</span> <small>只读</small></>}
      aside={budget ? <span className={styles.budget}>{budget}</span> : undefined}
    >
      <div className={styles.profileMeta}>
        <span>默认 provider：{text(config?.provider) ?? '未配置'}</span>
        <span>探索 → 四路并行 → 裁决 → 汇报</span>
      </div>
      {steps.length ? (
        <ol className={styles.profileSteps} aria-label={`${profile.name} 拓扑步骤`}>
          {steps.map((step, index) => (
            <li key={`${step.stage}-${index}`} className={styles.profileStep}>
              <span className={styles.profileStepN}>{index + 1} · {step.stage}</span>
              <span className={styles.profileStepWhat}>{step.what}</span>
              <span className={styles.profileStepWho}>{step.who}</span>
            </li>
          ))}
        </ol>
      ) : <span className="small muted">配置未提供拓扑步骤。</span>}
      <details className={ui.disclosure}>
        <summary>查看完整档位配置</summary>
        <pre className={`pre ${ui.jsonBlock}`}>{formatJson(config)}</pre>
      </details>
    </Panel>
  )
}

export function ModelsPage() {
  const canWrite = useCanWrite()
  const queryClient = useQueryClient()
  const modelsId = useId()
  const profilesId = useId()
  const models = useQuery({ queryKey: ['models'], queryFn: listModels, retry: false })
  const profiles = useQuery({ queryKey: ['profiles'], queryFn: () => listProfiles(), retry: false })
  const [editing, setEditing] = useState<Model>()
  const [drawerOpen, setDrawerOpen] = useState(false)
  const [deleting, setDeleting] = useState<Model>()
  const [deleteError, setDeleteError] = useState<unknown>()

  const saved = () => {
    void queryClient.invalidateQueries({ queryKey: ['models'] })
    void queryClient.invalidateQueries({ queryKey: ['providers'] })
    void queryClient.invalidateQueries({ queryKey: ['profiles'] })
  }

  const confirmDelete = async () => {
    if (!deleting) return
    try {
      await deleteModel(deleting.id, deleting.revision)
      setDeleting(undefined)
      setDeleteError(undefined)
      saved()
    } catch (error) {
      setDeleteError(error)
    }
  }

  const reviewProfiles = (profiles.data?.profiles ?? []).filter((profile) => profile.name === REVIEW_PROFILE)
  const onboarding = (profiles.data?.profiles ?? []).find((profile) => profile.name === 'onboarding')
  const onboardingProvider = text(asRecord(onboarding?.config?.agent)?.provider)

  return (
    <main className={ui.page}>
      <div className={ui.pageStack}>
        <PageHeader
          title="模型与档位"
          description="配置模型来自平台配置文件，用户模型可在控制台管理。API key 只写不回显。"
        />

        <section className={styles.section} aria-labelledby={modelsId}>
          <div className={styles.sectionHeader}>
            <h2 className={styles.sectionTitle} id={modelsId}>模型</h2>
            {canWrite ? <Button onClick={() => { setEditing(undefined); setDrawerOpen(true) }}>新增模型</Button> : null}
          </div>
          {models.isPending ? <LoadingState /> : null}
          {models.isError ? <ErrorState error={models.error} onRetry={models.refetch} /> : null}
          {models.isSuccess && models.data.models.length === 0 ? <EmptyState title="还没有模型" description="新增用户模型，或检查平台配置。" /> : null}
          <div className={styles.cards}>
            {models.data?.models.map((model) => (
              <ModelCard
                key={model.id}
                model={model}
                canWrite={canWrite}
                onEdit={() => { setEditing(model); setDrawerOpen(true) }}
                onDelete={() => { setDeleting(model); setDeleteError(undefined) }}
              />
            ))}
          </div>
        </section>

        <section className={styles.section} aria-labelledby={profilesId}>
          <div className={styles.sectionHeader}>
            <h2 className={styles.sectionTitle} id={profilesId}>评审档位</h2>
            <span className={styles.sectionAside}>定义来自平台配置，只读</span>
          </div>
          {profiles.isPending ? <LoadingState /> : null}
          {profiles.isError ? <ErrorState error={profiles.error} onRetry={profiles.refetch} /> : null}
          {profiles.isSuccess && !reviewProfiles.length ? <EmptyState title="平台还没有评审档位配置" /> : null}
          {reviewProfiles.map((profile) => <ProfilePanel key={profile.name} profile={profile} />)}
          {onboarding ? <span className="small muted">项目接入（onboarding，不是评审档位）使用 provider：{onboardingProvider ?? '未配置'}</span> : null}
        </section>
      </div>

      <ModelDrawer open={drawerOpen} model={editing} onClose={() => setDrawerOpen(false)} onSaved={saved} />
      <ConfirmDialog
        open={Boolean(deleting)}
        onClose={() => setDeleting(undefined)}
        onConfirm={() => { void confirmDelete() }}
        title="删除模型"
        description={deleting ? `删除 ${deleting.display_name}？进行中的评审继续使用旧 client。` : ''}
        confirmLabel="删除"
        error={deleteError}
      />
    </main>
  )
}
