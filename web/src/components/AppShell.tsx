import { useEffect, useRef, useState } from 'react'
import { NavLink, Outlet, useLocation } from 'react-router-dom'
import { logout } from '../api/api'
import type { Me } from '../api/types'
import { BrandMark, Icon } from './Icons'
import { classNames, useToast } from './ui'
import { SessionProvider } from './RoleContext'
import styles from './AppShell.module.css'

const navItems = [
  { to: '/', label: '总览', icon: 'overview' as const, end: true },
  { to: '/reviews', label: '评审', icon: 'reviews' as const },
  { to: '/projects', label: '项目', icon: 'projects' as const },
  { to: '/extensions', label: '扩展', icon: 'extensions' as const },
  { to: '/models', label: '模型与档位', icon: 'models' as const },
  { to: '/statistics', label: '统计与反馈', icon: 'stats' as const },
  { to: '/system', label: '系统', icon: 'system' as const },
]

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
      <Icon name={theme === 'dark' ? 'sun' : 'moon'} size={16} />
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
              <span className={styles.brandSub}>评审平台</span>
            </span>
          </NavLink>
          <div className={styles.navItems}>
            {navItems.map((item) => (
              <NavLink
                key={item.to}
                to={item.to}
                end={item.end}
                className={({ isActive }) => classNames(styles.navItem, isActive && styles.navItemActive)}
                onClick={() => setMenuOpen(false)}
              >
                <Icon name={item.icon} />
                {item.label}
              </NavLink>
            ))}
          </div>
          <div className={styles.navSpacer} />
          <div className={styles.sessionArea}>
            <div className={styles.sessionRow}>
              <span style={avatarStyle} aria-hidden="true">
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
                <Icon name="logout" size={16} />
                {loggingOut ? '退出中…' : '退出登录'}
              </button>
            </div>
          </div>
        </nav>

        <div className={styles.main}>
          <div className={styles.mobileBar}>
            <NavLink to="/" className={styles.mobileBrand} end>
              <BrandMark size={24} />
              Tribunal
            </NavLink>
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
          </div>
          <Outlet />
        </div>
      </div>
    </SessionProvider>
  )
}

const avatarStyle = {
  width: 32,
  height: 32,
  borderRadius: '50%',
  background: '#343A46',
  color: '#fff',
  display: 'inline-flex',
  alignItems: 'center',
  justifyContent: 'center',
  fontWeight: 700,
  flex: '0 0 auto',
} as const

