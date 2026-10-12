// Rôle : API REST, flux temps réel (SSE), authentification et service de l'interface web.

// Package web exposes the REST API, the live event stream (SSE) and the UI.
package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"ratiopilot/internal/engine"
)

// Server serves the API and the embedded UI.
type Server struct {
	eng      *engine.Engine
	static   fs.FS
	password string
	secret   []byte
	version  string

	mu       sync.Mutex
	attempts map[string][]time.Time // anti brute-force
}

// New creates the HTTP handler. An empty password disables the login page.
func New(eng *engine.Engine, static fs.FS, dataDir, password, version string) (*Server, error) {
	s := &Server{eng: eng, static: static, password: password, version: version, attempts: map[string][]time.Time{}}
	secretPath := filepath.Join(dataDir, ".session-secret")
	if b, err := os.ReadFile(secretPath); err == nil && len(b) >= 32 {
		s.secret = b
	} else {
		s.secret = make([]byte, 32)
		_, _ = rand.Read(s.secret)
		if err := os.WriteFile(secretPath, s.secret, 0o600); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Handler returns the root http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("GET /api/me", s.me)

	api := http.NewServeMux()
	api.HandleFunc("GET /api/state", s.state)
	api.HandleFunc("GET /api/stream", s.stream)
	api.HandleFunc("POST /api/start", func(w http.ResponseWriter, r *http.Request) {
		if err := s.eng.Start(); err != nil {
			jsonErr(w, http.StatusBadRequest, err)
			return
		}
		jsonOK(w, map[string]bool{"ok": true})
	})
	api.HandleFunc("POST /api/stop", func(w http.ResponseWriter, r *http.Request) {
		s.eng.Stop()
		jsonOK(w, map[string]bool{"ok": true})
	})
	api.HandleFunc("PUT /api/config", s.putConfig)
	api.HandleFunc("POST /api/torrents", s.upload)
	api.HandleFunc("POST /api/torrents/{hash}/{action}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.eng.Action(r.PathValue("hash"), r.PathValue("action")); err != nil {
			jsonErr(w, http.StatusBadRequest, err)
			return
		}
		jsonOK(w, map[string]bool{"ok": true})
	})
	mux.Handle("/api/", s.requireAuth(api))
	mux.Handle("/", s.staticHandler())
	return securityHeaders(mux)
}

// ---------- auth ----------

const cookieName = "rp_session"

func (s *Server) sign(exp int64) string {
	payload := strconv.FormatInt(exp, 10)
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Server) valid(token string) bool {
	payload, _, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(payload, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(s.sign(exp)), []byte(token))
}

func (s *Server) authed(r *http.Request) bool {
	if s.password == "" {
		return true
	}
	c, err := r.Cookie(cookieName)
	return err == nil && s.valid(c.Value)
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			jsonErr(w, http.StatusUnauthorized, errors.New("connexion requise"))
			return
		}
		if r.Method != http.MethodGet && !sameOrigin(r) {
			jsonErr(w, http.StatusForbidden, errors.New("origine refusée"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin blocks cross-site form posts (CSRF) on top of SameSite cookies.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return strings.HasSuffix(origin, "://"+host)
}

func clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		return strings.TrimSpace(strings.Split(f, ",")[0])
	}
	ip := r.RemoteAddr
	if i := strings.LastIndex(ip, ":"); i > 0 {
		ip = ip[:i]
	}
	return ip
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.password == "" {
		jsonOK(w, map[string]bool{"ok": true})
		return
	}
	ip := clientIP(r)
	s.mu.Lock()
	recent := s.attempts[ip][:0]
	for _, t := range s.attempts[ip] {
		if time.Since(t) < 10*time.Minute {
			recent = append(recent, t)
		}
	}
	s.attempts[ip] = recent
	blocked := len(recent) >= 8
	s.mu.Unlock()
	if blocked {
		jsonErr(w, http.StatusTooManyRequests, errors.New("trop de tentatives, réessaie dans quelques minutes"))
		return
	}
	var body struct {
		Password string `json:"password"`
		Remember bool   `json:"remember"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		jsonErr(w, http.StatusBadRequest, errors.New("requête invalide"))
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Password), []byte(s.password)) != 1 {
		s.mu.Lock()
		s.attempts[ip] = append(s.attempts[ip], time.Now())
		s.mu.Unlock()
		time.Sleep(700 * time.Millisecond)
		jsonErr(w, http.StatusUnauthorized, errors.New("mot de passe incorrect"))
		return
	}
	dur := 12 * time.Hour
	if body.Remember {
		dur = 30 * 24 * time.Hour
	}
	exp := time.Now().Add(dur)
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: s.sign(exp.Unix()), Path: "/", Expires: exp,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
	jsonOK(w, map[string]bool{"ok": true})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	jsonOK(w, map[string]bool{"ok": true})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]any{"authenticated": s.authed(r), "authEnabled": s.password != "", "version": s.version})
}

// ---------- API ----------

type fullState struct {
	Snapshot engine.Snapshot `json:"snapshot"`
	Config   engine.Config   `json:"config"`
	Clients  []string        `json:"clients"`
	History  []engine.Sample `json:"history"`
	Events   []engine.Event  `json:"events"`
	Version  string          `json:"version"`
	Auth     bool            `json:"authEnabled"`
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, fullState{
		Snapshot: s.eng.Snapshot(), Config: s.eng.Config(), Clients: s.eng.ClientNames(),
		History: s.eng.History(), Events: s.eng.Events(), Version: s.version, Auth: s.password != "",
	})
}

// stream pushes {snapshot, events(new), config} roughly every second (SSE).
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming non supporté", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	lastID, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	ch, unsub := s.eng.Subscribe()
	defer unsub()
	keep := time.NewTicker(20 * time.Second)
	defer keep.Stop()
	send := func() error {
		var fresh []engine.Event
		for _, ev := range s.eng.Events() {
			if ev.ID > lastID {
				fresh = append(fresh, ev)
				lastID = ev.ID
			}
		}
		data, _ := json.Marshal(map[string]any{"snapshot": s.eng.Snapshot(), "events": fresh, "config": s.eng.Config()})
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return err
		}
		fl.Flush()
		return nil
	}
	if send() != nil {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			if send() != nil {
				return
			}
		case <-keep.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	var c engine.Config
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		jsonErr(w, http.StatusBadRequest, fmt.Errorf("configuration invalide: %v", err))
		return
	}
	if err := s.eng.UpdateConfig(c); err != nil {
		jsonErr(w, http.StatusBadRequest, err)
		return
	}
	jsonOK(w, s.eng.Config())
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		jsonErr(w, http.StatusBadRequest, errors.New("envoi invalide (32 Mo max)"))
		return
	}
	type result struct {
		File  string `json:"file"`
		Name  string `json:"name,omitempty"`
		Error string `json:"error,omitempty"`
	}
	var out []result
	for _, fh := range r.MultipartForm.File["files"] {
		res := result{File: fh.Filename}
		f, err := fh.Open()
		if err == nil {
			var data []byte
			data, err = io.ReadAll(io.LimitReader(f, 10<<20))
			f.Close()
			if err == nil {
				res.Name, err = s.eng.AddTorrent(fh.Filename, data)
			}
		}
		if err != nil {
			res.Error = err.Error()
		}
		out = append(out, res)
	}
	jsonOK(w, out)
}

// ---------- static ----------

func (s *Server) staticHandler() http.Handler {
	fsrv := http.FileServer(http.FS(s.static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		fsrv.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-eval'; img-src 'self' data:; connect-src 'self'")
		next.ServeHTTP(w, r)
	})
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// RandomPassword generates a readable password when none is configured.
func RandomPassword() string {
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
