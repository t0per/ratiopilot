// Package engine is the seeding simulator: it schedules tracker announces,
// dispatches a fake upload bandwidth across torrents and keeps live stats.
package engine

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ratiopilot/internal/emu"
	"ratiopilot/internal/torrent"
	"ratiopilot/internal/tracker"
)

// Torrent statuses shown in the UI.
const (
	StQueued   = "queued"   // en attente d'un créneau
	StStarting = "starting" // annonce "started" en cours
	StSeeding  = "seeding"  // actif
	StRetrying = "retrying" // dernière annonce en échec, nouvel essai prévu
	StStopping = "stopping"
	StPaused   = "paused"
	StDone     = "done"    // ratio cible atteint
	StNoPeers  = "nopeers" // plus aucun pair (si keepZeroLeechers=false)
	StError    = "error"   // trop d'échecs consécutifs
	StIdle     = "idle"    // seed global arrêté
)

type tState struct {
	hash        [20]byte
	HexHash     string
	Name        string
	Size        int64
	Trackers    []string
	Private     bool
	File        string
	AddedAt     time.Time
	Status      string
	Paused      bool
	Done        bool
	NoPeers     bool
	Errored     bool
	TrackerIdx  int
	Seeders     int
	Leechers    int
	Interval    int
	LastAnn     time.Time
	NextAnn     time.Time
	LastError   string
	Warning     string
	Failures    int
	SessionUp   float64 // octets annoncés depuis le dernier "started"
	TotalUp     float64 // cumul toutes sessions
	Speed       float64 // octets/s
	weight      float64
	active      bool
	cancel      context.CancelFunc
	force       chan struct{}
	announcedOK bool
}

// Engine is safe for concurrent use.
type Engine struct {
	dataDir     string
	torrentsDir string
	clientsDir  string
	embedded    fs.FS

	mu          sync.Mutex
	cfg         Config
	running     bool
	startedAt   time.Time
	torrents    map[string]*tState
	client      *emu.Client
	globalRate  float64 // octets/s
	nextRefresh time.Time
	history     []Sample
	events      []Event
	listener    net.Listener
	listenPort  int
	sessionUp   float64
	dirty       bool
	subs        map[chan struct{}]struct{}
	eventSeq    int64
}

// Sample is one point of the speed chart.
type Sample struct {
	T     int64   `json:"t"` // unix seconds
	Speed float64 `json:"s"` // octets/s
}

// Event is one line of the activity log.
type Event struct {
	ID      int64  `json:"id"`
	T       int64  `json:"t"`
	Level   string `json:"level"` // info | ok | warn | error
	Torrent string `json:"torrent,omitempty"`
	Msg     string `json:"msg"`
}

type persisted struct {
	TotalUp map[string]float64 `json:"totalUploaded"`
	Paused  map[string]bool    `json:"paused"`
	Done    map[string]bool    `json:"done"`
	Added   map[string]int64   `json:"addedAt"`
	Running bool               `json:"running"`
}

// New loads config and state from dataDir. embeddedClients holds the
// built-in *.client files (data/clients/*.client override them).
func New(dataDir string, embeddedClients fs.FS) (*Engine, error) {
	e := &Engine{
		dataDir:     dataDir,
		torrentsDir: filepath.Join(dataDir, "torrents"),
		clientsDir:  filepath.Join(dataDir, "clients"),
		embedded:    embeddedClients,
		torrents:    map[string]*tState{},
		subs:        map[chan struct{}]struct{}{},
	}
	for _, d := range []string{dataDir, e.torrentsDir, e.clientsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	cfg, err := loadConfig(filepath.Join(dataDir, "config.json"))
	if err != nil {
		return nil, err
	}
	if cfg.Port == 0 {
		cfg.Port = 49152 + rand.IntN(65534-49152)
	}
	if _, err := e.loadClient(cfg.Client); err != nil {
		names := e.ClientNames()
		if len(names) == 0 {
			return nil, errors.New("aucun fichier .client disponible")
		}
		log.Printf("client %q introuvable, utilisation de %q", cfg.Client, names[len(names)-1])
		cfg.Client = names[len(names)-1]
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config.json: %w", err)
	}
	e.cfg = cfg
	_ = writeJSON(filepath.Join(dataDir, "config.json"), cfg)
	e.scanTorrents(true)
	st := e.loadState()
	if cfg.AutoStart || st.Running {
		if err := e.Start(); err != nil {
			e.logf("error", "", "Démarrage automatique impossible : %v", err)
		}
	}
	return e, nil
}

// Run blocks until ctx is cancelled: bandwidth ticks, folder watch, persistence.
func (e *Engine) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	last := time.Now()
	n := 0
	for {
		select {
		case <-ctx.Done():
			e.shutdown()
			return
		case now := <-tick.C:
			dt := now.Sub(last).Seconds()
			last = now
			e.bandwidthTick(now, dt)
			n++
			if n%5 == 0 {
				e.scanTorrents(false)
			}
			if n%30 == 0 {
				e.saveState()
			}
			e.notify()
		}
	}
}

// ---------- clients ----------

// ClientNames lists available .client files, sorted naturally.
func (e *Engine) ClientNames() []string {
	set := map[string]bool{}
	if e.embedded != nil {
		entries, _ := fs.ReadDir(e.embedded, ".")
		for _, en := range entries {
			if strings.HasSuffix(en.Name(), ".client") {
				set[en.Name()] = true
			}
		}
	}
	if entries, err := os.ReadDir(e.clientsDir); err == nil {
		for _, en := range entries {
			if strings.HasSuffix(en.Name(), ".client") {
				set[en.Name()] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return naturalLess(out[i], out[j]) })
	return out
}

func (e *Engine) loadClient(name string) (*emu.Client, error) {
	if name != filepath.Base(name) {
		return nil, errors.New("nom de client invalide")
	}
	data, err := os.ReadFile(filepath.Join(e.clientsDir, name))
	if err != nil && e.embedded != nil {
		data, err = fs.ReadFile(e.embedded, name)
	}
	if err != nil {
		return nil, fmt.Errorf("client %s introuvable", name)
	}
	return emu.Load(name, data)
}

// ---------- global start/stop ----------

// Start begins seeding.
func (e *Engine) Start() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return nil
	}
	cl, err := e.loadClient(e.cfg.Client)
	if err != nil {
		return err
	}
	if err := e.openPort(); err != nil {
		e.logf("warn", "", "Port %d non ouvrable (%v) : il sera quand même annoncé", e.cfg.Port, err)
	}
	e.client = cl
	e.running = true
	e.startedAt = time.Now()
	e.sessionUp = 0
	e.refreshRate()
	e.logf("ok", "", "Seed démarré — client %s, port %d", strings.TrimSuffix(cl.Name, ".client"), e.cfg.Port)
	e.reconcile()
	e.dirty = true
	return nil
}

// Stop ends seeding (sends "stopped" to trackers).
func (e *Engine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running {
		return
	}
	e.running = false
	e.globalRate = 0
	e.logf("info", "", "Seed arrêté")
	e.reconcile()
	e.closePort()
	e.dirty = true
}

func (e *Engine) openPort() error {
	e.closePort()
	l, err := net.Listen("tcp", ":"+strconv.Itoa(e.cfg.Port))
	if err != nil {
		return err
	}
	e.listener, e.listenPort = l, e.cfg.Port
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close() // port "ouvert" mais aucun échange de données
		}
	}()
	return nil
}

func (e *Engine) closePort() {
	if e.listener != nil {
		_ = e.listener.Close()
		e.listener = nil
	}
}

// UpdateConfig validates and applies a new configuration.
func (e *Engine) UpdateConfig(c Config) error {
	if c.Port == 0 {
		c.Port = e.Config().Port
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if _, err := e.loadClient(c.Client); err != nil {
		return err
	}
	e.mu.Lock()
	old := e.cfg
	e.cfg = c
	if err := writeJSON(filepath.Join(e.dataDir, "config.json"), c); err != nil {
		e.cfg = old
		e.mu.Unlock()
		return err
	}
	e.logf("info", "", "Configuration enregistrée")
	restart := e.running && (old.Client != c.Client || old.Port != c.Port)
	if e.running && (old.MinUploadRate != c.MinUploadRate || old.MaxUploadRate != c.MaxUploadRate || old.SpeedRefreshMinutes != c.SpeedRefreshMinutes) {
		e.refreshRate()
	}
	if e.running && !restart {
		e.reconcile()
	}
	e.mu.Unlock()
	if restart {
		e.logf("info", "", "Client ou port modifié : redémarrage du seed")
		e.Stop()
		e.waitIdle(15 * time.Second)
		return e.Start()
	}
	return nil
}

func (e *Engine) waitIdle(max time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		busy := false
		for _, t := range e.torrents {
			if t.active {
				busy = true
				break
			}
		}
		e.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Config returns a copy of the current config.
func (e *Engine) Config() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg
}

// ---------- scheduling ----------

func eligible(t *tState) bool {
	return !t.Paused && !t.Done && !t.NoPeers && !t.Errored
}

// reconcile starts/stops announcers so that the active set matches config.
// Must be called with e.mu held.
func (e *Engine) reconcile() {
	list := e.sortedLocked()
	activeCount := 0
	for _, t := range list {
		if t.active && t.Status != StStopping {
			if !e.running || !eligible(t) || activeCount >= e.cfg.SimultaneousSeed {
				e.stopLocked(t)
				continue
			}
			activeCount++
		}
	}
	for _, t := range list {
		if t.active {
			continue
		}
		switch {
		case t.Done:
			t.Status = StDone
		case t.Errored:
			t.Status = StError
		case t.NoPeers:
			t.Status = StNoPeers
		case t.Paused:
			t.Status = StPaused
		case !e.running:
			t.Status = StIdle
		case activeCount < e.cfg.SimultaneousSeed:
			e.launchLocked(t)
			activeCount++
		default:
			t.Status = StQueued
		}
	}
}

func (e *Engine) launchLocked(t *tState) {
	ctx, cancel := context.WithCancel(context.Background())
	t.active = true
	t.cancel = cancel
	t.force = make(chan struct{}, 1)
	t.Status = StStarting
	t.SessionUp = 0
	t.Failures = 0
	t.LastError = ""
	t.announcedOK = false
	t.weight = 0
	t.Speed = 0
	go e.announcer(ctx, t, e.client, e.cfg.Port, t.force)
}

func (e *Engine) stopLocked(t *tState) {
	if t.cancel != nil {
		t.Status = StStopping
		t.cancel()
	}
}

func (e *Engine) announcer(ctx context.Context, t *tState, cl *emu.Client, port int, force chan struct{}) {
	ev := emu.EventStarted
	for {
		e.mu.Lock()
		maxFail := e.cfg.MaxFailures
		url := t.Trackers[t.TrackerIdx%len(t.Trackers)]
		st := emu.Stats{Uploaded: int64(t.SessionUp)}
		e.mu.Unlock()

		actx, cancel := context.WithTimeout(ctx, 45*time.Second)
		resp, err := tracker.Announce(actx, tracker.Request{URL: url, InfoHash: t.hash, Event: ev, Stats: st, Port: port, Client: cl})
		cancel()
		if ctx.Err() != nil {
			break
		}

		var wait time.Duration
		e.mu.Lock()
		t.LastAnn = time.Now()
		if err != nil {
			t.Failures++
			t.LastError = err.Error()
			t.Status = StRetrying
			t.weight = 0
			t.TrackerIdx = (t.TrackerIdx + 1) % len(t.Trackers)
			e.logf("warn", t.Name, "Annonce échouée (%s) : %v", hostOf(url), err)
			if t.Failures >= maxFail {
				t.Errored = true
				e.logf("error", t.Name, "%d échecs consécutifs : torrent mis en erreur", t.Failures)
				e.mu.Unlock()
				break
			}
			wait = time.Duration(30*(1<<min(t.Failures-1, 5))) * time.Second
			if t.Interval > 0 && wait > time.Duration(t.Interval)*time.Second {
				wait = time.Duration(t.Interval) * time.Second
			}
		} else {
			t.Failures = 0
			t.LastError = ""
			t.Warning = resp.Warning
			t.Seeders, t.Leechers, t.Interval = resp.Seeders, resp.Leechers, resp.Interval
			t.announcedOK = true
			t.weight = peersWeight(resp.Seeders, resp.Leechers)
			t.Status = StSeeding
			if ev == emu.EventStarted {
				e.logf("ok", t.Name, "Démarré sur %s — %d seeders, %d leechers", hostOf(url), resp.Seeders, resp.Leechers)
			} else {
				e.logf("info", t.Name, "Annonce OK — %s envoyés, %d S / %d L", humanBytes(st.Uploaded), resp.Seeders, resp.Leechers)
			}
			ev = emu.EventNone
			wait = time.Duration(resp.Interval) * time.Second
			if !e.cfg.KeepZeroLeechers && resp.Seeders+resp.Leechers == 0 {
				t.NoPeers = true
				e.logf("warn", t.Name, "Aucun pair : torrent retiré de la rotation")
				e.mu.Unlock()
				break
			}
		}
		t.NextAnn = time.Now().Add(wait)
		e.mu.Unlock()

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-force:
			timer.Stop()
		case <-timer.C:
		}
		if ctx.Err() != nil {
			break
		}
	}

	// send "stopped" if the tracker knows us
	e.mu.Lock()
	sendStop := t.announcedOK && !t.Errored
	url := t.Trackers[t.TrackerIdx%len(t.Trackers)]
	st := emu.Stats{Uploaded: int64(t.SessionUp)}
	t.weight, t.Speed = 0, 0
	e.mu.Unlock()
	if sendStop {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := tracker.Announce(sctx, tracker.Request{URL: url, InfoHash: t.hash, Event: emu.EventStopped, Stats: st, Port: port, Client: cl})
		cancel()
		if err != nil {
			e.logf("warn", t.Name, "Annonce \"stopped\" échouée : %v", err)
		} else {
			e.logf("info", t.Name, "Arrêté proprement (%s annoncés)", humanBytes(st.Uploaded))
		}
	}
	e.mu.Lock()
	t.active = false
	t.cancel = nil
	t.NextAnn = time.Time{}
	e.reconcile()
	e.dirty = true
	e.mu.Unlock()
	e.notify()
}

// peersWeight: torrents with more leechers (relative
// to seeders) receive more of the global bandwidth; no leecher = no upload.
func peersWeight(seeders, leechers int) float64 {
	total := seeders + leechers
	if seeders == 0 || total == 0 || leechers == 0 {
		return 0
	}
	r := float64(leechers) / float64(total)
	return r * 100 * r * float64(leechers)
}

func (e *Engine) refreshRate() {
	lo := float64(e.cfg.MinUploadRate) * 1000
	hi := float64(e.cfg.MaxUploadRate) * 1000
	e.globalRate = lo
	if hi > lo {
		e.globalRate = lo + rand.Float64()*(hi-lo)
	}
	e.nextRefresh = time.Now().Add(time.Duration(e.cfg.SpeedRefreshMinutes) * time.Minute)
}

func (e *Engine) bandwidthTick(now time.Time, dt float64) {
	e.mu.Lock()
	if dt > 5 { // machine en veille : on ne rattrape pas
		dt = 1
	}
	if e.running && now.After(e.nextRefresh) {
		e.refreshRate()
		e.logf("info", "", "Nouveau débit global : %s/s", humanBytes(int64(e.globalRate)))
	}
	total := 0.0
	for _, t := range e.torrents {
		if t.active && t.Status == StSeeding {
			total += t.weight
		}
	}
	var sum float64
	for _, t := range e.torrents {
		if !(t.active && t.Status == StSeeding) || total == 0 || !e.running {
			t.Speed = 0
			continue
		}
		t.Speed = e.globalRate * t.weight / total
		inc := t.Speed * dt
		t.SessionUp += inc
		t.TotalUp += inc
		e.sessionUp += inc
		sum += t.Speed
		if e.cfg.UploadRatioTarget > 0 && t.Size > 0 && t.SessionUp >= e.cfg.UploadRatioTarget*float64(t.Size) && !t.Done {
			t.Done = true
			e.logf("ok", t.Name, "Ratio cible %s atteint", strconv.FormatFloat(e.cfg.UploadRatioTarget, 'f', -1, 64))
			e.stopLocked(t)
			e.dirty = true
		}
	}
	if len(e.history) == 0 || now.Unix()-e.history[len(e.history)-1].T >= 5 {
		e.history = append(e.history, Sample{T: now.Unix(), Speed: sum})
		if len(e.history) > 720 { // 1 h à 5 s
			e.history = e.history[len(e.history)-720:]
		}
	}
	e.mu.Unlock()
}

// ---------- torrents ----------

func (e *Engine) sortedLocked() []*tState {
	list := make([]*tState, 0, len(e.torrents))
	for _, t := range e.torrents {
		list = append(list, t)
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].AddedAt.Equal(list[j].AddedAt) {
			return list[i].AddedAt.Before(list[j].AddedAt)
		}
		return list[i].HexHash < list[j].HexHash
	})
	return list
}

// scanTorrents syncs the in-memory list with data/torrents/*.torrent.
func (e *Engine) scanTorrents(initial bool) {
	entries, err := os.ReadDir(e.torrentsDir)
	if err != nil {
		return
	}
	seenFiles := map[string]bool{}
	type found struct {
		path string
		tor  *torrent.Torrent
		mod  time.Time
	}
	var news []found
	e.mu.Lock()
	known := map[string]string{}
	for h, t := range e.torrents {
		known[t.File] = h
	}
	e.mu.Unlock()
	for _, en := range entries {
		if en.IsDir() || !strings.HasSuffix(strings.ToLower(en.Name()), ".torrent") {
			continue
		}
		p := filepath.Join(e.torrentsDir, en.Name())
		seenFiles[p] = true
		if _, ok := known[p]; ok {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		tor, err := torrent.Parse(data)
		if err != nil {
			e.mu.Lock()
			e.logf("error", en.Name(), "Fichier ignoré : %v", err)
			e.mu.Unlock()
			_ = os.Rename(p, p+".invalide")
			continue
		}
		info, _ := en.Info()
		mod := time.Now()
		if info != nil {
			mod = info.ModTime()
		}
		news = append(news, found{p, tor, mod})
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, f := range news {
		h := f.tor.HexHash()
		if old, dup := e.torrents[h]; dup {
			if old.File != f.path {
				e.logf("warn", f.tor.Name, "Doublon ignoré (%s)", filepath.Base(f.path))
				_ = os.Remove(f.path)
			}
			continue
		}
		t := &tState{
			hash: f.tor.InfoHash, HexHash: h, Name: f.tor.Name, Size: f.tor.Size,
			Trackers: f.tor.Trackers, Private: f.tor.Private, File: f.path, AddedAt: f.mod, Status: StIdle,
		}
		e.torrents[h] = t
		if !initial {
			e.logf("info", t.Name, "Torrent ajouté (%s, %d tracker(s))", humanBytes(t.Size), len(t.Trackers))
		}
	}
	for h, t := range e.torrents {
		if !seenFiles[t.File] {
			delete(e.torrents, h)
			e.stopLocked(t)
			e.logf("info", t.Name, "Torrent retiré")
		}
	}
	if len(news) > 0 || initial {
		e.reconcile()
	}
}

// AddTorrent saves an uploaded .torrent into the watched folder.
func (e *Engine) AddTorrent(filename string, data []byte) (string, error) {
	tor, err := torrent.Parse(data)
	if err != nil {
		return "", err
	}
	e.mu.Lock()
	_, dup := e.torrents[tor.HexHash()]
	e.mu.Unlock()
	if dup {
		return "", fmt.Errorf("%s est déjà présent", tor.Name)
	}
	base := sanitize(strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename)))
	if base == "" {
		base = tor.HexHash()
	}
	p := filepath.Join(e.torrentsDir, base+".torrent")
	if _, err := os.Stat(p); err == nil {
		p = filepath.Join(e.torrentsDir, base+"-"+tor.HexHash()[:8]+".torrent")
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return "", err
	}
	e.scanTorrents(false)
	return tor.Name, nil
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '/' || r == '\\' || r == 0 || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if len(out) > 150 {
		out = out[:150]
	}
	return strings.Trim(out, ".")
}

// Action applies a per-torrent action: pause, resume, remove, announce, reset.
func (e *Engine) Action(hash, action string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.torrents[strings.ToLower(hash)]
	if !ok {
		return errors.New("torrent introuvable")
	}
	switch action {
	case "pause":
		t.Paused = true
	case "resume":
		t.Paused = false
	case "reset": // relancer un torrent terminé / en erreur / sans pairs
		t.Done, t.Errored, t.NoPeers, t.Failures, t.LastError = false, false, false, 0, ""
		e.logf("info", t.Name, "Torrent réactivé")
	case "announce":
		if !t.active || t.force == nil {
			return errors.New("le torrent n'est pas actif")
		}
		select {
		case t.force <- struct{}{}:
		default:
		}
		return nil
	case "remove":
		if err := os.Remove(t.File); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		delete(e.torrents, t.HexHash)
		e.stopLocked(t)
		e.logf("info", t.Name, "Torrent supprimé")
	default:
		return errors.New("action inconnue")
	}
	e.dirty = true
	e.reconcile()
	return nil
}

// ---------- snapshot for the UI ----------

// TorrentView is the JSON shape of one torrent.
type TorrentView struct {
	Hash      string   `json:"hash"`
	Name      string   `json:"name"`
	Size      int64    `json:"size"`
	Status    string   `json:"status"`
	Private   bool     `json:"private"`
	Tracker   string   `json:"tracker"`
	Trackers  []string `json:"trackers"`
	Seeders   int      `json:"seeders"`
	Leechers  int      `json:"leechers"`
	Speed     float64  `json:"speed"`
	SessionUp int64    `json:"sessionUp"`
	TotalUp   int64    `json:"totalUp"`
	Ratio     float64  `json:"ratio"`
	Interval  int      `json:"interval"`
	LastAnn   int64    `json:"lastAnnounce"`
	NextAnn   int64    `json:"nextAnnounce"`
	LastError string   `json:"lastError,omitempty"`
	Warning   string   `json:"warning,omitempty"`
	Failures  int      `json:"failures"`
	AddedAt   int64    `json:"addedAt"`
	Paused    bool     `json:"paused"`
	Share     float64  `json:"share"` // part de la bande passante (0-1)
}

// Snapshot is the full live state pushed to the UI.
type Snapshot struct {
	Running     bool          `json:"running"`
	StartedAt   int64         `json:"startedAt"`
	Now         int64         `json:"now"`
	GlobalRate  float64       `json:"globalRate"`
	Speed       float64       `json:"speed"`
	SessionUp   int64         `json:"sessionUp"`
	TotalUp     int64         `json:"totalUp"`
	Port        int           `json:"port"`
	PortOpen    bool          `json:"portOpen"`
	Client      string        `json:"client"`
	Active      int           `json:"active"`
	Torrents    []TorrentView `json:"torrents"`
	NextRefresh int64         `json:"nextRefresh"`
}

// Snapshot returns the current state.
func (e *Engine) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := Snapshot{
		Running: e.running, Now: time.Now().Unix(), GlobalRate: e.globalRate,
		SessionUp: int64(e.sessionUp), Port: e.cfg.Port, PortOpen: e.listener != nil,
		Client: e.cfg.Client, Torrents: []TorrentView{},
	}
	if e.running {
		s.StartedAt = e.startedAt.Unix()
		s.NextRefresh = e.nextRefresh.Unix()
	}
	for _, t := range e.sortedLocked() {
		v := TorrentView{
			Hash: t.HexHash, Name: t.Name, Size: t.Size, Status: t.Status, Private: t.Private,
			Tracker: hostOf(t.Trackers[t.TrackerIdx%len(t.Trackers)]), Trackers: t.Trackers,
			Seeders: t.Seeders, Leechers: t.Leechers, Speed: t.Speed,
			SessionUp: int64(t.SessionUp), TotalUp: int64(t.TotalUp), Interval: t.Interval,
			LastError: t.LastError, Warning: t.Warning, Failures: t.Failures, AddedAt: t.AddedAt.Unix(), Paused: t.Paused,
		}
		if t.Size > 0 {
			v.Ratio = math.Round(t.SessionUp/float64(t.Size)*1000) / 1000
		}
		if !t.LastAnn.IsZero() {
			v.LastAnn = t.LastAnn.Unix()
		}
		if !t.NextAnn.IsZero() {
			v.NextAnn = t.NextAnn.Unix()
		}
		if e.globalRate > 0 {
			v.Share = t.Speed / e.globalRate
		}
		if t.active && t.Status != StStopping {
			s.Active++
		}
		s.Speed += t.Speed
		s.TotalUp += int64(t.TotalUp)
		s.Torrents = append(s.Torrents, v)
	}
	return s
}

// History returns the speed samples (last hour).
func (e *Engine) History() []Sample {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Sample(nil), e.history...)
}

// Events returns the activity log (most recent last).
func (e *Engine) Events() []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Event(nil), e.events...)
}

// Subscribe returns a channel signalled on every state change (≈1/s).
func (e *Engine) Subscribe() (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	e.mu.Lock()
	e.subs[ch] = struct{}{}
	e.mu.Unlock()
	return ch, func() {
		e.mu.Lock()
		delete(e.subs, ch)
		e.mu.Unlock()
	}
}

func (e *Engine) notify() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for ch := range e.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// logf must be called with e.mu held.
func (e *Engine) logf(level, torrentName, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	e.eventSeq++
	e.events = append(e.events, Event{ID: e.eventSeq, T: time.Now().Unix(), Level: level, Torrent: torrentName, Msg: msg})
	if len(e.events) > 300 {
		e.events = e.events[len(e.events)-300:]
	}
	if torrentName != "" {
		log.Printf("[%s] %s — %s", level, torrentName, msg)
	} else {
		log.Printf("[%s] %s", level, msg)
	}
}

// ---------- persistence ----------

func (e *Engine) loadState() persisted {
	var p persisted
	data, err := os.ReadFile(filepath.Join(e.dataDir, "state.json"))
	if err == nil {
		_ = json.Unmarshal(data, &p)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for h, t := range e.torrents {
		t.TotalUp = p.TotalUp[h]
		t.Paused = p.Paused[h]
		t.Done = p.Done[h]
		if a, ok := p.Added[h]; ok && a > 0 {
			t.AddedAt = time.Unix(a, 0)
		}
	}
	e.reconcile()
	return p
}

func (e *Engine) saveState() {
	e.mu.Lock()
	p := persisted{TotalUp: map[string]float64{}, Paused: map[string]bool{}, Done: map[string]bool{}, Added: map[string]int64{}, Running: e.running}
	for h, t := range e.torrents {
		p.TotalUp[h] = math.Round(t.TotalUp)
		if t.Paused {
			p.Paused[h] = true
		}
		if t.Done {
			p.Done[h] = true
		}
		p.Added[h] = t.AddedAt.Unix()
	}
	e.dirty = false
	e.mu.Unlock()
	if err := writeJSON(filepath.Join(e.dataDir, "state.json"), p); err != nil {
		log.Printf("sauvegarde de l'état impossible: %v", err)
	}
}

func (e *Engine) shutdown() {
	e.mu.Lock()
	wasRunning := e.running
	e.mu.Unlock()
	e.Stop()
	e.waitIdle(12 * time.Second)
	e.mu.Lock()
	e.running = wasRunning // reprise automatique au prochain lancement
	e.mu.Unlock()
	e.saveState()
}

// ---------- helpers ----------

func hostOf(u string) string {
	s := u
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?"); i >= 0 {
		s = s[:i]
	}
	return s
}

func humanBytes(b int64) string {
	const unit = 1000
	if b < unit {
		return fmt.Sprintf("%d o", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return strings.Replace(fmt.Sprintf("%.1f %co", float64(b)/float64(div), "kMGTP"[exp]), ".", ",", 1)
}

// naturalLess sorts "qbittorrent-4.10" after "qbittorrent-4.9".
func naturalLess(a, b string) bool {
	for a != "" && b != "" {
		da, db := isDigit(a[0]), isDigit(b[0])
		if da && db {
			i, j := 0, 0
			for i < len(a) && isDigit(a[i]) {
				i++
			}
			for j < len(b) && isDigit(b[j]) {
				j++
			}
			na, _ := strconv.Atoi(a[:i])
			nb, _ := strconv.Atoi(b[:j])
			if na != nb {
				return na < nb
			}
			a, b = a[i:], b[j:]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// HexToHash is exported for tests.
func HexToHash(s string) ([20]byte, error) {
	var h [20]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 20 {
		return h, errors.New("hash invalide")
	}
	copy(h[:], b)
	return h, nil
}
