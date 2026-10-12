// testtracker — tracker BitTorrent factice (HTTP + UDP) pour tester RatioPilot
// sans toucher à un vrai tracker. Il journalise chaque annonce et peut générer
// des fichiers .torrent de test pointant vers lui-même.
//
//	GET /                 tableau des annonces reçues
//	GET /announce         annonce HTTP (BEP 3)
//	GET /make?name=X&size=Mo   télécharge un .torrent de test
//	UDP :6969             annonce UDP (BEP 15)
package main

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"ratiopilot/internal/bencode"
)

type announce struct {
	At        time.Time
	Proto     string
	Hash      string
	Event     string
	Uploaded  int64
	PeerID    string
	UserAgent string
	Key       string
	Port      string
	Query     string
}

type swarm struct {
	seeders, leechers int
	lastUploaded      int64
	lastAt            time.Time
	speed             float64
	announces         int
}

var (
	mu       sync.Mutex
	log_     []announce
	swarms   = map[string]*swarm{}
	interval = envInt("TT_INTERVAL", 60)
	selfURL  = env("TT_ANNOUNCE_URL", "http://localhost:6969/announce")
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envInt(k string, d int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return n
	}
	return d
}

func getSwarm(hash string) *swarm {
	s, ok := swarms[hash]
	if !ok {
		// quelques pairs fictifs pour que RatioPilot ait des leechers à "servir"
		s = &swarm{seeders: 3 + mrand.IntN(20), leechers: 2 + mrand.IntN(30)}
		swarms[hash] = s
	}
	return s
}

func record(a announce) *swarm {
	mu.Lock()
	defer mu.Unlock()
	s := getSwarm(a.Hash)
	if !s.lastAt.IsZero() && a.Event == "" {
		dt := a.At.Sub(s.lastAt).Seconds()
		if dt > 0 {
			s.speed = float64(a.Uploaded-s.lastUploaded) / dt
		}
	}
	s.lastUploaded, s.lastAt = a.Uploaded, a.At
	s.announces++
	log_ = append(log_, a)
	if len(log_) > 500 {
		log_ = log_[len(log_)-500:]
	}
	log.Printf("[%s] %s event=%q uploaded=%d peer_id=%q ua=%q", a.Proto, a.Hash[:8], a.Event, a.Uploaded, a.PeerID, a.UserAgent)
	return s
}

func httpAnnounce(w http.ResponseWriter, r *http.Request) {
	// parse the raw query so binary info_hash/peer_id survive
	vals, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(vals.Get("info_hash")) != 20 {
		_, _ = w.Write(bencode.Encode(map[string]any{"failure reason": "info_hash invalide"}))
		return
	}
	up, _ := strconv.ParseInt(vals.Get("uploaded"), 10, 64)
	s := record(announce{
		At: time.Now(), Proto: "HTTP", Hash: hex.EncodeToString([]byte(vals.Get("info_hash"))),
		Event: vals.Get("event"), Uploaded: up, PeerID: vals.Get("peer_id"),
		UserAgent: r.UserAgent(), Key: vals.Get("key"), Port: vals.Get("port"), Query: r.URL.RawQuery,
	})
	mu.Lock()
	resp := map[string]any{"interval": interval, "complete": s.seeders + 1, "incomplete": s.leechers, "peers": ""}
	mu.Unlock()
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write(bencode.Encode(resp))
}

func udpServer(addr string) {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		log.Printf("UDP désactivé: %v", err)
		return
	}
	conns := map[uint64]time.Time{}
	buf := make([]byte, 2048)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil || n < 16 {
			continue
		}
		action := binary.BigEndian.Uint32(buf[8:])
		tx := buf[12:16]
		switch {
		case action == 0 && binary.BigEndian.Uint64(buf[0:]) == 0x41727101980:
			var id [8]byte
			_, _ = rand.Read(id[:])
			conns[binary.BigEndian.Uint64(id[:])] = time.Now()
			out := make([]byte, 16)
			copy(out[4:8], tx)
			copy(out[8:], id[:])
			_, _ = pc.WriteTo(out, from)
		case action == 1 && n >= 98:
			if _, ok := conns[binary.BigEndian.Uint64(buf[0:])]; !ok {
				continue
			}
			ev := map[uint32]string{0: "", 1: "completed", 2: "started", 3: "stopped"}[binary.BigEndian.Uint32(buf[80:])]
			s := record(announce{
				At: time.Now(), Proto: "UDP", Hash: hex.EncodeToString(buf[16:36]), Event: ev,
				Uploaded: int64(binary.BigEndian.Uint64(buf[72:])), PeerID: string(buf[36:56]),
				Key: fmt.Sprintf("%08x", binary.BigEndian.Uint32(buf[88:])), Port: strconv.Itoa(int(binary.BigEndian.Uint16(buf[96:]))),
			})
			out := make([]byte, 20)
			binary.BigEndian.PutUint32(out[0:], 1)
			copy(out[4:8], tx)
			mu.Lock()
			binary.BigEndian.PutUint32(out[8:], uint32(interval))
			binary.BigEndian.PutUint32(out[12:], uint32(s.leechers))
			binary.BigEndian.PutUint32(out[16:], uint32(s.seeders+1))
			mu.Unlock()
			_, _ = pc.WriteTo(out, from)
		}
	}
}

// buildTorrent creates a fake single-file .torrent announcing to announceURL.
func buildTorrent(name string, sizeMB int, announceURL string, private bool) []byte {
	pieceLen := int64(1 << 20)
	size := int64(sizeMB) << 20
	nPieces := (size + pieceLen - 1) / pieceLen
	pieces := make([]byte, 20*nPieces)
	_, _ = rand.Read(pieces)
	salt := make([]byte, 8)
	_, _ = rand.Read(salt)
	info := map[string]any{"name": name, "length": size, "piece length": pieceLen, "pieces": string(pieces), "x-salt": hex.EncodeToString(salt)}
	if private {
		info["private"] = int64(1)
	}
	meta := map[string]any{"announce": announceURL, "info": info, "created by": "testtracker", "creation date": time.Now().Unix()}
	log.Printf("torrent de test créé: %s (%d Mo) infohash=%x", name, sizeMB, sha1.Sum(bencode.Encode(info)))
	return bencode.Encode(meta)
}

func makeTorrent(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "test-" + strconv.Itoa(mrand.IntN(100000))
	}
	sizeMB, _ := strconv.Atoi(r.URL.Query().Get("size"))
	if sizeMB <= 0 {
		sizeMB = 700
	}
	announceURL := r.URL.Query().Get("announce")
	if announceURL == "" {
		announceURL = selfURL
	}
	data := buildTorrent(name, sizeMB, announceURL, r.URL.Query().Get("private") == "1")
	w.Header().Set("Content-Type", "application/x-bittorrent")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.torrent"`, name))
	_, _ = w.Write(data)
}

// seedDir writes TT_SEED_COUNT test torrents into TT_SEED_DIR (once).
func seedDir() {
	dir := os.Getenv("TT_SEED_DIR")
	n := envInt("TT_SEED_COUNT", 0)
	if dir == "" || n <= 0 {
		return
	}
	marker := filepath.Join(dir, ".testtracker-seeded")
	if _, err := os.Stat(marker); err == nil {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("TT_SEED_DIR: %v", err)
		return
	}
	names := []string{"Debian-13-amd64-netinst", "Fedora-Workstation-42", "Ubuntu-24.04-desktop", "Linux-Mint-22-cinnamon", "Arch-Linux-2026.10", "openSUSE-Tumbleweed", "Proxmox-VE-9", "TrueNAS-Scale-25"}
	udpURL := "udp://" + mustHost(selfURL) + "/announce"
	for i := 0; i < n; i++ {
		name := names[i%len(names)]
		if i >= len(names) {
			name += "-" + strconv.Itoa(i)
		}
		announceURL := selfURL
		if i%3 == 2 {
			announceURL = udpURL // un torrent sur trois en UDP
		}
		data := buildTorrent(name, 500+mrand.IntN(6000), announceURL, i%2 == 0)
		if err := os.WriteFile(filepath.Join(dir, name+".torrent"), data, 0o644); err != nil {
			log.Printf("TT_SEED_DIR: %v", err)
			return
		}
	}
	_ = os.WriteFile(marker, []byte("ok"), 0o644)
	log.Printf("%d torrents de test déposés dans %s", n, dir)
}

func mustHost(u string) string {
	pu, err := url.Parse(u)
	if err != nil {
		return "localhost:6969"
	}
	return pu.Host
}

var page = template.Must(template.New("p").Funcs(template.FuncMap{
	"mb":  func(b int64) string { return fmt.Sprintf("%.2f Mo", float64(b)/1e6) },
	"kbs": func(b float64) string { return fmt.Sprintf("%.1f kB/s", b/1000) },
	"t":   func(t time.Time) string { return t.Format("15:04:05") },
}).Parse(`<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="5">
<title>Tracker de test</title>
<style>body{font:14px system-ui;margin:24px;background:#111;color:#ddd}table{border-collapse:collapse;width:100%;margin-bottom:24px}
td,th{border-bottom:1px solid #333;padding:4px 8px;text-align:left;font-variant-numeric:tabular-nums}th{color:#999}code{color:#8fd}a{color:#8cf}</style>
<h1>Tracker de test</h1>
<p>Intervalle d'annonce : {{.Interval}} s · <a href="/make?size=700">Générer un .torrent de test</a> (paramètres : name, size en Mo, private=1)</p>
<h2>Torrents</h2><table><tr><th>Info hash</th><th>Annonces</th><th>Uploaded déclaré</th><th>Débit déduit</th><th>Seeders/Leechers simulés</th></tr>
{{range .Swarms}}<tr><td><code>{{.Hash}}</code></td><td>{{.S.Announces}}</td><td>{{mb .S.LastUploaded}}</td><td>{{kbs .S.Speed}}</td><td>{{.S.Seeders}} / {{.S.Leechers}}</td></tr>{{end}}</table>
<h2>Dernières annonces</h2><table><tr><th>Heure</th><th>Proto</th><th>Hash</th><th>Event</th><th>Uploaded</th><th>peer_id</th><th>key</th><th>port</th><th>User-Agent</th></tr>
{{range .Log}}<tr><td>{{t .At}}</td><td>{{.Proto}}</td><td><code>{{printf "%.8s" .Hash}}</code></td><td>{{.Event}}</td><td>{{mb .Uploaded}}</td><td><code>{{printf "%q" .PeerID}}</code></td><td>{{.Key}}</td><td>{{.Port}}</td><td>{{.UserAgent}}</td></tr>{{end}}</table>`))

type swarmRow struct {
	Hash string
	S    struct {
		Announces         int
		LastUploaded      int64
		Speed             float64
		Seeders, Leechers int
	}
}

func index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	mu.Lock()
	var rows []swarmRow
	for h, s := range swarms {
		var row swarmRow
		row.Hash = h
		row.S.Announces, row.S.LastUploaded, row.S.Speed, row.S.Seeders, row.S.Leechers = s.announces, s.lastUploaded, s.speed, s.seeders, s.leechers
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Hash < rows[j].Hash })
	last := make([]announce, 0, 50)
	for i := len(log_) - 1; i >= 0 && len(last) < 50; i-- {
		last = append(last, log_[i])
	}
	mu.Unlock()
	_ = page.Execute(w, map[string]any{"Interval": interval, "Swarms": rows, "Log": last})
}

func main() {
	addr := env("TT_LISTEN", ":6969")
	go udpServer(addr)
	seedDir()
	http.HandleFunc("/announce", httpAnnounce)
	http.HandleFunc("/make", makeTorrent)
	http.HandleFunc("/", index)
	log.Printf("tracker de test sur %s (HTTP + UDP), intervalle %ds, URL annoncée %s", addr, interval, selfURL)
	log.Fatal(http.ListenAndServe(addr, nil))
}
