// throwaway mock of the Go daemon for local UI smoke-testing: node mock-api.mjs (port 4517)
import { createServer } from 'node:http'

const suggestions = [
  {
    id: 'sg_1',
    createdAt: '2026-06-10T18:00:00Z',
    status: 'pending',
    title: 'Run migrations with the wrapper script, not alembic directly',
    signal: 'correction',
    scope: 'repo',
    placement: 'always_on',
    sensitivity: false,
    confidence: 0.86,
    project: 'orbit',
    repoRoot: '/Users/alex/Documents/Code/orbit',
    targetPath: 'AGENTS.md',
    body: '## migrations\n\nAlways run `scripts/migrate.sh` instead of calling alembic directly; it pins the env and runs the post-migration smoke check.',
    rationale:
      'In three separate sessions the agent invoked alembic directly and was corrected each time; the wrapper script exists precisely to avoid the broken env it produced.',
    evidence: [
      { excerpt: '$ alembic upgrade head\nERROR: target database is not up to date', sessionId: 's_91', tool: 'cursor' },
      { excerpt: 'user: no — use scripts/migrate.sh, it sets DATABASE_URL for you', sessionId: 's_91', tool: 'claude' },
    ],
  },
  {
    id: 'sg_2',
    createdAt: '2026-06-10T17:00:00Z',
    status: 'pending',
    title: 'Internal API tokens live in 1Password, never in .env',
    signal: 'convention',
    scope: 'machine',
    placement: 'skill',
    sensitivity: true,
    confidence: 0.62,
    project: 'temper',
    repoRoot: '/Users/hoklims/Code/temper',
    targetPath: '.cursor/skills/secrets/SKILL.md',
    body: '## secrets\n\nFetch internal API tokens via `op read` at runtime; never write them to .env files.',
    rationale:
      'The user rewrote agent-authored .env additions twice, replacing literal tokens with op references; this looks like a standing machine-wide convention.',
    evidence: [{ excerpt: 'user: pull that from 1password, do not hardcode it', sessionId: 's_77', tool: 'cursor' }],
  },
]

const routes = {
  '/api/stats': () => ({ pending: 2, accepted: 14, rejected: 5, sessions: 41, projects: 3 }),
  '/api/projects': () => ({
    projects: [
      { name: 'orbit', repoRoot: '/Users/alex/Documents/Code/orbit', pending: 1 },
      { name: 'temper', repoRoot: '/Users/hoklims/Code/temper', pending: 1 },
      { name: 'trace', repoRoot: '/Users/alex/Desktop/Trace/components', pending: 0 },
    ],
  }),
}

createServer((req, res) => {
  const url = new URL(req.url, 'http://localhost')
  res.setHeader('Content-Type', 'application/json')
  if (req.method === 'POST' && /^\/api\/suggestions\/[^/]+\/decision$/.test(url.pathname)) {
    res.end(JSON.stringify({ ok: true, writtenPath: '/Users/alex/Documents/Code/orbit/AGENTS.md' }))
    return
  }
  if (url.pathname === '/api/suggestions') {
    const status = url.searchParams.get('status') ?? 'pending'
    res.end(JSON.stringify({ suggestions: status === 'pending' || status === 'all' ? suggestions : [] }))
    return
  }
  const handler = routes[url.pathname]
  if (handler) {
    res.end(JSON.stringify(handler()))
    return
  }
  res.statusCode = 404
  res.end('{}')
}).listen(4517, '127.0.0.1', () => console.log('mock api on 4517'))
