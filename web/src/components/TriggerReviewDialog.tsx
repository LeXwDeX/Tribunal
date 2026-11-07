import { useEffect, useId, useState } from 'react'
import { triggerReview } from '../api/api'
import type { Project } from '../api/types'
import { Button, Dialog, ErrorState, Field } from './ui'

export function TriggerReviewDialog({
  open, onClose, projects, onTriggered, presetProjectId, presetIid, presetProjectLabel,
}: {
  open: boolean
  onClose: () => void
  projects: Project[]
  onTriggered: (jobId?: string) => void
  presetProjectId?: number | string
  presetIid?: number | string
  presetProjectLabel?: string
}) {
  const projectId = useId()
  const iid = useId()
  const [selectedProject, setSelectedProject] = useState(String(presetProjectId ?? ''))
  const [mrIid, setMrIid] = useState(String(presetIid ?? ''))
  const [full, setFull] = useState(false)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [fieldErrors, setFieldErrors] = useState<{ project?: string; iid?: string }>({})

  useEffect(() => {
    if (!open) return
    setSelectedProject(String(presetProjectId ?? ''))
    setMrIid(String(presetIid ?? ''))
    setFull(false)
    setError(null)
    setFieldErrors({})
    setPending(false)
  }, [open, presetProjectId, presetIid])

  const submit = async () => {
    const errors: { project?: string; iid?: string } = {}
    if (!selectedProject) errors.project = '请选择项目。'
    const parsedIid = Number(mrIid)
    if (!Number.isInteger(parsedIid) || parsedIid <= 0) errors.iid = 'MR IID 必须是正整数。'
    setFieldErrors(errors)
    if (Object.keys(errors).length) return
    setPending(true)
    setError(null)
    try {
      const result = await triggerReview(selectedProject, parsedIid, { full })
      onTriggered(result?.job_id)
    } catch (caught) {
      setError(caught)
    } finally {
      setPending(false)
    }
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="手动触发评审"
      description="提交后服务端创建评审任务。这里只显示任务 ID；评审记录出现在列表中后即可查看执行图。"
      footer={(
        <>
          <Button variant="secondary" onClick={onClose} disabled={pending}>取消</Button>
          <Button variant="primary" onClick={() => { void submit() }} loading={pending}>触发评审</Button>
        </>
      )}
    >
      {error ? <ErrorState error={error} title="触发失败" /> : null}
      {projects.length === 0 && !presetProjectId ? <div className="muted">尚无已注册项目，请先注册项目。</div> : null}
      <div className="formGrid">
        <Field label="项目" htmlFor={projectId} error={fieldErrors.project}>
          <select id={projectId} className="select" value={selectedProject} onChange={(event) => setSelectedProject(event.target.value)} aria-invalid={Boolean(fieldErrors.project)} aria-describedby={fieldErrors.project ? `${projectId}-error` : undefined}>
            <option value="">选择项目</option>
            {presetProjectId && !projects.some((project) => String(project.id) === String(presetProjectId)) ? (
              <option value={String(presetProjectId)}>{presetProjectLabel || `项目 ${presetProjectId}`}</option>
            ) : null}
            {projects.map((project) => (
              <option key={project.id} value={project.id}>{project.path_with_namespace}{project.enabled ? '' : '（已停用）'}</option>
            ))}
          </select>
        </Field>
        <Field label="MR IID" htmlFor={iid} error={fieldErrors.iid}>
          <input id={iid} className="input" inputMode="numeric" value={mrIid} onChange={(event) => setMrIid(event.target.value)} placeholder="例如 318" aria-invalid={Boolean(fieldErrors.iid)} aria-describedby={fieldErrors.iid ? `${iid}-error` : undefined} />
        </Field>
      </div>
      <label className="checkboxRow">
        <input type="checkbox" checked={full} onChange={(event) => setFull(event.target.checked)} />
        <span>全量评审（full）</span>
      </label>
    </Dialog>
  )
}
