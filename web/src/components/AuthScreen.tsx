import { authLoginUrl } from '../api/api'
import { BrandMark } from './Icons'
import { Button, ErrorState } from './ui'
import styles from './AuthScreen.module.css'

export function AuthScreen({
  notice, error, onRetry,
}: { notice?: string; error?: unknown; onRetry?: () => void }) {
  return (
    <main className={styles.wrap}>
      <div style={{ width: 'min(440px, 100%)' }}>
        <div className={styles.brandBar}>
          <BrandMark size={30} />
          <div className={styles.brandText}>
            <span className={styles.brandName}>Tribunal</span>
            <span className={styles.brandSub}>GitLab 合并请求评审平台</span>
          </div>
        </div>
        <section className={styles.card} aria-labelledby="login-title">
          <h1 className={styles.title} id="login-title">登录评审平台</h1>
          <p className={styles.text}>
            使用 GitLab 账号登录。管理员可注册项目和维护配置，只读用户只能查看评审证据。
          </p>
          {notice ? <p className={styles.notice} role="status">{notice}</p> : null}
          {error ? (
            <>
              <ErrorState error={error} onRetry={onRetry} title="无法检查登录状态" />
            </>
          ) : null}
          <a href={authLoginUrl} style={loginButtonStyle}>
            通过 GitLab 登录
          </a>
          <span className={styles.footer}>需要 GitLab OAuth 会话。平台不会在此页面保存任何访问令牌。</span>
        </section>
      </div>
    </main>
  )
}

const loginButtonStyle = {
  display: 'inline-flex',
  alignItems: 'center',
  justifyContent: 'center',
  minHeight: 44,
  padding: '0 16px',
  borderRadius: 8,
  background: 'var(--accent)',
  color: 'var(--on-accent)',
  fontWeight: 500,
  textDecoration: 'none',
} as const

export function BootLoading() {
  return (
    <div className={styles.loadingWrap}>
      <span role="status">正在加载控制台…</span>
    </div>
  )
}
