// Package emu emulates real BitTorrent clients when talking to trackers.
// It reads the same ".client" JSON files as JOAL.
package emu

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event is a tracker announce event.
type Event string

const (
	EventNone    Event = ""
	EventStarted Event = "started"
	EventStopped Event = "stopped"
)

// ---- JSON model ----

type algoSpec struct {
	Type                string `json:"type"`
	Pattern             string `json:"pattern"`
	Length              int    `json:"length"`
	Prefix              string `json:"prefix"`
	CharactersPool      string `json:"charactersPool"`
	Base                int    `json:"base"`
	InclusiveLowerBound int64  `json:"inclusiveLowerBound"`
	InclusiveUpperBound int64  `json:"inclusiveUpperBound"`
}

type genSpec struct {
	Algorithm       algoSpec `json:"algorithm"`
	RefreshOn       string   `json:"refreshOn"`
	RefreshEvery    int      `json:"refreshEvery"`
	KeyCase         string   `json:"keyCase"`
	ShouldURLEncode bool     `json:"shouldUrlEncode"`
}

type header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type fileSpec struct {
	KeyGenerator    *genSpec `json:"keyGenerator"`
	PeerIDGenerator genSpec  `json:"peerIdGenerator"`
	URLEncoder      struct {
		EncodingExclusionPattern string `json:"encodingExclusionPattern"`
		EncodedHexCase           string `json:"encodedHexCase"`
	} `json:"urlEncoder"`
	Query          string   `json:"query"`
	Numwant        int      `json:"numwant"`
	NumwantOnStop  int      `json:"numwantOnStop"`
	RequestHeaders []header `json:"requestHeaders"`
}

// ---- algorithms ----

type algorithm func() []byte

func buildAlgorithm(a algoSpec) (algorithm, error) {
	const hexChars = "0123456789ABCDEF"
	switch a.Type {
	case "REGEX":
		p, err := CompilePattern(a.Pattern)
		if err != nil {
			return nil, err
		}
		return p.Generate, nil
	case "HASH", "HASH_NO_LEADING_ZERO":
		if a.Length <= 0 {
			return nil, fmt.Errorf("length invalide")
		}
		strip := a.Type == "HASH_NO_LEADING_ZERO"
		return func() []byte {
			b := make([]byte, a.Length)
			for i := range b {
				b[i] = hexChars[randInt(16)]
			}
			if strip {
				return []byte(strings.TrimLeft(string(b), "0"))
			}
			return b
		}, nil
	case "DIGIT_RANGE_TRANSFORMED_TO_HEX_WITHOUT_LEADING_ZEROES":
		if a.InclusiveUpperBound < a.InclusiveLowerBound {
			return nil, fmt.Errorf("bornes invalides")
		}
		return func() []byte {
			return []byte(strconv.FormatInt(randInt64Between(a.InclusiveLowerBound, a.InclusiveUpperBound), 16))
		}, nil
	case "RANDOM_POOL_WITH_CHECKSUM":
		if a.Prefix == "" || a.CharactersPool == "" || a.Base <= 0 || a.Base > len(a.CharactersPool) {
			return nil, fmt.Errorf("paramètres RANDOM_POOL_WITH_CHECKSUM invalides")
		}
		return func() []byte {
			suffixLen := 20 - len(a.Prefix)
			rnd := make([]byte, suffixLen-1)
			_, _ = rand.Read(rnd)
			buf := make([]byte, suffixLen)
			total := 0
			for i := 0; i < suffixLen-1; i++ {
				v := int(rnd[i]) % a.Base
				total += v
				buf[i] = a.CharactersPool[v]
			}
			v := 0
			if total%a.Base != 0 {
				v = a.Base - total%a.Base
			}
			buf[suffixLen-1] = a.CharactersPool[v]
			return append([]byte(a.Prefix), buf...)
		}, nil
	}
	return nil, fmt.Errorf("algorithme inconnu %q", a.Type)
}

// ---- refresh strategies (peer_id and key) ----

type generator struct {
	mu        sync.Mutex
	gen       func() string
	refreshOn string
	every     time.Duration
	value     string
	last      time.Time
	perTorr   map[[20]byte]string
}

func newGenerator(spec genSpec, gen func() string) (*generator, error) {
	g := &generator{gen: gen, refreshOn: spec.RefreshOn, every: time.Duration(spec.RefreshEvery) * time.Second, perTorr: map[[20]byte]string{}}
	switch spec.RefreshOn {
	case "NEVER", "ALWAYS", "TORRENT_VOLATILE", "TORRENT_PERSISTENT":
	case "TIMED", "TIMED_OR_AFTER_STARTED_ANNOUNCE":
		if spec.RefreshEvery < 1 {
			return nil, fmt.Errorf("refreshEvery doit être > 0")
		}
	default:
		return nil, fmt.Errorf("refreshOn inconnu %q", spec.RefreshOn)
	}
	return g, nil
}

func (g *generator) get(hash [20]byte, ev Event) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch g.refreshOn {
	case "NEVER":
		if g.value == "" {
			g.value = g.gen()
		}
		return g.value
	case "ALWAYS":
		return g.gen()
	case "TIMED":
		if g.last.IsZero() || time.Since(g.last) >= g.every {
			g.last, g.value = time.Now(), g.gen()
		}
		return g.value
	case "TIMED_OR_AFTER_STARTED_ANNOUNCE":
		if g.last.IsZero() || time.Since(g.last) >= g.every {
			g.last, g.value = time.Now(), g.gen()
		}
		v := g.value
		if ev == EventStarted {
			g.value = g.gen()
		}
		return v
	case "TORRENT_VOLATILE", "TORRENT_PERSISTENT":
		v, ok := g.perTorr[hash]
		if !ok {
			v = g.gen()
			g.perTorr[hash] = v
		}
		if g.refreshOn == "TORRENT_VOLATILE" && ev == EventStopped {
			delete(g.perTorr, hash)
		}
		return v
	}
	return g.gen()
}

// ---- url encoder ----

type urlEncoder struct {
	keep  [256]bool
	upper bool
}

func newURLEncoder(pattern, hexCase string) (*urlEncoder, error) {
	re, err := regexp.Compile("^" + pattern + "$")
	if err != nil {
		return nil, fmt.Errorf("encodingExclusionPattern: %w", err)
	}
	e := &urlEncoder{upper: strings.EqualFold(hexCase, "upper")}
	for i := 0; i < 128; i++ {
		e.keep[i] = re.MatchString(string(rune(i)))
	}
	return e, nil
}

func (e *urlEncoder) encode(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		if e.keep[c] {
			sb.WriteByte(c)
			continue
		}
		h := fmt.Sprintf("%%%02x", c)
		if e.upper {
			h = strings.ToUpper(h)
		}
		sb.WriteString(h)
	}
	return sb.String()
}

// ---- client ----

// Client is one emulated BitTorrent client.
type Client struct {
	Name          string
	query         string
	headers       []header
	numwant       int
	numwantOnStop int
	peerID        *generator
	peerIDEncode  bool
	key           *generator
	enc           *urlEncoder
}

var (
	reAmpDupes = regexp.MustCompile(`&{2,}`)
	reIPQuery  = regexp.MustCompile(`&*\w+=\{ip(?:v6)?\}`)
	reEventQ   = regexp.MustCompile(`&*\w+=\{event\}`)
	rePlace    = regexp.MustCompile(`\{.*?\}`)
)

// Load parses a .client file.
func Load(name string, data []byte) (*Client, error) {
	var s fileSpec
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s: JSON invalide: %w", name, err)
	}
	if strings.TrimSpace(s.Query) == "" {
		return nil, fmt.Errorf("%s: query vide", name)
	}
	enc, err := newURLEncoder(s.URLEncoder.EncodingExclusionPattern, s.URLEncoder.EncodedHexCase)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	c := &Client{
		Name:          name,
		query:         reAmpDupes.ReplaceAllString(s.Query, "&"),
		numwant:       s.Numwant,
		numwantOnStop: s.NumwantOnStop,
		peerIDEncode:  s.PeerIDGenerator.ShouldURLEncode,
		enc:           enc,
	}
	pAlgo, err := buildAlgorithm(s.PeerIDGenerator.Algorithm)
	if err != nil {
		return nil, fmt.Errorf("%s: peerId: %w", name, err)
	}
	if c.peerID, err = newGenerator(s.PeerIDGenerator, func() string { return string(pAlgo()) }); err != nil {
		return nil, fmt.Errorf("%s: peerId: %w", name, err)
	}
	if len(pAlgo()) != 20 {
		return nil, fmt.Errorf("%s: peer_id doit faire 20 octets", name)
	}
	if s.KeyGenerator != nil {
		kAlgo, err := buildAlgorithm(s.KeyGenerator.Algorithm)
		if err != nil {
			return nil, fmt.Errorf("%s: key: %w", name, err)
		}
		kc := s.KeyGenerator.KeyCase
		if c.key, err = newGenerator(*s.KeyGenerator, func() string {
			k := string(kAlgo())
			switch strings.ToLower(kc) {
			case "upper":
				return strings.ToUpper(k)
			case "lower":
				return strings.ToLower(k)
			}
			return k
		}); err != nil {
			return nil, fmt.Errorf("%s: key: %w", name, err)
		}
	} else if strings.Contains(s.Query, "{key}") {
		return nil, fmt.Errorf("%s: la query contient {key} sans keyGenerator", name)
	}
	for _, h := range s.RequestHeaders {
		v := strings.NewReplacer("{java}", "17.0.12", "{os}", "Linux", "{locale}", "en-US").Replace(h.Value)
		if rePlace.MatchString(v) {
			return nil, fmt.Errorf("%s: placeholder inconnu dans l'en-tête %s", name, h.Name)
		}
		c.headers = append(c.headers, header{h.Name, v})
	}
	return c, nil
}

// Stats is what we report to the tracker.
type Stats struct {
	Uploaded, Downloaded, Left int64
}

// PeerID returns the raw peer id (20 bytes) for a torrent/event.
func (c *Client) PeerID(hash [20]byte, ev Event) []byte {
	return []byte(c.peerID.get(hash, ev))
}

// Key returns the key value (used by UDP announces too).
func (c *Client) Key(hash [20]byte, ev Event) string {
	if c.key == nil {
		return ""
	}
	return c.key.get(hash, ev)
}

// Numwant for the given event.
func (c *Client) Numwant(ev Event) int {
	if ev == EventStopped {
		return c.numwantOnStop
	}
	return c.numwant
}

// Headers returns the HTTP headers to send.
func (c *Client) Headers() [][2]string {
	out := make([][2]string, len(c.headers))
	for i, h := range c.headers {
		out[i] = [2]string{h.Name, h.Value}
	}
	return out
}

// Query builds the announce query string exactly like the emulated client.
func (c *Client) Query(hash [20]byte, ev Event, st Stats, port int) (string, error) {
	q := c.query
	r := strings.NewReplacer(
		"{infohash}", c.enc.encode(hash[:]),
		"{uploaded}", strconv.FormatInt(st.Uploaded, 10),
		"{downloaded}", strconv.FormatInt(st.Downloaded, 10),
		"{left}", strconv.FormatInt(st.Left, 10),
		"{port}", strconv.Itoa(port),
		"{numwant}", strconv.Itoa(c.Numwant(ev)),
	)
	q = r.Replace(q)
	q = reIPQuery.ReplaceAllString(q, "")
	if ev == EventNone {
		q = reEventQ.ReplaceAllString(q, "")
	} else {
		q = strings.ReplaceAll(q, "{event}", string(ev))
	}
	if strings.Contains(q, "{key}") {
		q = strings.ReplaceAll(q, "{key}", c.enc.encode([]byte(c.Key(hash, ev))))
	}
	// peer id substituted last: a raw (non-encoded) peer id may contain braces
	if m := rePlace.FindString(strings.ReplaceAll(q, "{peerid}", "")); m != "" {
		return "", fmt.Errorf("placeholder %s non reconnu", m)
	}
	pid := c.PeerID(hash, ev)
	pidStr := string(pid)
	if c.peerIDEncode {
		pidStr = c.enc.encode(pid)
	}
	q = strings.ReplaceAll(q, "{peerid}", pidStr)
	q = reAmpDupes.ReplaceAllString(q, "&")
	return strings.Trim(q, "&"), nil
}
