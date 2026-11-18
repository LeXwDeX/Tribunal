import { Component, type ReactNode } from 'react'
import { Button, ButtonLink, PageHeader, Panel } from './ui'
import ui from '../styles/ui.module.css'
import styles from './ErrorBoundary.module.css'

type ErrorBoundaryProps = {
  children: ReactNode
  // Navigation replaces this key; a stale error screen must not survive the
  // route change that the user made to recover.
  resetKey?: string
  scope?: 'page' | 'app'
}

type ErrorBoundaryState = { failed: boolean }

// The fallback never renders the caught error: its message and stack can hold
// internal paths. React still reports the error to the developer console.
function ErrorFallback({ scope }: { scope: 'page' | 'app' }) {
  const app = scope === 'app'
  return (
    <main className={ui.page}>
      <div className={ui.pageStack}>
        <PageHeader
          title={app ? '控制台遇到错误' : '页面遇到错误'}
          description={app
            ? '这个页面在渲染时发生异常。重新加载后可以继续使用控制台。'
            : '这个页面在渲染时发生异常，导航和其他页面仍然可用。'}
        />
        <Panel title="页面无法显示" note="错误详情不会显示在这里，以免暴露堆栈或内部路径。">
          <div className={styles.body} role="alert">
            <p className={styles.message}>你可以重新加载当前页面，或返回总览继续使用其他功能。</p>
            <div className={styles.actions}>
              <ButtonLink to="/" variant="primary">返回总览</ButtonLink>
              <Button variant="secondary" onClick={() => window.location.reload()}>重新加载</Button>
            </div>
          </div>
        </Panel>
      </div>
    </main>
  )
}

export class ErrorBoundary extends Component<ErrorBoundaryProps, ErrorBoundaryState> {
  state: ErrorBoundaryState = { failed: false }

  static getDerivedStateFromError(): ErrorBoundaryState {
    return { failed: true }
  }

  componentDidUpdate(prevProps: ErrorBoundaryProps) {
    if (this.state.failed && prevProps.resetKey !== this.props.resetKey) {
      this.setState({ failed: false })
    }
  }

  render() {
    if (this.state.failed) return <ErrorFallback scope={this.props.scope ?? 'page'} />
    return this.props.children
  }
}