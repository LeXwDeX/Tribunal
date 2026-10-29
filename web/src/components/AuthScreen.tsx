import { authLoginUrl } from '../api/api'
import { BrandMark } from './Icons'
import { Button, ButtonLink, ErrorState } from './ui'
import styles from './AuthScreen.module.css'

const authErrorMessages: Record<string, string> = {
  oauth_disabled: '平台未启用 GitLab OAuth 登录，请联系管理员。',
  oauth_denied: 'GitLab 登录被取消或拒绝。',
  oauth_state: '登录状态已失效，请重试。',
  oauth_forbidden: '该 GitLab 账号未被允许登录。',
  oauth_failed: 'GitLab 登录失败，请重试。',
}

export function authErrorMessage(search: string): string | undefined {
  const code = new URLSearchParams(search).get('auth_error')
  return code ? authErrorMessages[code] ?? '登录未完成，请重试。' : undefined
}

export function AuthScreen({
  notice, error, onRetry, oauthEnabled = true,
}: { notice?: string; error?: unknown; onRetry?: () => void; oauthEnabled?: boolean }) {
  return (
    <main className={styles.auth}>
      <div className={styles.authArt}>
        <div className={styles.artBrand}>
          <BrandMark size={30} />
          <span className={styles.brandName}>Tribunal</span>
        </div>
        <div className={styles.artBody}>
          <p className={styles.tagline}>每一条评审意见，都经过提出、挑战与裁决。</p>
          <ol className={styles.authFlow}>
            <li><b>01</b>独立 reviewer 提出问题</li>
            <li><b>02</b>challenger 尝试推翻</li>
            <li><b>03</b>确定性规则约束最终发布</li>
          </ol>
        </div>
        <span className={styles.artFoot}>面向 GitLab CE 的多模型代码评审</span>
      </div>
      <div className={styles.authPanel}>
        <section className={styles.authCard} aria-labelledby="login-title">
          <div className={styles.heading}>
            <span className={styles.cardMark}><BrandMark size={30} /></span>
            <h1 className={styles.title} id="login-title">登录评审平台</h1>
            <p className={styles.text}>
              使用 GitLab 账号登录。管理员可注册项目和维护配置，只读用户只能查看评审证据。
            </p>
          </div>
          {notice ? <p className={styles.notice} role="status">{notice}</p> : null}
          {error ? <ErrorState error={error} onRetry={onRetry} title="无法检查登录状态" /> : null}
          {oauthEnabled ? (
            <>
              <ButtonLink href={authLoginUrl} variant="primary" className={styles.loginButton}>
                通过 GitLab 登录
              </ButtonLink>
              <span className={styles.footer}>需要 GitLab OAuth 会话。平台不会在此页面保存任何访问令牌。</span>
            </>
          ) : (
            <p className={styles.notice} role="status">
              平台尚未启用 GitLab OAuth 登录。请联系管理员配置 <code>auth.oauth.client_id_env</code> 和{' '}
              <code>auth.oauth.client_secret_env</code>，然后刷新此页面。
            </p>
          )}
        </section>
      </div>
    </main>
  )
}


export function BootLoading() {
  return (
    <div className={styles.loadingWrap}>
      <span role="status">正在加载控制台…</span>
    </div>
  )
}
