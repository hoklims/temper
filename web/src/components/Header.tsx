import type { Stats } from '../lib/api'
import { StatBar } from './StatBar'
import './Header.scss'

interface HeaderProps {
  stats: Stats | null
}

export function Header({ stats }: HeaderProps) {
  return (
    <header className="header">
      <div className="header__inner">
        <div className="header__wordmark">
          {/* terminal/diff glyph, same thin-stroke schematic language as Trace card icons */}
          <svg className="header__glyph" width="18" height="18" viewBox="0 0 18 18" fill="none" aria-hidden="true">
            <rect x="1" y="2.5" width="16" height="13" rx="2.5" stroke="var(--color-dark)" strokeWidth="1" />
            <path d="M4.5 7l2.5 2-2.5 2" stroke="var(--color-dark)" strokeWidth="1.2" strokeLinecap="round" strokeLinejoin="round" />
            <path d="M9.5 11.5h4" stroke="var(--color-dark)" strokeWidth="1.2" strokeLinecap="round" />
          </svg>
          <span className="header__logo-label">temper</span>
        </div>
        {stats && <StatBar stats={stats} />}
      </div>
    </header>
  )
}
