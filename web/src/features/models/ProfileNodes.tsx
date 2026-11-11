import { useEffect, useId, useMemo, useRef, useState } from 'react'
import { deleteProfileOverrides, putProfileOverrides } from '../../api/api'
import { ApiError, errorMessage } from '../../api/client'
import type { Model, ProfileDetail, ProfileNodeParams, ProfileNodeView } from '../../api/types'
import {
  ActionHint, Badge, Button, ConfirmDialog, Drawer, Field, FieldHint, Note, NumberField, Select,
} from '../../components/ui'
import ui from '../../styles/ui.module.css'
import styles from './profile.module.css'

export function shortID(id: string): string {
  return id.length > 16 ? `${id.slice(0, 8)}…${id.slice(-4)}` : id
}

export const PROFILE_NODES = [
  ['default', '全局默认'],
  ['explore', '探索'],
  ['lanes.code_review', 'code-review'],
  ['lanes.review', 'review'],
  ['lanes.reasoner', 'reasoner'],
  ['lanes.validate', 'validate'],
  ['challenge', 'challenge'],
] as const

const CONFIGURED_NODES = PROFILE_NODES.filter(([node]) => node !== 'default')

const SOURCE_LABELS: Record<string, string> = {
  override: '覆盖',
  override_default: '全局默认',
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

// paramSourceLabel names where the effective params come from: a node or
// default override, or the model itself when no override sets the field.
function paramSourceLabel(source?: string): string {
  if (!source) return '模型'
  return source === 'override_default' ? '全局默认' : sourceLabel(source)
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
  if (params.thinking?.type) parts.push(`thinking.type ${params.thinking.type}`)
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
  const defaultNode = nodes.find((node) => node.node === 'default')
  const configuredNodes = nodes.filter((node) => node.node !== 'default')
  return (
    <>
      {defaultNode ? (
        <section className={styles.globalDefault} aria-label="全局默认">
          <div className={styles.nodeHead}>
            <h3 className={styles.nodeName}>全局默认</h3>
            <SourceBadge source={defaultNode.source} />
          </div>
          <div className={styles.nodeModel}>
            {defaultNode.model ? (
              <>
                <span>{modelName(models, defaultNode.model)}</span>
                <span className="mono">{shortID(defaultNode.model)}</span>
              </>
            ) : <span className={styles.muted}>不可用</span>}
          </div>
          {defaultNode.configured && defaultNode.configured !== defaultNode.model ? (
            <span className={styles.nodeMeta}>
              配置值 {modelName(models, defaultNode.configured)}（{sourceLabel(defaultNode.configured_source)}）
            </span>
          ) : null}
          <span className={styles.nodeMeta}>
            生效参数 {paramsText(defaultNode.effective_params)}
            {defaultNode.effective_params ? <> · 参数来源 {paramSourceLabel(defaultNode.param_source)}</> : null}
          </span>
          <span className={styles.nodeMeta}>
            全局默认覆盖逐字段作用于下方节点；节点未覆盖字段继承全局默认，仍未设置的继承自身模型
          </span>
        </section>
      ) : null}
      <ul className={styles.nodes} aria-label="节点生效模型">
        {configuredNodes.map((node) => {
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
                {node.effective_params ? <> · 参数来源 {paramSourceLabel(node.param_source)}</> : null}
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
  // thinking.type is openai-chat only. It is part of the document even when
  // budget_tokens is inherited, so it must round-trip like the other params.
  thinkingType: string
}

type ParamKey = 'maxTokens' | 'temperature' | 'effort' | 'budget' | 'thinkingType'
type FieldKey = keyof NodeForm | 'params'
type NodeErrors = Partial<Record<FieldKey, string>>

const EMPTY_FORM: NodeForm = { model: '', maxTokens: '', temperature: '', effort: '', budget: '', thinkingType: '' }

interface ParamBag {
  max_tokens?: number
  temperature?: number | null
  reasoning?: { effort?: string } | null
  thinking?: { budget_tokens?: number; type?: string } | null
}

const EFFORT_OPTIONS = [
  { value: '', label: '继承' },
  { value: 'low', label: 'low' },
  { value: 'medium', label: 'medium' },
  { value: 'high', label: 'high' },
  { value: 'max', label: 'max' },
]

const THINKING_TYPE_OPTIONS = [
  { value: '', label: '继承' },
  { value: 'enabled', label: 'enabled' },
  { value: 'disabled', label: 'disabled' },
]

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
      thinkingType: item?.params?.thinking?.type ?? '',
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
  if (form.thinkingType && form.thinkingType !== 'enabled' && form.thinkingType !== 'disabled') {
    errors.thinkingType = 'thinking.type 只能是 enabled 或 disabled。'
  }
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
    if (form.budget || form.thinkingType) {
      params.thinking = {}
      if (form.budget) params.thinking.budget_tokens = Number(form.budget)
      if (form.thinkingType) params.thinking.type = form.thinkingType
    }
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
  ['thinking.type', 'thinkingType'],
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

function isDirty(form: NodeForm): boolean {
  return Object.values(form).some(Boolean)
}

function modelText(models: Model[], id: string): string {
  return id ? `${modelName(models, id)}（${shortID(id)}）` : '未配置'
}

function fieldValue(params: ParamBag | undefined, key: ParamKey): string {
  switch (key) {
    case 'maxTokens': {
      const value = params?.max_tokens
      return typeof value === 'number' && value > 0 ? String(value) : ''
    }
    case 'temperature': {
      const value = params?.temperature
      return typeof value === 'number' ? String(value) : ''
    }
    case 'effort':
      return params?.reasoning?.effort ?? ''
    case 'budget': {
      const value = params?.thinking?.budget_tokens
      return typeof value === 'number' && value > 0 ? String(value) : ''
    }
    case 'thinkingType':
      return params?.thinking?.type ?? ''
  }
}

interface InheritedValue {
  value: string
  source: '全局默认' | '模型'
  unknown?: boolean
}

interface InheritedThinking {
  budget: string
  type: string
  source: '全局默认' | '模型'
}

function paramHint(inherited: InheritedValue, overridden: boolean): string {
  const prefix = overridden ? '覆盖中 · ' : ''
  if (inherited.unknown) return `${prefix}继承值：未知`
  return `${prefix}继承值：${inherited.value || '未设置'} · 来源：${inherited.source}`
}

function modelHint(models: Model[], id: string, source: string, overridden: boolean): string {
  const prefix = overridden ? '覆盖中 · ' : ''
  if (!id) return `${prefix}继承值：未知（无配置来源）`
  return `${prefix}继承值：${modelText(models, id)} · 来源：${source}`
}

// lookupModel separates a catalog that has not loaded (or is empty) from a
// loaded catalog that does not contain the effective model. effective_params
// is only a safe fallback for the latter; the former must say unknown so a
// just-cleared saved override is never presented as an inherited value.
function lookupModel(models: Model[], id: string): { model?: Model; missing: boolean; unknown: boolean } {
  if (!id || !models.length) return { missing: false, unknown: true }
  const model = models.find((item) => item.id === id)
  return model ? { model, missing: false, unknown: false } : { missing: true, unknown: false }
}

function effectiveModelID(form: NodeForm, defaultForm: NodeForm, node?: ProfileNodeView): string {
  // view.model may itself be the node override; only form.model, the global
  // default and the configuration value can act as the inherited model.
  return form.model || defaultForm.model || node?.configured || ''
}

// inheritedNodeParam is what a node would use after clearing its own value:
// the global default override if it sets the field, otherwise the model.
function inheritedNodeParam(
  key: ParamKey,
  form: NodeForm,
  defaultForm: NodeForm,
  node: ProfileNodeView | undefined,
  models: Model[],
): InheritedValue {
  if (defaultForm[key]) return { value: defaultForm[key], source: '全局默认' }
  const lookup = lookupModel(models, effectiveModelID(form, defaultForm, node))
  if (lookup.model) return { value: fieldValue(lookup.model.params, key), source: '模型' }
  if (lookup.missing && !form[key] && node?.effective_params) {
    return { value: fieldValue(node.effective_params, key), source: '模型' }
  }
  return { value: '', source: '模型', unknown: true }
}

// inheritedModelParam is the model's own value for a global default field.
function inheritedModelParam(
  key: ParamKey,
  form: NodeForm,
  node: ProfileNodeView | undefined,
  models: Model[],
): InheritedValue {
  const lookup = lookupModel(models, form.model || node?.configured || '')
  if (lookup.model) return { value: fieldValue(lookup.model.params, key), source: '模型' }
  if (lookup.missing && !form[key] && node?.effective_params) {
    return { value: fieldValue(node.effective_params, key), source: '模型' }
  }
  return { value: '', source: '模型', unknown: true }
}

function inheritedModelSource(defaultForm: NodeForm, node?: ProfileNodeView): string {
  if (defaultForm.model) return '全局默认'
  return sourceLabel(node?.configured_source ?? node?.source)
}

function modelThinking(model: Model): InheritedThinking {
  return {
    budget: fieldValue(model.params, 'budget'),
    type: fieldValue(model.params, 'thinkingType'),
    source: '模型',
  }
}

// inheritedNodeThinking returns the thinking object the node inherits as a
// whole: the global default's object when it overrides thinking, otherwise the
// effective model's object. thinking is never merged field by field.
function inheritedNodeThinking(
  defaultForm: NodeForm,
  form: NodeForm,
  node: ProfileNodeView | undefined,
  models: Model[],
): InheritedThinking | undefined {
  if (defaultForm.budget || defaultForm.thinkingType) {
    return { budget: defaultForm.budget, type: defaultForm.thinkingType, source: '全局默认' }
  }
  const lookup = lookupModel(models, effectiveModelID(form, defaultForm, node))
  return lookup.model ? modelThinking(lookup.model) : undefined
}

function inheritedDefaultThinking(
  form: NodeForm,
  node: ProfileNodeView | undefined,
  models: Model[],
): InheritedThinking | undefined {
  const lookup = lookupModel(models, form.model || node?.configured || '')
  return lookup.model ? modelThinking(lookup.model) : undefined
}

// thinkingHints follows the backend's whole-object replacement: once this
// layer sets budget_tokens or type, the other subfield does not inherit.
function thinkingHints(
  form: NodeForm,
  inherited: InheritedThinking | undefined,
): { budget: string; thinkingType: string } {
  if (form.budget || form.thinkingType) {
    const whole = 'thinking 由本层整体覆盖；未填写的子字段不继承'
    return {
      budget: `${form.budget ? '覆盖中 · ' : ''}${whole}`,
      thinkingType: `${form.thinkingType ? '覆盖中 · ' : ''}${whole}`,
    }
  }
  if (!inherited) return { budget: '继承值：未知', thinkingType: '继承值：未知' }
  const object = `继承 thinking：budget_tokens ${inherited.budget || '未设置'} · type ${inherited.type || '未设置'} · 来源：${inherited.source}`
  return { budget: object, thinkingType: object }
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
  const alertRef = useRef<HTMLDivElement>(null)

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

  // The error notice sits below every fieldset, past the scroll position that
  // triggered it. Bring it into view and focus its action (or the notice).
  useEffect(() => {
    if (!formError) return
    const container = alertRef.current
    if (!container) return
    container.scrollIntoView?.({ block: 'nearest' })
    const target = container.querySelector<HTMLElement>('button') ?? container
    target.focus()
  }, [formError])

  const hasOverrides = Boolean(detail && Object.keys(detail.overrides ?? {}).length)
  const nodeByName = useMemo(() => new Map((detail?.nodes ?? []).map((node) => [node.node, node])), [detail])

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

  const defaultForm = forms.default ?? EMPTY_FORM
  const defaultNode = nodeByName.get('default')
  const defaultErrors = errors.default ?? {}
  const defaultId = `${baseId}-default`
  const defaultDirty = isDirty(defaultForm)
  const defaultParamsErr = defaultErrors.params ? `${defaultId}-params-error` : undefined
  const defaultModelInherited = defaultNode?.configured || ''
  const defaultHintFor = (key: ParamKey) => paramHint(inheritedModelParam(key, defaultForm, defaultNode, models), Boolean(defaultForm[key]))
  const defaultThinking = thinkingHints(defaultForm, inheritedDefaultThinking(defaultForm, defaultNode, models))
  const nodeHintFor = (node: ProfileNodeView | undefined, form: NodeForm) => (key: ParamKey) => {
    const inherited = inheritedNodeParam(key, form, defaultForm, node, models)
    return paramHint(inherited, Boolean(form[key]))
  }
  const nodeThinkingFor = (node: ProfileNodeView | undefined, form: NodeForm) => thinkingHints(form, inheritedNodeThinking(defaultForm, form, node, models))

  return (
    <>
    <Drawer
      open={open}
      onClose={onClose}
      title="编辑评审档位"
      description="全局默认作用于 6 个节点；节点留空或清除覆盖即继承全局默认，未设置的继承模型。对之后启动的评审生效。"
      footer={(
        <>
          <Button variant="secondary" onClick={() => setConfirmReset(true)} loading={pending} disabled={!hasOverrides}>恢复为配置</Button>
          <Button variant="secondary" onClick={onClose}>取消</Button>
          <Button onClick={() => { void save() }} loading={pending} disabled={stale}>保存</Button>
        </>
      )}
    >
      <div ref={bodyRef} className={styles.editor}>
      <Note tone="info">
        节点输入下方显示继承值与来源；保存只提交实际覆盖项。
      </Note>
      <fieldset className={`${styles.fieldset} ${styles.globalCard}`}>
        <legend className={styles.legend}>全局默认</legend>
        <Field
          label="全局默认 模型"
          htmlFor={`${defaultId}-model`}
          hint={modelHint(models, defaultModelInherited, sourceLabel(defaultNode?.configured_source), Boolean(defaultForm.model))}
          error={defaultErrors.model}
        >
          <Select
            id={`${defaultId}-model`}
            value={defaultForm.model}
            options={[
              { value: '', label: '继承配置' },
              ...models.map((model) => ({
                value: model.id,
                label: `${model.display_name}（${shortID(model.id)}）${model.available ? '' : `不可用：${model.unavailable_reason || '未知原因'}`}`,
                disabled: !model.available,
              })),
            ]}
            onChange={(value) => update('default', { model: value })}
            aria-invalid={defaultErrors.model ? true : undefined}
            aria-describedby={defaultParamsErr}
            emptyLabel="继承配置"
          />
        </Field>
        <div className={styles.paramGrid}>
          <Field label="全局默认 max_tokens" htmlFor={`${defaultId}-max`} hint={defaultHintFor('maxTokens')} error={defaultErrors.maxTokens}>
            <NumberField
              id={`${defaultId}-max`}
              placeholder="继承模型"
              value={defaultForm.maxTokens}
              aria-invalid={defaultErrors.maxTokens ? true : undefined}
              aria-describedby={defaultParamsErr}
              onChange={(event) => update('default', { maxTokens: event.target.value })}
            />
          </Field>
          <Field label="全局默认 temperature" htmlFor={`${defaultId}-temp`} hint={defaultHintFor('temperature')} error={defaultErrors.temperature}>
            <NumberField
              id={`${defaultId}-temp`}
              step="any"
              placeholder="继承模型"
              value={defaultForm.temperature}
              aria-invalid={defaultErrors.temperature ? true : undefined}
              aria-describedby={defaultParamsErr}
              onChange={(event) => update('default', { temperature: event.target.value })}
            />
          </Field>
          <Field label="全局默认 reasoning.effort" htmlFor={`${defaultId}-effort`} hint={defaultHintFor('effort')} error={defaultErrors.effort}>
            <Select
              id={`${defaultId}-effort`}
              value={defaultForm.effort}
              options={EFFORT_OPTIONS}
              onChange={(value) => update('default', { effort: value })}
              aria-invalid={defaultErrors.effort ? true : undefined}
              aria-describedby={defaultParamsErr}
              emptyLabel="继承"
            />
          </Field>
          <Field label="全局默认 thinking.budget_tokens" htmlFor={`${defaultId}-budget`} hint={defaultThinking.budget} error={defaultErrors.budget}>
            <NumberField
              id={`${defaultId}-budget`}
              placeholder="继承模型"
              value={defaultForm.budget}
              aria-invalid={defaultErrors.budget ? true : undefined}
              aria-describedby={defaultParamsErr}
              onChange={(event) => update('default', { budget: event.target.value })}
            />
          </Field>
          <Field label="全局默认 thinking.type" htmlFor={`${defaultId}-thinking-type`} hint={defaultThinking.thinkingType} error={defaultErrors.thinkingType}>
            <Select
              id={`${defaultId}-thinking-type`}
              value={defaultForm.thinkingType}
              options={THINKING_TYPE_OPTIONS}
              onChange={(value) => update('default', { thinkingType: value })}
              aria-invalid={defaultErrors.thinkingType ? true : undefined}
              aria-describedby={defaultParamsErr}
              emptyLabel="继承"
            />
          </Field>
        </div>
        {defaultErrors.params ? <span className={styles.errorText} id={defaultParamsErr} role="alert">{defaultErrors.params}</span> : null}
        {defaultDirty ? (
          <div className={styles.nodeActions}>
            <Button variant="secondary" className={ui.smallButton} onClick={() => update('default', EMPTY_FORM)}>
              清除全局默认覆盖
            </Button>
          </div>
        ) : null}
        {defaultDirty ? <ActionHint>清除后保存，全局默认恢复配置值。</ActionHint> : null}
      </fieldset>
      {CONFIGURED_NODES.map(([node, label]) => {
        const form = forms[node] ?? EMPTY_FORM
        const nodeErrors = errors[node] ?? {}
        const view = nodeByName.get(node)
        const id = `${baseId}-${node}`
        const paramsErr = nodeErrors.params ? `${id}-params-error` : undefined
        const hintFor = nodeHintFor(view, form)
        const thinkingHintsForNode = nodeThinkingFor(view, form)
        const dirty = isDirty(form)
        return (
          <fieldset key={node} className={styles.fieldset}>
            <legend className={styles.legend}>{label}</legend>
            <Field
              label={`${label} 模型`}
              htmlFor={`${id}-model`}
              hint={modelHint(models, defaultForm.model || view?.configured || '', inheritedModelSource(defaultForm, view), Boolean(form.model))}
              error={nodeErrors.model}
            >
              <Select
                id={`${id}-model`}
                value={form.model}
                options={[
                  { value: '', label: '继承' },
                  ...models.map((model) => ({
                    value: model.id,
                    label: `${model.display_name}（${shortID(model.id)}）${model.available ? '' : `不可用：${model.unavailable_reason || '未知原因'}`}`,
                    disabled: !model.available,
                  })),
                ]}
                onChange={(value) => update(node, { model: value })}
                aria-invalid={nodeErrors.model ? true : undefined}
                aria-describedby={paramsErr}
                emptyLabel="继承"
              />
            </Field>
            <div className={styles.paramGrid}>
              <Field label={`${label} max_tokens`} htmlFor={`${id}-max`} hint={hintFor('maxTokens')} error={nodeErrors.maxTokens}>
                <NumberField
                  id={`${id}-max`}
                  placeholder="继承"
                  value={form.maxTokens}
                  aria-invalid={nodeErrors.maxTokens ? true : undefined}
                  aria-describedby={paramsErr}
                  onChange={(event) => update(node, { maxTokens: event.target.value })}
                />
              </Field>
              <Field label={`${label} temperature`} htmlFor={`${id}-temp`} hint={hintFor('temperature')} error={nodeErrors.temperature}>
                <NumberField
                  id={`${id}-temp`}
                  step="any"
                  placeholder="继承"
                  value={form.temperature}
                  aria-invalid={nodeErrors.temperature ? true : undefined}
                  aria-describedby={paramsErr}
                  onChange={(event) => update(node, { temperature: event.target.value })}
                />
              </Field>
              <Field label={`${label} reasoning.effort`} htmlFor={`${id}-effort`} hint={hintFor('effort')} error={nodeErrors.effort}>
                <Select
                  id={`${id}-effort`}
                  value={form.effort}
                  options={EFFORT_OPTIONS}
                  onChange={(value) => update(node, { effort: value })}
                  aria-invalid={nodeErrors.effort ? true : undefined}
                  aria-describedby={paramsErr}
                  emptyLabel="继承"
                />
              </Field>
              <Field label={`${label} thinking.budget_tokens`} htmlFor={`${id}-budget`} hint={thinkingHintsForNode.budget} error={nodeErrors.budget}>
                <NumberField
                  id={`${id}-budget`}
                  placeholder="继承"
                  value={form.budget}
                  aria-invalid={nodeErrors.budget ? true : undefined}
                  aria-describedby={paramsErr}
                  onChange={(event) => update(node, { budget: event.target.value })}
                />
              </Field>
              <Field label={`${label} thinking.type`} htmlFor={`${id}-thinking-type`} hint={thinkingHintsForNode.thinkingType} error={nodeErrors.thinkingType}>
                <Select
                  id={`${id}-thinking-type`}
                  value={form.thinkingType}
                  options={THINKING_TYPE_OPTIONS}
                  onChange={(value) => update(node, { thinkingType: value })}
                  aria-invalid={nodeErrors.thinkingType ? true : undefined}
                  aria-describedby={paramsErr}
                  emptyLabel="继承"
                />
              </Field>
            </div>
            {nodeErrors.params ? <span className={styles.errorText} id={paramsErr} role="alert">{nodeErrors.params}</span> : null}
            {dirty ? (
              <div className={styles.nodeActions}>
                <Button variant="secondary" className={ui.smallButton} onClick={() => update(node, EMPTY_FORM)}>
                  {`清除${label}覆盖`}
                </Button>
              </div>
            ) : null}
            {dirty ? <ActionHint>清除后保存，该节点恢复继承。</ActionHint> : null}
          </fieldset>
        )
      })}
      {formError || stale ? (
        <div ref={alertRef} className={styles.formAlert} role="alert" tabIndex={-1}>
          {formError ? <Note tone="danger">{formError}</Note> : null}
          {stale ? <Button variant="secondary" onClick={() => { void reload() }} loading={pending}>刷新并重新加载</Button> : null}
        </div>
      ) : null}
      </div>
    </Drawer>
    <ConfirmDialog
      open={confirmReset}
      onClose={() => setConfirmReset(false)}
      onConfirm={() => { setConfirmReset(false); void reset() }}
      title="恢复为配置"
      description="将清除全局默认与 6 个节点的全部覆盖，恢复为配置文件的值；对之后启动的评审生效。"
      confirmLabel="恢复为配置"
    />
    </>
  )
}
