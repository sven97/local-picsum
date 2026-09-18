package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chai2010/webp"
	"github.com/rwcarlsen/goexif/exif"
	"golang.org/x/crypto/bcrypt"
	xdraw "golang.org/x/image/draw"
	xwebp "golang.org/x/image/webp"
	_ "modernc.org/sqlite"
)

type app struct {
	db       *sql.DB
	root     string
	interval time.Duration
	mu       sync.Mutex
	secret   []byte
}
type photo struct {
	ID, Path      string
	Width, Height int
}
type node struct {
	Name     string  `json:"name"`
	Path     string  `json:"path"`
	Children []*node `json:"children,omitempty"`
	Selected bool    `json:"selected"`
	Disabled bool    `json:"disabled"`
	Count    int     `json:"count"`
}

//go:embed static
var adminUI embed.FS

// buildTree builds a folder tree from a flat list of relative directory
// paths ("/"-separated, not including the root itself), the set of paths
// currently selected for indexing, and a count of supported image files
// found directly inside each path (keyed the same way, "" for the root).
// Missing intermediate ancestors are created automatically. Each node's
// Count is the total across itself and all its descendants. The returned
// root node represents the library root itself.
func buildTree(dirs []string, selected map[string]bool, counts map[string]int) *node {
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

	var aggregate func(n *node) int
	aggregate = func(n *node) int {
		total := counts[n.Path]
		for _, c := range n.Children {
			total += aggregate(c)
		}
		n.Count = total
		return total
	}
	aggregate(byPath[""])

	return byPath[""]
}

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

func main() {
	data := env("DATA_DIR", "/data")
	root := filepath.Clean(env("LIBRARY_ROOT", "/photos"))
	port := env("PORT", "8080")
	if err := os.MkdirAll(data, 0700); err != nil {
		log.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(data, "local-picsum.db"))
	if err != nil {
		log.Fatal(err)
	}
	a := &app{db: db, root: root, interval: duration(env("REFRESH_INTERVAL", "6h"))}
	if err := a.init(); err != nil {
		log.Fatal(err)
	}
	a.secret = a.loadSecret(data)
	if a.configured() {
		go a.refreshLoop()
		go a.refresh()
	}
	mux := http.NewServeMux()
	a.routes(mux)
	log.Printf("Local Picsum listening on :%s (library root %s)", port, root)
	log.Fatal(http.ListenAndServe(":"+port, securityHeaders(mux)))
}

func (a *app) init() error {
	_, err := a.db.Exec(`PRAGMA journal_mode=WAL;
 CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY, value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS photos(id TEXT PRIMARY KEY, path TEXT UNIQUE NOT NULL, width INTEGER NOT NULL, height INTEGER NOT NULL, mtime INTEGER NOT NULL, size INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS folders(path TEXT PRIMARY KEY);`)
	return err
}
func (a *app) loadSecret(dir string) []byte {
	p := filepath.Join(dir, "session.secret")
	if b, e := os.ReadFile(p); e == nil && len(b) > 0 {
		return b
	}
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		log.Fatal(e)
	}
	if e := os.WriteFile(p, b, 0600); e != nil {
		log.Fatal(e)
	}
	return b
}
func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func duration(v string) time.Duration {
	d, e := time.ParseDuration(v)
	if e != nil || d <= 0 {
		return 6 * time.Hour
	}
	return d
}
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (a *app) routes(m *http.ServeMux) {
	m.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	m.HandleFunc("/setup", a.setup)
	m.HandleFunc("/login", a.login)
	m.HandleFunc("/logout", a.logout)
	m.HandleFunc("/admin", a.admin)
	m.HandleFunc("/admin/", a.admin)
	m.HandleFunc("/api/admin/folders", a.folders)
	m.HandleFunc("/api/admin/browse", a.browse)
	m.HandleFunc("/api/admin/status", a.adminStatus)
	m.HandleFunc("/api/admin/refresh", a.manualRefresh)
	m.HandleFunc("/", a.image)
}
func (a *app) configured() bool {
	var n int
	_ = a.db.QueryRow("SELECT count(*) FROM settings WHERE key='password_hash'").Scan(&n)
	return n == 1
}
func (a *app) setting(k string) string {
	var v string
	_ = a.db.QueryRow("SELECT value FROM settings WHERE key=?", k).Scan(&v)
	return v
}
func (a *app) set(k, v string) error {
	_, e := a.db.Exec("INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", k, v)
	return e
}
func (a *app) session(w http.ResponseWriter, user string) {
	exp := strconv.FormatInt(time.Now().Add(7*24*time.Hour).Unix(), 10)
	value := user + "." + exp
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(value))
	http.SetCookie(w, &http.Cookie{Name: "lp_session", Value: base64.RawURLEncoding.EncodeToString([]byte(value + "." + hex.EncodeToString(mac.Sum(nil)))), Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: false, MaxAge: 604800})
}
func (a *app) authed(r *http.Request) bool {
	c, e := r.Cookie("lp_session")
	if e != nil {
		return false
	}
	b, e := base64.RawURLEncoding.DecodeString(c.Value)
	if e != nil {
		return false
	}
	p := strings.Split(string(b), ".")
	if len(p) != 3 || p[0] != "admin" {
		return false
	}
	x, e := strconv.ParseInt(p[1], 10, 64)
	if e != nil || time.Now().Unix() > x {
		return false
	}
	m := hmac.New(sha256.New, a.secret)
	m.Write([]byte(p[0] + "." + p[1]))
	expected := hex.EncodeToString(m.Sum(nil))
	return hmac.Equal([]byte(p[2]), []byte(expected))
}
func (a *app) require(w http.ResponseWriter, r *http.Request) bool {
	if !a.authed(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return false
	}
	return true
}

func (a *app) setup(w http.ResponseWriter, r *http.Request) {
	if a.configured() {
		http.Redirect(w, r, "/login", 303)
		return
	}
	if r.Method == "POST" {
		p := r.FormValue("password")
		if len(p) < 10 {
			render(w, "Create admin account", `<div class="form-error" role="alert">Password must be at least 10 characters.</div>`+setupForm())
			return
		}
		h, e := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
		if e != nil {
			http.Error(w, "unable to create account", 500)
			return
		}
		_ = a.set("password_hash", string(h))
		a.session(w, "admin")
		go a.refresh()
		http.Redirect(w, r, "/admin", 303)
		return
	}
	render(w, "Set up Local Picsum", setupForm())
}
func setupForm() string {
	return `<p class="description">Secure the dashboard with a password. Your image URLs will remain public.</p><form method="post"><label for="password">Password</label><input id="password" name="password" type="password" minlength="10" autocomplete="new-password" placeholder="At least 10 characters" required autofocus><button type="submit">Create account <span aria-hidden="true">→</span></button></form><p class="footnote">Your password is stored locally as a secure hash.</p>`
}
func (a *app) login(w http.ResponseWriter, r *http.Request) {
	if !a.configured() {
		http.Redirect(w, r, "/setup", 303)
		return
	}
	if r.Method == "POST" {
		if bcrypt.CompareHashAndPassword([]byte(a.setting("password_hash")), []byte(r.FormValue("password"))) == nil {
			a.session(w, "admin")
			http.Redirect(w, r, "/admin", 303)
			return
		}
		render(w, "Sign in", `<div class="form-error" role="alert">The password you entered is incorrect.</div>`+loginForm())
		return
	}
	render(w, "Sign in", loginForm())
}
func loginForm() string {
	return `<p class="description">Enter your admin password to manage the photo library.</p><form method="post"><label for="password">Password</label><input id="password" name="password" type="password" autocomplete="current-password" placeholder="Enter your password" required autofocus><button type="submit">Sign in <span aria-hidden="true">→</span></button></form>`
}
func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "lp_session", Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", 303)
}
func render(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="color-scheme" content="light dark"><title>%s · Local Picsum</title><style>%s</style></head><body><main><section class="auth-card"><a class="brand" href="/" aria-label="Local Picsum"><span class="brand-mark" aria-hidden="true"><span></span></span><span>Local Picsum</span></a><div class="heading"><span class="eyebrow">Administrator</span><h1>%s</h1></div>%s</section></main></body></html>`, html(title), authCSS, html(title), body)
}

const authCSS = `
@font-face{font-family:Geist;src:url('/admin/assets/Geist-Variable.woff2') format('woff2');font-weight:100 900;font-display:swap}
:root{font-family:Geist,-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;color:#1d1d1f;background:#fafafa;color-scheme:light dark}
*{box-sizing:border-box}body{margin:0;min-width:320px;min-height:100vh;background:radial-gradient(circle at 50% 0,rgba(0,112,243,.04),transparent 360px),#fafafa}
main{min-height:100vh;padding:48px 20px;display:grid;place-items:center}.auth-card{width:min(100%,420px);padding:32px;border:1px solid #e6e6e6;border-radius:12px;background:#fff;box-shadow:0 8px 30px rgba(0,0,0,.06)}
.brand{display:inline-flex;align-items:center;gap:9px;color:inherit;text-decoration:none;font-size:14px;line-height:20px;font-weight:600}.brand-mark{width:26px;height:26px;border-radius:6px;display:grid;place-items:center;background:#1d1d1f}.brand-mark span{width:0;height:0;border-left:6px solid transparent;border-right:6px solid transparent;border-bottom:11px solid #fff}
.heading{margin-top:40px}.eyebrow{display:block;margin-bottom:8px;color:#666;font-size:11px;line-height:16px;letter-spacing:.06em;text-transform:uppercase}h1{margin:0;font-size:28px;line-height:36px;letter-spacing:-.04em;font-weight:600}.description{margin:10px 0 24px;color:#666;font-size:14px;line-height:22px}
form{display:grid;gap:8px}label{font-size:13px;line-height:20px;font-weight:500}input{width:100%;height:40px;padding:0 12px;color:#1d1d1f;background:#fff;border:1px solid #c9c9c9;border-radius:6px;font:inherit;font-size:14px;outline:0;transition:border-color .15s ease,box-shadow .15s ease}input::placeholder{color:#8f8f8f}input:focus{border-color:#1d1d1f;box-shadow:0 0 0 1px #1d1d1f}input:focus-visible{outline:0}
button{height:40px;margin-top:8px;padding:0 14px;border:1px solid #1d1d1f;border-radius:6px;display:flex;align-items:center;justify-content:center;gap:8px;color:#fff;background:#1d1d1f;font:inherit;font-size:14px;font-weight:500;cursor:pointer;transition:background .15s ease}button:hover{background:#383838}.footnote{margin:18px 0 0;color:#7d7d7d;font-size:12px;line-height:18px;text-align:center}.form-error{margin:20px 0 -8px;padding:10px 12px;color:#b42318;background:#fff0f0;border:1px solid #ffd7d7;border-radius:6px;font-size:13px;line-height:20px}
:focus-visible{outline:2px solid #1d1d1f;outline-offset:2px}
@media(max-width:480px){main{padding:20px 16px;align-items:start}.auth-card{margin-top:28px;padding:24px}.heading{margin-top:32px}h1{font-size:26px;line-height:34px}}
@media(prefers-color-scheme:dark){:root{color:#ededed;background:#0a0a0a}body{background:radial-gradient(circle at 50% 0,rgba(0,112,243,.10),transparent 360px),#0a0a0a}.auth-card{background:#111;border-color:#2e2e2e;box-shadow:none}.brand-mark{background:#ededed}.brand-mark span{border-bottom-color:#0a0a0a}.eyebrow,.description{color:#a1a1a1}input{color:#ededed;background:#0a0a0a;border-color:#454545}input:focus{border-color:#ededed;box-shadow:0 0 0 1px #ededed}button{color:#0a0a0a;background:#ededed;border-color:#ededed}button:hover{background:#ccc}.footnote{color:#8f8f8f}.form-error{color:#ff7373;background:#2a1616;border-color:#5c2525}:focus-visible{outline-color:#ededed}}
`

func (a *app) admin(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/")
	if r.URL.Path == "/admin" || path == "" {
		path = "index.html"
	} else {
		path = filepath.ToSlash(filepath.Clean(path))
		if strings.HasPrefix(path, "../") {
			http.NotFound(w, r)
			return
		}
	}
	publicAsset := strings.HasPrefix(path, "assets/") || path == "GEIST-LICENSE.txt"
	if !publicAsset && !a.require(w, r) {
		return
	}
	b, err := adminUI.ReadFile("static/" + path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch filepath.Ext(path) {
	case ".html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case ".woff2":
		w.Header().Set("Content-Type", "font/woff2")
	}
	if path != "index.html" {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	_, _ = w.Write(b)
}

func html(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;").Replace(s)
}

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
func (a *app) browse(w http.ResponseWriter, r *http.Request) {
	if !a.requireAPI(w, r) {
		return
	}
	var dirs []string
	counts := map[string]int{}
	_ = filepath.WalkDir(a.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if isThumbnailCacheDir(d) {
			return filepath.SkipDir
		}
		if d.IsDir() {
			if path == a.root || len(dirs) >= 10000 {
				return nil
			}
			if rel, err := filepath.Rel(a.root, path); err == nil {
				dirs = append(dirs, filepath.ToSlash(rel))
			}
			return nil
		}
		if !supported(path) {
			return nil
		}
		rel, err := filepath.Rel(a.root, filepath.Dir(path))
		if err != nil {
			return nil
		}
		if rel == "." {
			rel = ""
		}
		counts[filepath.ToSlash(rel)]++
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
	json.NewEncoder(w).Encode(buildTree(dirs, selected, counts))
}

func (a *app) adminStatus(w http.ResponseWriter, r *http.Request) {
	if !a.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var count int
	if err := a.db.QueryRow("SELECT count(*) FROM photos").Scan(&count); err != nil {
		http.Error(w, "unable to load library status", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Count           int    `json:"count"`
		Root            string `json:"root"`
		RefreshInterval string `json:"refreshInterval"`
	}{Count: count, Root: a.root, RefreshInterval: compactDuration(a.interval)})
}

func compactDuration(d time.Duration) string {
	if d%time.Hour == 0 {
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	}
	if d%time.Minute == 0 {
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	}
	return d.String()
}

func (a *app) manualRefresh(w http.ResponseWriter, r *http.Request) {
	if !a.requireAPI(w, r) {
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	go a.refresh()
	w.WriteHeader(202)
}
func (a *app) requireAPI(w http.ResponseWriter, r *http.Request) bool {
	if !a.authed(r) {
		http.Error(w, "authentication required", 401)
		return false
	}
	return true
}
func (a *app) safePath(rel string) (string, error) {
	p := filepath.Clean(filepath.Join(a.root, rel))
	r, e := filepath.Rel(a.root, p)
	if e != nil || strings.HasPrefix(r, "..") || filepath.IsAbs(r) {
		return "", fmt.Errorf("path must be inside the library root")
	}
	return p, nil
}
func (a *app) refreshLoop() {
	t := time.NewTicker(a.interval)
	defer t.Stop()
	for range t.C {
		a.refresh()
	}
}
func (a *app) refresh() {
	a.mu.Lock()
	defer a.mu.Unlock()
	rows, e := a.db.Query("SELECT path FROM folders")
	if e != nil {
		return
	}
	var folders []string
	for rows.Next() {
		var p string
		rows.Scan(&p)
		folders = append(folders, p)
	}
	rows.Close()
	if len(folders) == 0 {
		return
	}
	seen := map[string]bool{}
	tx, e := a.db.Begin()
	if e != nil {
		return
	}
	defer tx.Rollback()
	for _, folder := range folders {
		base, e := a.safePath(folder)
		if e != nil {
			continue
		}
		filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if isThumbnailCacheDir(d) {
				return filepath.SkipDir
			}
			if d.IsDir() {
				return nil
			}
			if !supported(path) {
				return nil
			}
			info, e := d.Info()
			if e != nil {
				return nil
			}
			rel, e := filepath.Rel(a.root, path)
			if e != nil {
				return nil
			}
			seen[rel] = true
			id := photoID(rel)
			w, h, e := dimensions(path)
			if e != nil {
				return nil
			}
			_, e = tx.Exec(`INSERT INTO photos(id,path,width,height,mtime,size) VALUES(?,?,?,?,?,?) ON CONFLICT(path) DO UPDATE SET id=excluded.id,width=excluded.width,height=excluded.height,mtime=excluded.mtime,size=excluded.size`, id, rel, w, h, info.ModTime().UnixNano(), info.Size())
			return nil
		})
	}
	existing, e := tx.Query("SELECT path FROM photos")
	if e == nil {
		for existing.Next() {
			var p string
			existing.Scan(&p)
			if !seen[p] {
				tx.Exec("DELETE FROM photos WHERE path=?", p)
			}
		}
		existing.Close()
	}
	if e := tx.Commit(); e != nil {
		log.Printf("catalog refresh: %v", e)
		return
	}
	log.Printf("catalog refresh complete: %d files", len(seen))
}

// isThumbnailCacheDir reports whether d is a Synology thumbnail-cache
// directory ("@eaDir", created automatically inside every media folder,
// containing one subdirectory per photo named after that photo's
// filename). It must never be treated as part of the photo library.
func isThumbnailCacheDir(d fs.DirEntry) bool {
	return d.IsDir() && d.Name() == "@eaDir"
}

func supported(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".jpg", ".jpeg", ".png", ".webp":
		return true
	}
	return false
}
func photoID(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:])[:16]
}
func dimensions(p string) (int, int, error) {
	f, e := os.Open(p)
	if e != nil {
		return 0, 0, e
	}
	defer f.Close()
	cfg, _, e := image.DecodeConfig(f)
	return cfg.Width, cfg.Height, e
}

func (a *app) image(w http.ResponseWriter, r *http.Request) {
	p := strings.Trim(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if p == "" {
		http.Redirect(w, r, "/admin", 303)
		return
	}
	parts := strings.Split(p, "/")
	var id, seed string
	var dims []string
	if len(parts) > 0 && parts[0] == "id" {
		if len(parts) < 3 {
			http.NotFound(w, r)
			return
		}
		id = parts[1]
		dims = parts[2:]
	} else if len(parts) > 0 && parts[0] == "seed" {
		if len(parts) < 3 {
			http.NotFound(w, r)
			return
		}
		seed = parts[1]
		dims = parts[2:]
	} else {
		dims = parts
	}
	if len(dims) > 2 {
		http.NotFound(w, r)
		return
	}
	format := "jpg"
	last := dims[len(dims)-1]
	if strings.HasSuffix(last, ".jpg") {
		dims[len(dims)-1] = strings.TrimSuffix(last, ".jpg")
	} else if strings.HasSuffix(last, ".webp") {
		format = "webp"
		dims[len(dims)-1] = strings.TrimSuffix(last, ".webp")
	}
	width, e := positive(dims[0])
	if e != nil {
		http.Error(w, "invalid width", 400)
		return
	}
	height := width
	if len(dims) == 2 {
		height, e = positive(dims[1])
		if e != nil {
			http.Error(w, "invalid height", 400)
			return
		}
	}
	blur := 0
	if raw, ok := r.URL.Query()["blur"]; ok {
		blur = 1
		if len(raw) > 0 && raw[0] != "" {
			blur, e = strconv.Atoi(raw[0])
			if e != nil || blur < 1 || blur > 10 {
				http.Error(w, "blur must be 1 through 10", 400)
				return
			}
		}
	}
	var ph photo
	if id != "" {
		e = a.db.QueryRow("SELECT id,path,width,height FROM photos WHERE id=?", id).Scan(&ph.ID, &ph.Path, &ph.Width, &ph.Height)
	} else {
		ph, e = a.pick(seed)
	}
	if e != nil {
		http.Error(w, "image not found", 404)
		return
	}
	full, e := a.safePath(ph.Path)
	if e != nil {
		http.Error(w, "image not found", 404)
		return
	}
	src, e := decode(full)
	if e != nil {
		http.Error(w, "image cannot be decoded", 422)
		return
	}
	dst := cover(src, width, height)
	if _, ok := r.URL.Query()["grayscale"]; ok {
		gray(dst)
	}
	if blur > 0 {
		dst = boxBlur(dst, blur)
	}
	w.Header().Set("Cache-Control", cache(seed, id, r))
	w.Header().Set("Picsum-ID", ph.ID)
	if format == "webp" {
		w.Header().Set("Content-Type", "image/webp")
		e = webp.Encode(w, dst, &webp.Options{Lossless: false, Quality: 85})
	} else {
		w.Header().Set("Content-Type", "image/jpeg")
		e = jpeg.Encode(w, dst, &jpeg.Options{Quality: 85})
	}
	if e != nil {
		log.Printf("encode: %v", e)
	}
}
func positive(v string) (int, error) {
	n, e := strconv.Atoi(v)
	if e != nil || n < 1 || n > 10000 {
		return 0, fmt.Errorf("invalid")
	}
	return n, nil
}
func (a *app) pick(seed string) (photo, error) {
	rows, e := a.db.Query("SELECT id,path,width,height FROM photos ORDER BY id")
	if e != nil {
		return photo{}, e
	}
	defer rows.Close()
	var all []photo
	for rows.Next() {
		var p photo
		rows.Scan(&p.ID, &p.Path, &p.Width, &p.Height)
		all = append(all, p)
	}
	if len(all) == 0 {
		return photo{}, sql.ErrNoRows
	}
	if seed == "" {
		b := make([]byte, 8)
		rand.Read(b)
		return all[int(binary.BigEndian.Uint64(b)%uint64(len(all)))], nil
	}
	h := sha256.Sum256([]byte(seed))
	return all[int(binary.BigEndian.Uint64(h[:8])%uint64(len(all)))], nil
}
func cache(seed, id string, r *http.Request) string {
	if seed != "" || id != "" {
		return "public, max-age=86400"
	}
	if _, ok := r.URL.Query()["random"]; ok {
		return "no-store"
	}
	return "no-store"
}
func decode(p string) (image.Image, error) {
	ext := strings.ToLower(filepath.Ext(p))
	if ext == ".webp" || ext == ".png" {
		f, e := os.Open(p)
		if e != nil {
			return nil, e
		}
		defer f.Close()
		if ext == ".webp" {
			return xwebp.Decode(f)
		}
		return png.Decode(f)
	}
	b, e := os.ReadFile(p)
	if e != nil {
		return nil, e
	}
	img, e := jpeg.Decode(bytes.NewReader(b))
	if e != nil {
		return nil, e
	}
	return applyOrientation(img, jpegOrientation(b)), nil
}

// jpegOrientation reads the EXIF Orientation tag from raw JPEG bytes,
// defaulting to 1 (identity) if it's missing or unreadable.
func jpegOrientation(b []byte) int {
	x, e := exif.Decode(bytes.NewReader(b))
	if e != nil {
		return 1
	}
	tag, e := x.Get(exif.Orientation)
	if e != nil {
		return 1
	}
	o, e := tag.Int(0)
	if e != nil {
		return 1
	}
	return o
}

// applyOrientation corrects src for the given EXIF orientation value (1-8).
// o<=1 or o>8 is treated as "no correction needed" and returns src unchanged.
func applyOrientation(src image.Image, o int) image.Image {
	if o <= 1 || o > 8 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	ow, oh := w, h
	if o >= 5 {
		ow, oh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, ow, oh))
	for y := 0; y < oh; y++ {
		for x := 0; x < ow; x++ {
			var sx, sy int
			switch o {
			case 2: // flip horizontal
				sx, sy = w-1-x, y
			case 3: // rotate 180
				sx, sy = w-1-x, h-1-y
			case 4: // flip vertical
				sx, sy = x, h-1-y
			case 5: // transpose
				sx, sy = y, x
			case 6: // rotate 90 CW
				sx, sy = y, h-1-x
			case 7: // transverse
				sx, sy = w-1-y, h-1-x
			case 8: // rotate 90 CCW (270 CW)
				sx, sy = w-1-y, x
			}
			dst.Set(x, y, src.At(b.Min.X+sx, b.Min.Y+sy))
		}
	}
	return dst
}

func cover(src image.Image, w, h int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	scale := max(float64(w)/float64(sw), float64(h)/float64(sh))
	tw, th := int(float64(sw)*scale+.5), int(float64(sh)*scale+.5)
	tmp := image.NewRGBA(image.Rect(0, 0, tw, th))
	xdraw.CatmullRom.Scale(tmp, tmp.Bounds(), src, b, draw.Over, nil)
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	x, y := (tw-w)/2, (th-h)/2
	draw.Draw(out, out.Bounds(), tmp, image.Pt(x, y), draw.Src)
	return out
}
func gray(img *image.RGBA) {
	for y := 0; y < img.Rect.Dy(); y++ {
		for x := 0; x < img.Rect.Dx(); x++ {
			i := img.PixOffset(x, y)
			v := uint8((299*uint16(img.Pix[i]) + 587*uint16(img.Pix[i+1]) + 114*uint16(img.Pix[i+2])) / 1000)
			img.Pix[i], img.Pix[i+1], img.Pix[i+2] = v, v, v
		}
	}
}
func boxBlur(src *image.RGBA, amount int) *image.RGBA {
	radius := amount * 2
	out := image.NewRGBA(src.Bounds())
	for y := 0; y < src.Rect.Dy(); y++ {
		for x := 0; x < src.Rect.Dx(); x++ {
			var rr, gg, bb, aa, n uint32
			for yy := maxi(0, y-radius); yy <= mini(src.Rect.Dy()-1, y+radius); yy++ {
				for xx := maxi(0, x-radius); xx <= mini(src.Rect.Dx()-1, x+radius); xx++ {
					c := src.RGBAAt(xx, yy)
					rr += uint32(c.R)
					gg += uint32(c.G)
					bb += uint32(c.B)
					aa += uint32(c.A)
					n++
				}
			}
			out.SetRGBA(x, y, color.RGBA{uint8(rr / n), uint8(gg / n), uint8(bb / n), uint8(aa / n)})
		}
	}
	return out
}
func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func mini(a, b int) int {
	if a < b {
		return a
	}
	return b
}
