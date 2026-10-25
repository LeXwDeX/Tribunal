import { useState } from 'react'
import { describe, expect, it } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { FindingRow } from '../src/pages/ReviewDetailPage'
import { finding } from './fixtures'

function Harness() {
  const [expanded, setExpanded] = useState(false)
  return (
    <table>
      <tbody>
        <FindingRow
          finding={finding({
            body: '<img src=x onerror=alert(1)>',
            evidence: '<script>alert(1)</script>',
            challenge_evidence: '<b>not markup</b>',
          })}
          index={0}
          expanded={expanded}
          onToggle={() => setExpanded((value) => !value)}
        />
      </tbody>
    </table>
  )
}

describe('findings rendering', () => {
  it('expands evidence as escaped text, never raw model HTML', async () => {
    const user = userEvent.setup()
    const { container } = render(<Harness />)
    const toggle = screen.getByRole('button', { name: /展开：并发写 map 未加锁/ })
    expect(toggle).toHaveAttribute('aria-expanded', 'false')
    await user.click(toggle)
    expect(toggle).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByText('<img src=x onerror=alert(1)>')).toBeInTheDocument()
    expect(screen.getByText('<script>alert(1)</script>')).toBeInTheDocument()
    expect(screen.getByText('<b>not markup</b>')).toBeInTheDocument()
    expect(container.querySelector('img')).toBeNull()
    expect(container.querySelector('script')).toBeNull()
  })

  it('shows 未标注 for a read-only legacy finding without an annotation', () => {
    render(
      <table>
        <tbody>
          <FindingRow
            finding={finding({ annotation: undefined })}
            index={0}
            expanded
            onToggle={() => {}}
          />
        </tbody>
      </table>,
    )
    const saved = screen.getByRole('status', { name: '已保存的人工标注' })
    expect(within(saved).getByText('未标注')).toBeInTheDocument()
    expect(saved.querySelector('[class*="badge"]')).toHaveTextContent('未标注')
  })
})
