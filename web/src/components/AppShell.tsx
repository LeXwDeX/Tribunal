import { useEffect, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link, NavLink, Outlet, useLocation, useMatch } from 'react-router-dom'
import { getProject, getReview, listReviews, logout } from '../api/api'
import type { Me, ReviewSummary } from '../api/types'
import { usePaginated } from '../lib/hooks'
import { BrandMark, Icon } from './Icons'
import { classNames, useToast } from './ui'
import { SessionProvider } from './RoleContext'
import styles from './AppShell.module.css'

type NavItem = {
  to: string
  label: string
  icon: 'overview' | 'reviews' | 'projects' | 'extensions' | 'models' | 'stats' | 'system'
  end?: boolean
  runningCount?: boolean
}

const navGroups: { label: string; items: NavItem[] }[] = [
  {
    label: '工作台',
    items: [
      { to: '/', label: '总览', icon: 'overview', end: true },
      { to: '/reviews', label: '评审', icon: 'reviews', runningCount: true },
    ],
  },
  {
    label: '配置',
    items: [
      { to: '/projects', label: '项目', icon: 'projects' },
      { to: '/extensions', label: '扩展', icon: 'extensions' },
      { to: '/models', label: '模型与档位', icon: 'models' },
    ],
  },
  {
    label: '洞察',
    items: [
      { to: '/statistics', label: '统计与反馈', icon: 'stats' },
      { to: '/system', label: '系统', icon: 'system' },
    ],
  },
]

const sectionLabels: Record<string, string> = {
  '': '总览',
  reviews: '评审',
  projects: '项目',
  extensions: '扩展',
  models: '模型与档位',
  statistics: '统计与反馈',
  system: '系统',
}

// Shares the overview page's query key, fetcher and interval so both read one cache entry.
function useRunningCount() {
  const running = usePaginated<ReviewSummary>(
    ['reviews', 'overview', 'running'],
    (cursor) => listReviews({ status: 'running' }, cursor),
    'reviews',
    { refetchInterval: 10_000 },
  )
  if (running.items.length === 0) return null
  return `${running.items.length}${running.hasNextPage ? '+' : ''}`
}

// Detail crumbs only observe the detail pages' cached queries; they never fetch on their own.
function Breadcrumbs() {
  const location = useLocation()
  const reviewMatch = useMatch('/reviews/:id')
  const projectMatch = useMatch('/projects/:id/*')
  const reviewId = reviewMatch?.params.id || ''
  const projectId = Number(projectMatch?.params.id)
  const review = useQuery({
    queryKey: ['review', reviewId, 'detail'],
    queryFn: () => getReview(reviewId),
    enabled: false,
  })
  const project = useQuery({
    queryKey: ['project', projectId],
    queryFn: () => getProject(projectId),
    enabled: false,
  })

  const section = location.pathname.split('/')[1] || ''
  const sectionLabel = sectionLabels[section]
  let parent: { to: string; label: string } | null = null
  let current = sectionLabel || '页面不存在'
  if (reviewMatch) {
    parent = { to: '/reviews', label: '评审' }
    const data = review.data
    current = data ? `${data.project_path || `项目 #${data.project_id}`} !${data.mr_iid}` : reviewId
  } else if (projectMatch && projectMatch.params.id) {
    parent = { to: '/projects', label: '项目' }
    current = project.data?.path_with_namespace || `#${projectMatch.params.id}`
  }

  return (
    <nav aria-label="面包屑" className={styles.crumbs}>
      {parent ? (
        <>
          <Link to={parent.to}>{parent.label}</Link>
          <span className={styles.sep} aria-hidden="true">/</span>
        </>
      ) : null}
      <b aria-current="page">{current}</b>
    </nav>
  )
}

const roleLabel = (role: string) => (role === 'admin' ? '管理员' : role === 'viewer' ? '只读用户' : role)

function ThemeToggle() {
  const [theme, setTheme] = useState(() => document.documentElement.dataset.theme === 'dark' ? 'dark' : 'light')
  const toggle = () => {
    const next = theme === 'dark' ? 'light' : 'dark'
    setTheme(next)
    document.documentElement.dataset.theme = next
    try { localStorage.setItem('tribunal-theme', next) } catch { /* storage may be unavailable */ }
  }
  return (
    <button
      type="button"
      className={styles.navAction}
      onClick={toggle}
      aria-label={theme === 'dark' ? '切换为亮色主题' : '切换为暗色主题'}
    >
      <Icon name={theme === 'dark' ? 'sun' : 'moon'} size={14} />
      {theme === 'dark' ? '亮色主题' : '暗色主题'}
    </button>
  )
}

export function AppShell({ me }: { me: Me }) {
  const [menuOpen, setMenuOpen] = useState(false)
  const [isMobile, setIsMobile] = useState(() => typeof window !== 'undefined' && Boolean(window.matchMedia?.('(max-width: 900px)').matches))
  const [loggingOut, setLoggingOut] = useState(false)
  const toast = useToast()
  const location = useLocation()
  const menuButtonRef = useRef<HTMLButtonElement>(null)
  const runningCount = useRunningCount()

  useEffect(() => {
    setMenuOpen(false)
  }, [location.pathname])

  useEffect(() => {
    if (typeof window.matchMedia !== 'function') return
    const media = window.matchMedia('(max-width: 900px)')
    const onChange = (event: MediaQueryListEvent) => setIsMobile(event.matches)
    media.addEventListener?.('change', onChange)
    return () => media.removeEventListener?.('change', onChange)
  }, [])

  useEffect(() => {
    if (!menuOpen) return
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        setMenuOpen(false)
        menuButtonRef.current?.focus()
      }
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [menuOpen])

  const onLogout = async () => {
    setLoggingOut(true)
    try {
      await logout()
    } catch {
      toast.push('error', '退出失败，请重试。')
    } finally {
      setLoggingOut(false)
    }
  }

  return (
    <SessionProvider me={me}>
      <div className={styles.layout}>
        <button
          type="button"
          className={classNames(styles.overlay, menuOpen && styles.overlayVisible)}
          aria-label="关闭导航菜单"
          onClick={() => setMenuOpen(false)}
          tabIndex={-1}
          aria-hidden="true"
        />
        <nav
          id="primary-navigation"
          aria-label="主导航"
          className={classNames(styles.nav, menuOpen && styles.navOpen)}
          data-open={menuOpen || undefined}
          inert={isMobile && !menuOpen}
          aria-hidden={isMobile && !menuOpen ? true : undefined}
        >
          <NavLink to="/" className={styles.brand} end>
            <BrandMark />
            <span className={styles.brandText}>
              <span className={styles.brandName}>Tribunal</span>
              <span className={styles.brandSub}>代码评审控制台</span>
            </span>
          </NavLink>
          <div className={styles.navItems}>
            {navGroups.map((group) => (
              <div key={group.label} role="group" aria-label={group.label} className={styles.navSection}>
                <div className={styles.navGroup} aria-hidden="true">{group.label}</div>
                {group.items.map((item) => (
                  <NavLink
                    key={item.to}
                    to={item.to}
                    end={item.end}
                    className={({ isActive }) => classNames(styles.navItem, isActive && styles.navItemActive)}
                    onClick={() => setMenuOpen(false)}
                  >
                    <Icon name={item.icon} size={17} />
                    {item.label}
                    {item.runningCount && runningCount ? (
                      <>
                        <span className={styles.navCount} aria-hidden="true">{runningCount}</span>
                        <span className="srOnly">{runningCount} 个运行中</span>
                      </>
                    ) : null}
                  </NavLink>
                ))}
              </div>
            ))}
          </div>
          <div className={styles.navSpacer} />
          <div className={styles.sessionArea}>
            <div className={styles.sessionRow}>
              <span className={styles.avatar} aria-hidden="true">
                {(me.name || me.username || '?').slice(0, 1).toUpperCase()}
              </span>
              <div className={styles.sessionText}>
                <span className={styles.sessionName}>{me.name || me.username}</span>
                <span className={styles.sessionRole}>{roleLabel(me.role)} · @{me.username}</span>
              </div>
            </div>
            <div className={styles.navActions}>
              <ThemeToggle />
              <button type="button" className={styles.navAction} onClick={() => { void onLogout() }} disabled={loggingOut}>
                <Icon name="logout" size={14} />
                {loggingOut ? '退出中…' : '退出登录'}
              </button>
            </div>
          </div>
        </nav>

        <div className={styles.main}>
          <div className={styles.mobileBar}>
            <button
              ref={menuButtonRef}
              type="button"
              className={styles.menuButton}
              aria-expanded={menuOpen}
              aria-controls="primary-navigation"
              aria-label={menuOpen ? '关闭导航菜单' : '打开导航菜单'}
              onClick={() => setMenuOpen((open) => !open)}
            >
              <Icon name={menuOpen ? 'close' : 'menu'} />
            </button>
            <NavLink to="/" className={styles.mobileBrand} end>
              <BrandMark size={24} />
              Tribunal
            </NavLink>
          </div>
          <div className={styles.topbar}>
            <Breadcrumbs />
          </div>
          <Outlet />
        </div>
      </div>
    </SessionProvider>
  )
}

