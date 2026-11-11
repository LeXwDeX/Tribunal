import { createRef, useState } from 'react'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {
  Checkbox, DatePicker, Disclosure, Field, NumberField, SearchField, Select, Textarea, Tooltip, computeTooltipPosition,
  formatLocalDate, parseLocalDate, type DatePickerProps, type SelectOption,
} from '../src/components/ui'
import ui from '../src/styles/ui.module.css'

const PROFILE_OPTIONS: SelectOption[] = [
  { value: 'a', label: '模型 A' },
  { value: 'b', label: '模型 B' },
  { value: 'c', label: '模型 C' },
  { value: 'd', label: '模型 D' },
]

function SelectHarness({
  initial = 'b', options = PROFILE_OPTIONS, disabled, emptyLabel,
}: { initial?: string; options?: SelectOption[]; disabled?: boolean; emptyLabel?: string }) {
  const [value, setValue] = useState(initial)
  return (
    <Field label="评审档位" htmlFor="profile">
      <Select
        id="profile"
        value={value}
        options={options}
        onChange={setValue}
        disabled={disabled}
        emptyLabel={emptyLabel}
      />
    </Field>
  )
}

function activeOptionText(trigger: HTMLElement) {
  const id = trigger.getAttribute('aria-activedescendant')
  return id ? document.getElementById(id)?.textContent ?? null : null
}

describe('Select', () => {
  it('exposes a named combobox with a listbox of selected options', async () => {
    const user = userEvent.setup()
    render(<SelectHarness />)
    const trigger = screen.getByRole('combobox', { name: '评审档位' })
    expect(trigger).toHaveAttribute('aria-expanded', 'false')
    expect(trigger).toHaveTextContent('模型 B')

    await user.click(trigger)
    expect(trigger).toHaveAttribute('aria-expanded', 'true')
    const listbox = screen.getByRole('listbox')
    expect(listbox.tagName).not.toBe('SELECT')
    expect(within(listbox).getAllByRole('option')).toHaveLength(4)
    expect(screen.getByRole('option', { name: '模型 B' })).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByRole('option', { name: '模型 A' })).toHaveAttribute('aria-selected', 'false')
  })

  it('skips disabled options and selects the active option with Enter', async () => {
    const user = userEvent.setup()
    const options: SelectOption[] = [
      { value: 'a', label: '模型 A' },
      { value: 'b', label: '模型 B', disabled: true },
      { value: 'c', label: '模型 C' },
    ]
    render(<SelectHarness initial="" options={options} />)
    const trigger = screen.getByRole('combobox', { name: '评审档位' })
    trigger.focus()
    await user.keyboard('{Enter}')
    expect(screen.getByRole('option', { name: '模型 B' })).toHaveAttribute('aria-disabled', 'true')
    expect(activeOptionText(trigger)).toBe('模型 A')

    await user.keyboard('{ArrowDown}')
    expect(activeOptionText(trigger)).toBe('模型 C')

    await user.keyboard('{ArrowDown}')
    expect(activeOptionText(trigger)).toBe('模型 C')

    await user.keyboard('{Enter}')
    expect(trigger).toHaveTextContent('模型 C')
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    expect(trigger).toHaveFocus()
  })

  it('supports Home, End, arrow navigation, Escape and Tab', async () => {
    const user = userEvent.setup()
    render(
      <>
        <SelectHarness initial="b" />
        <button type="button">下一步</button>
      </>,
    )
    const trigger = screen.getByRole('combobox', { name: '评审档位' })
    trigger.focus()

    await user.keyboard('{Enter}')
    expect(activeOptionText(trigger)).toBe('模型 B')
    await user.keyboard('{Home}')
    expect(activeOptionText(trigger)).toBe('模型 A')
    await user.keyboard('{End}')
    expect(activeOptionText(trigger)).toBe('模型 D')
    await user.keyboard('{ArrowUp}')
    expect(activeOptionText(trigger)).toBe('模型 C')

    await user.keyboard('{Escape}')
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    expect(trigger).toHaveFocus()

    await user.keyboard(' ')
    expect(screen.getByRole('listbox')).toBeInTheDocument()
    await user.tab()
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '下一步' })).toHaveFocus()
  })

  it('moves the active option with typeahead', async () => {
    const user = userEvent.setup()
    const options: SelectOption[] = [
      { value: 'alpha', label: 'Alpha' },
      { value: 'beta', label: 'Beta' },
      { value: 'gamma', label: 'Gamma' },
    ]
    render(<SelectHarness initial="" options={options} />)
    const trigger = screen.getByRole('combobox', { name: '评审档位' })
    trigger.focus()

    await user.keyboard('{Enter}')
    await user.keyboard('ga')
    expect(activeOptionText(trigger)).toBe('Gamma')
    await user.keyboard('{Enter}')
    expect(trigger).toHaveTextContent('Gamma')
  })

  it('closes when clicking outside', async () => {
    const user = userEvent.setup()
    render(<SelectHarness />)
    const trigger = screen.getByRole('combobox', { name: '评审档位' })
    await user.click(trigger)
    expect(screen.getByRole('listbox')).toBeInTheDocument()
    await user.click(document.body)
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
  })

  it('shows the inherit label for an empty value and falls back to unknown values', () => {
    const { rerender } = render(
      <Select id="value-empty" value="" options={PROFILE_OPTIONS} onChange={() => {}} />,
    )
    expect(screen.getByRole('combobox')).toHaveTextContent('继承')

    rerender(<Select id="unknown-value" value="legacy-model" options={PROFILE_OPTIONS} onChange={() => {}} />)
    expect(screen.getByRole('combobox')).toHaveTextContent('legacy-model')
  })

  it('truncates a long selected label without overflowing the trigger', async () => {
    const user = userEvent.setup()
    const longLabel = '超长模型名称'.repeat(24)
    const options: SelectOption[] = [{ value: 'long', label: longLabel }]
    const originalWidth = window.innerWidth
    Object.defineProperty(window, 'innerWidth', { configurable: true, value: 390 })
    try {
      render(<Select id="long" value="long" options={options} onChange={() => {}} />)
      const trigger = screen.getByRole('combobox')
      const value = trigger.firstElementChild as HTMLElement
      expect(trigger).toHaveTextContent(longLabel)
      expect(getComputedStyle(value).textOverflow).toBe('ellipsis')
      expect(getComputedStyle(value).whiteSpace).toBe('nowrap')
      expect(value.classList.contains(ui.selectValue)).toBe(true)

      await user.click(trigger)
      const listbox = screen.getByRole('listbox')
      expect(listbox).toHaveTextContent(longLabel)
      expect(parseFloat(listbox.style.width)).toBeLessThanOrEqual(390 - 16)
      expect(parseFloat(listbox.style.left)).toBeGreaterThanOrEqual(8)
    } finally {
      Object.defineProperty(window, 'innerWidth', { configurable: true, value: originalWidth })
    }
  })

  it('forwards its trigger ref', () => {
    const ref = createRef<HTMLButtonElement>()
    render(<Select ref={ref} id="ref-select" value="b" options={PROFILE_OPTIONS} onChange={() => {}} />)
    expect(ref.current).toBe(screen.getByRole('combobox'))
  })

  it('keeps the trigger disabled and closed when disabled', async () => {
    const user = userEvent.setup()
    render(<SelectHarness disabled />)
    const trigger = screen.getByRole('combobox', { name: '评审档位' })
    expect(trigger).toBeDisabled()
    await user.click(trigger)
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
  })

  it('shows a readable state for empty and fully disabled option lists', async () => {
    const user = userEvent.setup()
    const { rerender } = render(<Select id="empty-options" value="" options={[]} onChange={() => {}} />)
    const trigger = screen.getByRole('combobox')
    await user.click(trigger)
    expect(screen.getByText('无可选项')).toBeInTheDocument()
    await user.keyboard('{Escape}')

    rerender(
      <Select
        id="disabled-options"
        value=""
        options={[{ value: 'a', label: '模型 A', disabled: true }]}
        onChange={() => {}}
      />,
    )
    await user.click(trigger)
    expect(screen.getByText('没有可用选项')).toBeInTheDocument()
  })
})

function CheckboxHarness({ disabled }: { disabled?: boolean }) {
  const [checked, setChecked] = useState(false)
  return (
    <Checkbox
      label="跳过 Draft"
      checked={checked}
      disabled={disabled}
      onChange={(event) => setChecked(event.target.checked)}
    />
  )
}

describe('Checkbox', () => {
  it('toggles from the label and with Space', async () => {
    const user = userEvent.setup()
    render(<CheckboxHarness />)
    const checkbox = screen.getByRole('checkbox', { name: '跳过 Draft' })
    expect(checkbox).not.toBeChecked()

    await user.click(screen.getByText('跳过 Draft'))
    expect(checkbox).toBeChecked()

    checkbox.focus()
    expect(checkbox).toHaveFocus()
    await user.keyboard(' ')
    expect(checkbox).not.toBeChecked()
  })

  it('keeps a focus-visible ring on the custom box', async () => {
    const user = userEvent.setup()
    render(<CheckboxHarness />)
    await user.tab()
    const checkbox = screen.getByRole('checkbox', { name: '跳过 Draft' })
    expect(checkbox).toHaveFocus()
    const css = readFileSync(resolve(process.cwd(), 'src/styles/ui.module.css'), 'utf8')
    expect(css).toMatch(/\.checkboxInput:focus-visible\s*\+\s*\.checkboxBox\s*\{[^}]*outline:/)
  })

  it('does not toggle when disabled', async () => {
    const user = userEvent.setup()
    render(<CheckboxHarness disabled />)
    const checkbox = screen.getByRole('checkbox', { name: '跳过 Draft' })
    expect(checkbox).toBeDisabled()
    await user.click(screen.getByText('跳过 Draft'))
    expect(checkbox).not.toBeChecked()
  })
})

function DateHarness(
  props: Omit<DatePickerProps, 'value' | 'onChange'> & { initial?: string; onChange?: (value: string) => void },
) {
  const { initial = '', onChange, ...rest } = props
  const [value, setValue] = useState(initial)
  return (
    <>
      <Field label="开始日期" htmlFor="from" hint="使用本地日期。">
        <DatePicker
          id="from"
          value={value}
          onChange={(next) => {
            onChange?.(next)
            setValue(next)
          }}
          {...rest}
        />
      </Field>
      <output aria-label="已提交日期">{value}</output>
    </>
  )
}

function committedDate() {
  return screen.getByLabelText('已提交日期')
}

describe('DatePicker', () => {
  it('parses and formats calendar days in local time', () => {
    expect(parseLocalDate('2026-11-01')).toEqual(new Date(2026, 10, 1))
    expect(formatLocalDate(new Date(2026, 10, 1))).toBe('2026-11-01')
    expect(parseLocalDate('2026-02-30')).toBeNull()
    expect(parseLocalDate('2026-13-01')).toBeNull()
    expect(parseLocalDate('2026-1-1')).toBeNull()
  })

  it('accepts a manually typed valid date and commits it to the parent', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(<DateHarness onChange={onChange} />)
    const input = screen.getByLabelText('开始日期')
    await user.type(input, '2026-11-01')
    expect(input).toHaveValue('2026-11-01')
    expect(committedDate()).toHaveTextContent('2026-11-01')
    expect(onChange).toHaveBeenLastCalledWith('2026-11-01')
    expect(input).not.toHaveAttribute('aria-invalid', 'true')
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('reports an impossible date without emitting it', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(<DateHarness onChange={onChange} />)
    const input = screen.getByLabelText('开始日期')
    await user.type(input, '2026-02-30')
    expect(screen.getByRole('alert')).toHaveTextContent('请输入 YYYY-MM-DD 格式的有效日期。')
    expect(input).toHaveAttribute('aria-invalid', 'true')
    expect(onChange).not.toHaveBeenCalled()
    expect(committedDate()).toBeEmptyDOMElement()
  })

  it('reports dates outside min and max', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(<DateHarness min="2026-10-01" max="2026-10-31" onChange={onChange} />)
    const input = screen.getByLabelText('开始日期')
    await user.type(input, '2026-09-30')
    expect(screen.getByRole('alert')).toHaveTextContent('日期不能早于 2026-10-01。')

    await user.clear(input)
    await user.type(input, '2026-11-02')
    expect(screen.getByRole('alert')).toHaveTextContent('日期不能晚于 2026-10-31。')
    expect(onChange).not.toHaveBeenCalled()
    expect(committedDate()).toBeEmptyDOMElement()
  })

  it('selects a day from the calendar and closes it', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(<DateHarness initial="2026-10-15" onChange={onChange} />)
    const input = screen.getByLabelText('开始日期')
    await user.click(screen.getByRole('button', { name: '打开日历' }))

    expect(screen.getByRole('dialog', { name: '选择日期' })).toBeInTheDocument()
    expect(screen.getByText('2026年10月')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '2026年10月15日' })).toHaveAttribute('aria-pressed', 'true')

    await user.click(screen.getByRole('button', { name: '2026年10月20日' }))
    expect(input).toHaveValue('2026-10-20')
    expect(committedDate()).toHaveTextContent('2026-10-20')
    expect(onChange).toHaveBeenLastCalledWith('2026-10-20')
    expect(screen.queryByRole('dialog', { name: '选择日期' })).not.toBeInTheDocument()
    expect(input).toHaveFocus()
  })

  it('disables calendar days outside min and max, keeping the boundaries enabled', async () => {
    const user = userEvent.setup()
    render(<DateHarness initial="2026-10-15" min="2026-10-10" max="2026-10-20" />)
    await user.click(screen.getByRole('button', { name: '打开日历' }))
    expect(screen.getByRole('button', { name: '2026年10月5日' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '2026年10月9日' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '2026年10月10日' })).toBeEnabled()
    expect(screen.getByRole('button', { name: '2026年10月20日' })).toBeEnabled()
    expect(screen.getByRole('button', { name: '2026年10月21日' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '2026年10月25日' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '2026年10月15日' })).toBeEnabled()
  })

  it('steps through local calendar days and months', async () => {
    // Node fixes the process time zone at start-up. This asserts local
    // calendar-day arithmetic (never UTC string parsing); run the suite with
    // TZ=America/New_York to exercise a real DST transition.
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(<DateHarness initial="2026-11-01" onChange={onChange} />)
    const input = screen.getByLabelText('开始日期')
    input.focus()
    await user.keyboard('{ArrowDown}')
    expect(screen.getByRole('dialog', { name: '选择日期' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '2026年11月1日' })).toHaveFocus()

    await user.keyboard('{ArrowRight}')
    expect(screen.getByRole('button', { name: '2026年11月2日' })).toHaveFocus()
    await user.keyboard('{Enter}')
    expect(input).toHaveValue('2026-11-02')
    expect(committedDate()).toHaveTextContent('2026-11-02')
    expect(onChange).toHaveBeenLastCalledWith('2026-11-02')
    expect(input).toHaveFocus()

    await user.click(screen.getByRole('button', { name: '打开日历' }))
    await user.click(screen.getByRole('button', { name: '下个月' }))
    expect(screen.getByText('2026年12月')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '上个月' }))
    expect(screen.getByText('2026年11月')).toBeInTheDocument()
  })

  it('keeps one grid tab stop when typing another month while open', async () => {
    const user = userEvent.setup()
    render(<DateHarness initial="2026-10-15" />)
    const input = screen.getByLabelText('开始日期')
    await user.click(screen.getByRole('button', { name: '打开日历' }))
    expect(screen.getByRole('button', { name: '2026年10月15日' })).toHaveFocus()

    input.focus()
    await user.clear(input)
    await user.type(input, '2026-12-20')
    expect(input).toHaveFocus()
    expect(committedDate()).toHaveTextContent('2026-12-20')
    expect(screen.getByText('2026年12月')).toBeInTheDocument()

    const dialog = screen.getByRole('dialog', { name: '选择日期' })
    const stops = within(dialog).getAllByRole('button').filter((button) => button.getAttribute('tabindex') === '0')
    expect(stops).toHaveLength(1)
    expect(stops[0]).toHaveAccessibleName('2026年12月20日')
  })

  it('supports Home, End, PageUp and PageDown in the day grid', async () => {
    const user = userEvent.setup()
    render(<DateHarness initial="2026-10-15" />)
    const input = screen.getByLabelText('开始日期')
    input.focus()
    await user.keyboard('{ArrowDown}')
    expect(screen.getByRole('button', { name: '2026年10月15日' })).toHaveFocus()

    await user.keyboard('{End}')
    expect(screen.getByRole('button', { name: '2026年10月18日' })).toHaveFocus()
    await user.keyboard('{Home}')
    expect(screen.getByRole('button', { name: '2026年10月12日' })).toHaveFocus()

    await user.keyboard('{PageDown}')
    expect(screen.getByText('2026年11月')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '2026年11月12日' })).toHaveFocus()
    await user.keyboard('{PageUp}')
    expect(screen.getByText('2026年10月')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '2026年10月12日' })).toHaveFocus()
  })

  it('returns focus to the input when Tab or Shift+Tab leaves the focused day', async () => {
    const user = userEvent.setup()
    render(<DateHarness initial="2026-10-15" />)
    const input = screen.getByLabelText('开始日期')

    await user.click(screen.getByRole('button', { name: '打开日历' }))
    expect(screen.getByRole('button', { name: '2026年10月15日' })).toHaveFocus()
    await user.tab()
    expect(screen.queryByRole('dialog', { name: '选择日期' })).not.toBeInTheDocument()
    expect(input).toHaveFocus()

    await user.click(screen.getByRole('button', { name: '打开日历' }))
    expect(screen.getByRole('button', { name: '2026年10月15日' })).toHaveFocus()
    await user.tab({ shift: true })
    expect(screen.queryByRole('dialog', { name: '选择日期' })).not.toBeInTheDocument()
    expect(input).toHaveFocus()
  })

  it('closes and validates when focus leaves the field while the calendar is open', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(
      <>
        <DateHarness onChange={onChange} />
        <button type="button">下一步</button>
      </>,
    )
    const input = screen.getByLabelText('开始日期')
    await user.type(input, '2026-10')
    await user.click(screen.getByRole('button', { name: '打开日历' }))
    expect(screen.getByRole('dialog', { name: '选择日期' })).toBeInTheDocument()

    input.focus()
    await user.tab()
    expect(screen.getByRole('button', { name: '关闭日历' })).toHaveFocus()
    expect(screen.getByRole('dialog', { name: '选择日期' })).toBeInTheDocument()

    await user.tab()
    expect(screen.getByRole('button', { name: '下一步' })).toHaveFocus()
    expect(screen.queryByRole('dialog', { name: '选择日期' })).not.toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent('请输入 YYYY-MM-DD 格式的有效日期。')
    expect(onChange).not.toHaveBeenCalled()
  })

  it('closes the calendar and stops edits when disabled becomes true', async () => {
    function DisabledHarness() {
      const [disabled, setDisabled] = useState(false)
      const [value, setValue] = useState('2026-10-15')
      return (
        <>
          <Field label="开始日期" htmlFor="from">
            <DatePicker id="from" value={value} onChange={setValue} disabled={disabled} />
          </Field>
          <button type="button" onClick={() => setDisabled(true)}>禁用日期</button>
        </>
      )
    }
    const user = userEvent.setup()
    render(<DisabledHarness />)
    await user.click(screen.getByRole('button', { name: '打开日历' }))
    expect(screen.getByRole('dialog', { name: '选择日期' })).toBeInTheDocument()

    // fireEvent.click does not emit mousedown, so only the disabled effect can
    // close the portal here.
    fireEvent.click(screen.getByRole('button', { name: '禁用日期' }))
    expect(screen.queryByRole('dialog', { name: '选择日期' })).not.toBeInTheDocument()
    expect(screen.getByLabelText('开始日期')).toBeDisabled()
  })

  it('closes with Escape or a click outside and supports clearing', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(<DateHarness initial="2026-10-15" onChange={onChange} />)
    const input = screen.getByLabelText('开始日期')
    expect(committedDate()).toHaveTextContent('2026-10-15')

    await user.click(screen.getByRole('button', { name: '打开日历' }))
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('dialog', { name: '选择日期' })).not.toBeInTheDocument()
    expect(input).toHaveFocus()

    await user.click(screen.getByRole('button', { name: '打开日历' }))
    await user.click(document.body)
    expect(screen.queryByRole('dialog', { name: '选择日期' })).not.toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '打开日历' }))
    await user.click(screen.getByRole('button', { name: '清除' }))
    expect(input).toHaveValue('')
    expect(committedDate()).toBeEmptyDOMElement()
    expect(onChange).toHaveBeenLastCalledWith('')
    expect(screen.queryByRole('dialog', { name: '选择日期' })).not.toBeInTheDocument()
  })
})

function NumberHarness() {
  const [value, setValue] = useState('10')
  return (
    <Field label="最大 tokens" htmlFor="max-tokens">
      <NumberField
        id="max-tokens"
        min={1}
        max={100}
        step={5}
        value={value}
        onChange={(event) => setValue(event.target.value)}
      />
    </Field>
  )
}

describe('NumberField', () => {
  it('keeps spinbutton semantics and passes min, max and step', async () => {
    const user = userEvent.setup()
    render(<NumberHarness />)
    const input = screen.getByRole('spinbutton', { name: '最大 tokens' })
    expect(input).toHaveAttribute('type', 'number')
    expect(input).toHaveAttribute('min', '1')
    expect(input).toHaveAttribute('max', '100')
    expect(input).toHaveAttribute('step', '5')

    await user.clear(input)
    await user.type(input, '25')
    expect(input).toHaveValue(25)
  })

  it('hides the native spinner controls in CSS', () => {
    const css = readFileSync(resolve(process.cwd(), 'src/styles/ui.module.css'), 'utf8')
    expect(css).toMatch(/\.numberField::-webkit-(?:outer|inner)-spin-button/)
    expect(css).toMatch(/\.numberField\s*\{[^}]*appearance:\s*textfield/)
  })
})

function SearchHarness() {
  const [value, setValue] = useState('alice')
  return (
    <Field label="搜索用户" htmlFor="user-search">
      <SearchField id="user-search" value={value} onChange={setValue} />
    </Field>
  )
}

describe('SearchField', () => {
  it('keeps searchbox semantics and clears through its own button', async () => {
    const user = userEvent.setup()
    render(<SearchHarness />)
    const input = screen.getByRole('searchbox', { name: '搜索用户' })
    expect(input).toHaveValue('alice')

    await user.click(screen.getByRole('button', { name: '清除搜索' }))
    expect(input).toHaveValue('')
    expect(screen.queryByRole('button', { name: '清除搜索' })).not.toBeInTheDocument()

    await user.type(input, 'bob')
    expect(input).toHaveValue('bob')
  })

  it('disables the clear button together with the field', () => {
    render(
      <Field label="搜索用户" htmlFor="user-search">
        <SearchField id="user-search" value="alice" onChange={() => {}} disabled />
      </Field>,
    )
    expect(screen.getByRole('searchbox', { name: '搜索用户' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '清除搜索' })).toBeDisabled()
  })

  it('removes the WebKit cancel button in CSS', () => {
    const css = readFileSync(resolve(process.cwd(), 'src/styles/ui.module.css'), 'utf8')
    expect(css).toMatch(/\.searchInput::-webkit-search-cancel-button/)
  })
})

describe('Textarea', () => {
  it('shares the textarea base and adds the code variant', async () => {
    const user = userEvent.setup()
    render(
      <>
        <Field label="说明" htmlFor="notes"><Textarea id="notes" /></Field>
        <Field label="代码" htmlFor="code"><Textarea id="code" code /></Field>
      </>,
    )
    const hasClass = (element: Element, value: string) => value.split(/\s+/).every((name) => element.classList.contains(name))
    const plain = screen.getByLabelText('说明')
    const code = screen.getByLabelText('代码')
    expect(hasClass(plain, ui.textarea)).toBe(true)
    expect(hasClass(plain, ui.textareaCode)).toBe(false)
    expect(hasClass(code, ui.textarea)).toBe(true)
    expect(hasClass(code, ui.textareaCode)).toBe(true)

    await user.type(plain, '普通文本')
    expect(plain).toHaveValue('普通文本')
  })
})

describe('Tooltip', () => {
  it('shows on hover and focus, describes the trigger and closes with Escape', async () => {
    const user = userEvent.setup()
    render(
      <Tooltip content="按 Esc 关闭">
        <button type="button">查看说明</button>
      </Tooltip>,
    )
    const trigger = screen.getByRole('button', { name: '查看说明' })
    expect(trigger).not.toHaveAttribute('title')
    const describedBy = trigger.getAttribute('aria-describedby')
    expect(describedBy).toBeTruthy()

    await user.hover(trigger)
    const tooltip = screen.getByRole('tooltip')
    expect(tooltip).toHaveTextContent('按 Esc 关闭')
    expect(tooltip).toHaveAttribute('id', describedBy)

    await user.unhover(trigger)
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument()

    await user.tab()
    expect(trigger).toHaveFocus()
    expect(screen.getByRole('tooltip')).toBeInTheDocument()

    await user.keyboard('{Escape}')
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument()
    expect(trigger).toHaveFocus()
  })

  it('stays visible when hover ends but focus remains', async () => {
    const user = userEvent.setup()
    render(
      <Tooltip content="按 Esc 关闭">
        <button type="button">查看说明</button>
      </Tooltip>,
    )
    const trigger = screen.getByRole('button', { name: '查看说明' })
    await user.tab()
    expect(trigger).toHaveFocus()
    expect(screen.getByRole('tooltip')).toBeInTheDocument()

    await user.hover(trigger)
    await user.unhover(trigger)
    expect(screen.getByRole('tooltip')).toBeInTheDocument()

    await user.keyboard('{Escape}')
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument()
    await user.hover(trigger)
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument()
  })

  it('sizes the tooltip independently of the applied left offset', () => {
    const css = readFileSync(resolve(process.cwd(), 'src/styles/ui.module.css'), 'utf8')
    const rule = css.match(/\.tooltip\s*\{[^}]*\}/)?.[0] ?? ''
    // width:auto would shrink-to-fit against the space left of `left`, so the
    // pre-position measurement would not match the final wrapped height.
    expect(rule).toMatch(/width:\s*max-content/)
    expect(rule).toMatch(/max-width:\s*min\(280px,\s*calc\(100vw - 16px\)\)/)
  })
})

describe('computeTooltipPosition', () => {
  it('keeps a 200px tooltip inside a 390px viewport edge', () => {
    const position = computeTooltipPosition(
      { left: 370, top: 100, width: 16, height: 16 },
      { width: 200, height: 40 },
      { width: 390, height: 800 },
    )
    expect(position.left).toBe(282)
    expect(position.left - 100).toBeGreaterThanOrEqual(8)
    expect(position.left + 100).toBeLessThanOrEqual(390 - 8)
  })

  it('centers a long tooltip that fills the 390px viewport', () => {
    const position = computeTooltipPosition(
      { left: 0, top: 100, width: 20, height: 20 },
      { width: 374, height: 60 },
      { width: 390, height: 800 },
    )
    expect(position.left).toBe(195)
    expect(position.left - 187).toBe(8)
    expect(position.left + 187).toBe(390 - 8)
  })

  it('flips below when there is no room above and above near the bottom', () => {
    const below = computeTooltipPosition(
      { left: 100, top: 10, width: 20, height: 20 },
      { width: 100, height: 40 },
      { width: 390, height: 800 },
    )
    expect(below).toMatchObject({ placement: 'below', top: 38 })

    const above = computeTooltipPosition(
      { left: 100, top: 740, width: 20, height: 20 },
      { width: 100, height: 40 },
      { width: 390, height: 800 },
    )
    expect(above).toMatchObject({ placement: 'above', top: 732 })
  })

  it('clamps vertically when neither side fits', () => {
    const position = computeTooltipPosition(
      { left: 100, top: 45, width: 20, height: 20 },
      { width: 100, height: 60 },
      { width: 390, height: 100 },
    )
    expect(position).toMatchObject({ placement: 'below', top: 32 })
  })
})

function clickSummary(summary: HTMLElement) {
  // jsdom toggles details on click but does not emit the toggle event a
  // browser emits after the default action, so send it explicitly.
  fireEvent.click(summary)
  fireEvent(summary.closest('details')!, new Event('toggle'))
}

describe('Disclosure', () => {
  it('keeps details/summary semantics with a custom chevron and aria-expanded', () => {
    render(
      <Disclosure summary="高级设置">
        <p>隐藏内容</p>
      </Disclosure>,
    )
    const summary = screen.getByText('高级设置').closest('summary')!
    const details = summary.closest('details')!
    expect(summary.tagName).toBe('SUMMARY')
    expect(details).not.toHaveAttribute('open')
    expect(summary).toHaveAttribute('aria-expanded', 'false')
    expect(summary.querySelector('svg')).not.toBeNull()

    clickSummary(summary)
    expect(details).toHaveAttribute('open')
    expect(summary).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByText('隐藏内容')).toBeVisible()
  })

  it('syncs from the native toggle event without depending on microtask order', () => {
    const onToggle = vi.fn()
    render(
      <Disclosure summary="高级设置" onToggle={onToggle}>
        <p>隐藏内容</p>
      </Disclosure>,
    )
    const summary = screen.getByText('高级设置').closest('summary')!
    const details = summary.closest('details')!

    details.open = true
    fireEvent(details, new Event('toggle'))
    expect(summary).toHaveAttribute('aria-expanded', 'true')
    expect(onToggle).toHaveBeenCalledTimes(1)
    expect(onToggle).toHaveBeenCalledWith(true)

    fireEvent(details, new Event('toggle'))
    expect(onToggle).toHaveBeenCalledTimes(1)
  })

  it('opens initially with defaultOpen and reports toggles in controlled mode', () => {
    function Controlled() {
      const [open, setOpen] = useState(false)
      return (
        <Disclosure summary="高级设置" open={open} onToggle={setOpen}>
          <p>隐藏内容</p>
        </Disclosure>
      )
    }
    render(
      <>
        <Disclosure summary="默认展开" defaultOpen><p>初始内容</p></Disclosure>
        <Controlled />
      </>,
    )
    expect(screen.getByText('默认展开').closest('details')).toHaveAttribute('open')
    const summary = screen.getByText('高级设置').closest('summary')!
    const details = summary.closest('details')!
    clickSummary(summary)
    expect(details).toHaveAttribute('open')
    expect(summary).toHaveAttribute('aria-expanded', 'true')
    clickSummary(summary)
    expect(details).not.toHaveAttribute('open')
    expect(summary).toHaveAttribute('aria-expanded', 'false')
  })

  it('keeps a rejected controlled toggle closed', () => {
    const onToggle = vi.fn()
    render(
      <Disclosure summary="高级设置" open={false} onToggle={onToggle}>
        <p>隐藏内容</p>
      </Disclosure>,
    )
    const summary = screen.getByText('高级设置').closest('summary')!
    const details = summary.closest('details')!
    clickSummary(summary)
    expect(onToggle).toHaveBeenCalledWith(true)
    expect(details).not.toHaveAttribute('open')
    expect(summary).toHaveAttribute('aria-expanded', 'false')
  })

  it('toggles with Enter and Space', async () => {
    const user = userEvent.setup()
    render(
      <Disclosure summary="高级设置">
        <p>隐藏内容</p>
      </Disclosure>,
    )
    const summary = screen.getByText('高级设置').closest('summary')!
    const details = summary.closest('details')!
    summary.focus()

    await user.keyboard('{Enter}')
    expect(details).toHaveAttribute('open')
    expect(summary).toHaveAttribute('aria-expanded', 'true')

    await user.keyboard(' ')
    expect(details).not.toHaveAttribute('open')
    expect(summary).toHaveAttribute('aria-expanded', 'false')
  })

  it('hides the native marker in CSS', () => {
    const css = readFileSync(resolve(process.cwd(), 'src/styles/ui.module.css'), 'utf8')
    expect(css).toMatch(/\.disclosureSummary::-webkit-details-marker/)
    expect(css).toMatch(/\.disclosureSummary::marker/)
  })
})
