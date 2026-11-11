import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { setCsrfToken } from '../src/api/client'
import { ExtensionsManager } from '../src/features/extensions/ExtensionsManager'
import { skill } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders, viewerMe } from './test-utils'

type Write = { url: string; method: string; body: unknown }

const upload = () => new File(['zip-bytes'], 'team-skill.zip', { type: 'application/zip' })

function routes({ writes = [] as Write[], uploadResponse = undefined as (() => Response) | undefined, registerResponse = undefined as (() => Response) | undefined } = {}) {
  let skills = [skill({ id: 'k-builtin', name: 'code-review', builtin: true })]
  return (url: string, init?: RequestInit) => {
    const method = (init?.method || 'GET').toUpperCase()
    if (method !== 'GET') writes.push({ url, method, body: init?.body })
    if (url === '/api/v1/skills' && method === 'POST' && init?.body instanceof FormData) {
      if (uploadResponse) return uploadResponse()
      const created = skill({ id: 'k-upload', name: 'team-skill', description: 'Uploaded skill', dir: '/managed/team-skill' })
      skills = [...skills, created]
      return jsonResponse(created, 201)
    }
    if (url === '/api/v1/skills' && method === 'POST') {
      if (registerResponse) return registerResponse()
      const created = skill({ id: 'k-dir', name: 'dir-skill', description: 'Registered skill', scope: 'project', project_id: 42, dir: '/managed/dir-skill' })
      skills = [...skills, created]
      return jsonResponse(created, 201)
    }
    if (url.startsWith('/api/v1/skills')) return jsonResponse({ skills })
    if (url.startsWith('/api/v1/block-bindings')) return jsonResponse({ bindings: [] })
    return jsonResponse({ servers: [] })
  }
}

afterEach(() => { vi.unstubAllGlobals(); setCsrfToken(null) })

describe('Skill creation entries', () => {
  it('shows upload and directory registration only to writers', async () => {
    installFetch(routes())
    const writer = renderWithProviders(<ExtensionsManager scope="platform" tab="skills" onTabChange={() => {}} />)
    expect(await screen.findByRole('button', { name: '上传 Skill zip' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '登记服务器目录' })).toBeInTheDocument()
    expect(screen.getByLabelText('Skill zip 文件')).toHaveAttribute('type', 'file')
    writer.unmount()

    installFetch(routes())
    renderWithProviders(<ExtensionsManager scope="platform" tab="skills" onTabChange={() => {}} />, { me: viewerMe })
    await screen.findByText('code-review')
    expect(screen.queryByRole('button', { name: '上传 Skill zip' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '登记服务器目录' })).not.toBeInTheDocument()
    expect(screen.queryByLabelText('Skill zip 文件')).not.toBeInTheDocument()
  })

  it('uploads a zip with the file field and refreshes the list', async () => {
    const writes: Write[] = []
    installFetch(routes({ writes }))
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<ExtensionsManager scope="platform" tab="skills" onTabChange={() => {}} />)

    await screen.findByText('code-review')
    await user.upload(screen.getByLabelText('Skill zip 文件'), upload())
    const dialog = screen.getByRole('dialog', { name: '上传 Skill zip' })
    expect(within(dialog).getByText('team-skill.zip')).toBeInTheDocument()
    await user.click(within(dialog).getByRole('button', { name: '上传' }))
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '上传 Skill zip' })).not.toBeInTheDocument())

    const write = writes.find((item) => item.url === '/api/v1/skills')
    const form = write?.body as FormData
    expect(form.get('scope')).toBe('platform')
    expect(form.get('project_id')).toBeNull()
    expect((form.get('file') as File).name).toBe('team-skill.zip')
    expect(await screen.findByText('Skill team-skill 已上传。')).toBeInTheDocument()
    expect(await screen.findByText('team-skill')).toBeInTheDocument()
  })

  it('shows an actionable invalid-manifest error without closing the dialog', async () => {
    installFetch(routes({
      uploadResponse: () => jsonResponse({
        error: {
          code: 'validation_failed',
          message: 'invalid skill archive',
          details: [{ field: 'file', message: 'archive must contain SKILL.md' }],
        },
      }, 400),
    }))
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<ExtensionsManager scope="platform" tab="skills" onTabChange={() => {}} />)

    await screen.findByText('code-review')
    await user.upload(screen.getByLabelText('Skill zip 文件'), upload())
    const dialog = screen.getByRole('dialog', { name: '上传 Skill zip' })
    await user.click(within(dialog).getByRole('button', { name: '上传' }))
    expect(await within(dialog).findByText('archive must contain SKILL.md')).toBeInTheDocument()
    expect(screen.getByRole('dialog', { name: '上传 Skill zip' })).toBeInTheDocument()
  })

  it('maps a duplicate name to an actionable conflict', async () => {
    installFetch(routes({
      uploadResponse: () => jsonResponse({ error: { code: 'conflict', message: 'skill already exists' } }, 409),
    }))
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<ExtensionsManager scope="platform" tab="skills" onTabChange={() => {}} />)

    await screen.findByText('code-review')
    await user.upload(screen.getByLabelText('Skill zip 文件'), upload())
    const dialog = screen.getByRole('dialog', { name: '上传 Skill zip' })
    await user.click(within(dialog).getByRole('button', { name: '上传' }))
    expect(await within(dialog).findByText('同名 Skill 已存在，请更换名称或删除旧项后重试。')).toBeInTheDocument()
    expect(screen.getByRole('dialog', { name: '上传 Skill zip' })).toBeInTheDocument()
  })

  it('rejects a file that is not a zip before uploading', async () => {
    const writes: Write[] = []
    installFetch(routes({ writes }))
    setCsrfToken('csrf-test')
    // accept filtering would drop the file before change; the dialog still
    // validates the selected file so a wrong pick gets an actionable error.
    const user = userEvent.setup({ applyAccept: false })
    renderWithProviders(<ExtensionsManager scope="platform" tab="skills" onTabChange={() => {}} />)

    await screen.findByText('code-review')
    await user.upload(
      screen.getByLabelText('Skill zip 文件'),
      new File(['not a zip'], 'notes.txt', { type: 'text/plain' }),
    )
    const dialog = screen.getByRole('dialog', { name: '上传 Skill zip' })
    await user.click(within(dialog).getByRole('button', { name: '上传' }))
    expect(await within(dialog).findByText('请选择 .zip 文件。')).toBeInTheDocument()
    expect(writes).toHaveLength(0)
  })

  it('rejects an archive larger than 8 MiB before uploading', async () => {
    const writes: Write[] = []
    installFetch(routes({ writes }))
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<ExtensionsManager scope="platform" tab="skills" onTabChange={() => {}} />)

    await screen.findByText('code-review')
    const big = new File(['x'], 'big.zip', { type: 'application/zip' })
    Object.defineProperty(big, 'size', { value: 8 * 1024 * 1024 + 1 })
    await user.upload(screen.getByLabelText('Skill zip 文件'), big)
    const dialog = screen.getByRole('dialog', { name: '上传 Skill zip' })
    await user.click(within(dialog).getByRole('button', { name: '上传' }))
    expect(await within(dialog).findByText('zip 文件不能超过 8 MiB。')).toBeInTheDocument()
    expect(writes).toHaveLength(0)
  })

  it('warns that a lost upload response is uncertain', async () => {
    installFetch(routes({
      uploadResponse: () => jsonResponse({ error: { code: 'internal', message: 'boom' } }, 500),
    }))
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<ExtensionsManager scope="platform" tab="skills" onTabChange={() => {}} />)

    await screen.findByText('code-review')
    await user.upload(screen.getByLabelText('Skill zip 文件'), upload())
    const dialog = screen.getByRole('dialog', { name: '上传 Skill zip' })
    await user.click(within(dialog).getByRole('button', { name: '上传' }))
    expect(await within(dialog).findByText(/请求结果可能不确定/)).toBeInTheDocument()
  })
})

describe('register a server-side Skill directory', () => {
  it('requires an absolute path and copies the directory through the JSON API', async () => {
    const writes: Write[] = []
    installFetch(routes({ writes }))
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<ExtensionsManager scope="project" projectId={42} canManage tab="skills" onTabChange={() => {}} />)

    await screen.findByText('code-review')
    await user.click(screen.getByRole('button', { name: '登记服务器目录' }))
    const dialog = screen.getByRole('dialog', { name: '登记服务器目录' })
    expect(within(dialog).getByText(/读取服务端文件系统路径并复制到受管目录/)).toBeInTheDocument()
    await user.type(within(dialog).getByRole('textbox', { name: '服务端绝对路径' }), 'skills/team')
    await user.click(within(dialog).getByRole('button', { name: '登记' }))
    expect(await within(dialog).findByText('请输入以 / 开头的绝对路径。')).toBeInTheDocument()
    expect(writes).toHaveLength(0)

    await user.clear(within(dialog).getByRole('textbox', { name: '服务端绝对路径' }))
    await user.type(within(dialog).getByRole('textbox', { name: '服务端绝对路径' }), '/srv/skills/team')
    await user.click(within(dialog).getByRole('button', { name: '登记' }))
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '登记服务器目录' })).not.toBeInTheDocument())
    expect(writes[0]).toMatchObject({
      url: '/api/v1/skills',
      method: 'POST',
      body: JSON.stringify({ scope: 'project', project_id: 42, dir: '/srv/skills/team' }),
    })
    expect(await screen.findByText('Skill dir-skill 已登记。')).toBeInTheDocument()
  })

  it('shows a directory-field error from the backend', async () => {
    installFetch(routes({
      registerResponse: () => jsonResponse({
        error: {
          code: 'validation_failed',
          message: 'invalid skill directory',
          details: [{ field: 'dir', message: 'dir must contain a regular SKILL.md' }],
        },
      }, 400),
    }))
    setCsrfToken('csrf-test')
    const user = userEvent.setup()
    renderWithProviders(<ExtensionsManager scope="platform" tab="skills" onTabChange={() => {}} />)

    await screen.findByText('code-review')
    await user.click(screen.getByRole('button', { name: '登记服务器目录' }))
    const dialog = screen.getByRole('dialog', { name: '登记服务器目录' })
    await user.type(within(dialog).getByRole('textbox', { name: '服务端绝对路径' }), '/srv/skills/missing')
    await user.click(within(dialog).getByRole('button', { name: '登记' }))
    expect(await within(dialog).findByText('dir must contain a regular SKILL.md')).toBeInTheDocument()
    expect(within(dialog).getByRole('textbox', { name: '服务端绝对路径' })).toHaveAttribute('aria-invalid', 'true')
  })
})
