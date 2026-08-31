import type { Project } from '../lib/api'
import { EmptyState } from './EmptyState'
import './ProjectsView.scss'

interface ProjectsViewProps {
  projects: Project[]
}

export function ProjectsView({ projects }: ProjectsViewProps) {
  if (projects.length === 0) {
    return <EmptyState hint="projects" message="no projects yet — run temper scan." />
  }

  return (
    <div className="projects-view">
      <div className="projects-view__row projects-view__row--head" aria-hidden="true">
        <span className="projects-view__label">project</span>
        <span className="projects-view__label">repo root</span>
        <span className="projects-view__label projects-view__label--right">pending</span>
      </div>
      <ul className="projects-view__list">
        {projects.map((project) => (
          <li key={project.repoRoot} className="projects-view__row">
            <span className="projects-view__name">{project.name}</span>
            <code className="projects-view__path">{project.repoRoot}</code>
            <span className="projects-view__count">{project.pending}</span>
          </li>
        ))}
      </ul>
    </div>
  )
}
