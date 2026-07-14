# Folder picker: hierarchical tree with multi-select

## Problem

The admin folder picker (`/admin`, backed by `GET /api/admin/browse`) currently
returns every nested subdirectory under the library root as one flat list,
rendered as a single `<select>` dropdown. For a real photo library with
deep, nested folder structures, this dumps hundreds of entries into one
unnavigable list instead of letting the admin browse level by level.

Folder membership (`/admin`'s "Selected folders" section, backed by
`POST`/`DELETE /api/admin/folders`) is also a separate UI element from the
picker, requiring the admin to browse in one control and manage selection in
another.

## Goals

- Replace the flat dropdown with an expandable/collapsible tree, fetched
  in one request (not lazy per-level).
- Merge folder browsing and folder selection into one control: the tree's
  checkboxes directly reflect and control library membership.
- Support checking multiple folders across different branches of the tree.
- Since indexing a folder already recursively includes its subfolders'
  photos (`refresh()` walks each selected folder's full subtree), checking
  a folder should disable checkboxes on its descendants — they're already
  covered — and adding a folder should clean up any now-redundant
  descendant selections already in the database.

## Non-goals

- No change to `refresh()` / catalog indexing behavior.
- No lazy, per-level fetching — the whole tree loads in one response.
- No change to the "Refresh catalog now" control or the URL-examples
  section of the admin page.

## Backend

### `GET /api/admin/browse`: flat list → nested tree

Response shape becomes a single nested tree instead of `[]string`:

```go
type node struct {
    Name     string `json:"name"`
    Path     string `json:"path"`               // relative to library root; "" = root
    Children []node `json:"children,omitempty"`
    Selected bool   `json:"selected"`            // directly present in the folders table
    Disabled bool   `json:"disabled"`            // an ancestor is selected, so this is already covered
}
```

The existing `filepath.WalkDir` traversal (directories only, 10,000-dir
safety cap retained) is restructured to build this tree instead of a flat
`[]string`. `Selected`/`Disabled` are computed in one pass against the
current rows in the `folders` table: a node is `Selected` if its path is a
row in `folders`; it is `Disabled` if any strict ancestor path is a row in
`folders`.

The root of the tree represents the library root itself (`Path: ""`,
`Name: "/ (library root)"`), and is checkable exactly like any other node.

### `POST /api/admin/folders`: keep the selected set an antichain

Two rules are added on top of the existing validation (path must resolve
inside the library root via `safePath`, must exist, must be a directory):

1. **Reject if an ancestor is already selected.** If any strict ancestor of
   the requested path is already a row in `folders`, return `400` with a
   message like `already covered by /2022_Photos`. The UI will not
   normally produce this request (ancestor selection disables descendant
   checkboxes client-side), but the server enforces it independently of
   client state (e.g. stale page, concurrent admin sessions).
2. **Clean up redundant descendants.** If the requested path has any
   descendants already present in `folders`, delete those rows as part of
   the same insert. This keeps the selected set free of
   ancestor/descendant pairs, so the tree's checkbox state stays
   consistent (parent checked, children simply disabled — never
   stale-checked).

Both checks operate on the `folders` table using simple path-prefix
comparison (a path `p` is a descendant of `q` iff `q == ""` or
`p == q` or `strings.HasPrefix(p, q+"/")`).

### `DELETE /api/admin/folders`

Unchanged — deletes the given row. Note that removing a folder does not
restore any descendant selections that were auto-deleted earlier when that
folder was added; that subtree simply becomes fully unselected and can be
re-checked from the tree as needed.

## Frontend (`/admin`)

The "Selected folders" list and the `<select>`-based "Add a folder" form
are both removed and replaced by a single recursive tree widget:

- On page load, one `fetch('/api/admin/browse')` retrieves the whole tree;
  a recursive render function builds nested `<ul>` elements from it.
- Each row shows an expand/collapse triangle (▸/▾ — purely client-side
  state, no fetch, since the whole tree is already in memory), a
  checkbox, and the folder name.
- Checkbox `checked` reflects `node.selected`; `disabled` reflects
  `node.disabled`, with disabled rows visually greyed and labeled
  "(included via parent)".
- Checking a box does `POST /api/admin/folders`; unchecking does
  `DELETE /api/admin/folders`; either is followed by `location.reload()`,
  consistent with how the existing Remove/Add actions on this page already
  behave (no client-side state reconciliation needed).
- The root row is labeled "/ (library root)" as today.

## Testing

Following the existing style in `main_test.go` (small pure-function unit
tests, no HTTP server):

- A pure function that builds the `node` tree from a directory listing
  plus a set of selected paths, tested for correct `Selected`/`Disabled`
  propagation (parent selected → child disabled; unrelated sibling
  untouched; deeply nested selection).
- A test for the folder-add path-prefix logic: adding a folder whose
  ancestor is already selected returns an error and does not insert;
  adding a folder whose descendant is already selected deletes that
  descendant's row as part of the same call.
