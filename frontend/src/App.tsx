import { useCallback, useEffect, useMemo, useState, type ButtonHTMLAttributes, type CSSProperties, type ReactNode } from 'react'
import {
  Check,
  ChevronDown,
  ChevronRight,
  Copy,
  ExternalLink,
  Folder,
  Image,
  ListFilter,
  LoaderCircle,
  RefreshCw,
  Search,
  X,
  ArrowUpDown,
} from 'lucide-react'

type LibraryStatus = {
  count: number
  root: string
  refreshInterval: string
}

type PreviewImage = {
  id: string
  name: string
  width: number
  height: number
}

type TreeNode = {
  name: string
  path: string
  children?: TreeNode[]
  selected: boolean
  disabled: boolean
  count: number
}

type FolderSort = 'name-asc' | 'name-desc' | 'count-desc' | 'count-asc'

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

function countSelected(node: TreeNode): number {
  return Number(node.selected) + (node.children?.reduce((total, child) => total + countSelected(child), 0) ?? 0)
}

function countFolders(node: TreeNode): number {
  return 1 + (node.children?.reduce((total, child) => total + countFolders(child), 0) ?? 0)
}

function filterTree(node: TreeNode, query: string, selectedOnly: boolean, isRoot = true): TreeNode | null {
  const children = node.children
    ?.map((child) => filterTree(child, query, selectedOnly, false))
    .filter((child): child is TreeNode => child !== null)
  const searchable = `${node.name} ${node.path}`.toLocaleLowerCase()
  const matchesQuery = !query || searchable.includes(query)
  const matchesSelection = !selectedOnly || node.selected

  if (!isRoot && !(matchesQuery && matchesSelection) && !children?.length) return null
  return { ...node, children }
}

const folderCollator = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' })

function sortTree(node: TreeNode, sort: FolderSort): TreeNode {
  const children = node.children?.map((child) => sortTree(child, sort))
  if (!children) return { ...node }

  children.sort((left, right) => {
    const primary = sort.startsWith('name')
      ? folderCollator.compare(left.name, right.name)
      : left.count - right.count
    const direction = sort.endsWith('desc') ? -1 : 1
    return (primary * direction) || folderCollator.compare(left.name, right.name)
  })
  return { ...node, children }
}

function FolderRow({ node, depth, busy, forceExpanded, onChange }: { node: TreeNode; depth: number; busy: boolean; forceExpanded: boolean; onChange: (node: TreeNode, selected: boolean) => void }) {
  const hasChildren = Boolean(node.children?.length)
  const [expanded, setExpanded] = useState(depth === 0 || hasSelectedDescendant(node))
  const isExpanded = forceExpanded || expanded
  const inputId = `folder-${node.path ? encodeURIComponent(node.path) : 'root'}`

  return (
    <li>
      <div className={`folder-row${node.selected ? ' selected' : ''}${node.disabled ? ' disabled' : ''}`} style={{ '--depth': depth } as CSSProperties}>
        <button
          type="button"
          className="tree-toggle"
          aria-label={hasChildren ? `${isExpanded ? 'Collapse' : 'Expand'} ${node.name}` : undefined}
          aria-expanded={hasChildren ? isExpanded : undefined}
          disabled={!hasChildren || forceExpanded}
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
      {hasChildren && isExpanded && (
        <ul>
          {node.children!.map((child) => (
            <FolderRow key={child.path} node={child} depth={depth + 1} busy={busy} forceExpanded={forceExpanded} onChange={onChange} />
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
  const [previews, setPreviews] = useState<PreviewImage[]>([])
  const [loading, setLoading] = useState(true)
  const [previewLoading, setPreviewLoading] = useState(true)
  const [busy, setBusy] = useState(false)
  const [refreshing, setRefreshing] = useState(false)
  const [error, setError] = useState('')
  const [toast, setToast] = useState<Toast | null>(null)
  const [folderQuery, setFolderQuery] = useState('')
  const [selectedOnly, setSelectedOnly] = useState(false)
  const [folderSort, setFolderSort] = useState<FolderSort>('name-asc')

  const normalizedQuery = folderQuery.trim().toLocaleLowerCase()
  const visibleTree = useMemo(() => {
    if (!tree) return null
    const filtered = filterTree(tree, normalizedQuery, selectedOnly)
    return filtered ? sortTree(filtered, folderSort) : null
  }, [tree, normalizedQuery, selectedOnly, folderSort])
  const selectedCount = useMemo(() => tree ? countSelected(tree) : 0, [tree])
  const visibleCount = useMemo(() => visibleTree ? Math.max(0, countFolders(visibleTree) - 1) : 0, [visibleTree])

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
      const nextPreviews = await request<PreviewImage[]>('/api/admin/preview')
      setPreviews(nextPreviews)
      setError('')
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Unable to load the library')
    } finally {
      setLoading(false)
      setPreviewLoading(false)
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

  const refreshPreview = async () => {
    setPreviewLoading(true)
    try {
      setPreviews(await request<PreviewImage[]>('/api/admin/preview'))
    } catch (cause) {
      notify(cause instanceof Error ? cause.message : 'Unable to load image preview', 'error')
    } finally {
      setPreviewLoading(false)
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
          <div className="tree-toolbar">
            <div className="search-input">
              <Search size={15} aria-hidden="true" />
              <input
                type="search"
                value={folderQuery}
                placeholder="Search folders…"
                aria-label="Search folders"
                onChange={(event) => setFolderQuery(event.target.value)}
              />
              {folderQuery && (
                <button type="button" aria-label="Clear folder search" onClick={() => setFolderQuery('')}>
                  <X size={14} />
                </button>
              )}
            </div>
            <Button
              className={`secondary small filter-button${selectedOnly ? ' active' : ''}`}
              aria-pressed={selectedOnly}
              onClick={() => setSelectedOnly((value) => !value)}
            >
              <ListFilter size={14} />
              Selected
              <span className="count-badge">{selectedCount}</span>
            </Button>
            <label className="sort-control">
              <ArrowUpDown size={14} aria-hidden="true" />
              <span className="sr-only">Sort folders</span>
              <select aria-label="Sort folders" value={folderSort} onChange={(event) => setFolderSort(event.target.value as FolderSort)}>
                <option value="name-asc">Name: A–Z</option>
                <option value="name-desc">Name: Z–A</option>
                <option value="count-desc">Images: most first</option>
                <option value="count-asc">Images: least first</option>
              </select>
              <ChevronDown size={14} aria-hidden="true" />
            </label>
            {!loading && !error && <span className="result-count">{visibleCount} {visibleCount === 1 ? 'folder' : 'folders'}</span>}
          </div>
          <div className="tree" aria-busy={loading || busy}>
            {loading && <div className="skeletons"><i /><i /><i /></div>}
            {error && (
              <div className="error-state" role="alert">
                <span>{error}</span>
                <Button className="secondary small" onClick={() => void load()}>Try Again</Button>
              </div>
            )}
            {!loading && !error && visibleTree && visibleCount > 0 && (
              <ul><FolderRow node={visibleTree} depth={0} busy={busy} forceExpanded={Boolean(normalizedQuery || selectedOnly)} onChange={changeFolder} /></ul>
            )}
            {!loading && !error && visibleCount === 0 && (
              <div className="empty-tree">
                <Search size={18} />
                <strong>No folders found</strong>
                <span>Try a different search or show all folders.</span>
                {(folderQuery || selectedOnly) && (
                  <Button className="secondary small" onClick={() => { setFolderQuery(''); setSelectedOnly(false) }}>Clear Filters</Button>
                )}
              </div>
            )}
          </div>
        </Card>

        <Card className="preview-card">
          <div className="card-heading">
            <div>
              <h2>Image preview</h2>
              <p>Random samples from the images currently indexed by the catalog.</p>
            </div>
            <Button className="secondary small" onClick={() => void refreshPreview()} disabled={previewLoading}>
              <RefreshCw size={14} className={previewLoading ? 'spin' : ''} />
              New samples
            </Button>
          </div>
          {previewLoading && previews.length === 0 && <div className="preview-skeletons"><i /><i /><i /></div>}
          {!previewLoading && previews.length === 0 && (
            <div className="preview-empty">
              <Image size={18} />
              <strong>No indexed images yet</strong>
              <span>Select a folder and refresh the catalog to preview images here.</span>
            </div>
          )}
          {previews.length > 0 && (
            <div className="preview-grid">
              {previews.map((preview) => (
                <figure key={preview.id} className="preview-item">
                  <img src={`/id/${encodeURIComponent(preview.id)}/360/240.webp`} alt={preview.name} loading="lazy" />
                  <figcaption title={preview.name}>
                    <span>{preview.name}</span>
                    <small>{preview.width} × {preview.height}</small>
                  </figcaption>
                </figure>
              ))}
            </div>
          )}
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
