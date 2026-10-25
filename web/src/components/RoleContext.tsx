import { createContext, useContext, type ReactNode } from 'react'
import type { Me } from '../api/types'

interface Session {
  me: Me
  // Platform-level administration (settings, providers, project registration).
  canWrite: boolean
  // The logged-in browser session may manage its own credentials.
  canSelfService: boolean
}

const SessionContext = createContext<Session | null>(null)

export function SessionProvider({ me, children }: { me: Me; children: ReactNode }) {
  const canWrite = me.role === 'admin' && Boolean(me.csrf_token)
  const canSelfService = Boolean(me.csrf_token)
  return <SessionContext.Provider value={{ me, canWrite, canSelfService }}>{children}</SessionContext.Provider>
}

export function useSession(): Session {
  const session = useContext(SessionContext)
  if (!session) throw new Error('useSession must be used inside SessionProvider')
  return session
}

export function useCanWrite(): boolean {
  return useSession().canWrite
}

export function useCanSelfService(): boolean {
  return useSession().canSelfService
}
