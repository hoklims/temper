import { useCallback, useEffect, useRef, useState } from 'react'
import {
  getProjects,
  getStats,
  getSuggestions,
  postDecision,
  type DecisionAction,
  type Project,
  type Stats,
  type Suggestion,
  type SuggestionStatus,
} from './lib/api'
import { Header } from './components/Header'
import { Sidebar, type View } from './components/Sidebar'
import { SuggestionCard } from './components/SuggestionCard'
import { ProjectsView } from './components/ProjectsView'
import { EmptyState } from './components/EmptyState'
import { InboxIntro } from './components/InboxIntro'
import './App.scss'

const VIEW_STATUS: Record<Exclude<View, 'projects'>, SuggestionStatus> = {
  inbox: 'pending',
  accepted: 'accepted',
  rejected: 'rejected',
}

const EMPTY_COPY: Record<Exclude<View, 'projects'>, string> = {
  inbox: 'no suggestions yet — run temper scan.',
  accepted: 'no accepted skills yet.',
  rejected: 'no rejected suggestions yet.',
}

const LEAVE_MS = 250

export default function App() {
  const [view, setView] = useState<View>('inbox')
  const [stats, setStats] = useState<Stats | null>(null)
  const [suggestions, setSuggestions] = useState<Suggestion[]>([])
  const [projects, setProjects] = useState<Project[]>([])
  // loading is derived: data on screen belongs to loadedView; anything else means a fetch is in flight
  const [loadedView, setLoadedView] = useState<View | null>(null)
  const [offline, setOffline] = useState(false)
  const [leaving, setLeaving] = useState<Set<string>>(new Set())
  const [toast, setToast] = useState<string | null>(null)
  const toastTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)

  const refreshStats = useCallback(() => {
    getStats().then(setStats).catch(() => setOffline(true))
  }, [])

  useEffect(() => {
    refreshStats()
  }, [refreshStats])

  useEffect(() => {
    let cancelled = false
    const load =
      view === 'projects'
        ? getProjects().then(({ projects }) => {
            if (!cancelled) setProjects(projects)
          })
        : getSuggestions(VIEW_STATUS[view]).then(({ suggestions }) => {
            if (!cancelled) setSuggestions(suggestions)
          })
    load
      .then(() => {
        if (!cancelled) setOffline(false)
      })
      .catch(() => {
        if (!cancelled) setOffline(true)
      })
      .finally(() => {
        if (!cancelled) setLoadedView(view)
      })
    return () => {
      cancelled = true
    }
  }, [view])

  const loading = loadedView !== view

  const showToast = useCallback((message: string) => {
    clearTimeout(toastTimer.current)
    setToast(message)
    toastTimer.current = setTimeout(() => setToast(null), 4000)
  }, [])

  const handleDecision = useCallback(
    (id: string, action: DecisionAction, body?: string) => {
      // optimistic: fade the card out immediately, then drop it from the list
      setLeaving((prev) => new Set(prev).add(id))
      setTimeout(() => {
        setSuggestions((prev) => prev.filter((s) => s.id !== id))
        setLeaving((prev) => {
          const next = new Set(prev)
          next.delete(id)
          return next
        })
      }, LEAVE_MS)
      setStats((prev) =>
        prev
          ? {
              ...prev,
              pending: Math.max(0, prev.pending - 1),
              accepted: prev.accepted + (action === 'accept' ? 1 : 0),
              rejected: prev.rejected + (action === 'reject' ? 1 : 0),
            }
          : prev,
      )
      postDecision(id, action, body)
        .then((res) => {
          if (action === 'accept' && res.writtenPath) {
            showToast(`written to ${res.writtenPath}`)
          } else if (action === 'reject') {
            showToast('rejected')
          }
          refreshStats()
        })
        .catch(() => {
          showToast('decision failed — is the daemon running?')
          refreshStats()
        })
    },
    [refreshStats, showToast],
  )

  return (
    <div className="app">
      <a href="#main" className="skip-link">
        skip to content
      </a>
      <Header stats={stats} />
      <div className="app__shell">
        <Sidebar view={view} stats={stats} onNavigate={setView} />
        <main id="main" className="app__main">
          {toast && (
            <div className="app__toast" role="status">
              {toast}
            </div>
          )}
          {offline && !loading ? (
            <EmptyState hint="daemon unreachable" message="could not reach the temper daemon — start it and reload." />
          ) : loading ? (
            <p className="app__loading mono-label">loading</p>
          ) : view === 'projects' ? (
            <ProjectsView projects={projects} />
          ) : suggestions.length === 0 ? (
            <EmptyState hint={view} message={EMPTY_COPY[view]} />
          ) : (
            <>
              {view === 'inbox' && <InboxIntro />}
              <div className="app__cards">
                {suggestions.map((suggestion) => (
                  <SuggestionCard
                    key={suggestion.id}
                    suggestion={suggestion}
                    leaving={leaving.has(suggestion.id)}
                    readonly={view !== 'inbox'}
                    onDecision={(action, body) => handleDecision(suggestion.id, action, body)}
                  />
                ))}
              </div>
            </>
          )}
        </main>
      </div>
    </div>
  )
}
