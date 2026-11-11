import { useEffect, useId, useMemo, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useCanWrite } from '../components/RoleContext'
import {
  createModel, deleteModel, deleteModelDefaults, getConfig, getDefaultProfile, getModelDefaults, listModelPresets, listModels, listProfiles,
  patchConfigOverlay, putModelDefaults, testModelDraft, updateModel,
} from '../api/api'
import { ApiError, errorMessage } from '../api/client'
import type {
  ConfigFileResponse, ConfigOrigin, ConfigOverlayChange, ConfigOverlayWriteResponse, ConfigProviderView,
  Model, ModelDefaults, ModelParams, ModelPreset, ModelWrite, ProfileConfig, ProfileDetail, ProfileNodeView, ProviderTestResult,
} from '../api/types'
import {
  Badge, Button, Checkbox, ConfirmDialog, Disclosure, Drawer, EmptyState, ErrorState, Field, FieldHint, LoadingState,
  Note, NumberField, PageHeader, Panel, Select,
} from '../components/ui'
import { PROFILE_NODES, ProfileEditor, ProfileNodeList, shortID } from '../features/models/ProfileNodes'
import { formatDuration, formatJson, formatNumber } from '../lib/format'
import ui from '../styles/ui.module.css'
import styles from './insights.module.css'

function fmt(value: number | undefined, suffix = ''): string {
  return typeof value === 'number' && Number.isFinite(value) ? `${formatNumber(value)}${suffix}` : '不可用'
}

function hostOf(value: string): string {
  try {
    return new URL(value).host || value
  } catch {
    return value || '不可用'
  }
}

// Flattens model params into label/value pairs and drops unset values
// (null, undefined, empty string), so the card shows only what was configured.
// Explicit zeros and false are meaningful capability switches and keep a label
// instead of disappearing, unless a platform default supplies that parameter:
// the zero/false is then not in effect and the inherited line shows the
// effective value (#413). Price is never rendered (#406/#407).
const EXPLICIT_PARAM_LABELS: Record<string, string> = {
  max_tokens: '0（模型默认，不发送）',
  context_window: '0（未知）',
  max_concurrency: '0（不限制本地并发）',
  rpm: '0（不限制）',
  strict_role_alternation: 'false（关闭）',
}

export function summarizeParams(params: ModelParams | undefined, sources?: Record<string, string>): Array<[string, string]> {
  const out: Array<[string, string]> = []
  const walk = (value: unknown, path: string) => {
    if (value === null || value === undefined || value === 0 || value === '' || value === false) return
    if (Array.isArray(value)) {
      if (value.length) out.push([path, value.join(', ')])
      return
    }
    if (typeof value === 'object') {
      for (const [key, child] of Object.entries(value as Record<string, unknown>)) walk(child, path ? `${path}.${key}` : key)
      return
    }
    out.push([path, String(value)])
  }
  for (const [key, value] of Object.entries(params ?? {})) {
    if (key === 'price') continue
    if (key === 'temperature' && typeof value === 'number') out.push([key, String(value)])
    else if ((value === 0 || value === false) && key in EXPLICIT_PARAM_LABELS) {
      // The effective value comes from the platform default, so the explicit
      // zero/false is overridden; do not present it as the active setting.
      if (sources?.[key] !== 'default') out.push([key, EXPLICIT_PARAM_LABELS[key]])
    } else walk(value, key)
  }
  return out
}

// inheritedParams keeps only the effective values the platform defaults
// supplied, so a model card can show what it inherits without repeating its
// own explicit parameters.
export function inheritedParams(model: Model): Array<[string, string]> {
  const sources = model.param_sources ?? {}
  const effective = model.effective_params
  if (!effective) return []
  const inherited: Record<string, unknown> = {}
  for (const [key, source] of Object.entries(sources)) {
    if (source !== 'default') continue
    inherited[key] = (effective as Record<string, unknown>)[key]
  }
  return summarizeParams(inherited)
}

type KeyMode = 'keep' | 'replace' | 'clear'

const PROTOCOL_OPTIONS = [
  { value: 'openai-chat', label: 'openai-chat' },
  { value: 'openai-responses', label: 'openai-responses' },
  { value: 'anthropic', label: 'anthropic' },
]

const EFFORT_OPTIONS = [
  { value: '', label: '不设置' },
  { value: 'low', label: 'low' },
  { value: 'medium', label: 'medium' },
  { value: 'high', label: 'high' },
  { value: 'max', label: 'max' },
]

const FORCED_TOOL_CHOICE_OPTIONS = [
  { value: '', label: '不设置' },
  { value: 'auto', label: 'auto（按协议判断）' },
  { value: 'supported', label: 'supported（允许强制 submit_result）' },
  { value: 'unsupported', label: 'unsupported（不强制）' },
]

const THINKING_TYPE_OPTIONS = [
  { value: '', label: '不设置' },
  { value: 'enabled', label: 'enabled' },
  { value: 'disabled', label: 'disabled' },
]

const KEY_MODE_OPTIONS = [
  { value: 'keep', label: '保持不变' },
  { value: 'replace', label: '替换' },
  { value: 'clear', label: '清除' },
]

interface ModelForm {
  displayName: string
  protocol: string
  baseURL: string
  upstreamModel: string
  maxTokens: string
  maxConcurrency: string
  contextWindow: string
  rpm: string
  temperature: string
  forcedToolChoice: string
  strictRoleAlternation: boolean
  effort: string
  thinkingType: string
  thinkingBudget: string
  preserveReasoningContent: boolean
}

type FormField = keyof ModelForm | 'apiKey'
type FormErrors = Partial<Record<FormField, string>>

// New models leave every optional number blank. The backend stores an absent
// value as 0: max_tokens is then not sent upstream, and max_concurrency 0
// creates no local semaphore. Only values the administrator types are written.
function emptyForm(): ModelForm {
  return {
    displayName: '', protocol: 'openai-chat', baseURL: '', upstreamModel: '',
    maxTokens: '', maxConcurrency: '', contextWindow: '', rpm: '', temperature: '',
    forcedToolChoice: '', strictRoleAlternation: false, effort: '', thinkingType: '', thinkingBudget: '',
    preserveReasoningContent: false,
  }
}

function numberText(value: unknown): string {
  return typeof value === 'number' && Number.isFinite(value) ? String(value) : ''
}

function formFromModel(model?: Model): ModelForm {
  const params = model?.params ?? {}
  return {
    displayName: model?.display_name ?? '',
    protocol: model?.protocol ?? 'openai-chat',
    baseURL: model?.base_url ?? '',
    upstreamModel: model?.model ?? '',
    // 0 means "not sent" for max_tokens and "unknown" for context_window, so
    // the inputs stay blank. A stored 0 max_concurrency or rpm is an explicit
    // "no limit" and stays visible.
    maxTokens: params.max_tokens ? numberText(params.max_tokens) : '',
    maxConcurrency: numberText(params.max_concurrency),
    contextWindow: params.context_window ? numberText(params.context_window) : '',
    rpm: numberText(params.rpm),
    temperature: numberText(params.temperature),
    forcedToolChoice: params.forced_tool_choice ?? '',
    strictRoleAlternation: params.strict_role_alternation === true,
    effort: params.reasoning?.effort ?? '',
    thinkingType: params.thinking?.type ?? '',
    thinkingBudget: params.thinking?.budget_tokens ? numberText(params.thinking.budget_tokens) : '',
    preserveReasoningContent: params.preserve_reasoning_content === true,
  }
}

// A preset fills the local form only. The user can change any field before
// saving; nothing is written until the normal create/update request (#405).
// A parameter the preset does not define (temperature is null) keeps the value
// already in the form, matching POST /models/{id}/preset semantics.
function formFromPreset(preset: ModelPreset, current: ModelForm): ModelForm {
  const params = preset.params
  return {
    ...current,
    displayName: preset.display_name,
    protocol: preset.protocol,
    baseURL: preset.base_url,
    upstreamModel: preset.model,
    maxTokens: numberText(params.max_tokens),
    maxConcurrency: numberText(params.max_concurrency),
    contextWindow: numberText(params.context_window),
    rpm: numberText(params.rpm),
    temperature: typeof params.temperature === 'number' ? numberText(params.temperature) : current.temperature,
    forcedToolChoice: params.forced_tool_choice ?? '',
    strictRoleAlternation: params.strict_role_alternation === true,
    effort: params.reasoning?.effort ?? '',
    thinkingType: params.thinking?.type ?? '',
    thinkingBudget: params.thinking?.budget_tokens ? numberText(params.thinking.budget_tokens) : '',
    preserveReasoningContent: params.preserve_reasoning_content === true,
  }
}

const NON_NEGATIVE_INT = /^\d+$/

// validateModelForm covers the rules the form can judge without the server.
// inheritedMaxTokens is the platform default a blank or zero model value
// inherits; an anthropic model may rely on it instead of storing its own
// max_tokens. Protocol-level requirements the server also enforces stay here
// so the user gets the error before any request is sent.
function validateModelForm(form: ModelForm, inheritedMaxTokens = 0): FormErrors {
  const errors: FormErrors = {}
  const maxTokens = form.maxTokens.trim()
  const effectiveMaxTokens = maxTokens ? Number(maxTokens) : inheritedMaxTokens
  if (maxTokens) {
    if (!NON_NEGATIVE_INT.test(maxTokens)) errors.maxTokens = 'max_tokens 必须是不小于 0 的整数。'
    else if (form.protocol === 'anthropic' && Number(maxTokens) === 0 && inheritedMaxTokens <= 0) {
      errors.maxTokens = 'anthropic 协议要求 max_tokens 大于 0。'
    }
  } else if (form.protocol === 'anthropic' && inheritedMaxTokens <= 0) {
    errors.maxTokens = 'anthropic 协议要求 max_tokens 大于 0，不能留空。'
  }
  if (form.maxConcurrency.trim() && !NON_NEGATIVE_INT.test(form.maxConcurrency.trim())) {
    errors.maxConcurrency = 'max_concurrency 必须是不小于 0 的整数。'
  }
  if (form.contextWindow.trim() && !NON_NEGATIVE_INT.test(form.contextWindow.trim())) {
    errors.contextWindow = 'context_window 必须是不小于 0 的整数。'
  }
  if (form.rpm.trim() && !NON_NEGATIVE_INT.test(form.rpm.trim())) {
    errors.rpm = 'rpm 必须是不小于 0 的整数。'
  }
  if (form.temperature.trim()) {
    const value = Number(form.temperature)
    if (!Number.isFinite(value) || value < 0 || value > 2) errors.temperature = 'temperature 必须在 0 到 2 之间。'
  }
  const budget = form.thinkingBudget.trim()
  if (budget && !NON_NEGATIVE_INT.test(budget)) {
    errors.thinkingBudget = 'thinking.budget_tokens 必须是不小于 0 的整数。'
  } else if (budget && form.protocol === 'anthropic' && Number(budget) > 0) {
    const value = Number(budget)
    if (value < 1024) errors.thinkingBudget = 'anthropic 的 thinking.budget_tokens 必须不小于 1024。'
    else if (effectiveMaxTokens > 0 && value >= effectiveMaxTokens) {
      errors.thinkingBudget = 'anthropic 的 thinking.budget_tokens 必须小于 max_tokens。'
    }
  }
  return errors
}

// buildParams merges the displayed fields over the stored parameter object.
// Parameters without a control (allow_reasoning_effort_none,
// output_config_effort, responses_params) survive an edit. price is legacy
// cost data and is never submitted (#406/#407).
function buildParams(model: Model | undefined, form: ModelForm): ModelParams {
  const params: ModelParams = { ...(model?.params ?? {}) }
  delete params.price
  if (form.maxTokens.trim()) params.max_tokens = Number(form.maxTokens)
  else delete params.max_tokens
  if (form.maxConcurrency.trim()) params.max_concurrency = Number(form.maxConcurrency)
  else delete params.max_concurrency
  if (form.contextWindow.trim()) params.context_window = Number(form.contextWindow)
  else delete params.context_window
  if (form.rpm.trim()) params.rpm = Number(form.rpm)
  else delete params.rpm
  if (form.temperature.trim()) params.temperature = Number(form.temperature)
  else delete params.temperature
  if (form.forcedToolChoice) params.forced_tool_choice = form.forcedToolChoice
  else delete params.forced_tool_choice
  params.strict_role_alternation = form.strictRoleAlternation
  if (form.effort) params.reasoning = { effort: form.effort }
  else delete params.reasoning
  // A zero budget means "not set": omit budget_tokens, and omit the whole
  // thinking object when no type is selected either.
  const thinkingBudget = form.thinkingBudget.trim()
  const hasThinkingBudget = thinkingBudget !== '' && Number(thinkingBudget) > 0
  if (form.thinkingType || hasThinkingBudget) {
    params.thinking = {}
    if (hasThinkingBudget) params.thinking.budget_tokens = Number(thinkingBudget)
    if (form.thinkingType) params.thinking.type = form.thinkingType
  } else delete params.thinking
  params.preserve_reasoning_content = form.preserveReasoningContent
  return params
}

const API_FIELD_MAP: Record<string, FormField> = {
  display_name: 'displayName',
  protocol: 'protocol',
  base_url: 'baseURL',
  model: 'upstreamModel',
  api_key: 'apiKey',
  'params.max_tokens': 'maxTokens',
  'params.context_window': 'contextWindow',
  'params.max_concurrency': 'maxConcurrency',
  'params.rpm': 'rpm',
  'params.temperature': 'temperature',
  'params.forced_tool_choice': 'forcedToolChoice',
  'params.strict_role_alternation': 'strictRoleAlternation',
  'params.preserve_reasoning_content': 'preserveReasoningContent',
  'params.reasoning': 'effort',
  'params.reasoning.effort': 'effort',
  'params.thinking': 'thinkingType',
  'params.thinking.type': 'thinkingType',
  'params.thinking.budget_tokens': 'thinkingBudget',
}

function apiFieldErrors(error: unknown): { fields: FormErrors; message?: string } {
  if (!(error instanceof ApiError)) return { fields: {}, message: errorMessage(error) }
  const fields: FormErrors = {}
  const unmapped: string[] = []
  for (const detail of error.details ?? []) {
    const key = detail.field ? API_FIELD_MAP[detail.field] : undefined
    if (!key) {
      const text = [detail.field, detail.message].filter(Boolean).join(': ')
      if (text) unmapped.push(text)
      continue
    }
    const message = detail.message || error.message
    fields[key] = fields[key] ? `${fields[key]}；${message}` : message
  }
  if (unmapped.length) return { fields, message: `${error.message}（${unmapped.join('；')}）` }
  // An API error without field details (for example a 422 provider_test_failed,
  // 503 unavailable or 403 forbidden) must still surface at form level.
  if (!Object.keys(fields).length) return { fields, message: errorMessage(error) }
  return { fields }
}

function configName(id: string): string {
  return id.startsWith('config:') ? id.slice('config:'.length) : id
}

// ------------------------------------------------- config file providers

const CONFIG_ORIGIN_LABELS: Record<ConfigOrigin, string> = {
  default: '内置默认',
  base: '基础配置',
  overlay: 'overlay',
}

const CONFIG_ORIGIN_TONES: Record<ConfigOrigin, 'neutral' | 'info' | 'ok'> = {
  default: 'neutral',
  base: 'info',
  overlay: 'ok',
}

function configOriginLabel(origin?: ConfigOrigin): string {
  return origin ? CONFIG_ORIGIN_LABELS[origin] ?? origin : '不可用'
}

function configProvidersOf(response?: ConfigFileResponse): ConfigProviderView[] {
  const providers = asRecord(response?.config)?.providers
  if (!Array.isArray(providers)) return []
  return providers.filter((item): item is ConfigProviderView => (
    Boolean(item) && typeof item === 'object' && typeof (item as ConfigProviderView).name === 'string'
  ))
}

function configOriginOf(origins: Record<string, ConfigOrigin> | undefined, name: string, path: string): ConfigOrigin | undefined {
  return origins?.[`providers.${name}.${path}`]
}

function OriginBadge({ origin }: { origin?: ConfigOrigin }) {
  if (!origin) return null
  return <Badge tone={CONFIG_ORIGIN_TONES[origin] ?? 'neutral'} plain>{configOriginLabel(origin)}</Badge>
}

// configProviderParams keeps only values the file-effective view actually
// returned. Zeros and false are omitted by the API; summarizeParams applies
// the same display rules as user models.
function configProviderParams(provider?: ConfigProviderView): ModelParams {
  if (!provider) return {}
  const params: ModelParams = {}
  if (provider.max_tokens !== undefined) params.max_tokens = provider.max_tokens
  if (provider.context_window !== undefined) params.context_window = provider.context_window
  if (provider.max_concurrency !== undefined) params.max_concurrency = provider.max_concurrency
  if (provider.rpm !== undefined) params.rpm = provider.rpm
  if (provider.temperature !== undefined) params.temperature = provider.temperature
  if (provider.forced_tool_choice !== undefined) params.forced_tool_choice = provider.forced_tool_choice
  if (provider.reasoning !== undefined) params.reasoning = provider.reasoning
  if (provider.thinking !== undefined) params.thinking = provider.thinking
  if (provider.strict_role_alternation !== undefined) params.strict_role_alternation = provider.strict_role_alternation
  if (provider.preserve_reasoning_content !== undefined) params.preserve_reasoning_content = provider.preserve_reasoning_content
  if (provider.allow_reasoning_effort_none !== undefined) params.allow_reasoning_effort_none = provider.allow_reasoning_effort_none
  if (provider.output_config_effort !== undefined) params.output_config_effort = provider.output_config_effort
  if (provider.responses_params !== undefined) params.responses_params = provider.responses_params
  return params
}

const PROFILE_NODE_LABELS = new Map<string, string>(PROFILE_NODES.map(([node, label]) => [node, label]))

function profileNodeLabel(node: string): string {
  return PROFILE_NODE_LABELS.get(node) ?? node
}

function nodeOverrideParams(node: ProfileNodeView): string {
  const params = node.params
  const parts: string[] = []
  if (params?.max_tokens) parts.push(`max_tokens ${params.max_tokens}`)
  if (typeof params?.temperature === 'number') parts.push(`temperature ${params.temperature}`)
  if (params?.reasoning?.effort) parts.push(`reasoning.effort ${params.reasoning.effort}`)
  if (params?.thinking?.budget_tokens) parts.push(`thinking.budget_tokens ${params.thinking.budget_tokens}`)
  if (params?.thinking?.type) parts.push(`thinking.type ${params.thinking.type}`)
  return parts.join(' · ')
}

// configNodeOverrides lists the stored 0028 overrides that affect this
// configured provider: a node's own model or parameters, and the profile
// default override. A default-level parameter override is listed once as
// "default" instead of repeating the inherited parameters on every node.
// The stored document is never modified here; the profile editor is the only
// clearing path.
function configNodeOverrides(nodes: ProfileNodeView[] | undefined, providerName: string): ProfileNodeView[] {
  const id = `config:${providerName}`
  const list = nodes ?? []
  const defaultNode = list.find((node) => node.node === 'default')
  const defaultLevel = defaultNode !== undefined && list.some((node) => (
    node.node !== 'default' && node.model === id &&
    (node.source === 'override_default' || node.param_source === 'override_default')
  ))
  const defaultApplies = defaultNode !== undefined && (
    defaultLevel ||
    (defaultNode.model === id && (
      defaultNode.source === 'override' || defaultNode.source === 'override_default' || defaultNode.param_source === 'override'
    ))
  )
  const overrides: ProfileNodeView[] = defaultApplies ? [defaultNode] : []
  for (const node of list) {
    if (node.node === 'default' || node.model !== id) continue
    if (node.source === 'override' || node.param_source === 'override') overrides.push(node)
  }
  return overrides
}

function NodeOverrideLine({ nodes }: { nodes: ProfileNodeView[] }) {
  return (
    <span>
      0028 节点覆盖：
      {nodes.map((node, index) => (
        <span key={node.node}>
          {index > 0 ? '、' : ''}
          {node.node === 'default' ? 'default' : profileNodeLabel(node.node)}
          {nodeOverrideParams(node) ? `（${nodeOverrideParams(node)}）` : ''}
        </span>
      ))}
    </span>
  )
}

// ------------------------------------------------- platform model defaults

interface DefaultsForm {
  maxTokens: string
  contextWindow: string
  maxConcurrency: string
  rpm: string
  temperature: string
  forcedToolChoice: string
  strictRoleAlternation: boolean
  effort: string
  thinkingType: string
  thinkingBudget: string
  allowReasoningEffortNone: boolean
  preserveReasoningContent: boolean
  outputConfigEffort: boolean
  responsesParams: string
}

type DefaultsField = keyof DefaultsForm
type DefaultsErrors = Partial<Record<DefaultsField, string>>

const RESPONSES_PARAMS_OPTIONS = [
  { value: '', label: '不设置' },
  { value: 'auto', label: 'auto（按协议判断）' },
  { value: 'supported', label: 'supported（发送 include/store）' },
  { value: 'unsupported', label: 'unsupported（不发送）' },
]

const DEFAULTS_EFFORT_OPTIONS = [
  ...EFFORT_OPTIONS,
  { value: 'none', label: 'none（需开启 allow_reasoning_effort_none）' },
]

function emptyDefaultsForm(): DefaultsForm {
  return {
    maxTokens: '', contextWindow: '', maxConcurrency: '', rpm: '', temperature: '',
    forcedToolChoice: '', strictRoleAlternation: false, effort: '', thinkingType: '', thinkingBudget: '',
    allowReasoningEffortNone: false, preserveReasoningContent: false, outputConfigEffort: false, responsesParams: '',
  }
}

// Zero values mean "unset" in the defaults document: 0, false and an empty
// string all inherit the built-in behavior instead of overriding it.
function defaultsFormFrom(defaults?: ModelDefaults): DefaultsForm {
  const params = defaults?.params ?? {}
  return {
    maxTokens: params.max_tokens ? numberText(params.max_tokens) : '',
    contextWindow: params.context_window ? numberText(params.context_window) : '',
    maxConcurrency: params.max_concurrency ? numberText(params.max_concurrency) : '',
    rpm: params.rpm ? numberText(params.rpm) : '',
    temperature: typeof params.temperature === 'number' ? numberText(params.temperature) : '',
    forcedToolChoice: params.forced_tool_choice ?? '',
    strictRoleAlternation: params.strict_role_alternation === true,
    effort: params.reasoning?.effort ?? '',
    thinkingType: params.thinking?.type ?? '',
    thinkingBudget: params.thinking?.budget_tokens ? numberText(params.thinking.budget_tokens) : '',
    allowReasoningEffortNone: params.allow_reasoning_effort_none === true,
    preserveReasoningContent: params.preserve_reasoning_content === true,
    outputConfigEffort: params.output_config_effort === true,
    responsesParams: params.responses_params ?? '',
  }
}

function validateDefaultsForm(form: DefaultsForm): DefaultsErrors {
  const errors: DefaultsErrors = {}
  const nonNegative = (value: string, field: DefaultsField, label: string) => {
    const text = value.trim()
    if (text && !NON_NEGATIVE_INT.test(text)) errors[field] = `${label} 必须是不小于 0 的整数。`
  }
  nonNegative(form.maxTokens, 'maxTokens', 'max_tokens')
  nonNegative(form.contextWindow, 'contextWindow', 'context_window')
  nonNegative(form.maxConcurrency, 'maxConcurrency', 'max_concurrency')
  nonNegative(form.rpm, 'rpm', 'rpm')
  nonNegative(form.thinkingBudget, 'thinkingBudget', 'thinking.budget_tokens')
  if (form.temperature.trim()) {
    const value = Number(form.temperature)
    if (!Number.isFinite(value) || value < 0 || value > 2) errors.temperature = 'temperature 必须在 0 到 2 之间。'
  }
  if (form.effort === 'none' && !form.allowReasoningEffortNone) {
    errors.effort = 'reasoning.effort 为 none 时需要同时开启 allow_reasoning_effort_none。'
  }
  return errors
}

// Only submitted keys are stored. Boolean false and numeric 0 are omitted so
// they keep meaning "unset" rather than overriding a built-in value.
function buildDefaultsParams(form: DefaultsForm): ModelParams {
  const params: ModelParams = {}
  if (form.maxTokens.trim()) params.max_tokens = Number(form.maxTokens)
  if (form.contextWindow.trim()) params.context_window = Number(form.contextWindow)
  if (form.maxConcurrency.trim()) params.max_concurrency = Number(form.maxConcurrency)
  if (form.rpm.trim()) params.rpm = Number(form.rpm)
  if (form.temperature.trim()) params.temperature = Number(form.temperature)
  if (form.forcedToolChoice) params.forced_tool_choice = form.forcedToolChoice
  if (form.effort) params.reasoning = { effort: form.effort }
  const budget = form.thinkingBudget.trim()
  if (form.thinkingType || (budget && Number(budget) > 0)) {
    params.thinking = {}
    if (budget && Number(budget) > 0) params.thinking.budget_tokens = Number(budget)
    if (form.thinkingType) params.thinking.type = form.thinkingType
  }
  if (form.strictRoleAlternation) params.strict_role_alternation = true
  if (form.allowReasoningEffortNone) params.allow_reasoning_effort_none = true
  if (form.preserveReasoningContent) params.preserve_reasoning_content = true
  if (form.outputConfigEffort) params.output_config_effort = true
  if (form.responsesParams) params.responses_params = form.responsesParams
  return params
}

const DEFAULTS_FIELD_MAP: Record<string, DefaultsField> = {
  'params.max_tokens': 'maxTokens',
  'params.context_window': 'contextWindow',
  'params.max_concurrency': 'maxConcurrency',
  'params.rpm': 'rpm',
  'params.temperature': 'temperature',
  'params.forced_tool_choice': 'forcedToolChoice',
  'params.strict_role_alternation': 'strictRoleAlternation',
  'params.reasoning': 'effort',
  'params.reasoning.effort': 'effort',
  'params.thinking': 'thinkingType',
  'params.thinking.type': 'thinkingType',
  'params.thinking.budget_tokens': 'thinkingBudget',
  'params.allow_reasoning_effort_none': 'allowReasoningEffortNone',
  'params.preserve_reasoning_content': 'preserveReasoningContent',
  'params.output_config_effort': 'outputConfigEffort',
  'params.responses_params': 'responsesParams',
}

function defaultsApiErrors(error: unknown): { fields: DefaultsErrors; message?: string } {
  if (!(error instanceof ApiError)) return { fields: {}, message: errorMessage(error) }
  const fields: DefaultsErrors = {}
  const unmapped: string[] = []
  for (const detail of error.details ?? []) {
    const key = detail.field ? DEFAULTS_FIELD_MAP[detail.field] : undefined
    if (!key) {
      const text = [detail.field, detail.message].filter(Boolean).join(': ')
      if (text) unmapped.push(text)
      continue
    }
    const message = detail.message || error.message
    fields[key] = fields[key] ? `${fields[key]}；${message}` : message
  }
  if (unmapped.length) return { fields, message: `${error.message}（${unmapped.join('；')}）` }
  if (!Object.keys(fields).length) return { fields, message: errorMessage(error) }
  return { fields }
}

function ModelDefaultsPanel({
  defaults, form, onFormChange, loading, loadError, onReload, onSaved,
}: {
  defaults?: ModelDefaults
  form: DefaultsForm
  onFormChange: (form: DefaultsForm) => void
  loading: boolean
  loadError?: unknown
  onReload: () => void
  onSaved: () => void
}) {
  const [errors, setErrors] = useState<DefaultsErrors>({})
  const [error, setError] = useState<string>()
  const [stale, setStale] = useState(false)
  const [status, setStatus] = useState<string>()
  const [pending, setPending] = useState(false)
  const [resetOpen, setResetOpen] = useState(false)
  const [resetError, setResetError] = useState<unknown>()
  const id = useId()
  const ids = {
    tokens: `${id}-tokens`, context: `${id}-context`, concurrency: `${id}-concurrency`, rpm: `${id}-rpm`,
    temperature: `${id}-temperature`, toolChoice: `${id}-tool-choice`, strict: `${id}-strict`,
    effort: `${id}-effort`, thinkingType: `${id}-thinking-type`, thinkingBudget: `${id}-thinking-budget`,
    preserve: `${id}-preserve`, allowNone: `${id}-allow-none`, outputEffort: `${id}-output-effort`,
    responses: `${id}-responses`,
  }
  const revision = defaults?.revision ?? 0
  // A stored document with only unset values has nothing to reset.
  const hasDefaults = Object.keys(buildDefaultsParams(defaultsFormFrom(defaults))).length > 0

  const update = (patch: Partial<DefaultsForm>) => {
    onFormChange({ ...form, ...patch })
  }

  const reload = () => {
    setError(undefined)
    setStale(false)
    setResetError(undefined)
    onReload()
  }

  const save = async () => {
    const found = validateDefaultsForm(form)
    if (Object.keys(found).length) {
      setErrors(found)
      setError(undefined)
      return
    }
    setPending(true)
    setErrors({})
    setError(undefined)
    setStale(false)
    setStatus(undefined)
    try {
      const saved = await putModelDefaults(buildDefaultsParams(form), revision)
      setStatus(`已保存（revision ${saved.revision}）。`)
      onSaved()
    } catch (err) {
      if (err instanceof ApiError && err.code === 'stale_revision') {
        setError('已被他人修改，请重新加载后再保存。')
        setStale(true)
      } else {
        const mapped = defaultsApiErrors(err)
        setErrors(mapped.fields)
        setError(mapped.message)
      }
    } finally {
      setPending(false)
    }
  }

  const reset = async () => {
    setPending(true)
    setResetError(undefined)
    try {
      const cleared = await deleteModelDefaults(revision)
      setResetOpen(false)
      setStatus(`已重置全部默认值（revision ${cleared.revision}）。`)
      onSaved()
    } catch (err) {
      if (err instanceof ApiError && err.code === 'stale_revision') {
        // The dialog cannot refresh the query itself; surface the same reload
        // path as a save conflict instead of leaving a dead-end error.
        setResetOpen(false)
        setError('已被他人修改，请重新加载后再重置。')
        setStale(true)
      } else {
        setResetError(err)
      }
    } finally {
      setPending(false)
    }
  }

  if (loading) return <LoadingState label="全局默认参数加载中…" />
  // A caller without the platform admin role must not see the panel at all;
  // the server already enforces the same rule on every read and write.
  if (loadError instanceof ApiError && loadError.status === 403) return null
  if (loadError) return <ErrorState error={loadError} onRetry={onReload} />

  return (
    <>
      <Panel
        title={<>全局默认参数 <small>仅 user: 模型</small></>}
        aside={<span>revision {revision}</span>}
      >
        <form className={styles.modelForm} onSubmit={(event) => event.preventDefault()}>
          <fieldset className={styles.modelFormGroup}>
            <legend className={styles.modelFormLegend}>限流与窗口</legend>
            <div className={styles.modelFormGrid}>
              <Field label="max_tokens" htmlFor={ids.tokens} hint="模型未设置时继承；留空由上游取默认。anthropic 用户模型需要此处或模型自身给出大于 0 的值。" error={errors.maxTokens}>
                <NumberField id={ids.tokens} min={0} step={1} value={form.maxTokens} aria-invalid={errors.maxTokens ? true : undefined} onChange={(event) => update({ maxTokens: event.target.value })} />
              </Field>
              <Field label="context_window" htmlFor={ids.context} hint="上游可接收的输入窗口；留空表示未知，不做上下文压缩。" error={errors.contextWindow}>
                <NumberField id={ids.context} min={0} step={1} value={form.contextWindow} aria-invalid={errors.contextWindow ? true : undefined} onChange={(event) => update({ contextWindow: event.target.value })} />
              </Field>
              <Field label="max_concurrency" htmlFor={ids.concurrency} hint="模型未设置时使用；留空或 0 不限制本地并发。" error={errors.maxConcurrency}>
                <NumberField id={ids.concurrency} min={0} step={1} value={form.maxConcurrency} aria-invalid={errors.maxConcurrency ? true : undefined} onChange={(event) => update({ maxConcurrency: event.target.value })} />
              </Field>
              <Field label="rpm" htmlFor={ids.rpm} hint="每分钟请求数；留空或 0 表示不限制。" error={errors.rpm}>
                <NumberField id={ids.rpm} min={0} step={1} value={form.rpm} aria-invalid={errors.rpm ? true : undefined} onChange={(event) => update({ rpm: event.target.value })} />
              </Field>
            </div>
          </fieldset>

          <fieldset className={styles.modelFormGroup}>
            <legend className={styles.modelFormLegend}>采样</legend>
            <div className={styles.modelFormGrid}>
              <Field label="temperature" htmlFor={ids.temperature} hint="0 到 2；留空表示不设置。" error={errors.temperature}>
                <NumberField id={ids.temperature} min={0} max={2} step={0.1} value={form.temperature} aria-invalid={errors.temperature ? true : undefined} onChange={(event) => update({ temperature: event.target.value })} />
              </Field>
              <Field label="forced_tool_choice" htmlFor={ids.toolChoice} hint="声明上游是否接受强制 submit_result 工具选择。" error={errors.forcedToolChoice}>
                <Select id={ids.toolChoice} value={form.forcedToolChoice} options={FORCED_TOOL_CHOICE_OPTIONS} onChange={(value) => update({ forcedToolChoice: value })} aria-invalid={errors.forcedToolChoice ? true : undefined} />
              </Field>
              <div className={styles.checkField}>
                <Checkbox
                  id={ids.strict}
                  label="strict_role_alternation"
                  checked={form.strictRoleAlternation}
                  onChange={(event) => update({ strictRoleAlternation: event.target.checked })}
                  aria-invalid={errors.strictRoleAlternation ? true : undefined}
                  aria-describedby={[`${ids.strict}-hint`, errors.strictRoleAlternation ? `${ids.strict}-error` : undefined].filter(Boolean).join(' ')}
                />
                <FieldHint id={`${ids.strict}-hint`}>开启后把用户提示折入最后一条工具消息；关闭不设置该默认值。</FieldHint>
                {errors.strictRoleAlternation ? <span className={styles.formError} id={`${ids.strict}-error`} role="alert">{errors.strictRoleAlternation}</span> : null}
              </div>
            </div>
          </fieldset>

          <fieldset className={styles.modelFormGroup}>
            <legend className={styles.modelFormLegend}>推理与能力</legend>
            <div className={styles.modelFormGrid}>
              <Field label="reasoning.effort" htmlFor={ids.effort} hint="none 需要同时开启 allow_reasoning_effort_none；anthropic 不接受 none。" error={errors.effort}>
                <Select id={ids.effort} value={form.effort} options={DEFAULTS_EFFORT_OPTIONS} onChange={(value) => update({ effort: value })} aria-invalid={errors.effort ? true : undefined} />
              </Field>
              <Field label="thinking.type" htmlFor={ids.thinkingType} hint="仅 openai-chat 发送 thinking.type；留空表示不设置。" error={errors.thinkingType}>
                <Select id={ids.thinkingType} value={form.thinkingType} options={THINKING_TYPE_OPTIONS} onChange={(value) => update({ thinkingType: value })} aria-invalid={errors.thinkingType ? true : undefined} />
              </Field>
              <Field label="thinking.budget_tokens" htmlFor={ids.thinkingBudget} hint="0 或留空表示不设置；仅 anthropic 生效。" error={errors.thinkingBudget}>
                <NumberField id={ids.thinkingBudget} min={0} step={1} value={form.thinkingBudget} aria-invalid={errors.thinkingBudget ? true : undefined} onChange={(event) => update({ thinkingBudget: event.target.value })} />
              </Field>
              <Field label="responses_params" htmlFor={ids.responses} hint="仅 openai-responses 端点使用；留空表示 auto。" error={errors.responsesParams}>
                <Select id={ids.responses} value={form.responsesParams} options={RESPONSES_PARAMS_OPTIONS} onChange={(value) => update({ responsesParams: value })} aria-invalid={errors.responsesParams ? true : undefined} />
              </Field>
              <div className={styles.checkField}>
                <Checkbox
                  id={ids.allowNone}
                  label="allow_reasoning_effort_none"
                  checked={form.allowReasoningEffortNone}
                  onChange={(event) => update({ allowReasoningEffortNone: event.target.checked })}
                  aria-invalid={errors.allowReasoningEffortNone ? true : undefined}
                  aria-describedby={[`${ids.allowNone}-hint`, errors.allowReasoningEffortNone ? `${ids.allowNone}-error` : undefined].filter(Boolean).join(' ')}
                />
                <FieldHint id={`${ids.allowNone}-hint`}>允许 effort 为 none 的上游（DeepSeek thinking）。</FieldHint>
                {errors.allowReasoningEffortNone ? <span className={styles.formError} id={`${ids.allowNone}-error`} role="alert">{errors.allowReasoningEffortNone}</span> : null}
              </div>
              <div className={styles.checkField}>
                <Checkbox
                  id={ids.preserve}
                  label="preserve_reasoning_content"
                  checked={form.preserveReasoningContent}
                  onChange={(event) => update({ preserveReasoningContent: event.target.checked })}
                  aria-invalid={errors.preserveReasoningContent ? true : undefined}
                  aria-describedby={[`${ids.preserve}-hint`, errors.preserveReasoningContent ? `${ids.preserve}-error` : undefined].filter(Boolean).join(' ')}
                />
                <FieldHint id={`${ids.preserve}-hint`}>openai-chat 回传 reasoning_content，DeepSeek thinking + tools 需要。</FieldHint>
                {errors.preserveReasoningContent ? <span className={styles.formError} id={`${ids.preserve}-error`} role="alert">{errors.preserveReasoningContent}</span> : null}
              </div>
              <div className={styles.checkField}>
                <Checkbox
                  id={ids.outputEffort}
                  label="output_config_effort"
                  checked={form.outputConfigEffort}
                  onChange={(event) => update({ outputConfigEffort: event.target.checked })}
                  aria-invalid={errors.outputConfigEffort ? true : undefined}
                  aria-describedby={[`${ids.outputEffort}-hint`, errors.outputConfigEffort ? `${ids.outputEffort}-error` : undefined].filter(Boolean).join(' ')}
                />
                <FieldHint id={`${ids.outputEffort}-hint`}>anthropic 通过 output_config.effort 发送 reasoning.effort。</FieldHint>
                {errors.outputConfigEffort ? <span className={styles.formError} id={`${ids.outputEffort}-error`} role="alert">{errors.outputConfigEffort}</span> : null}
              </div>
            </div>
          </fieldset>

          <div className={styles.cardActions}>
            <Button onClick={() => { void save() }} loading={pending}>保存全局默认</Button>
            <Button variant="secondary" onClick={() => { setResetError(undefined); setResetOpen(true) }} disabled={pending || revision === 0 || !hasDefaults}>重置</Button>
          </div>
          {status ? <span className={styles.resultOk} role="status">{status}</span> : null}
          {error ? (
            <span className={styles.resultError} role="alert">
              {error}
              {stale ? <> <Button variant="secondary" className={ui.smallButton} onClick={reload}>重新加载</Button></> : null}
            </span>
          ) : null}
          <Note tone="neutral">
            只对 user: 模型生效；config: 模型由配置文件负责。模型显式设置优先于这里的默认值；
            整数字段的 0、布尔 false 或留空表示未设置，继续使用内置/协议默认；temperature 是显式值，填 0 会覆盖默认。
            保存时用候选值合并现有 user: 模型并按各自协议交叉校验，会让模型不可用的组合被拒绝且不写库。
            保存后对之后启动的评审生效。
          </Note>
        </form>
      </Panel>
      <ConfirmDialog
        open={resetOpen}
        onClose={() => setResetOpen(false)}
        onConfirm={() => { void reset() }}
        title="重置全局默认参数"
        description="清除全部平台默认参数？各 user: 模型会恢复为只使用自身参数与内置/协议默认。"
        confirmLabel="重置"
        pending={pending}
        error={resetError}
      />
    </>
  )
}

const PRESET_LABELS: Record<string, string> = {
  'deepseek-v4-pro-max': 'DeepSeek V4 Pro 最大能力',
  'deepseek-flash-max': 'DeepSeek Flash 最大能力',
}

function presetLabel(preset: ModelPreset): string {
  return PRESET_LABELS[preset.id] ?? preset.display_name
}

function ModelCard({ model, canWrite, configProvider, origins, nodeOverrides, applying, onEdit, onEditConfig, onDelete, onClearNodeOverrides }: {
  model: Model
  canWrite: boolean
  configProvider?: ConfigProviderView
  origins?: Record<string, ConfigOrigin>
  nodeOverrides: ProfileNodeView[]
  applying?: boolean
  onEdit: () => void
  onEditConfig: () => void
  onDelete: () => void
  onClearNodeOverrides: () => void
}) {
  const stats = model.stats_24h
  const errorRate = typeof stats?.error_rate === 'number'
    ? `${(stats.error_rate <= 1 ? stats.error_rate * 100 : stats.error_rate).toFixed(2)}%`
    : '不可用'
  const p50 = typeof stats?.p50_latency_ms === 'number' ? formatDuration(stats.p50_latency_ms) : '不可用'
  const isConfig = model.source === 'config'
  const name = configName(model.id)
  const protocol = isConfig ? (configProvider?.protocol ?? model.protocol) : model.protocol
  const upstreamModel = isConfig ? (configProvider?.model ?? model.model) : model.model
  const baseURL = isConfig ? (configProvider?.base_url ?? model.base_url) : model.base_url
  const hasAPIKey = isConfig ? (configProvider?.api_key_configured ?? model.has_api_key) : model.has_api_key
  // Config cards render the file-effective values and their origins so a
  // pending restart is visible instead of silently showing the runtime copy.
  const params = isConfig && configProvider
    ? summarizeParams(configProviderParams(configProvider))
    : summarizeParams(model.params, model.param_sources)
  const inherited = isConfig ? [] : inheritedParams(model)
  const originFor = (path: string) => configOriginOf(origins, name, path)
  return (
    <article className={`${ui.panel} ${styles.providerCard}`}>
      <div className={styles.providerHead}>
        <span className={styles.cardTitle}>{model.display_name}</span>
        <Badge tone={model.source === 'config' ? 'neutral' : 'info'} plain>{model.source === 'config' ? '配置' : '用户'}</Badge>
        <Badge tone={model.available ? 'ok' : 'danger'} plain>{model.available ? '可用' : '不可用'}</Badge>
      </div>
      <div className={styles.cardMeta}>
        <span>ID <span className="mono">{shortID(model.id)}</span></span>
        <span>协议 {protocol || '不可用'} <OriginBadge origin={isConfig ? originFor('protocol') : undefined} /></span>
      </div>
      <div className={styles.cardMeta}>
        <span>上游模型 <span className="mono">{upstreamModel || '不可用'}</span> <OriginBadge origin={isConfig ? originFor('model') : undefined} /></span>
        <span>base_url <code>{hostOf(baseURL)}</code> <OriginBadge origin={isConfig ? originFor('base_url') : undefined} /></span>
      </div>
      <div className={styles.cardMeta}>
        <span>API key {hasAPIKey ? '已配置' : '未配置'}</span>
      </div>
      {isConfig ? (
        <div className={styles.cardMeta}>
          <span>配置来源 <code>providers.{name}</code> · 通过 overlay 编辑，重启生效</span>
        </div>
      ) : null}
      <div className={styles.cardMeta} role="group" aria-label={`${model.display_name} 参数`}>
        <span>参数</span>
        {params.length
          ? params.map(([key, value]) => (
            <span key={key}>{key} <code>{value}</code> <OriginBadge origin={isConfig ? originFor(key) : undefined} /></span>
          ))
          : <span>未设置</span>}
      </div>
      {inherited.length ? (
        <div className={styles.cardMeta} role="group" aria-label={`${model.display_name} 全局默认参数`}>
          <span>全局默认</span>
          {inherited.map(([key, value]) => <span key={key}>{key} <code>{value}</code></span>)}
        </div>
      ) : null}
      {isConfig && nodeOverrides.length ? (
        <div className={styles.cardMeta}>
          <NodeOverrideLine nodes={nodeOverrides} />
        </div>
      ) : null}
      {model.unavailable_reason ? <span className={styles.resultError}>不可用原因：{model.unavailable_reason}</span> : null}
      {model.referenced_by?.length ? <span className={styles.cardMeta}>引用：{model.referenced_by.join('、')}</span> : null}
      <ul className={styles.cardStats} aria-label={`${model.display_name} 近 24 小时`}>
        {[['24h 调用', fmt(stats?.calls)], ['错误率', errorRate], ['P50 延迟', p50]].map(([label, value]) => (
          <li key={label}><span>{label}</span><b className={value === '不可用' ? styles.statUnavailable : undefined}>{value}</b></li>
        ))}
      </ul>
      {canWrite ? (
        <div className={styles.cardActions}>
          {isConfig ? (
            <Button variant="secondary" className={ui.smallButton} onClick={onEditConfig} disabled={!configProvider || applying}>编辑配置</Button>
          ) : (
            <>
              <Button variant="secondary" className={ui.smallButton} onClick={onEdit}>编辑</Button>
              <Button variant="danger" className={ui.smallButton} onClick={onDelete}>删除</Button>
            </>
          )}
          {isConfig && nodeOverrides.length ? (
            <Button variant="secondary" className={ui.smallButton} onClick={onClearNodeOverrides}>在评审档位中清除</Button>
          ) : null}
        </div>
      ) : null}
      {isConfig && !configProvider ? (
        <span className="small muted">
          {canWrite ? '配置文件快照不可用，暂不能在控制台编辑。' : '只读：仅平台管理员可通过配置文件（overlay）编辑。'}
        </span>
      ) : null}
      {isConfig && applying ? <span className="small muted">配置快照应用中，稍后可再次编辑。</span> : null}
    </article>
  )
}

function ModelDrawer({
  open, model, presets, presetsLoading, presetsFailed, presetsUnavailable, inheritedMaxTokens, onReloadPresets, onClose, onSaved,
}: {
  open: boolean
  model?: Model
  presets: ModelPreset[]
  presetsLoading: boolean
  presetsFailed: boolean
  presetsUnavailable: boolean
  inheritedMaxTokens: number
  onReloadPresets: () => void
  onClose: () => void
  onSaved: () => void
}) {
  const [form, setForm] = useState<ModelForm>(emptyForm)
  const [errors, setErrors] = useState<FormErrors>({})
  const [keyMode, setKeyMode] = useState<KeyMode>('keep')
  const [apiKey, setApiKey] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string>()
  const [test, setTest] = useState<ProviderTestResult>()
  const [presetNote, setPresetNote] = useState<string>()
  const id = useId()
  const ids = {
    display: `${id}-display`, protocol: `${id}-protocol`, base: `${id}-base`, model: `${id}-model`,
    tokens: `${id}-tokens`, concurrency: `${id}-concurrency`, context: `${id}-context`, rpm: `${id}-rpm`,
    temperature: `${id}-temperature`, toolChoice: `${id}-tool-choice`, strict: `${id}-strict`,
    effort: `${id}-effort`, thinkingType: `${id}-thinking-type`, thinkingBudget: `${id}-thinking-budget`,
    preserve: `${id}-preserve`, key: `${id}-key`,
  }

  useEffect(() => {
    if (!open) {
      setApiKey('')
      setKeyMode('keep')
      setError(undefined)
      setTest(undefined)
      setErrors({})
      setPresetNote(undefined)
      return
    }
    setForm(formFromModel(model))
    setApiKey('')
    setKeyMode('keep')
    setError(undefined)
    setTest(undefined)
    setErrors({})
    setPresetNote(undefined)
  }, [open, model])

  const update = (patch: Partial<ModelForm>) => {
    setForm((current) => ({ ...current, ...patch }))
  }

  const input = (): ModelWrite => {
    const value: ModelWrite = {
      display_name: form.displayName.trim(),
      protocol: form.protocol,
      base_url: form.baseURL.trim(),
      model: form.upstreamModel.trim(),
      params: buildParams(model, form),
    }
    if (keyMode === 'replace') value.api_key = apiKey
    if (keyMode === 'clear') value.api_key = ''
    if (model) {
      value.id = model.id
      value.revision = model.revision
    }
    return value
  }

  const applyPreset = (preset: ModelPreset) => {
    setForm((current) => formFromPreset(preset, current))
    setPresetNote(presetLabel(preset))
    setErrors({})
    setError(undefined)
    setTest(undefined)
  }

  const handleError = (err: unknown, prefix?: string) => {
    if (err instanceof ApiError && err.code === 'stale_revision') {
      setError('已被他人修改，请重新加载。')
      return
    }
    if (prefix) {
      setError(prefix)
      return
    }
    const mapped = apiFieldErrors(err)
    setErrors(mapped.fields)
    setError(mapped.message)
  }

  const save = async () => {
    const found = validateModelForm(form, inheritedMaxTokens)
    if (Object.keys(found).length) {
      setErrors(found)
      setError(undefined)
      return
    }
    setPending(true)
    setErrors({})
    setError(undefined)
    try {
      if (model) await updateModel(model.id, input())
      else await createModel(input())
      onSaved()
      onClose()
    } catch (err) {
      handleError(err)
    } finally {
      setPending(false)
    }
  }

  const runTest = async () => {
    const found = validateModelForm(form, inheritedMaxTokens)
    if (Object.keys(found).length) {
      setErrors(found)
      setError(undefined)
      setTest(undefined)
      return
    }
    setPending(true)
    setErrors({})
    setError(undefined)
    setTest(undefined)
    try {
      setTest(await testModelDraft(input()))
    } catch (err) {
      handleError(err, err instanceof ApiError && err.code === 'egress_denied'
        ? '出站地址被拒绝：请检查 base_url 或服务端出站规则。'
        : undefined)
    } finally {
      setPending(false)
    }
  }

  const configModel = model?.read_only ?? false

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
      <div className={styles.modelForm}>
        {!configModel && !model && !presetsUnavailable ? (
          <section className={styles.presetBlock} aria-label="模型预设">
            <span className={styles.presetTitle}>DeepSeek 预设</span>
            {presetsLoading ? <LoadingState label="预设加载中…" /> : presetsFailed ? (
              <Note tone="danger" role="alert">
                <div className={styles.presetError}>
                  <span>预设加载失败，仍可手动填写。</span>
                  <Button variant="secondary" className={ui.smallButton} onClick={onReloadPresets}>重试</Button>
                </div>
              </Note>
            ) : presets.length ? (
              <>
                <div className={styles.presetRow}>
                  {presets.map((preset) => (
                    <Button key={preset.id} variant="secondary" className={ui.smallButton} onClick={() => applyPreset(preset)}>
                      {presetLabel(preset)}
                    </Button>
                  ))}
                </div>
                <FieldHint>预设只填入表单，不立即保存；保存后对之后启动的评审生效。</FieldHint>
                {presetNote ? <span className={styles.presetApplied} role="status">已填入 {presetNote}，可继续修改。</span> : null}
              </>
            ) : <FieldHint>没有可用预设，请手动填写。</FieldHint>}
          </section>
        ) : null}

        <fieldset className={styles.modelFormGroup}>
          <legend className={styles.modelFormLegend}>连接</legend>
          <div className={styles.modelFormGrid}>
            <Field label="显示名" htmlFor={ids.display} error={errors.displayName}>
              <input id={ids.display} className={ui.input} value={form.displayName} aria-invalid={errors.displayName ? true : undefined} onChange={(event) => update({ displayName: event.target.value })} />
            </Field>
            <Field label="协议" htmlFor={ids.protocol} error={errors.protocol}>
              <Select id={ids.protocol} value={form.protocol} options={PROTOCOL_OPTIONS} onChange={(value) => update({ protocol: value })} aria-invalid={errors.protocol ? true : undefined} />
            </Field>
            <Field label="base_url" htmlFor={ids.base} error={errors.baseURL}>
              <input id={ids.base} className={ui.input} value={form.baseURL} aria-invalid={errors.baseURL ? true : undefined} onChange={(event) => update({ baseURL: event.target.value })} />
            </Field>
            <Field label="上游模型" htmlFor={ids.model} error={errors.upstreamModel}>
              <input id={ids.model} className={ui.input} value={form.upstreamModel} aria-invalid={errors.upstreamModel ? true : undefined} onChange={(event) => update({ upstreamModel: event.target.value })} />
            </Field>
            <Field label="API key" htmlFor={ids.key} error={errors.apiKey}>
              <Select
                id={`${ids.key}-mode`}
                value={keyMode}
                options={KEY_MODE_OPTIONS}
                onChange={(value) => setKeyMode(value as KeyMode)}
                aria-label="API key 状态"
                aria-invalid={errors.apiKey ? true : undefined}
                aria-describedby={errors.apiKey ? `${ids.key}-error` : undefined}
              />
              {keyMode === 'replace' ? (
                <input
                  id={ids.key}
                  className={ui.input}
                  type="password"
                  value={apiKey}
                  aria-invalid={errors.apiKey ? true : undefined}
                  aria-describedby={errors.apiKey ? `${ids.key}-error` : undefined}
                  onChange={(event) => setApiKey(event.target.value)}
                />
              ) : null}
            </Field>
          </div>
        </fieldset>

        <fieldset className={styles.modelFormGroup}>
          <legend className={styles.modelFormLegend}>采样</legend>
          <div className={styles.modelFormGrid}>
            <Field label="temperature" htmlFor={ids.temperature} hint="0 到 2；留空使用上游默认。" error={errors.temperature}>
              <NumberField id={ids.temperature} min={0} max={2} step={0.1} value={form.temperature} aria-invalid={errors.temperature ? true : undefined} onChange={(event) => update({ temperature: event.target.value })} />
            </Field>
            <Field label="forced_tool_choice" htmlFor={ids.toolChoice} hint="声明上游是否接受强制 submit_result 工具选择。" error={errors.forcedToolChoice}>
              <Select
                id={ids.toolChoice}
                value={form.forcedToolChoice}
                options={FORCED_TOOL_CHOICE_OPTIONS}
                onChange={(value) => update({ forcedToolChoice: value })}
                aria-invalid={errors.forcedToolChoice ? true : undefined}
              />
            </Field>
            <div className={styles.checkField}>
              <Checkbox
                id={ids.strict}
                label="strict_role_alternation"
                checked={form.strictRoleAlternation}
                onChange={(event) => update({ strictRoleAlternation: event.target.checked })}
                aria-invalid={errors.strictRoleAlternation ? true : undefined}
                aria-describedby={[`${ids.strict}-hint`, errors.strictRoleAlternation ? `${ids.strict}-error` : undefined].filter(Boolean).join(' ')}
              />
              <FieldHint id={`${ids.strict}-hint`}>开启后把用户提示折入最后一条工具消息，满足严格的角色交替。</FieldHint>
              {errors.strictRoleAlternation ? <span className={styles.formError} id={`${ids.strict}-error`} role="alert">{errors.strictRoleAlternation}</span> : null}
            </div>
          </div>
        </fieldset>

        <fieldset className={styles.modelFormGroup}>
          <legend className={styles.modelFormLegend}>推理</legend>
          <div className={styles.modelFormGrid}>
            <Field label="reasoning.effort" htmlFor={ids.effort} hint="anthropic 不发送 effort，改用 thinking.budget_tokens。" error={errors.effort}>
              <Select id={ids.effort} value={form.effort} options={EFFORT_OPTIONS} onChange={(value) => update({ effort: value })} aria-invalid={errors.effort ? true : undefined} />
            </Field>
            <Field label="thinking.type" htmlFor={ids.thinkingType} hint="仅 openai-chat 发送 thinking.type；anthropic 由 budget_tokens 决定。" error={errors.thinkingType}>
              <Select id={ids.thinkingType} value={form.thinkingType} options={THINKING_TYPE_OPTIONS} onChange={(value) => update({ thinkingType: value })} aria-invalid={errors.thinkingType ? true : undefined} />
            </Field>
            <Field label="thinking.budget_tokens" htmlFor={ids.thinkingBudget} hint="0 或留空表示不设置；仅 anthropic 生效，启用时必须不小于 1024 且小于 max_tokens。" error={errors.thinkingBudget}>
              <NumberField id={ids.thinkingBudget} min={0} step={1} value={form.thinkingBudget} aria-invalid={errors.thinkingBudget ? true : undefined} onChange={(event) => update({ thinkingBudget: event.target.value })} />
            </Field>
            <div className={styles.checkField}>
              <Checkbox
                id={ids.preserve}
                label="preserve_reasoning_content"
                checked={form.preserveReasoningContent}
                onChange={(event) => update({ preserveReasoningContent: event.target.checked })}
                aria-invalid={errors.preserveReasoningContent ? true : undefined}
                aria-describedby={[`${ids.preserve}-hint`, errors.preserveReasoningContent ? `${ids.preserve}-error` : undefined].filter(Boolean).join(' ')}
              />
              <FieldHint id={`${ids.preserve}-hint`}>仅在 openai-chat 生效：把 reasoning_content 回传给后续请求。</FieldHint>
              {errors.preserveReasoningContent ? <span className={styles.formError} id={`${ids.preserve}-error`} role="alert">{errors.preserveReasoningContent}</span> : null}
            </div>
          </div>
        </fieldset>

        <fieldset className={styles.modelFormGroup}>
          <legend className={styles.modelFormLegend}>限流</legend>
          <div className={styles.modelFormGrid}>
            <Field label="max_tokens" htmlFor={ids.tokens} hint="不发送时由模型默认（DeepSeek 8K/64K/128K，不是最大值）。anthropic 需模型自身或全局默认给出大于 0 的值。" error={errors.maxTokens}>
              <NumberField id={ids.tokens} min={0} step={1} value={form.maxTokens} aria-invalid={errors.maxTokens ? true : undefined} onChange={(event) => update({ maxTokens: event.target.value })} />
            </Field>
            <Field label="max_concurrency" htmlFor={ids.concurrency} hint="留空或 0 表示不限制本地并发。" error={errors.maxConcurrency}>
              <NumberField id={ids.concurrency} min={0} step={1} value={form.maxConcurrency} aria-invalid={errors.maxConcurrency ? true : undefined} onChange={(event) => update({ maxConcurrency: event.target.value })} />
            </Field>
            <Field label="context_window" htmlFor={ids.context} hint="上游可接收的输入窗口；0 表示未知。" error={errors.contextWindow}>
              <NumberField id={ids.context} min={0} step={1} value={form.contextWindow} aria-invalid={errors.contextWindow ? true : undefined} onChange={(event) => update({ contextWindow: event.target.value })} />
            </Field>
            <Field label="rpm" htmlFor={ids.rpm} hint="每分钟请求数；0 表示不限制。" error={errors.rpm}>
              <NumberField id={ids.rpm} min={0} step={1} value={form.rpm} aria-invalid={errors.rpm ? true : undefined} onChange={(event) => update({ rpm: event.target.value })} />
            </Field>
          </div>
          {form.maxConcurrency.trim() === '0' ? (
            <Note tone="warn" role="status">max_concurrency = 0：不创建本地并发信号量，不限制本地并发。</Note>
          ) : null}
        </fieldset>

        {configModel ? (
          <Note tone="neutral">
            该模型来自平台配置文件，控制台不写数据库。YAML 字段路径：<code>providers.{configName(model?.id ?? '')}</code> 下的
            protocol、base_url、model、max_tokens、context_window、temperature、reasoning、thinking、max_concurrency、rpm、
            forced_tool_choice、strict_role_alternation；修改后重启生效。
          </Note>
        ) : null}
        {error ? <span className={styles.resultError}>{error}</span> : null}
        {test ? (
          <span className={test.ok === false ? styles.resultError : styles.resultOk}>
            {test.ok === true ? '测试成功。' : '测试失败。'} {test.model ? `模型：${test.model}。` : ''}
            {typeof test.latency_ms === 'number' ? `延迟：${formatDuration(test.latency_ms)}。` : ''}
          </span>
        ) : null}
      </div>
    </Drawer>
  )
}

// --------------------------------------------- config model overlay editor

interface ConfigModelForm {
  protocol: string
  baseURL: string
  model: string
  maxTokens: string
  contextWindow: string
  maxConcurrency: string
  rpm: string
  temperature: string
  forcedToolChoice: string
  strictRoleAlternation: boolean
  effort: string
  thinkingType: string
  thinkingBudget: string
  preserveReasoningContent: boolean
  allowReasoningEffortNone: boolean
  outputConfigEffort: boolean
  responsesParams: string
}

type ConfigField = keyof ConfigModelForm
type ConfigErrors = Partial<Record<ConfigField, string>>

const CONFIG_PATH_FIELDS: Array<[string, ConfigField]> = [
  ['thinking.budget_tokens', 'thinkingBudget'],
  ['thinking.type', 'thinkingType'],
  ['reasoning.effort', 'effort'],
  ['preserve_reasoning_content', 'preserveReasoningContent'],
  ['allow_reasoning_effort_none', 'allowReasoningEffortNone'],
  ['output_config_effort', 'outputConfigEffort'],
  ['strict_role_alternation', 'strictRoleAlternation'],
  ['forced_tool_choice', 'forcedToolChoice'],
  ['responses_params', 'responsesParams'],
  ['max_concurrency', 'maxConcurrency'],
  ['context_window', 'contextWindow'],
  ['max_tokens', 'maxTokens'],
  ['temperature', 'temperature'],
  ['base_url', 'baseURL'],
  ['protocol', 'protocol'],
  ['rpm', 'rpm'],
  ['model', 'model'],
]

// configFormFrom mirrors the file-effective provider view. max_tokens and
// context_window render blank for zero because zero means "not sent" and
// "unknown" respectively. max_concurrency and rpm keep an explicit zero when
// the view returns one: zero there means "no local limit", matching the user
// model form and the card's explicit-zero labels.
function configFormFrom(provider?: ConfigProviderView): ConfigModelForm {
  return {
    protocol: provider?.protocol ?? 'openai-chat',
    baseURL: provider?.base_url ?? '',
    model: provider?.model ?? '',
    maxTokens: provider?.max_tokens ? numberText(provider.max_tokens) : '',
    contextWindow: provider?.context_window ? numberText(provider.context_window) : '',
    maxConcurrency: provider?.max_concurrency !== undefined ? numberText(provider.max_concurrency) : '',
    rpm: provider?.rpm !== undefined ? numberText(provider.rpm) : '',
    temperature: typeof provider?.temperature === 'number' ? numberText(provider.temperature) : '',
    forcedToolChoice: provider?.forced_tool_choice ?? '',
    strictRoleAlternation: provider?.strict_role_alternation === true,
    effort: provider?.reasoning?.effort ?? '',
    thinkingType: provider?.thinking?.type ?? '',
    thinkingBudget: provider?.thinking?.budget_tokens ? numberText(provider.thinking.budget_tokens) : '',
    preserveReasoningContent: provider?.preserve_reasoning_content === true,
    allowReasoningEffortNone: provider?.allow_reasoning_effort_none === true,
    outputConfigEffort: provider?.output_config_effort === true,
    responsesParams: provider?.responses_params ?? '',
  }
}

// providerBaseURLValid mirrors the server's URL rule; an empty base_url is
// allowed and means the protocol adapter's default endpoint.
function providerBaseURLValid(raw: string): boolean {
  if (raw.includes('?') || raw.includes('#')) return false
  try {
    const url = new URL(raw)
    return (url.protocol === 'http:' || url.protocol === 'https:') &&
      Boolean(url.hostname) && !url.username && !url.password && !url.search && !url.hash
  } catch {
    return false
  }
}

// validateConfigForm applies the same allowlist ranges as the overlay writer
// so a bad value is reported before any request is sent.
function validateConfigForm(form: ConfigModelForm): ConfigErrors {
  const errors: ConfigErrors = {}
  if (!PROTOCOL_OPTIONS.some((option) => option.value === form.protocol)) errors.protocol = '协议不受支持。'
  const baseURL = form.baseURL.trim()
  if (baseURL && !providerBaseURLValid(baseURL)) {
    errors.baseURL = 'base_url 必须是绝对 http(s) URL，不含凭据、查询或片段。'
  }
  if (!form.model.trim()) errors.model = '上游模型不能为空。'
  const maxTokens = form.maxTokens.trim()
  if (maxTokens) {
    if (!NON_NEGATIVE_INT.test(maxTokens)) errors.maxTokens = 'max_tokens 必须是不小于 0 的整数。'
    else if (form.protocol === 'anthropic' && Number(maxTokens) === 0) errors.maxTokens = 'anthropic 协议要求 max_tokens 大于 0。'
  } else if (form.protocol === 'anthropic') {
    errors.maxTokens = 'anthropic 协议要求 max_tokens 大于 0，不能留空。'
  }
  if (form.maxConcurrency.trim() && !NON_NEGATIVE_INT.test(form.maxConcurrency.trim())) {
    errors.maxConcurrency = 'max_concurrency 必须是不小于 0 的整数。'
  }
  if (form.contextWindow.trim() && !NON_NEGATIVE_INT.test(form.contextWindow.trim())) {
    errors.contextWindow = 'context_window 必须是不小于 0 的整数。'
  }
  if (form.rpm.trim() && !NON_NEGATIVE_INT.test(form.rpm.trim())) {
    errors.rpm = 'rpm 必须是不小于 0 的整数。'
  }
  if (form.temperature.trim()) {
    const value = Number(form.temperature)
    if (!Number.isFinite(value) || value < 0 || value > 2) errors.temperature = 'temperature 必须在 0 到 2 之间。'
  }
  const budget = form.thinkingBudget.trim()
  if (budget && !NON_NEGATIVE_INT.test(budget)) {
    errors.thinkingBudget = 'thinking.budget_tokens 必须是不小于 0 的整数。'
  } else if (budget && form.protocol === 'anthropic' && Number(budget) > 0) {
    const value = Number(budget)
    if (value < 1024) errors.thinkingBudget = 'anthropic 的 thinking.budget_tokens 必须不小于 1024。'
    else if (maxTokens && Number(maxTokens) > 0 && value >= Number(maxTokens)) {
      errors.thinkingBudget = 'anthropic 的 thinking.budget_tokens 必须小于 max_tokens。'
    }
  }
  if (form.effort === 'none') {
    if (form.protocol === 'anthropic') errors.effort = 'anthropic 不支持 reasoning.effort 为 none。'
    else if (!form.allowReasoningEffortNone) errors.effort = 'reasoning.effort 为 none 时需要同时开启 allow_reasoning_effort_none。'
  }
  return errors
}

// buildConfigChanges emits only leaves the administrator actually changed.
// Blank selects and blank numbers delete the overlay leaf so the base value
// applies again; an edited temperature is an explicit value, including 0.
function buildConfigChanges(form: ConfigModelForm, initial: ConfigModelForm, providerName: string): ConfigOverlayChange[] {
  const prefix = ['providers', providerName]
  const changes: ConfigOverlayChange[] = []
  const set = (path: string[], value: unknown) => changes.push({ path: [...prefix, ...path], value })
  const remove = (path: string[]) => changes.push({ path: [...prefix, ...path], delete: true })

  if (form.protocol !== initial.protocol) set(['protocol'], form.protocol)
  if (form.baseURL.trim() !== initial.baseURL.trim()) set(['base_url'], form.baseURL.trim())
  if (form.model.trim() !== initial.model.trim()) set(['model'], form.model.trim())

  const numberField = (field: 'maxTokens' | 'contextWindow' | 'maxConcurrency' | 'rpm', path: string[]) => {
    if (form[field].trim() === initial[field]) return
    const text = form[field].trim()
    if (text) set(path, Number(text))
    else remove(path)
  }
  numberField('maxTokens', ['max_tokens'])
  numberField('contextWindow', ['context_window'])
  numberField('maxConcurrency', ['max_concurrency'])
  numberField('rpm', ['rpm'])
  if (form.temperature.trim() !== initial.temperature) {
    set(['temperature'], form.temperature.trim() ? Number(form.temperature) : null)
  }
  const selectField = (field: 'forcedToolChoice' | 'responsesParams', path: string[]) => {
    if (form[field] === initial[field]) return
    if (form[field]) set(path, form[field])
    else remove(path)
  }
  selectField('forcedToolChoice', ['forced_tool_choice'])
  selectField('responsesParams', ['responses_params'])
  if (form.effort !== initial.effort) {
    if (form.effort) set(['reasoning', 'effort'], form.effort)
    else remove(['reasoning'])
  }
  if (form.thinkingType !== initial.thinkingType || form.thinkingBudget.trim() !== initial.thinkingBudget) {
    if (!form.thinkingType && !form.thinkingBudget.trim()) {
      remove(['thinking'])
    } else {
      if (form.thinkingType !== initial.thinkingType) {
        if (form.thinkingType) set(['thinking', 'type'], form.thinkingType)
        else remove(['thinking', 'type'])
      }
      if (form.thinkingBudget.trim() !== initial.thinkingBudget) {
        if (form.thinkingBudget.trim()) set(['thinking', 'budget_tokens'], Number(form.thinkingBudget))
        else remove(['thinking', 'budget_tokens'])
      }
    }
  }
  if (form.strictRoleAlternation !== initial.strictRoleAlternation) set(['strict_role_alternation'], form.strictRoleAlternation)
  if (form.preserveReasoningContent !== initial.preserveReasoningContent) set(['preserve_reasoning_content'], form.preserveReasoningContent)
  if (form.allowReasoningEffortNone !== initial.allowReasoningEffortNone) set(['allow_reasoning_effort_none'], form.allowReasoningEffortNone)
  if (form.outputConfigEffort !== initial.outputConfigEffort) set(['output_config_effort'], form.outputConfigEffort)
  return changes
}

function mapConfigFieldError(message: string, providerName: string): ConfigField | undefined {
  const prefix = `providers.${providerName}.`
  return CONFIG_PATH_FIELDS.find(([leaf]) => message.includes(prefix + leaf))?.[1]
}

// configApiErrors translates the overlay writer's stable codes. Referrer
// details are shown as a list and never trigger a database change.
function configApiErrors(error: unknown, providerName: string): {
  fields: ConfigErrors
  message?: string
  details: string[]
  stale: boolean
} {
  if (!(error instanceof ApiError)) return { fields: {}, message: errorMessage(error), details: [], stale: false }
  if (error.code === 'stale_config') {
    return { fields: {}, message: '配置已被修改，请重新加载后再保存。', details: [], stale: true }
  }
  const messages: Record<string, string> = {
    provider_in_use: '该 provider 仍被引用，未写入 overlay。不会自动修改数据库或档位；请先在配置或评审档位中清除引用。',
    overlay_not_configured: '服务未配置 overlay，无法在控制台保存配置。',
    config_read_only: 'overlay 文件或目录只读，无法保存配置。',
    config_symlink: 'overlay 路径是符号链接，拒绝写入。',
    config_too_large: 'overlay 超过大小限制，无法保存。',
    config_busy: '配置写入繁忙，请稍后重试。',
    overlay_conflicts_with_base: 'overlay 与基础配置文件冲突，拒绝写入。',
    invalid_change: error.message,
    invalid_config: error.message,
    config_write_failed: error.message,
    validation_failed: error.message,
    bad_request: error.message,
  }
  const fields: ConfigErrors = {}
  const details: string[] = []
  for (const detail of error.details ?? []) {
    const message = detail.message ?? ''
    const text = error.code === 'provider_in_use' ? message : [detail.field, message].filter(Boolean).join(': ')
    if (!text) continue
    const field = message ? mapConfigFieldError(message, providerName) : undefined
    if (field && error.code !== 'provider_in_use') {
      fields[field] = fields[field] ? `${fields[field]}；${message}` : message
      continue
    }
    details.push(text)
  }
  return { fields, message: messages[error.code] ?? error.message, details, stale: false }
}

function ConfigModelDrawer({
  open, model, provider, origins, nodeOverrides, baseRevision, overlayRevision,
  onClose, onSaved, onReload, onOpenProfileEditor,
}: {
  open: boolean
  model?: Model
  provider?: ConfigProviderView
  origins?: Record<string, ConfigOrigin>
  nodeOverrides: ProfileNodeView[]
  baseRevision: string
  overlayRevision: string
  onClose: () => void
  onSaved: (result: ConfigOverlayWriteResponse) => void
  onReload: () => void
  onOpenProfileEditor: () => void
}) {
  const [form, setForm] = useState<ConfigModelForm>(configFormFrom)
  const [errors, setErrors] = useState<ConfigErrors>({})
  const [error, setError] = useState<string>()
  const [details, setDetails] = useState<string[]>([])
  const [stale, setStale] = useState(false)
  const [status, setStatus] = useState<string>()
  const [pending, setPending] = useState(false)
  const [resetOpen, setResetOpen] = useState(false)
  const id = useId()
  const ids = {
    protocol: `${id}-protocol`, base: `${id}-base`, model: `${id}-model`,
    tokens: `${id}-tokens`, context: `${id}-context`, concurrency: `${id}-concurrency`, rpm: `${id}-rpm`,
    temperature: `${id}-temperature`, toolChoice: `${id}-tool-choice`, strict: `${id}-strict`,
    effort: `${id}-effort`, thinkingType: `${id}-thinking-type`, thinkingBudget: `${id}-thinking-budget`,
    preserve: `${id}-preserve`, allowNone: `${id}-allow-none`, outputEffort: `${id}-output-effort`,
    responses: `${id}-responses`,
  }
  const providerName = model ? configName(model.id) : ''
  const formSnapshot = useRef<{ id?: string; baseRevision: string; overlayRevision: string } | undefined>(undefined)

  // Reinitialize on open and after a file revision change. A revision change
  // while the drawer is already open means an external edit or a reload
  // replaced the file snapshot; tell the user their draft was replaced instead
  // of silently discarding it.
  useEffect(() => {
    if (!open) {
      formSnapshot.current = undefined
      setErrors({})
      setError(undefined)
      setDetails([])
      setStale(false)
      setStatus(undefined)
      return
    }
    const previous = formSnapshot.current
    const external = Boolean(previous && previous.id === model?.id &&
      (previous.baseRevision !== baseRevision || previous.overlayRevision !== overlayRevision))
    setForm(configFormFrom(provider))
    setErrors({})
    setError(undefined)
    setDetails([])
    setStale(false)
    setStatus(external ? '配置已被他人修改，已重新加载。' : undefined)
    formSnapshot.current = { id: model?.id, baseRevision, overlayRevision }
  }, [open, model?.id, baseRevision, overlayRevision])

  const update = (patch: Partial<ConfigModelForm>) => {
    setForm((current) => ({ ...current, ...patch }))
  }
  const originOf = (path: string) => configOriginOf(origins, providerName, path)
  const hintFor = (path: string, description: string) => (
    <>来源：{configOriginLabel(originOf(path))} · {description}</>
  )

  const handleError = (err: unknown) => {
    const mapped = configApiErrors(err, providerName)
    setErrors(mapped.fields)
    setError(mapped.message)
    setDetails(mapped.details)
    setStale(mapped.stale)
    setStatus(undefined)
  }

  const save = async () => {
    const found = validateConfigForm(form)
    if (Object.keys(found).length) {
      setErrors(found)
      setError(undefined)
      setDetails([])
      setStatus(undefined)
      return
    }
    const changes = buildConfigChanges(form, configFormFrom(provider), providerName)
    if (!changes.length) {
      setErrors({})
      setError(undefined)
      setDetails([])
      setStale(false)
      setStatus('没有需要保存的更改。')
      return
    }
    setPending(true)
    setErrors({})
    setError(undefined)
    setDetails([])
    setStale(false)
    setStatus(undefined)
    try {
      onSaved(await patchConfigOverlay({ base_revision: baseRevision, overlay_revision: overlayRevision, changes }))
    } catch (err) {
      handleError(err)
    } finally {
      setPending(false)
    }
  }

  const reset = async () => {
    setPending(true)
    setError(undefined)
    setDetails([])
    setStale(false)
    setStatus(undefined)
    try {
      const result = await patchConfigOverlay({
        base_revision: baseRevision,
        overlay_revision: overlayRevision,
        changes: [{ path: ['providers', providerName], delete: true }],
      })
      setResetOpen(false)
      onSaved(result)
    } catch (err) {
      // Close the confirmation so the referrer list is visible in the editor.
      setResetOpen(false)
      handleError(err)
    } finally {
      setPending(false)
    }
  }

  return (
    <>
      <Drawer
        open={open}
        onClose={onClose}
        title="编辑配置模型"
        description={`通过 overlay 修改 providers.${providerName}；保存后需重启生效，重启可能取消在途评审。API key 与 api_key_env 只读。`}
        footer={(
          <>
            <Button variant="secondary" onClick={() => setResetOpen(true)} loading={pending}>重置为 base</Button>
            <Button variant="secondary" onClick={onClose}>取消</Button>
            <Button onClick={() => { void save() }} loading={pending}>保存</Button>
          </>
        )}
      >
        <div className={styles.modelForm}>
          <fieldset className={styles.modelFormGroup}>
            <legend className={styles.modelFormLegend}>连接</legend>
            <div className={styles.modelFormGrid}>
              <Field label="协议" htmlFor={ids.protocol} hint={hintFor('protocol', 'openai-chat、openai-responses 或 anthropic。')} error={errors.protocol}>
                <Select id={ids.protocol} value={form.protocol} options={PROTOCOL_OPTIONS} onChange={(value) => update({ protocol: value })} aria-invalid={errors.protocol ? true : undefined} />
              </Field>
              <Field label="base_url" htmlFor={ids.base} hint={hintFor('base_url', '上游 API 根地址；凭据值不会回显。')} error={errors.baseURL}>
                <input id={ids.base} className={ui.input} value={form.baseURL} aria-invalid={errors.baseURL ? true : undefined} onChange={(event) => update({ baseURL: event.target.value })} />
              </Field>
              <Field label="上游模型" htmlFor={ids.model} hint={hintFor('model', '发送给上游的模型 ID。')} error={errors.model}>
                <input id={ids.model} className={ui.input} value={form.model} aria-invalid={errors.model ? true : undefined} onChange={(event) => update({ model: event.target.value })} />
              </Field>
              <div className={styles.checkField}>
                <span className={ui.label}>API key</span>
                <FieldHint>{provider?.api_key_configured ? 'API key 已配置（只读）' : 'API key 未配置（只读）'}</FieldHint>
              </div>
            </div>
          </fieldset>

          <fieldset className={styles.modelFormGroup}>
            <legend className={styles.modelFormLegend}>采样</legend>
            <div className={styles.modelFormGrid}>
              <Field label="temperature" htmlFor={ids.temperature} hint={hintFor('temperature', '0 到 2；留空表示不设置该字段。')} error={errors.temperature}>
                <NumberField id={ids.temperature} min={0} max={2} step={0.1} value={form.temperature} aria-invalid={errors.temperature ? true : undefined} onChange={(event) => update({ temperature: event.target.value })} />
              </Field>
              <Field label="forced_tool_choice" htmlFor={ids.toolChoice} hint={hintFor('forced_tool_choice', '声明上游是否接受强制 submit_result 工具选择。')} error={errors.forcedToolChoice}>
                <Select id={ids.toolChoice} value={form.forcedToolChoice} options={FORCED_TOOL_CHOICE_OPTIONS} onChange={(value) => update({ forcedToolChoice: value })} aria-invalid={errors.forcedToolChoice ? true : undefined} />
              </Field>
              <div className={styles.checkField}>
                <Checkbox
                  id={ids.strict}
                  label="strict_role_alternation"
                  checked={form.strictRoleAlternation}
                  onChange={(event) => update({ strictRoleAlternation: event.target.checked })}
                  aria-invalid={errors.strictRoleAlternation ? true : undefined}
                  aria-describedby={[`${ids.strict}-hint`, errors.strictRoleAlternation ? `${ids.strict}-error` : undefined].filter(Boolean).join(' ')}
                />
                <FieldHint id={`${ids.strict}-hint`}>来源：{configOriginLabel(originOf('strict_role_alternation'))} · 开启后把用户提示折入最后一条工具消息。</FieldHint>
                {errors.strictRoleAlternation ? <span className={styles.formError} id={`${ids.strict}-error`} role="alert">{errors.strictRoleAlternation}</span> : null}
              </div>
            </div>
          </fieldset>

          <fieldset className={styles.modelFormGroup}>
            <legend className={styles.modelFormLegend}>推理与能力</legend>
            <div className={styles.modelFormGrid}>
              <Field label="reasoning.effort" htmlFor={ids.effort} hint={hintFor('reasoning.effort', 'anthropic 不发送 effort，改用 thinking.budget_tokens；none 需要同时开启 allow_reasoning_effort_none。')} error={errors.effort}>
                <Select id={ids.effort} value={form.effort} options={DEFAULTS_EFFORT_OPTIONS} onChange={(value) => update({ effort: value })} aria-invalid={errors.effort ? true : undefined} />
              </Field>
              <Field label="thinking.type" htmlFor={ids.thinkingType} hint={hintFor('thinking.type', '仅 openai-chat 发送 thinking.type。')} error={errors.thinkingType}>
                <Select id={ids.thinkingType} value={form.thinkingType} options={THINKING_TYPE_OPTIONS} onChange={(value) => update({ thinkingType: value })} aria-invalid={errors.thinkingType ? true : undefined} />
              </Field>
              <Field label="thinking.budget_tokens" htmlFor={ids.thinkingBudget} hint={hintFor('thinking.budget_tokens', '仅 anthropic 生效；启用时必须不小于 1024 且小于 max_tokens。')} error={errors.thinkingBudget}>
                <NumberField id={ids.thinkingBudget} min={0} step={1} value={form.thinkingBudget} aria-invalid={errors.thinkingBudget ? true : undefined} onChange={(event) => update({ thinkingBudget: event.target.value })} />
              </Field>
              <Field label="responses_params" htmlFor={ids.responses} hint={hintFor('responses_params', '仅 openai-responses 端点使用。')} error={errors.responsesParams}>
                <Select id={ids.responses} value={form.responsesParams} options={RESPONSES_PARAMS_OPTIONS} onChange={(value) => update({ responsesParams: value })} aria-invalid={errors.responsesParams ? true : undefined} />
              </Field>
              <div className={styles.checkField}>
                <Checkbox
                  id={ids.allowNone}
                  label="allow_reasoning_effort_none"
                  checked={form.allowReasoningEffortNone}
                  onChange={(event) => update({ allowReasoningEffortNone: event.target.checked })}
                  aria-describedby={`${ids.allowNone}-hint`}
                />
                <FieldHint id={`${ids.allowNone}-hint`}>来源：{configOriginLabel(originOf('allow_reasoning_effort_none'))} · 允许 effort 为 none 的上游。</FieldHint>
              </div>
              <div className={styles.checkField}>
                <Checkbox
                  id={ids.preserve}
                  label="preserve_reasoning_content"
                  checked={form.preserveReasoningContent}
                  onChange={(event) => update({ preserveReasoningContent: event.target.checked })}
                  aria-describedby={`${ids.preserve}-hint`}
                />
                <FieldHint id={`${ids.preserve}-hint`}>来源：{configOriginLabel(originOf('preserve_reasoning_content'))} · openai-chat 回传 reasoning_content。</FieldHint>
              </div>
              <div className={styles.checkField}>
                <Checkbox
                  id={ids.outputEffort}
                  label="output_config_effort"
                  checked={form.outputConfigEffort}
                  onChange={(event) => update({ outputConfigEffort: event.target.checked })}
                  aria-describedby={`${ids.outputEffort}-hint`}
                />
                <FieldHint id={`${ids.outputEffort}-hint`}>来源：{configOriginLabel(originOf('output_config_effort'))} · anthropic 通过 output_config.effort 发送 reasoning.effort。</FieldHint>
              </div>
            </div>
          </fieldset>

          <fieldset className={styles.modelFormGroup}>
            <legend className={styles.modelFormLegend}>限流与窗口</legend>
            <div className={styles.modelFormGrid}>
              <Field label="max_tokens" htmlFor={ids.tokens} hint={hintFor('max_tokens', '0 表示不发送，由上游取默认；anthropic 需要大于 0。')} error={errors.maxTokens}>
                <NumberField id={ids.tokens} min={0} step={1} value={form.maxTokens} aria-invalid={errors.maxTokens ? true : undefined} onChange={(event) => update({ maxTokens: event.target.value })} />
              </Field>
              <Field label="context_window" htmlFor={ids.context} hint={hintFor('context_window', '上游可接收的输入窗口；0 表示未知。')} error={errors.contextWindow}>
                <NumberField id={ids.context} min={0} step={1} value={form.contextWindow} aria-invalid={errors.contextWindow ? true : undefined} onChange={(event) => update({ contextWindow: event.target.value })} />
              </Field>
              <Field label="max_concurrency" htmlFor={ids.concurrency} hint={hintFor('max_concurrency', '0 或留空都表示不限制本地并发；填写 0 会显式写入 overlay。')} error={errors.maxConcurrency}>
                <NumberField id={ids.concurrency} min={0} step={1} value={form.maxConcurrency} aria-invalid={errors.maxConcurrency ? true : undefined} onChange={(event) => update({ maxConcurrency: event.target.value })} />
              </Field>
              <Field label="rpm" htmlFor={ids.rpm} hint={hintFor('rpm', '每分钟请求数；0 或留空表示不限制，填写 0 会显式写入 overlay。')} error={errors.rpm}>
                <NumberField id={ids.rpm} min={0} step={1} value={form.rpm} aria-invalid={errors.rpm ? true : undefined} onChange={(event) => update({ rpm: event.target.value })} />
              </Field>
            </div>
          </fieldset>

          {nodeOverrides.length ? (
            <Note tone="info">
              <div><NodeOverrideLine nodes={nodeOverrides} /></div>
              <div>节点级覆盖存于数据库（0028），不随这里保存的 overlay 改变。</div>
              <Button variant="secondary" className={ui.smallButton} onClick={() => { onClose(); onOpenProfileEditor() }}>在评审档位中清除</Button>
            </Note>
          ) : null}

          <Note tone="neutral">
            配置模型通过 overlay 编辑，不写数据库；字段值来自“内置默认 &lt; base YAML &lt; overlay”，节点级参数由 0028 覆盖。
            <code>user:</code> 模型的全局默认（0029）不作用于配置模型。保存后显示“待重启”，重启可能取消在途评审。
          </Note>

          {status ? <span className={styles.resultOk} role="status">{status}</span> : null}
          {error ? (
            <Note tone="danger" role="alert">
              <div>{error}</div>
              {details.length ? (
                <ul className={styles.configDetails}>
                  {details.map((detail) => <li key={detail}>{detail}</li>)}
                </ul>
              ) : null}
              {stale ? <Button variant="secondary" className={ui.smallButton} onClick={onReload}>重新加载</Button> : null}
            </Note>
          ) : null}
        </div>
      </Drawer>
      <ConfirmDialog
        open={resetOpen}
        onClose={() => setResetOpen(false)}
        onConfirm={() => { void reset() }}
        title={`重置配置模型 ${providerName}`}
        description={`删除 overlay 中 providers.${providerName} 的全部覆盖，恢复为 base 配置。若该 provider 只存在于 overlay，将移除该模型；仍被配置档位或 0028 节点覆盖引用时返回 409，不会自动修改数据库。`}
        confirmLabel="重置为 base"
        pending={pending}
      />
    </>
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
  if (key === 'max_tokens' && (value === 0 || value === null || value === undefined)) {
    return '不限（由上游 key 额度决定）'
  }
  return typeof value === 'object' ? JSON.stringify(value) : String(value)
}

function ProfilePanel({ detail, models, canWrite, onEdit }: {
  detail: ProfileDetail
  models: Model[]
  canWrite: boolean
  onEdit: () => void
}) {
  const config = detail.effective
  const steps = useMemo(() => profileSteps(config), [config])
  const budget = budgetLabel(asRecord(config?.budget))
  return (
    <Panel
      title={<><span className={styles.profileTitle}>{detail.profile}</span> <small>可覆盖节点</small></>}
      aside={canWrite ? <Button variant="secondary" className={ui.smallButton} onClick={onEdit}>编辑档位</Button> : undefined}
    >
      <div className={styles.profileMeta}>
        <span>默认 provider：{text(config?.provider) ?? '未配置'}</span>
        <span>探索 → 四路并行 → 裁决 → 汇报</span>
      </div>
      {steps.length ? (
        <ol className={styles.profileSteps} aria-label={`${detail.profile} 拓扑步骤`}>
          {steps.map((step, index) => (
            <li key={`${step.stage}-${index}`} className={styles.profileStep}>
              <span className={styles.profileStepN}>{index + 1} · {step.stage}</span>
              <span className={styles.profileStepWhat}>{step.what}</span>
              <span className={styles.profileStepWho}>{step.who}</span>
            </li>
          ))}
        </ol>
      ) : <span className="small muted">配置未提供拓扑步骤。</span>}
      {detail.nodes?.length ? <ProfileNodeList nodes={detail.nodes} models={models} /> : null}
      {budget ? <span className={styles.budget}>{budget}</span> : null}
      <Disclosure summary="查看完整档位配置">
        <pre className={`pre ${ui.jsonBlock}`}>{formatJson(config)}</pre>
      </Disclosure>
    </Panel>
  )
}

export function ModelsPage() {
  const canWrite = useCanWrite()
  const queryClient = useQueryClient()
  const modelsId = useId()
  const defaultsId = useId()
  const profilesId = useId()
  const models = useQuery({ queryKey: ['models'], queryFn: listModels, retry: false })
  const modelDefaults = useQuery({ queryKey: ['model-defaults'], queryFn: getModelDefaults, retry: false, enabled: canWrite })
  const modelPresets = useQuery({ queryKey: ['model-presets'], queryFn: listModelPresets, retry: false, enabled: canWrite })
  const profiles = useQuery({ queryKey: ['profiles'], queryFn: () => listProfiles(), retry: false })
  const defaultProfile = useQuery({ queryKey: ['profile-default'], queryFn: getDefaultProfile, retry: false })
  // The config snapshot is the file-effective view; it is the source for the
  // config: model editor and its per-field origins. Writes still go to the
  // overlay endpoint and need a restart before the runtime catalog changes.
  // After a write, poll faster until the watcher has published the revision
  // the write returned (~2-4s), then fall back to the normal refresh interval.
  const [applyingOverlayRevision, setApplyingOverlayRevision] = useState<string>()
  const fileConfig = useQuery({
    queryKey: ['config'],
    queryFn: getConfig,
    retry: false,
    enabled: canWrite,
    refetchInterval: (query) => {
      if (!applyingOverlayRevision) return 5000
      return query.state.data?.overlay_revision === applyingOverlayRevision ? 5000 : 1000
    },
  })
  const configApplying = Boolean(
    applyingOverlayRevision && fileConfig.data?.overlay_revision !== applyingOverlayRevision,
  )
  useEffect(() => {
    if (!applyingOverlayRevision || fileConfig.data?.overlay_revision !== applyingOverlayRevision) return
    setApplyingOverlayRevision(undefined)
    setConfigNotice((current) => (
      current?.startsWith('已保存到 overlay，正在应用')
        ? '已保存到 overlay，待重启生效。重启服务可能取消在途评审。'
        : current
    ))
  }, [applyingOverlayRevision, fileConfig.data?.overlay_revision])
  const [profileEditing, setProfileEditing] = useState(false)
  const [editing, setEditing] = useState<Model>()
  const [configEditing, setConfigEditing] = useState<Model>()
  const [configNotice, setConfigNotice] = useState<string>()
  const [drawerOpen, setDrawerOpen] = useState(false)
  const [deleting, setDeleting] = useState<Model>()
  const [deleteError, setDeleteError] = useState<unknown>()
  const configProviders = useMemo(() => configProvidersOf(fileConfig.data), [fileConfig.data])
  const configProviderByName = useMemo(
    () => new Map(configProviders.map((provider) => [provider.name, provider])),
    [configProviders],
  )
  const configOrigins = fileConfig.data?.origins
  const configNodes = defaultProfile.data?.nodes
  // The defaults form lives at page level so an unsaved draft survives the
  // panel unmounting while a model or profile drawer is open.
  const defaultsRevision = modelDefaults.data?.revision ?? 0
  const [defaultsForm, setDefaultsForm] = useState<DefaultsForm>(emptyDefaultsForm)
  useEffect(() => {
    setDefaultsForm(defaultsFormFrom(modelDefaults.data))
  }, [defaultsRevision])

  const saved = () => {
    void queryClient.invalidateQueries({ queryKey: ['models'] })
    void queryClient.invalidateQueries({ queryKey: ['model-defaults'] })
    void queryClient.invalidateQueries({ queryKey: ['providers'] })
    void queryClient.invalidateQueries({ queryKey: ['profiles'] })
    void queryClient.invalidateQueries({ queryKey: ['profile-default'] })
    void queryClient.invalidateQueries({ queryKey: ['config'] })
    void queryClient.invalidateQueries({ queryKey: ['config-status'] })
  }

  const configSaved = (result: ConfigOverlayWriteResponse) => {
    setConfigEditing(undefined)
    if (!result.changed) {
      setConfigNotice('没有需要保存的更改。')
    } else {
      setApplyingOverlayRevision(result.overlay_revision)
      setConfigNotice(result.restart_required
        ? '已保存到 overlay，正在应用配置快照；待重启生效。重启服务可能取消在途评审。'
        : '已保存到 overlay，正在应用配置快照。')
    }
    saved()
    void fileConfig.refetch()
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

  const onboarding = (profiles.data?.profiles ?? []).find((profile) => profile.name === 'onboarding')
  const onboardingProvider = text(asRecord(onboarding?.config?.agent)?.provider)
  const defaultsForbidden = modelDefaults.isError && modelDefaults.error instanceof ApiError && modelDefaults.error.status === 403

  return (
    <main className={ui.page}>
      <div className={ui.pageStack}>
        <PageHeader
          title="模型与档位"
          description="配置模型来自平台配置文件，可通过 overlay 编辑并重启生效；用户模型在此管理。API key 只写不回显。"
        />

        {canWrite && !defaultsForbidden && !drawerOpen && !profileEditing && !configEditing ? (
          <section className={styles.section} aria-labelledby={defaultsId}>
            <div className={styles.sectionHeader}>
              <h2 className={styles.sectionTitle} id={defaultsId}>全局默认参数</h2>
            </div>
            <ModelDefaultsPanel
              defaults={modelDefaults.data}
              form={defaultsForm}
              onFormChange={setDefaultsForm}
              loading={modelDefaults.isPending}
              loadError={modelDefaults.isError ? modelDefaults.error : undefined}
              onReload={() => { void modelDefaults.refetch() }}
              onSaved={saved}
            />
          </section>
        ) : null}

        <section className={styles.section} aria-labelledby={modelsId}>
          <div className={styles.sectionHeader}>
            <h2 className={styles.sectionTitle} id={modelsId}>模型</h2>
            {canWrite ? <Button onClick={() => { setEditing(undefined); setDrawerOpen(true) }}>新增模型</Button> : null}
          </div>
          {models.isPending ? <LoadingState /> : null}
          {models.isError ? <ErrorState error={models.error} onRetry={models.refetch} /> : null}
          {models.isSuccess && models.data.models.length === 0 ? <EmptyState title="还没有模型" description="新增用户模型，或检查平台配置。" /> : null}
          {configNotice ? <Note tone="success" role="status">{configNotice}</Note> : null}
          {canWrite && fileConfig.isError && !(fileConfig.error instanceof ApiError && fileConfig.error.status === 403) ? (
            <Note tone="warn" role="status">配置文件快照不可用，config: 模型暂不能在控制台编辑。</Note>
          ) : null}
          <div className={styles.cards}>
            {models.data?.models.map((model) => (
              <ModelCard
                key={model.id}
                model={model}
                canWrite={canWrite}
                configProvider={configProviderByName.get(configName(model.id))}
                origins={configOrigins}
                nodeOverrides={configNodeOverrides(configNodes, configName(model.id))}
                applying={configApplying && model.source === 'config'}
                onEdit={() => { setEditing(model); setDrawerOpen(true) }}
                onEditConfig={() => { setConfigNotice(undefined); setConfigEditing(model) }}
                onDelete={() => { setDeleting(model); setDeleteError(undefined) }}
                onClearNodeOverrides={() => setProfileEditing(true)}
              />
            ))}
          </div>
        </section>

        <section className={styles.section} aria-labelledby={profilesId}>
          <div className={styles.sectionHeader}>
            <h2 className={styles.sectionTitle} id={profilesId}>评审档位</h2>
          </div>
          {defaultProfile.isPending ? <LoadingState /> : null}
          {defaultProfile.isError ? <ErrorState error={defaultProfile.error} onRetry={defaultProfile.refetch} /> : null}
          {defaultProfile.isSuccess && defaultProfile.data
            ? <ProfilePanel detail={defaultProfile.data} models={models.data?.models ?? []} canWrite={canWrite} onEdit={() => setProfileEditing(true)} />
            : null}
          <span className={styles.sectionAside}>基线来自平台配置；默认档位可按节点覆盖模型与参数</span>
          {onboarding ? <span className="small muted">项目接入（onboarding，不是评审档位）使用 provider：{onboardingProvider ?? '未配置'}</span> : null}
        </section>
      </div>

      <ModelDrawer
        open={drawerOpen}
        model={editing}
        presets={modelPresets.data?.presets ?? []}
        presetsLoading={modelPresets.isPending}
        presetsFailed={modelPresets.isError}
        presetsUnavailable={modelPresets.isError && modelPresets.error instanceof ApiError && modelPresets.error.status === 404}
        inheritedMaxTokens={modelDefaults.data?.params?.max_tokens ?? 0}
        onReloadPresets={() => { void modelPresets.refetch() }}
        onClose={() => setDrawerOpen(false)}
        onSaved={saved}
      />
      <ConfigModelDrawer
        open={Boolean(configEditing)}
        model={configEditing}
        provider={configEditing ? configProviderByName.get(configName(configEditing.id)) : undefined}
        origins={configOrigins}
        nodeOverrides={configEditing ? configNodeOverrides(configNodes, configName(configEditing.id)) : []}
        baseRevision={fileConfig.data?.base_revision ?? ''}
        overlayRevision={fileConfig.data?.overlay_revision ?? ''}
        onClose={() => setConfigEditing(undefined)}
        onSaved={configSaved}
        onReload={() => { void fileConfig.refetch() }}
        onOpenProfileEditor={() => setProfileEditing(true)}
      />
      <ProfileEditor
        open={profileEditing}
        detail={defaultProfile.data}
        models={models.data?.models ?? []}
        onClose={() => setProfileEditing(false)}
        onSaved={saved}
        onReload={async () => {
          const result = await defaultProfile.refetch()
          return !result.isError
        }}
      />
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
