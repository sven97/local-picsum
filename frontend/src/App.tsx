import { useCallback, useEffect, useState, type ButtonHTMLAttributes, type CSSProperties, type ReactNode } from 'react'
import {
  Check,
  ChevronRight,
  Copy,
  ExternalLink,
  Folder,
  Image,
  LoaderCircle,
  RefreshCw,
  X,
} from 'lucide-react'

type LibraryStatus = {
  count: number
  root: string
  refreshInterval: string
}

type TreeNode = {
  name: string
  path: string
  children?: TreeNode[]
  selected: boolean
  disabled: boolean
  count: number
}

type Toast = { message: string; tone?: 'success' | 'error' }

async function request<T>(input: RequestInfo | URL, init?: RequestInit): Promise<T> {
  const response = await fetch(input, init)
  if (response.status === 401) {
    window.location.assign('/login')
    throw new Error('Authentication required')
  }
  if (!response.ok) {
    const message = (await response.text()).trim()
    throw new Error(message || 'Request failed')
  }
  if (response.status === 204 || response.status === 202) return undefined as T
  return response.json() as Promise<T>
}

function Button({ className = '', children, ...props }: ButtonHTMLAttributes<HTMLButtonElement>) {
  return (
    <button className={`button ${className}`} {...props}>
      {children}
    </button>
  )
}

function BrandMark() {
  return (
    <span className="brand-mark" aria-hidden="true">
      <span />
    </span>
  )
}

function Card({ children, className = '' }: { children: ReactNode; className?: string }) {
  return <section className={`card ${className}`}>{children}</section>
}

function Stat({ icon, label, children, className = '' }: { icon: ReactNode; label: string; children: ReactNode; className?: string }) {
  return (
    <div className={`stat ${className}`}>
      <span className="icon-box" aria-hidden="true">{icon}</span>
      <div className="stat-copy">
        <span>{label}</span>
        {children}
      </div>
    </div>
  )
}

function hasSelectedDescendant(node: TreeNode): boolean {
  return Boolean(node.children?.some((child) => child.selected || hasSelectedDescendant(child)))
}

function FolderRow({ node, depth, busy, onChange }: { node: TreeNode; depth: number; busy: boolean; onChange: (node: TreeNode, selected: boolean) => void }) {
  const hasChildren = Boolean(node.children?.length)
  const [expanded, setExpanded] = useState(depth === 0 || hasSelectedDescendant(node))
  const inputId = `folder-${node.path ? encodeURIComponent(node.path) : 'root'}`

  return (
    <li>
      <div className={`folder-row${node.selected ? ' selected' : ''}${node.disabled ? ' disabled' : ''}`} style={{ '--depth': depth } as CSSProperties}>
        <button
          type="button"
          className="tree-toggle"
          aria-label={hasChildren ? `${expanded ? 'Collapse' : 'Expand'} ${node.name}` : undefined}
          aria-expanded={hasChildren ? expanded : undefined}
          disabled={!hasChildren}
          onClick={() => setExpanded((value) => !value)}
        >
          {hasChildren && <ChevronRight size={14} strokeWidth={1.8} />}
        </button>
        <span className="checkbox-wrap">
          <input
            id={inputId}
            type="checkbox"
            checked={node.selected}
            disabled={node.disabled || busy}
            onChange={(event) => onChange(node, event.target.checked)}
          />
          <span className="checkbox-ui" aria-hidden="true"><Check size={12} strokeWidth={2.5} /></span>
        </span>
        <label htmlFor={inputId}>
          <span className="folder-name">{node.name}</span>
          <span className="folder-meta">
            {node.count.toLocaleString()} {node.count === 1 ? 'image' : 'images'}
            {node.disabled && ' · Included via parent'}
          </span>
        </label>
      </div>
      {hasChildren && expanded && (
        <ul>
          {node.children!.map((child) => (
            <FolderRow key={child.path} node={child} depth={depth + 1} busy={busy} onChange={onChange} />
          ))}
        </ul>
      )}
    </li>
  )
}

function Snippet({ label, value, onCopy }: { label: string; value: string; onCopy: (value: string) => void }) {
  return (
    <div className="snippet">
      <div>
        <span>{label}</span>
        <code>{value}</code>
      </div>
      <Button className="icon-button" aria-label={`Copy ${label.toLowerCase()} URL`} onClick={() => onCopy(value)}>
        <Copy size={15} />
      </Button>
    </div>
  )
}

export default function App() {
  const [status, setStatus] = useState<LibraryStatus | null>(null)
  const [tree, setTree] = useState<TreeNode | null>(null)
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState(false)
  const [refreshing, setRefreshing] = useState(false)
  const [error, setError] = useState('')
  const [toast, setToast] = useState<Toast | null>(null)

  const notify = useCallback((message: string, tone?: Toast['tone']) => {
    setToast({ message, tone })
    window.setTimeout(() => setToast(null), 3500)
  }, [])

  const load = useCallback(async () => {
    try {
      const [nextStatus, nextTree] = await Promise.all([
        request<LibraryStatus>('/api/admin/status'),
        request<TreeNode>('/api/admin/browse'),
      ])
      setStatus(nextStatus)
      setTree(nextTree)
      setError('')
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Unable to load the library')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { void load() }, [load])

  const changeFolder = async (node: TreeNode, selected: boolean) => {
    setBusy(true)
    try {
      if (selected) {
        await request<void>('/api/admin/folders', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ path: node.path }),
        })
      } else {
        await request<void>(`/api/admin/folders?path=${encodeURIComponent(node.path)}`, { method: 'DELETE' })
      }
      await load()
      notify(selected ? `${node.name} added to the library` : `${node.name} removed from the library`, 'success')
    } catch (cause) {
      notify(cause instanceof Error ? cause.message : 'Unable to update the folder', 'error')
    } finally {
      setBusy(false)
    }
  }

  const refresh = async () => {
    setRefreshing(true)
    try {
      await request<void>('/api/admin/refresh', { method: 'POST' })
      notify('Catalog refresh started', 'success')
      window.setTimeout(() => void load(), 1200)
    } catch (cause) {
      notify(cause instanceof Error ? cause.message : 'Unable to refresh the catalog', 'error')
    } finally {
      setRefreshing(false)
    }
  }

  const copy = async (value: string) => {
    try {
      await navigator.clipboard.writeText(value)
      notify('URL copied to clipboard', 'success')
    } catch {
      notify('Unable to copy the URL', 'error')
    }
  }

  return (
    <>
      <header className="topbar">
        <a className="brand" href="/admin" aria-label="Local Picsum admin">
          <BrandMark />
          <span>Local Picsum</span>
          <span className="badge">Admin</span>
        </a>
        <a className="button secondary small" href="/logout">Sign out <ExternalLink size={13} /></a>
      </header>

      <main className="page">
        <div className="page-heading">
          <div>
            <span className="eyebrow">Overview</span>
            <h1>Photo library</h1>
            <p>Choose which folders feed your placeholder image catalog.</p>
          </div>
          <Button className="primary" onClick={refresh} disabled={refreshing}>
            {refreshing ? <LoaderCircle className="spin" size={15} /> : <RefreshCw size={15} />}
            Refresh Catalog
          </Button>
        </div>

        <div className="stats" aria-label="Library summary">
          <Stat icon={<Image size={17} />} label="Indexed images">
            <strong>{status ? status.count.toLocaleString() : '—'}</strong>
          </Stat>
          <Stat icon={<Folder size={17} />} label="Library root" className="wide">
            <code>{status?.root ?? 'Loading…'}</code>
          </Stat>
        </div>

        <Card>
          <div className="card-heading">
            <div>
              <h2>Folders</h2>
              <p>Select a folder to include it and all of its subfolders.</p>
            </div>
            {status && <span className="refresh-note"><i /> Auto refresh every {status.refreshInterval}</span>}
          </div>
          <div className="tree" aria-busy={loading || busy}>
            {loading && <div className="skeletons"><i /><i /><i /></div>}
            {error && (
              <div className="error-state" role="alert">
                <span>{error}</span>
                <Button className="secondary small" onClick={() => void load()}>Try Again</Button>
              </div>
            )}
            {!loading && !error && tree && <ul><FolderRow node={tree} depth={0} busy={busy} onChange={changeFolder} /></ul>}
          </div>
        </Card>

        <Card>
          <div className="card-heading">
            <div>
              <h2>Example URLs</h2>
              <p>Use these paths anywhere you need a placeholder image.</p>
            </div>
          </div>
          <div className="snippets">
            <Snippet label="Random image" value="/800/600" onCopy={copy} />
            <Snippet label="Seeded WebP" value="/seed/home/800/600.webp?grayscale&blur=2" onCopy={copy} />
          </div>
        </Card>

        <footer>Local Picsum <span>·</span> Self-hosted image placeholders</footer>
      </main>

      {toast && (
        <div className={`toast ${toast.tone ?? ''}`} role="status">
          {toast.tone === 'success' && <Check size={15} />}
          {toast.tone === 'error' && <X size={15} />}
          <span>{toast.message}</span>
          <button type="button" aria-label="Dismiss notification" onClick={() => setToast(null)}><X size={14} /></button>
        </div>
      )}
    </>
  )
}
