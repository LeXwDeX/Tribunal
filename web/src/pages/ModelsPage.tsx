import { useId, useMemo, useState } from 'react'
import { usePaginated } from '../lib/hooks'
import { useCanWrite } from '../components/RoleContext'
import { listProfiles, listProviders, testProvider } from '../api/api'
import { errorMessage } from '../api/client'
import type { Profile, ProfileConfig, Provider, ProviderTestResult } from '../api/types'
import {
  Badge, Button, EmptyState, ErrorState, LoadingState, PageHeader, Panel,
} from '../components/ui'
import { formatDuration, formatJson, formatNumber } from '../lib/format'
import ui from '../styles/ui.module.css'
import styles from './insights.module.css'

function fmt(value: number | undefined, suffix = ''): string {
  return typeof value === 'number' && Number.isFinite(value) ? `${formatNumber(value)}${suffix}` : '不可用'
}

type TestState = { pending?: boolean; result?: ProviderTestResult; invalid?: boolean; error?: string }

function ProviderCard({ provider, canWrite }: { provider: Provider; canWrite: boolean }) {
  const [test, setTest] = useState<TestState>({})
  const titleId = useId()
  const stats = provider.stats_24h
  const runTest = async () => {
    setTest({ pending: true })
    try {
      const result = await testProvider(provider.name)
      if (typeof result?.ok !== 'boolean') {
        setTest({ invalid: true, result })
      } else {
        setTest({ result })
      }
    } catch (error) {
      setTest({ error: errorMessage(error) })
    }
  }
  const testOk = test.result?.ok === true
  const testFailed = test.result?.ok === false || test.invalid || Boolean(test.error)

  const errorRate = typeof stats?.error_rate === 'number'
    ? `${(stats.error_rate <= 1 ? stats.error_rate * 100 : stats.error_rate).toFixed(2)}%`
    : '不可用'
  const p50 = typeof stats?.p50_latency_ms === 'number' ? formatDuration(stats.p50_latency_ms) : '不可用'
  const cells: Array<[string, string]> = [['24h 调用', fmt(stats?.calls)], ['错误率', errorRate], ['P50 延迟', p50]]

  return (
    <article className={`${ui.panel} ${styles.providerCard}`} aria-labelledby={titleId}>
      <div className={styles.providerHead}>
        <span className={styles.cardTitle} id={titleId}>{provider.name}</span>
        {provider.protocol ? <Badge tone="info" plain>{provider.protocol}</Badge> : <Badge tone="neutral" plain>协议未提供</Badge>}
      </div>
      <div className={styles.cardMeta}>
        <span>模型 <span className="mono">{provider.model || '不可用'}</span></span>
        {typeof stats?.errors === 'number' ? <span>近 24 小时错误 {fmt(stats.errors)}</span> : null}
      </div>
      <ul className={styles.cardStats} aria-label={`${provider.name} 近 24 小时`}>
        {cells.map(([label, value]) => (
          <li key={label}>
            <span>{label}</span>
            <b className={value === '不可用' ? styles.statUnavailable : undefined}>{value}</b>
          </li>
        ))}
      </ul>
      <div className={styles.cardMeta}>
        <span>并发 {provider.max_concurrency !== undefined ? provider.max_concurrency : '不可用'} · {provider.rpm !== undefined ? provider.rpm : '不可用'} RPM</span>
        <span>base_url <code>{provider.base_url || '不可用'}</code></span>
        <span>API key 环境变量 <code>{provider.api_key_env || '不可用'}</code></span>
      </div>
      {provider.last_error ? <span className={styles.resultError}>{provider.last_error}</span> : null}
      {canWrite ? (
        <div className={styles.cardActions}>
          <Button variant="secondary" className={ui.smallButton} onClick={() => { void runTest() }} loading={test.pending}>测试连通性</Button>
        </div>
      ) : <span className="small muted">只读账号不能发起连通性测试。</span>}
      <div className={styles.testResult} role="status">
        {test.pending ? '正在发送最小请求…' : null}
        {test.error ? <span className={styles.resultError}>请求失败：{test.error}</span> : null}
        {test.invalid ? <span className={styles.resultError}>测试响应无效：缺少 ok 布尔字段，不能判定成功。</span> : null}
        {test.result ? (
          <div className={testOk ? styles.resultOk : testFailed ? styles.resultError : ''}>
            {test.result.ok === true ? '测试成功。' : test.result.ok === false ? '测试失败。' : '结果不可判定。'}
            {test.result.message ? ` ${test.result.message}` : ''}
            {test.result.model ? ` 模型：${test.result.model}。` : ''}
            {typeof test.result.latency_ms === 'number' ? ` 延迟：${formatDuration(test.result.latency_ms)}。` : ''}
            {test.result.error ? ` 错误：${test.result.error}` : ''}
          </div>
        ) : null}
      </div>
    </article>
  )
}

function asRecord(value: unknown): Record<string, unknown> | undefined {
  return value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : undefined
}

interface Step { stage: string; what: string; who: string }

function profileSteps(config?: ProfileConfig): Step[] {
  if (!config) return []
  const steps: Step[] = []
  const plan = asRecord(config.plan)
  if (plan) {
    steps.push({ stage: '规划', what: typeof plan.mode === 'string' ? `mode=${plan.mode}` : '规划配置', who: typeof plan.provider === 'string' ? plan.provider : '代码' })
  }
  const propose = Array.isArray(config.propose) ? config.propose : []
  for (const entry of propose) {
    const item = asRecord(entry)
    if (!item) continue
    steps.push({
      stage: '提出',
      what: typeof item.block === 'string' ? item.block : '提出方',
      who: typeof item.provider === 'string' ? item.provider : '未配置 provider',
    })
  }
  const reasoner = asRecord(config.reasoner)
  if (reasoner) {
    steps.push({ stage: '推理', what: typeof reasoner.when === 'string' ? `when=${reasoner.when}` : 'reasoner', who: typeof reasoner.provider === 'string' ? reasoner.provider : '未配置 provider' })
  }
  const challenge = asRecord(config.challenge)
  if (challenge) {
    const only = Array.isArray(challenge.only) ? challenge.only.map(String).join(', ') : ''
    steps.push({
      stage: '挑战',
      what: only ? `仅 ${only}` : '挑战方',
      who: typeof challenge.provider === 'string'
        ? `${challenge.provider}${typeof challenge.fallback === 'string' ? `（fallback ${challenge.fallback}）` : ''}`
        : '未配置 provider',
    })
  }
  const synthesize = asRecord(config.synthesize)
  if (synthesize) {
    steps.push({ stage: '汇总', what: typeof synthesize.mode === 'string' ? `mode=${synthesize.mode}` : '汇总配置', who: typeof synthesize.provider === 'string' ? synthesize.provider : '代码' })
  }
  return steps
}

function budgetLabel(budget?: Record<string, unknown>): string | undefined {
  if (!budget) return undefined
  const entries = Object.entries(budget)
  if (!entries.length) return undefined
  return `预算 ${entries.map(([key, value]) => `${key}: ${typeof value === 'object' ? JSON.stringify(value) : String(value)}`).join(' · ')}`
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
        <span>on_uncertain：{config?.on_uncertain || '不可用'}</span>
        <span>consensus：{config?.consensus ? '已配置' : '未配置'}</span>
        <span>agent：{config?.agent ? '已配置' : '未配置'}</span>
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
  const providersId = useId()
  const profilesId = useId()
  const providers = usePaginated<Provider>(
    ['providers', 'models'],
    (cursor) => listProviders(cursor),
    'providers',
  )
  const profiles = usePaginated<Profile>(
    ['profiles', 'models'],
    (cursor) => listProfiles(cursor),
    'profiles',
  )

  return (
    <main className={ui.page}>
      <div className={ui.pageStack}>
        <PageHeader
          title="模型与档位"
          description="来自平台配置文件，首版只读。API key 只从环境变量读取，界面不显示密钥值。"
        />

        <section className={styles.section} aria-labelledby={providersId}>
          <div className={styles.sectionHeader}>
            <h2 className={styles.sectionTitle} id={providersId}>Provider</h2>
            {providers.isSuccess ? <span className={styles.sectionAside}>{providers.items.length}{providers.hasNextPage ? '+' : ''} 个</span> : null}
          </div>
          {providers.isPending ? <LoadingState /> : null}
          {providers.isError ? <ErrorState error={providers.error} onRetry={providers.retry} /> : null}
          {providers.isSuccess && providers.items.length === 0 ? <EmptyState title="平台还没有配置 provider" description="在服务端配置文件中添加 provider 并重启或热加载后，会显示在这里。" /> : null}
          {providers.items.length ? (
            <div className={styles.cards}>
              {providers.items.map((provider) => <ProviderCard key={provider.name} provider={provider} canWrite={canWrite} />)}
            </div>
          ) : null}
          {providers.hasNextPage ? (
            <div className={styles.loadMore}>
              <Button variant="secondary" onClick={providers.fetchNextPage} loading={providers.isFetchingNextPage}>加载更多 Provider</Button>
            </div>
          ) : null}
        </section>

        <section className={styles.section} aria-labelledby={profilesId}>
          <div className={styles.sectionHeader}>
            <h2 className={styles.sectionTitle} id={profilesId}>评审档位</h2>
            <span className={styles.sectionAside}>定义来自平台配置，只读</span>
          </div>
          {profiles.isPending ? <LoadingState /> : null}
          {profiles.isError ? <ErrorState error={profiles.error} onRetry={profiles.retry} /> : null}
          {profiles.isSuccess && profiles.items.length === 0 ? <EmptyState title="平台还没有评审档位配置" /> : null}
          {profiles.items.map((profile) => <ProfilePanel key={profile.name} profile={profile} />)}
          {profiles.hasNextPage ? (
            <div className={styles.loadMore}>
              <Button variant="secondary" onClick={profiles.fetchNextPage} loading={profiles.isFetchingNextPage}>加载更多档位</Button>
            </div>
          ) : null}
        </section>
      </div>
    </main>
  )
}

