import { ButtonLink, ErrorState } from '../components/ui'
import { ApiError } from '../api/client'
import styles from './pages.module.css'

export function NotFoundPage() {
  return (
    <main className={styles.notFound}>
      <ErrorState error={new ApiError(404, 'not_found', '请检查地址是否正确。')} title="页面不存在" />
      <div>
        <ButtonLink to="/">返回总览</ButtonLink>
      </div>
    </main>
  )
}
