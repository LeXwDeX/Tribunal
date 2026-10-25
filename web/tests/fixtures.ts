// Test-only fixtures. Production pages never import this module.
import type {
  BlockBinding, ContextDoc, Finding, GraphEdge, GraphNode, McpServer, Project, ReviewDetail, Skill,
} from '../src/api/types'

export function project(overrides: Partial<Project> = {}): Project {
  return {
    id: 42,
    path_with_namespace: 'group/backend',
    web_url: 'https://gitlab.example.com/group/backend',
    default_branch: 'main',
    profile: 'default',
    settings: {},
    enabled: true,
    access: 'manage',
    ...overrides,
  }
}

export function review(overrides: Partial<ReviewDetail> = {}): ReviewDetail {
  return {
    id: '0192f0c1-aaaa-bbbb-cccc-000000000001',
    project_id: 42,
    project_path: 'group/backend',
    mr_iid: 318,
    mr_title: 'feat: 支持批量导入',
    profile: 'default',
    trigger: 'mr_update',
    status: 'running',
    verdict: null,
    created_at: '2026-10-22T05:00:00Z',
    access: 'manage',
    ...overrides,
  }
}

export const nodes: GraphNode[] = [
  { id: 'plan', kind: 'agent', status: 'completed', depends_on: [] },
  { id: 'review.a', kind: 'agent', status: 'completed', depends_on: ['plan'] },
  { id: 'review.b', kind: 'agent', status: 'running', depends_on: ['plan'] },
  { id: 'verify.f7', kind: 'agent', status: 'pending', depends_on: ['review.a', 'review.b'] },
]
export const edges: GraphEdge[] = [
  { from: 'plan', to: 'review.a' },
  { from: 'plan', to: 'review.b' },
  { from: 'review.a', to: 'verify.f7' },
]

export function finding(overrides: Partial<Finding> = {}): Finding {
  return {
    id: 'f1',
    workflow_id: 'wf',
    origin_node: 'review.a',
    severity: 'P1',
    title: '并发写 map 未加锁',
    body: 'body text',
    evidence: '{"path":"file.go","line":3}',
    challenge_status: 'confirmed',
    decision: 'publish',
    decision_reason: '确认',
    annotation: '',
    path: 'internal/file.go',
    line: 88,
    ...overrides,
  }
}

export function mcpServer(overrides: Partial<McpServer> = {}): McpServer {
  return {
    id: 's1',
    scope: 'platform',
    name: 'codebase-memory',
    config: { command: 'codebase-memory-mcp', env: { TOKEN: '[REDACTED]' } },
    enabled: true,
    ...overrides,
  }
}

export function skill(overrides: Partial<Skill> = {}): Skill {
  return { id: 'k1', scope: 'platform', name: 'coding-standards', description: '标准', enabled: true, ...overrides }
}

export function binding(overrides: Partial<BlockBinding> = {}): BlockBinding {
  return { scope: 'platform', block: 'review', mcp_tools: [], skills: [], ...overrides }
}

export function contextDoc(overrides: Partial<ContextDoc> = {}): ContextDoc {
  return { id: 'd1', project_id: 42, kind: 'agents', source: 'generated', content: '# Context', locked: false, ...overrides }
}
