package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
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
			render(w, "Setup", "<p>Password must be at least 10 characters.</p>"+setupForm())
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
	return `<p>Create the administrator password. Image URLs remain public.</p><form method="post"><label>Password <input name="password" type="password" minlength="10" required autofocus></label><button>Create account</button></form>`
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
		render(w, "Sign in", `<p>Invalid password.</p>`+loginForm())
		return
	}
	render(w, "Sign in", loginForm())
}
func loginForm() string {
	return `<form method="post"><label>Password <input name="password" type="password" required autofocus></label><button>Sign in</button></form>`
}
func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "lp_session", Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", 303)
}
func render(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title><style>body{font-family:system-ui;max-width:760px;margin:3rem auto;padding:0 1rem;color:#17202a}input,button{font:inherit;padding:.5rem;margin:.4rem}button{cursor:pointer}code{background:#eef;padding:.2rem}.folder{padding:.35rem;border-bottom:1px solid #ddd}</style></head><body><h1>%s</h1>%s</body></html>`, title, title, body)
}

func (a *app) admin(w http.ResponseWriter, r *http.Request) {
	if !a.require(w, r) {
		return
	}
	var count int
	_ = a.db.QueryRow("SELECT count(*) FROM photos").Scan(&count)
	rows, _ := a.db.Query("SELECT path FROM folders ORDER BY path")
	defer rows.Close()
	var selected []string
	for rows.Next() {
		var p string
		rows.Scan(&p)
		selected = append(selected, p)
	}
	body := `<p><strong>` + strconv.Itoa(count) + `</strong> indexed images. The library root is <code>` + html(a.root) + `</code>.</p><p><a href="/logout">Sign out</a></p><h2>Selected folders</h2><div id="folders">`
	for _, p := range selected {
		label := p
		if label == "" {
			label = "/ (library root)"
		}
		body += `<div class="folder">` + html(label) + ` <button data-path="` + html(p) + `" class="remove">Remove</button></div>`
	}
	body += `</div><h2>Add a folder</h2><form id="add"><select name="path" id="path"><option value="">/ (library root)</option></select><button>Add selected folder</button></form><p><button id="refresh">Refresh catalog now</button> <span id="status"></span></p><h2>URL examples</h2><code>/800/600</code> · <code>/seed/home/800/600.webp?grayscale&amp;blur=2</code><script>const f=document.querySelector('#add'),path=document.querySelector('#path');async function browse(p=''){let r=await fetch('/api/admin/browse?path='+encodeURIComponent(p));if(!r.ok)return;for(let d of await r.json()){let o=document.createElement('option');o.value=d;o.textContent='/'+d;path.append(o)}}browse();f.onsubmit=async e=>{e.preventDefault();let r=await fetch('/api/admin/folders',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({path:new FormData(f).get('path')})});if(r.ok)location.reload();else alert(await r.text())};document.querySelectorAll('.remove').forEach(b=>b.onclick=async()=>{if(confirm('Remove this folder from the library?')){let r=await fetch('/api/admin/folders?path='+encodeURIComponent(b.dataset.path),{method:'DELETE'});if(r.ok)location.reload();else alert(await r.text())}});document.querySelector('#refresh').onclick=async()=>{status.textContent='Scanning…';let r=await fetch('/api/admin/refresh',{method:'POST'});status.textContent=r.ok?'Refresh started':'Failed'}</script>`
	render(w, "Local Picsum", body)
}
func html(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;").Replace(s)
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
	_, err = a.db.Exec("INSERT OR IGNORE INTO folders(path) VALUES(?)", rel)
	if err != nil {
		http.Error(w, "unable to save folder", 500)
		return
	}
	go a.refresh()
	w.WriteHeader(204)
}
func (a *app) browse(w http.ResponseWriter, r *http.Request) {
	if !a.requireAPI(w, r) {
		return
	}
	p, e := a.safePath(r.URL.Query().Get("path"))
	if e != nil {
		http.Error(w, e.Error(), 400)
		return
	}
	if info, err := os.Stat(p); err != nil || !info.IsDir() {
		http.Error(w, "folder unreadable", 400)
		return
	}
	var dirs []string
	_ = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == p || !d.IsDir() || len(dirs) >= 10000 {
			return nil
		}
		rel, err := filepath.Rel(a.root, path)
		if err == nil {
			dirs = append(dirs, rel)
		}
		return nil
	})
	sort.Strings(dirs)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dirs)
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
			if err != nil || d.IsDir() {
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
	f, e := os.Open(p)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	ext := strings.ToLower(filepath.Ext(p))
	if ext == ".webp" {
		return xwebp.Decode(f)
	}
	if ext == ".png" {
		return png.Decode(f)
	}
	return jpeg.Decode(f)
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
