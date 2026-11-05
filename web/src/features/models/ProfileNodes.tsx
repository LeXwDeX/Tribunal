import { useEffect, useId, useMemo, useRef, useState } from 'react'
import { deleteProfileOverrides, putProfileOverrides } from '../../api/api'
import { ApiError, errorMessage } from '../../api/client'
import type { Model, ProfileDetail, ProfileNodeParams, ProfileNodeView } from '../../api/types'
import { Badge, Button, ConfirmDialog, Drawer, Field } from '../../components/ui'
import ui from '../../styles/ui.module.css'
import styles from './profile.module.css'

export function shortID(id: string): string {
  return id.length > 16 ? `${id.slice(0, 8)}…${id.slice(-4)}` : id
}

export const PROFILE_NODES = [
  ['default', '默认'],
  ['explore', '探索'],
  ['lanes.code_review', 'code-review'],
  ['lanes.review', 'review'],
  ['lanes.reasoner', 'reasoner'],
  ['lanes.validate', 'validate'],
  ['challenge', 'challenge'],
] as const

const SOURCE_LABELS: Record<string, string> = {
  override: '覆盖',
  override_default: '继承覆盖默认',
  config: '配置',
  config_default: '配置默认',
  derived: '派生',
}

const SOURCE_TONES: Record<string, 'info' | 'warn' | 'neutral'> = {
  override: 'info',
  override_default: 'warn',
}

function sourceLabel(source?: string): string {
  return source ? SOURCE_LABELS[source] ?? source : '不可用'
}

function SourceBadge({ source }: { source?: string }) {
  return <Badge tone={(source && SOURCE_TONES[source]) || 'neutral'} plain>{sourceLabel(source)}</Badge>
}

function nodeLabel(node: string): string {
  return PROFILE_NODES.find(([name]) => name === node)?.[1] ?? node
}

function modelName(models: Model[], id: string): string {
  return models.find((model) => model.id === id)?.display_name ?? id
}

function paramsText(params?: ProfileNodeParams): string {
  if (!params) return '不可用'
  const parts: string[] = []
  if (params.max_tokens) parts.push(`max_tokens ${params.max_tokens}`)
  if (typeof params.temperature === 'number') parts.push(`temperature ${params.temperature}`)
  if (params.reasoning?.effort) parts.push(`reasoning.effort ${params.reasoning.effort}`)
  if (params.thinking?.budget_tokens) parts.push(`thinking.budget_tokens ${params.thinking.budget_tokens}`)
  return parts.length ? parts.join(' · ') : '无'
}

function sameModelGroups(nodes: ProfileNodeView[]): Array<{ identity: string; nodes: string[] }> {
  const groups = new Map<string, string[]>()
  for (const node of nodes) {
    if (!node.vote_identity) continue
    groups.set(node.vote_identity, [...(groups.get(node.vote_identity) ?? []), nodeLabel(node.node)])
  }
  return [...groups.entries()].filter(([, names]) => names.length >= 2).map(([identity, names]) => ({ identity, nodes: names }))
}

export function ProfileNodeList({ nodes, models }: { nodes: ProfileNodeView[]; models: Model[] }) {
  const groups = useMemo(() => sameModelGroups(nodes), [nodes])
  return (
    <>
      <ul className={styles.nodes} aria-label="节点生效模型">
        {nodes.map((node) => {
          const configuredDiffers = node.configured && node.configured !== node.model
          return (
            <li key={node.node} className={styles.node}>
              <div className={styles.nodeHead}>
                <span className={styles.nodeName}>{nodeLabel(node.node)}</span>
                <SourceBadge source={node.source} />
              </div>
              <div className={styles.nodeModel}>
                {node.model ? (
                  <>
                    <span>{modelName(models, node.model)}</span>
                    <span className="mono">{shortID(node.model)}</span>
                  </>
                ) : <span className={styles.muted}>继承</span>}
              </div>
              {configuredDiffers ? (
                <span className={styles.nodeMeta}>
                  配置值 {modelName(models, node.configured ?? '')}（{sourceLabel(node.configured_source)}）
                </span>
              ) : null}
              <span className={styles.nodeMeta}>
                生效参数 {paramsText(node.effective_params)}
                {node.param_source ? <> · 参数来源 {sourceLabel(node.param_source)}</> : null}
              </span>
            </li>
          )
        })}
      </ul>
      {groups.map((group) => (
        <p key={group.identity} className={styles.sameModel} role="note">
          这些节点会被计为同一模型：{group.nodes.join('、')}
        </p>
      ))}
    </>
  )
}

interface NodeForm {
  model: string
  maxTokens: string
  temperature: string
  effort: string
  budget: string
}

type FieldKey = keyof NodeForm | 'params'
type NodeErrors = Partial<Record<FieldKey, string>>

const EMPTY_FORM: NodeForm = { model: '', maxTokens: '', temperature: '', effort: '', budget: '' }

function docNode(doc: Record<string, unknown>, path: string): Record<string, unknown> | undefined {
  let current: unknown = doc
  for (const part of path.split('.')) {
    if (!current || typeof current !== 'object') return undefined
    current = (current as Record<string, unknown>)[part]
  }
  return current && typeof current === 'object' ? current as Record<string, unknown> : undefined
}

function formFromDoc(doc: Record<string, unknown>): Record<string, NodeForm> {
  const forms: Record<string, NodeForm> = {}
  for (const [node] of PROFILE_NODES) {
    const item = docNode(doc ?? {}, node) as { model?: string; params?: ProfileNodeParams } | undefined
    forms[node] = {
      model: item?.model ?? '',
      maxTokens: item?.params?.max_tokens ? String(item.params.max_tokens) : '',
      temperature: typeof item?.params?.temperature === 'number' ? String(item.params.temperature) : '',
      effort: item?.params?.reasoning?.effort ?? '',
      budget: item?.params?.thinking?.budget_tokens ? String(item.params.thinking.budget_tokens) : '',
    }
  }
  return forms
}

function isPositiveInt(value: string): boolean {
  return /^\d+$/.test(value) && Number(value) > 0
}

function validate(form: NodeForm): NodeErrors {
  const errors: NodeErrors = {}
  if (form.maxTokens && !isPositiveInt(form.maxTokens)) errors.maxTokens = 'max_tokens 必须是正整数。'
  const temperature = form.temperature.trim()
  if (form.temperature && !(/^(\d+\.?\d*|\.\d+)$/.test(temperature) && Number(temperature) <= 2)) {
    errors.temperature = 'temperature 必须在 0 到 2 之间。'
  }
  if (form.budget && !isPositiveInt(form.budget)) errors.budget = 'thinking.budget_tokens 必须是正整数。'
  return errors
}

function buildDocument(forms: Record<string, NodeForm>): Record<string, unknown> {
  const doc: Record<string, unknown> = {}
  for (const [node] of PROFILE_NODES) {
    const form = forms[node] ?? EMPTY_FORM
    const params: ProfileNodeParams = {}
    if (form.maxTokens) params.max_tokens = Number(form.maxTokens)
    if (form.temperature) params.temperature = Number(form.temperature.trim())
    if (form.effort) params.reasoning = { effort: form.effort }
    if (form.budget) params.thinking = { budget_tokens: Number(form.budget) }
    const item: Record<string, unknown> = {}
    if (form.model) item.model = form.model
    if (Object.keys(params).length) item.params = params
    if (!Object.keys(item).length) continue
    if (node.startsWith('lanes.')) {
      const lanes = (doc.lanes as Record<string, unknown>) ?? {}
      lanes[node.slice('lanes.'.length)] = item
      doc.lanes = lanes
    } else {
      doc[node] = item
    }
  }
  return doc
}

const PARAM_FIELDS: Array<[string, FieldKey]> = [
  ['max_tokens', 'maxTokens'], ['temperature', 'temperature'],
  ['reasoning.effort', 'effort'], ['thinking.budget_tokens', 'budget'],
]

// mapFieldError routes an API validation path such as
// overrides.lanes.validate.params.thinking.budget_tokens to the matching input.
function mapFieldError(field: string, message: string): { node: string; key: FieldKey } | undefined {
  if (!field.startsWith('overrides.')) return undefined
  const path = field.slice('overrides.'.length)
  const nodes = PROFILE_NODES.map(([name]) => name).sort((a, b) => b.length - a.length)
  const node = nodes.find((name) => path === name || path.startsWith(`${name}.`))
  if (!node) return undefined
  const rest = path.slice(node.length + 1)
  if (rest === 'model') return { node, key: 'model' }
  const param = PARAM_FIELDS.find(([name]) => rest === `params.${name}` || (rest === 'params' && message.includes(`.${name}`)))
  if (param) return { node, key: param[1] }
  if (rest === 'params') return { node, key: 'params' }
  return undefined
}

export function ProfileEditor({ open, detail, models, onClose, onSaved, onReload }: {
  open: boolean
  detail?: ProfileDetail
  models: Model[]
  onClose: () => void
  onSaved: () => void
  onReload: () => Promise<boolean>
}) {
  const baseId = useId()
  const [forms, setForms] = useState<Record<string, NodeForm>>({})
  const [errors, setErrors] = useState<Record<string, NodeErrors>>({})
  const [formError, setFormError] = useState<string>()
  const [stale, setStale] = useState(false)
  const [pending, setPending] = useState(false)
  const [confirmReset, setConfirmReset] = useState(false)
  const [focusTick, setFocusTick] = useState(0)
  const bodyRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (open && detail) {
      setForms(formFromDoc(detail.overrides))
      setErrors({})
      setFormError(undefined)
      setStale(false)
    }
  }, [open, detail])

  // After an error render, move focus to the first invalid input.
  useEffect(() => {
    if (!focusTick) return
    const target = bodyRef.current?.querySelector<HTMLElement>('[aria-invalid="true"]')
    if (!target) return
    target.focus()
    target.scrollIntoView?.({ block: 'center' })
  }, [focusTick])

  const hasOverrides = Boolean(detail && Object.keys(detail.overrides ?? {}).length)

  const update = (node: string, patch: Partial<NodeForm>) => {
    setForms((current) => ({ ...current, [node]: { ...(current[node] ?? EMPTY_FORM), ...patch } }))
    setErrors((current) => {
      const existing = current[node]
      if (!existing) return current
      const next: NodeErrors = { ...existing }
      for (const key of Object.keys(patch) as Array<keyof NodeForm>) delete next[key]
      if (Object.keys(patch).some((key) => key !== 'model')) delete next.params
      return { ...current, [node]: next }
    })
  }

  const handleError = (err: unknown) => {
    if (err instanceof ApiError && err.status === 409) {
      setStale(true)
      setFormError('档位覆盖已被他人修改，请刷新并重新加载后再保存。')
      return
    }
    if (err instanceof ApiError && err.status === 400 && err.details?.length) {
      const mapped: Record<string, NodeErrors> = {}
      const unmapped: string[] = []
      for (const detailItem of err.details) {
        const target = mapFieldError(detailItem.field ?? '', detailItem.message ?? '')
        if (!target) {
          unmapped.push([detailItem.field, detailItem.message].filter(Boolean).join(': '))
          continue
        }
        const message = detailItem.message || err.message
        const previous = mapped[target.node]?.[target.key]
        mapped[target.node] = { ...mapped[target.node], [target.key]: previous ? `${previous}；${message}` : message }
      }
      setErrors(mapped)
      setFormError(unmapped.length ? unmapped.join('；') : undefined)
      setFocusTick((tick) => tick + 1)
      return
    }
    setFormError(errorMessage(err))
  }

  const save = async () => {
    if (!detail) return
    const local: Record<string, NodeErrors> = {}
    for (const [node] of PROFILE_NODES) {
      const found = validate(forms[node] ?? EMPTY_FORM)
      if (Object.keys(found).length) local[node] = found
    }
    setErrors(local)
    setFormError(undefined)
    if (Object.keys(local).length) {
      setFocusTick((tick) => tick + 1)
      return
    }
    setPending(true)
    try {
      await putProfileOverrides(buildDocument(forms), detail.revision)
      onSaved()
      onClose()
    } catch (err) {
      handleError(err)
    } finally {
      setPending(false)
    }
  }

  const reset = async () => {
    if (!detail) return
    setPending(true)
    setErrors({})
    setFormError(undefined)
    try {
      await deleteProfileOverrides(detail.revision)
      onSaved()
      onClose()
    } catch (err) {
      handleError(err)
    } finally {
      setPending(false)
    }
  }

  const reload = async () => {
    // A changed document resets the form through the effect above; an
    // unchanged one keeps the user's edits. The notice stays until the
    // refetch succeeds.
    setPending(true)
    try {
      if (await onReload()) {
        setStale(false)
        setFormError(undefined)
      } else {
        setFormError('重新加载失败，请稍后重试。')
      }
    } finally {
      setPending(false)
    }
  }

  return (
    <>
    <Drawer
      open={open}
      onClose={onClose}
      title="编辑评审档位"
      description="留空表示继承配置；保存后对之后启动的评审生效。"
      footer={(
        <>
          <Button variant="secondary" onClick={() => setConfirmReset(true)} loading={pending} disabled={!hasOverrides}>恢复为配置</Button>
          <Button variant="secondary" onClick={onClose}>取消</Button>
          <Button onClick={() => { void save() }} loading={pending} disabled={stale}>保存</Button>
        </>
      )}
    >
      <div ref={bodyRef} className={styles.editor}>
      {formError || stale ? (
        <div className={styles.formAlert}>
          {formError ? <span className={styles.errorText} role="alert">{formError}</span> : null}
          {stale ? <Button variant="secondary" onClick={() => { void reload() }} loading={pending}>刷新并重新加载</Button> : null}
        </div>
      ) : null}
      {PROFILE_NODES.map(([node, label]) => {
        const form = forms[node] ?? EMPTY_FORM
        const nodeErrors = errors[node] ?? {}
        const id = `${baseId}-${node}`
        const paramsErr = nodeErrors.params ? `${id}-params-error` : undefined
        const desc = (key: FieldKey, htmlFor: string, withParams = false) => [nodeErrors[key] ? `${htmlFor}-error` : undefined, withParams ? paramsErr : undefined].filter(Boolean).join(' ') || undefined
        const dirty = Object.values(form).some(Boolean)
        return (
          <fieldset key={node} className={styles.fieldset}>
            <legend className={styles.legend}>{label}</legend>
            <Field label={`${label} 模型`} htmlFor={`${id}-model`} error={nodeErrors.model}>
              <select
                id={`${id}-model`}
                className={ui.input}
                value={form.model}
                aria-invalid={nodeErrors.model ? true : undefined}
                aria-describedby={desc('model', `${id}-model`)}
                onChange={(event) => update(node, { model: event.target.value })}
              >
                <option value="">继承</option>
                {models.map((model) => (
                  <option key={model.id} value={model.id} disabled={!model.available}>
                    {model.display_name}（{shortID(model.id)}）{model.available ? '' : `不可用：${model.unavailable_reason || '未知原因'}`}
                  </option>
                ))}
              </select>
            </Field>
            <div className={styles.paramGrid}>
              <Field label={`${label} max_tokens`} htmlFor={`${id}-max`} error={nodeErrors.maxTokens}>
                <input id={`${id}-max`} className={ui.input} inputMode="numeric" placeholder="继承" value={form.maxTokens} aria-invalid={nodeErrors.maxTokens ? true : undefined} aria-describedby={desc('maxTokens', `${id}-max`, true)} onChange={(event) => update(node, { maxTokens: event.target.value })} />
              </Field>
              <Field label={`${label} temperature`} htmlFor={`${id}-temp`} error={nodeErrors.temperature}>
                <input id={`${id}-temp`} className={ui.input} inputMode="decimal" placeholder="继承" value={form.temperature} aria-invalid={nodeErrors.temperature ? true : undefined} aria-describedby={desc('temperature', `${id}-temp`, true)} onChange={(event) => update(node, { temperature: event.target.value })} />
              </Field>
              <Field label={`${label} reasoning.effort`} htmlFor={`${id}-effort`} error={nodeErrors.effort}>
                <select id={`${id}-effort`} className={ui.input} value={form.effort} aria-invalid={nodeErrors.effort ? true : undefined} aria-describedby={desc('effort', `${id}-effort`, true)} onChange={(event) => update(node, { effort: event.target.value })}>
                  <option value="">继承</option>
                  <option value="low">low</option>
                  <option value="medium">medium</option>
                  <option value="high">high</option>
                  <option value="max">max</option>
                </select>
              </Field>
              <Field label={`${label} thinking.budget_tokens`} htmlFor={`${id}-budget`} error={nodeErrors.budget}>
                <input id={`${id}-budget`} className={ui.input} inputMode="numeric" placeholder="继承" value={form.budget} aria-invalid={nodeErrors.budget ? true : undefined} aria-describedby={desc('budget', `${id}-budget`, true)} onChange={(event) => update(node, { budget: event.target.value })} />
              </Field>
            </div>
            {nodeErrors.params ? <span className={styles.errorText} id={paramsErr} role="alert">{nodeErrors.params}</span> : null}
            {dirty ? (
              <Button variant="secondary" className={ui.smallButton} onClick={() => update(node, EMPTY_FORM)}>
                {`清除${label}覆盖`}
              </Button>
            ) : null}
          </fieldset>
        )
      })}
      </div>
    </Drawer>
    <ConfirmDialog
      open={confirmReset}
      onClose={() => setConfirmReset(false)}
      onConfirm={() => { setConfirmReset(false); void reset() }}
      title="恢复为配置"
      description="将清除所有节点的模型和参数覆盖，恢复为配置文件的值；对之后启动的评审生效。"
      confirmLabel="恢复为配置"
    />
    </>
  )
}
