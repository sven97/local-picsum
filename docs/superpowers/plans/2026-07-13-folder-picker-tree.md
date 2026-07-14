# Folder Picker Tree Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the flat, recursively-flattened folder dropdown on `/admin` with a single hierarchical, multi-select tree, where checking a folder also disables (and visually marks) its already-covered descendants.

**Architecture:** Everything lives in the existing single-file `cmd/local-picsum/main.go`, following the codebase's current convention (no new packages/files). Backend: `GET /api/admin/browse` returns one nested JSON tree instead of a flat list; `POST /api/admin/folders` gains ancestor-rejection and descendant-cleanup logic, factored into a testable `addFolder` method. Frontend: the admin page's separate "Selected folders" list and `<select>` picker are replaced by one recursive vanilla-JS tree widget whose checkboxes directly reflect and control library membership.

**Tech Stack:** Go 1.24, `database/sql` + `modernc.org/sqlite`, stdlib `net/http`, vanilla JS (no framework, matches existing admin page).

## Global Constraints

- Spec: `docs/superpowers/specs/2026-07-13-folder-picker-tree-design.md` — implement exactly what it describes, no scope additions.
- No change to `refresh()` / catalog indexing behavior (spec's explicit non-goal).
- No lazy per-level fetching — the whole tree loads in one `GET /api/admin/browse` request (spec's explicit non-goal).
- Keep the existing single-file structure (`cmd/local-picsum/main.go` + `cmd/local-picsum/main_test.go`) — do not split into new files/packages.
- Match existing code style: compact Go, `http.Error` for handler failures, `go a.refresh()` fire-and-forget after folder mutations.
- Per user instruction: do **not** `git commit` automatically after each task or push/upload anything — leave changes uncommitted in the working tree unless the user explicitly asks for a commit.

---

## File Structure

- Modify: `cmd/local-picsum/main.go`
  - Add `node` struct + `buildTree` pure function (tree construction)
  - Add `isAncestor` pure function (path-prefix logic)
  - Add `folderPaths`, `addFolder`, `displayPath`, `errFolderCovered` (folder-selection business logic)
  - Rewrite `browse` handler (flat list → nested tree JSON)
  - Rewrite `folders` POST handler to use `addFolder`
  - Rewrite `admin` handler body + embedded JS/CSS (tree widget UI)
- Modify: `cmd/local-picsum/main_test.go`
  - Add tests for `buildTree`, `isAncestor`, `addFolder`

---

### Task 1: `node` struct + `buildTree` pure function

**Files:**
- Modify: `cmd/local-picsum/main.go` — add after the existing `type photo struct { ... }` block (around line 46)
- Test: `cmd/local-picsum/main_test.go`

**Interfaces:**
- Produces: `type node struct { Name, Path string; Children []*node; Selected, Disabled bool }` and `func buildTree(dirs []string, selected map[string]bool) *node` — used by Task 5 (`browse` handler).

- [ ] **Step 1: Write the failing test**

Add to `cmd/local-picsum/main_test.go`:

```go
func TestBuildTreeMarksSelectedAndDisablesDescendants(t *testing.T) {
	dirs := []string{"2022_Photos", "2022_Photos/Vacation", "2022_Photos/Vacation/Alaska", "2023_Photos"}
	selected := map[string]bool{"2022_Photos": true}
	root := buildTree(dirs, selected)

	if root.Path != "" || root.Name != "/ (library root)" {
		t.Fatalf("unexpected root: %+v", root)
	}
	if root.Disabled {
		t.Fatalf("root must never be disabled")
	}
	if len(root.Children) != 2 {
		t.Fatalf("expected 2 top-level children, got %d", len(root.Children))
	}

	var photos2022, photos2023 *node
	for _, c := range root.Children {
		switch c.Path {
		case "2022_Photos":
			photos2022 = c
		case "2023_Photos":
			photos2023 = c
		}
	}
	if photos2022 == nil || photos2023 == nil {
		t.Fatalf("missing expected top-level nodes: %+v", root.Children)
	}
	if !photos2022.Selected {
		t.Fatalf("2022_Photos should be selected")
	}
	if photos2023.Selected || photos2023.Disabled {
		t.Fatalf("2023_Photos should be untouched: %+v", photos2023)
	}
	if len(photos2022.Children) != 1 || photos2022.Children[0].Path != "2022_Photos/Vacation" {
		t.Fatalf("unexpected children of 2022_Photos: %+v", photos2022.Children)
	}
	vacation := photos2022.Children[0]
	if !vacation.Disabled {
		t.Fatalf("Vacation should be disabled (covered by selected parent)")
	}
	if len(vacation.Children) != 1 || !vacation.Children[0].Disabled {
		t.Fatalf("Alaska should also be disabled (covered by selected grandparent): %+v", vacation.Children)
	}
}

func TestBuildTreeAutoCreatesMissingAncestors(t *testing.T) {
	root := buildTree([]string{"a/b/c"}, nil)
	if len(root.Children) != 1 || root.Children[0].Path != "a" {
		t.Fatalf("expected auto-created 'a' node, got %+v", root.Children)
	}
	b := root.Children[0].Children
	if len(b) != 1 || b[0].Path != "a/b" {
		t.Fatalf("expected auto-created 'a/b' node, got %+v", b)
	}
	if len(b[0].Children) != 1 || b[0].Children[0].Path != "a/b/c" || b[0].Children[0].Name != "c" {
		t.Fatalf("expected 'a/b/c' leaf node, got %+v", b[0].Children)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/local-picsum/... -run TestBuildTree -v`
Expected: FAIL with `undefined: buildTree` (or `undefined: node`)

- [ ] **Step 3: Write minimal implementation**

In `cmd/local-picsum/main.go`, add immediately after the `type photo struct` block:

```go
type node struct {
	Name     string  `json:"name"`
	Path     string  `json:"path"`
	Children []*node `json:"children,omitempty"`
	Selected bool    `json:"selected"`
	Disabled bool    `json:"disabled"`
}

// buildTree builds a folder tree from a flat list of relative directory
// paths ("/"-separated, not including the root itself) and the set of
// paths currently selected for indexing. Missing intermediate ancestors
// are created automatically. The returned root node represents the
// library root itself.
func buildTree(dirs []string, selected map[string]bool) *node {
	sorted := append([]string(nil), dirs...)
	sort.Strings(sorted)

	byPath := map[string]*node{"": {Name: "/ (library root)", Path: ""}}
	var ensure func(p string) *node
	ensure = func(p string) *node {
		if n, ok := byPath[p]; ok {
			return n
		}
		parent, name := "", p
		if i := strings.LastIndex(p, "/"); i >= 0 {
			parent, name = p[:i], p[i+1:]
		}
		n := &node{Name: name, Path: p}
		byPath[p] = n
		pn := ensure(parent)
		pn.Children = append(pn.Children, n)
		return n
	}
	for _, p := range sorted {
		ensure(p)
	}
	for p, n := range byPath {
		n.Selected = selected[p]
	}
	var mark func(n *node, ancestorSelected bool)
	mark = func(n *node, ancestorSelected bool) {
		n.Disabled = ancestorSelected
		for _, c := range n.Children {
			mark(c, ancestorSelected || n.Selected)
		}
	}
	mark(byPath[""], false)
	return byPath[""]
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/local-picsum/... -run TestBuildTree -v`
Expected: PASS (both `TestBuildTreeMarksSelectedAndDisablesDescendants` and `TestBuildTreeAutoCreatesMissingAncestors`)

- [ ] **Step 5: Commit**

Per the Global Constraints, do not commit automatically — leave this change in the working tree and move to Task 2.

---

### Task 2: `isAncestor` pure function

**Files:**
- Modify: `cmd/local-picsum/main.go` — add near `buildTree` (e.g. immediately after it)
- Test: `cmd/local-picsum/main_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `func isAncestor(a, b string) bool` — used by Task 3's `addFolder`.

- [ ] **Step 1: Write the failing test**

Add to `cmd/local-picsum/main_test.go`:

```go
func TestIsAncestor(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"", "x", true},
		{"", "", false},
		{"a", "a/b", true},
		{"a", "a/b/c", true},
		{"a", "ab", false},
		{"a/b", "a", false},
		{"a", "a", false},
	}
	for _, c := range cases {
		if got := isAncestor(c.a, c.b); got != c.want {
			t.Errorf("isAncestor(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/local-picsum/... -run TestIsAncestor -v`
Expected: FAIL with `undefined: isAncestor`

- [ ] **Step 3: Write minimal implementation**

In `cmd/local-picsum/main.go`, add after `buildTree`:

```go
// isAncestor reports whether a is a strict ancestor of b. The root ("")
// is a strict ancestor of every other path, but not of itself.
func isAncestor(a, b string) bool {
	if a == b {
		return false
	}
	if a == "" {
		return b != ""
	}
	return strings.HasPrefix(b, a+"/")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/local-picsum/... -run TestIsAncestor -v`
Expected: PASS

- [ ] **Step 5: Commit**

Do not commit automatically — leave this change in the working tree and move to Task 3.

---

### Task 3: `folderPaths`, `addFolder`, `displayPath` (folder-selection business logic)

**Files:**
- Modify: `cmd/local-picsum/main.go` — add near the existing `folders` handler (around line 263, before it)
- Modify: `cmd/local-picsum/main.go` — add `"errors"` to the import block
- Test: `cmd/local-picsum/main_test.go`

**Interfaces:**
- Consumes: `isAncestor` (Task 2).
- Produces:
  - `func (a *app) folderPaths() ([]string, error)` — used by Task 5's rewritten `browse` handler.
  - `func (a *app) addFolder(rel string) error` — used by Task 4's rewritten `folders` POST handler.
  - `var errFolderCovered error` — sentinel checked via `errors.Is` in Task 4.
  - `func displayPath(p string) string` — used inside `addFolder`'s error message.

- [ ] **Step 1: Write the failing test**

Add to `cmd/local-picsum/main_test.go`:

```go
func newTestApp(t *testing.T) *app {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // keep all queries on the same in-memory database
	a := &app{db: db, root: "/photos"}
	if err := a.init(); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAddFolderRejectsWhenAncestorAlreadySelected(t *testing.T) {
	a := newTestApp(t)
	if err := a.addFolder("2022_Photos"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	err := a.addFolder("2022_Photos/Vacation")
	if !errors.Is(err, errFolderCovered) {
		t.Fatalf("expected errFolderCovered, got %v", err)
	}
	paths, _ := a.folderPaths()
	if len(paths) != 1 || paths[0] != "2022_Photos" {
		t.Fatalf("rejected add must not change folder set, got %v", paths)
	}
}

func TestAddFolderRemovesRedundantDescendants(t *testing.T) {
	a := newTestApp(t)
	if err := a.addFolder("2022_Photos/Vacation"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := a.addFolder("2022_Photos"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	paths, err := a.folderPaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "2022_Photos" {
		t.Fatalf("expected only [2022_Photos] after adding ancestor, got %v", paths)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/local-picsum/... -run TestAddFolder -v`
Expected: FAIL with `undefined: newTestApp` / `undefined: errFolderCovered` (compile error)

- [ ] **Step 3: Write minimal implementation**

In `cmd/local-picsum/main.go`, add `"errors"` to the import block (alphabetically, after `"encoding/json"` and before `"fmt"`):

```go
	"errors"
```

Then add this block immediately before `func (a *app) folders(...)`:

```go
var errFolderCovered = errors.New("already covered by an existing folder")

func displayPath(p string) string {
	if p == "" {
		return "/ (library root)"
	}
	return "/" + p
}

func (a *app) folderPaths() ([]string, error) {
	rows, err := a.db.Query("SELECT path FROM folders")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

// addFolder adds rel to the folder set, keeping it free of
// ancestor/descendant pairs: it rejects the add if an existing folder
// already covers rel, and removes any existing folders that rel now
// covers.
func (a *app) addFolder(rel string) error {
	existing, err := a.folderPaths()
	if err != nil {
		return err
	}
	for _, p := range existing {
		if isAncestor(p, rel) {
			return fmt.Errorf("%w: %s", errFolderCovered, displayPath(p))
		}
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range existing {
		if isAncestor(rel, p) {
			if _, err := tx.Exec("DELETE FROM folders WHERE path=?", p); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec("INSERT OR IGNORE INTO folders(path) VALUES(?)", rel); err != nil {
		return err
	}
	return tx.Commit()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/local-picsum/... -run TestAddFolder -v`
Expected: PASS (both `TestAddFolderRejectsWhenAncestorAlreadySelected` and `TestAddFolderRemovesRedundantDescendants`)

- [ ] **Step 5: Commit**

Do not commit automatically — leave this change in the working tree and move to Task 4.

---

### Task 4: Wire `folders` POST handler to `addFolder`

**Files:**
- Modify: `cmd/local-picsum/main.go` — replace the body of `func (a *app) folders(...)`  (around lines 263–308)

**Interfaces:**
- Consumes: `a.addFolder` and `errFolderCovered` (Task 3).
- Produces: no new exported names; `DELETE`/`POST /api/admin/folders` behavior as described in the spec.

- [ ] **Step 1: Replace the handler**

Replace the entire existing `func (a *app) folders(w http.ResponseWriter, r *http.Request) { ... }` function with:

```go
func (a *app) folders(w http.ResponseWriter, r *http.Request) {
	if !a.requireAPI(w, r) {
		return
	}
	if r.Method == "DELETE" {
		if _, err := a.db.Exec("DELETE FROM folders WHERE path=?", r.URL.Query().Get("path")); err != nil {
			http.Error(w, "unable to remove folder", 500)
			return
		}
		go a.refresh()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}
	p, err := a.safePath(req.Path)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	info, err := os.Stat(p)
	if err != nil || !info.IsDir() {
		http.Error(w, "folder does not exist", 400)
		return
	}
	rel, _ := filepath.Rel(a.root, p)
	if rel == "." {
		rel = ""
	}
	if err := a.addFolder(rel); err != nil {
		if errors.Is(err, errFolderCovered) {
			http.Error(w, err.Error(), 400)
		} else {
			http.Error(w, "unable to save folder", 500)
		}
		return
	}
	go a.refresh()
	w.WriteHeader(204)
}
```

This is a pure refactor (the DELETE branch and POST validation are unchanged); Tasks 2–3's tests already cover the new ancestor/descendant behavior this delegates to, so no new test is needed here.

- [ ] **Step 2: Verify it builds**

Run: `go build ./...`
Expected: no errors

- [ ] **Step 3: Run the full test suite**

Run: `go test ./cmd/local-picsum/... -v`
Expected: PASS (all tests, including the pre-existing ones)

- [ ] **Step 4: Commit**

Do not commit automatically — leave this change in the working tree and move to Task 5.

---

### Task 5: Rewrite `browse` handler to return the nested tree

**Files:**
- Modify: `cmd/local-picsum/main.go` — replace the body of `func (a *app) browse(...)` (around lines 309–336)

**Interfaces:**
- Consumes: `buildTree` (Task 1), `a.folderPaths` (Task 3).
- Produces: `GET /api/admin/browse` now returns a single JSON `node` object (see Task 1's struct) instead of a flat `[]string`. No more `?path=` query parameter — it always returns the tree from the library root.

- [ ] **Step 1: Replace the handler**

Replace the entire existing `func (a *app) browse(w http.ResponseWriter, r *http.Request) { ... }` function with:

```go
func (a *app) browse(w http.ResponseWriter, r *http.Request) {
	if !a.requireAPI(w, r) {
		return
	}
	var dirs []string
	_ = filepath.WalkDir(a.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == a.root || !d.IsDir() || len(dirs) >= 10000 {
			return nil
		}
		rel, err := filepath.Rel(a.root, path)
		if err == nil {
			dirs = append(dirs, filepath.ToSlash(rel))
		}
		return nil
	})
	paths, err := a.folderPaths()
	if err != nil {
		http.Error(w, "unable to load folders", 500)
		return
	}
	selected := make(map[string]bool, len(paths))
	for _, p := range paths {
		selected[p] = true
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(buildTree(dirs, selected))
}
```

- [ ] **Step 2: Verify it builds**

Run: `go build ./...`
Expected: no errors (the `sort` import, previously used only here, is still used by `buildTree` in Task 1, so no unused-import error)

- [ ] **Step 3: Manual verification**

Run the server against a scratch library:

```bash
mkdir -p /tmp/lp-verify/photos/2022_Photos/Vacation /tmp/lp-verify/data
LIBRARY_ROOT=/tmp/lp-verify/photos DATA_DIR=/tmp/lp-verify/data PORT=8091 go run ./cmd/local-picsum &
sleep 1
curl -s -X POST -d 'password=verificationpassword123' http://localhost:8091/setup -c /tmp/lp-verify/cookies.txt -o /dev/null
curl -s -b /tmp/lp-verify/cookies.txt http://localhost:8091/api/admin/browse
kill %1
```

Expected: JSON like `{"name":"/ (library root)","path":"","children":[{"name":"2022_Photos","path":"2022_Photos","children":[{"name":"Vacation","path":"2022_Photos/Vacation","selected":false,"disabled":false}],"selected":false,"disabled":false}],"selected":false,"disabled":false}` — a nested tree, folders only, both selected/disabled false (nothing added to the library yet).

- [ ] **Step 4: Commit**

Do not commit automatically — leave this change in the working tree and move to Task 6.

---

### Task 6: Rewrite the admin page — tree widget UI

**Files:**
- Modify: `cmd/local-picsum/main.go` — replace the body of `func (a *app) admin(...)` (around lines 234–258)
- Modify: `cmd/local-picsum/main.go` — update the `<style>` block inside `render` (around line 231)

**Interfaces:**
- Consumes: the tree JSON shape from Task 5 (`{name, path, children, selected, disabled}`), `POST`/`DELETE /api/admin/folders` from Task 4.
- Produces: no new Go-level interfaces — this is the terminal UI layer.

- [ ] **Step 1: Update the CSS in `render`**

In `cmd/local-picsum/main.go`, find this line inside `func render(...)`:

```go
	fmt.Fprintf(w, `<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title><style>body{font-family:system-ui;max-width:760px;margin:3rem auto;padding:0 1rem;color:#17202a}input,button{font:inherit;padding:.5rem;margin:.4rem}button{cursor:pointer}code{background:#eef;padding:.2rem}.folder{padding:.35rem;border-bottom:1px solid #ddd}</style></head><body><h1>%s</h1>%s</body></html>`, title, title, body)
```

Replace the `.folder{padding:.35rem;border-bottom:1px solid #ddd}` segment with tree-related classes, so the full line becomes:

```go
	fmt.Fprintf(w, `<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title><style>body{font-family:system-ui;max-width:760px;margin:3rem auto;padding:0 1rem;color:#17202a}input,button{font:inherit;padding:.5rem;margin:.4rem}button{cursor:pointer}code{background:#eef;padding:.2rem}ul.tree{list-style:none;margin:0;padding-left:1.1rem}ul.tree.root{padding-left:0}.node{padding:.15rem 0}.node.disabled{color:#888}.toggle{display:inline-block;width:1rem;cursor:pointer;user-select:none}</style></head><body><h1>%s</h1>%s</body></html>`, title, title, body)
```

- [ ] **Step 2: Replace the `admin` handler**

Replace the entire existing `func (a *app) admin(w http.ResponseWriter, r *http.Request) { ... }` function (everything from `func (a *app) admin` up to its closing `}`, including the old `label := p` folder-list loop) with:

```go
func (a *app) admin(w http.ResponseWriter, r *http.Request) {
	if !a.require(w, r) {
		return
	}
	var count int
	_ = a.db.QueryRow("SELECT count(*) FROM photos").Scan(&count)
	body := `<p><strong>` + strconv.Itoa(count) + `</strong> indexed images. The library root is <code>` + html(a.root) + `</code>.</p><p><a href="/logout">Sign out</a></p><h2>Folders</h2><p>Check a folder to add it to the library. Its subfolders are covered automatically and shown disabled.</p><div id="tree">Loading…</div><p><button id="refresh">Refresh catalog now</button> <span id="status"></span></p><h2>URL examples</h2><code>/800/600</code> · <code>/seed/home/800/600.webp?grayscale&amp;blur=2</code><script>` + adminScript + `</script>`
	render(w, "Local Picsum", body)
}

const adminScript = `
const treeEl = document.querySelector('#tree');
function anySelected(n) {
  if (n.selected) return true;
  return (n.children || []).some(anySelected);
}
function renderNode(n, depth) {
  const li = document.createElement('li');
  const row = document.createElement('div');
  row.className = 'node' + (n.disabled ? ' disabled' : '');
  const hasChildren = n.children && n.children.length > 0;
  const toggle = document.createElement('span');
  toggle.className = 'toggle';
  row.append(toggle);
  const cb = document.createElement('input');
  cb.type = 'checkbox';
  cb.checked = n.selected;
  cb.disabled = n.disabled;
  cb.onchange = () => setFolder(n.path, cb.checked);
  row.append(cb);
  const label = document.createElement('span');
  label.textContent = ' ' + n.name + (n.disabled ? ' (included via parent)' : '');
  row.append(label);
  li.append(row);
  if (hasChildren) {
    const ul = document.createElement('ul');
    ul.className = 'tree';
    for (const c of n.children) ul.append(renderNode(c, depth + 1));
    const expanded = depth === 0 || n.children.some(anySelected);
    ul.hidden = !expanded;
    toggle.textContent = expanded ? '▾' : '▸';
    toggle.onclick = () => { ul.hidden = !ul.hidden; toggle.textContent = ul.hidden ? '▸' : '▾'; };
  }
  return li;
}
async function loadTree() {
  const r = await fetch('/api/admin/browse');
  if (!r.ok) { treeEl.textContent = 'Unable to load folders.'; return; }
  const root = await r.json();
  treeEl.innerHTML = '';
  const ul = document.createElement('ul');
  ul.className = 'tree root';
  ul.append(renderNode(root, 0));
  treeEl.append(ul);
}
async function setFolder(path, add) {
  const r = add
    ? await fetch('/api/admin/folders', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({path})})
    : await fetch('/api/admin/folders?path=' + encodeURIComponent(path), {method: 'DELETE'});
  if (r.ok) location.reload();
  else alert(await r.text());
}
loadTree();
document.querySelector('#refresh').onclick = async () => {
  status.textContent = 'Scanning…';
  const r = await fetch('/api/admin/refresh', {method: 'POST'});
  status.textContent = r.ok ? 'Refresh started' : 'Failed';
};
`
```

Note: `▾`, `▸`, and `…` above are literal UTF-8 characters (down/right triangle, ellipsis) — paste them as-is; Go source files are UTF-8 and raw string literals (backtick-delimited) cannot contain backslash escapes, so they must be the literal characters, not `\u...` sequences.

- [ ] **Step 3: Verify it builds**

Run: `go build ./...`
Expected: no errors

- [ ] **Step 4: Run the full test suite**

Run: `go test ./cmd/local-picsum/... -v`
Expected: PASS (all tests)

- [ ] **Step 5: Commit**

Do not commit automatically — leave this change in the working tree and move to Task 7.

---

### Task 7: End-to-end manual verification

**Files:** none (verification only)

- [ ] **Step 1: Run the app against a realistic scratch library**

```bash
mkdir -p /tmp/lp-e2e/photos/2022_Photos/Vacation/Alaska /tmp/lp-e2e/photos/2023_Photos /tmp/lp-e2e/data
cp "$(find / -iname '*.jpg' -size +10k 2>/dev/null | head -1)" /tmp/lp-e2e/photos/2022_Photos/Vacation/sample.jpg
LIBRARY_ROOT=/tmp/lp-e2e/photos DATA_DIR=/tmp/lp-e2e/data PORT=8092 go run ./cmd/local-picsum &
sleep 1
```

- [ ] **Step 2: Open `http://localhost:8092/setup` in a browser**

Set an admin password (10+ characters). You should land on `/admin` and see a "Folders" section with an expandable tree showing `2022_Photos` and `2023_Photos` at the top level (both expanded by default, since depth 0 always expands), and `Vacation` / `Alaska` nested under `2022_Photos` (collapsed by default, since nothing is selected yet).

- [ ] **Step 3: Exercise the ancestor/descendant behavior**

Check `2022_Photos`. Confirm:
- The page reloads and `2022_Photos` shows checked.
- Expanding it shows `Vacation` and `Alaska` with greyed-out, disabled checkboxes labeled "(included via parent)".
- `<strong>N</strong> indexed images` eventually shows `1` after the background refresh completes (reload the page or wait a few seconds — `sample.jpg` lives under the now-selected folder).

Uncheck `2022_Photos`. Confirm `Vacation`/`Alaska` become enabled, unchecked checkboxes again, and the indexed count drops back to `0` after refresh.

- [ ] **Step 4: Clean up**

```bash
kill %1
rm -rf /tmp/lp-e2e /tmp/lp-verify
```

- [ ] **Step 5: Report results**

Summarize what was verified (tree renders, ancestor disables descendants, catalog re-indexes on add/remove) and flag anything that didn't match expectations before considering this plan complete. Do not commit or push — leave the working tree as-is per the Global Constraints, and let the user decide when to commit.
