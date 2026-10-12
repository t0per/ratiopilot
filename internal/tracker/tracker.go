// Rôle : envoi des annonces aux trackers HTTP(S) et UDP (BEP 15) et lecture des réponses.

// Package tracker sends announces to HTTP(S) and UDP trackers.
package tracker

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ratiopilot/internal/bencode"
	"ratiopilot/internal/emu"
)

// Request describes one announce.
type Request struct {
	URL      string
	InfoHash [20]byte
	Event    emu.Event
	Stats    emu.Stats
	Port     int
	Client   *emu.Client
}

// Response is the useful part of a tracker answer.
type Response struct {
	Interval int // seconds
	Seeders  int // complete (excluding us)
	Leechers int // incomplete
	Warning  string
}

var httpClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment, // HTTP_PROXY / HTTPS_PROXY (ex: Gluetun)
		DisableCompression:  true,                      // we send the client's own Accept-Encoding
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     60 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("trop de redirections")
		}
		return nil
	},
}

// Announce dispatches to the HTTP or UDP implementation.
func Announce(ctx context.Context, r Request) (*Response, error) {
	u, err := url.Parse(r.URL)
	if err != nil {
		return nil, err
	}
	var resp *Response
	switch u.Scheme {
	case "http", "https":
		resp, err = announceHTTP(ctx, r)
	case "udp":
		resp, err = announceUDP(ctx, u.Host, r)
	default:
		return nil, fmt.Errorf("protocole %q non supporté", u.Scheme)
	}
	if err != nil {
		return nil, err
	}
	if resp.Interval <= 0 {
		resp.Interval = 1800
	}
	if resp.Seeders > 0 {
		resp.Seeders-- // we count as one seeder
	}
	return resp, nil
}

func announceHTTP(ctx context.Context, r Request) (*Response, error) {
	q, err := r.Client.Query(r.InfoHash, r.Event, r.Stats, r.Port)
	if err != nil {
		return nil, err
	}
	sep := "?"
	if strings.Contains(r.URL, "?") {
		sep = "&"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL+sep+q, nil)
	if err != nil {
		return nil, err
	}
	for _, h := range r.Client.Headers() {
		if strings.EqualFold(h[0], "Connection") && strings.EqualFold(h[1], "close") {
			req.Close = true
		}
		req.Header.Set(h[0], h[1])
	}
	res, err := httpClient.Do(req)
	if err != nil {
		// never echo the URL: private trackers embed the passkey in it
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("connexion au tracker impossible: %v", err)
	}
	defer res.Body.Close()
	var body io.Reader = io.LimitReader(res.Body, 4<<20)
	if strings.Contains(strings.ToLower(res.Header.Get("Content-Encoding")), "gzip") {
		gz, err := gzip.NewReader(body)
		if err != nil {
			return nil, fmt.Errorf("réponse gzip invalide: %w", err)
		}
		defer gz.Close()
		body = gz
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("lecture réponse: %w", err)
	}
	v, err := bencode.Decode(data)
	if err != nil {
		if res.StatusCode >= 300 {
			return nil, fmt.Errorf("tracker HTTP %d", res.StatusCode)
		}
		return nil, fmt.Errorf("réponse tracker illisible: %w", err)
	}
	d, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("réponse tracker inattendue")
	}
	if fr, ok := d["failure reason"].(string); ok {
		return nil, fmt.Errorf("refus du tracker: %s", fr)
	}
	out := &Response{}
	out.Interval = int(asInt(d["interval"]))
	if mi := int(asInt(d["min interval"])); mi > out.Interval {
		out.Interval = mi
	}
	out.Seeders = int(asInt(d["complete"]))
	out.Leechers = int(asInt(d["incomplete"]))
	out.Warning, _ = d["warning message"].(string)
	return out, nil
}

func asInt(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	}
	return 0
}

// ---- UDP (BEP 15) ----

const udpProtocolID = 0x41727101980

func announceUDP(ctx context.Context, host string, r Request) (*Response, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", host)
	if err != nil {
		return nil, fmt.Errorf("connexion UDP impossible: %w", err)
	}
	defer conn.Close()

	// connect
	tx := randUint32()
	pkt := make([]byte, 16)
	binary.BigEndian.PutUint64(pkt[0:], udpProtocolID)
	binary.BigEndian.PutUint32(pkt[8:], 0)
	binary.BigEndian.PutUint32(pkt[12:], tx)
	resp, err := udpRoundTrip(ctx, conn, pkt, tx, 16)
	if err != nil {
		return nil, err
	}
	connID := binary.BigEndian.Uint64(resp[8:])

	// announce
	tx = randUint32()
	a := make([]byte, 98)
	binary.BigEndian.PutUint64(a[0:], connID)
	binary.BigEndian.PutUint32(a[8:], 1)
	binary.BigEndian.PutUint32(a[12:], tx)
	copy(a[16:36], r.InfoHash[:])
	copy(a[36:56], r.Client.PeerID(r.InfoHash, r.Event))
	binary.BigEndian.PutUint64(a[56:], uint64(r.Stats.Downloaded))
	binary.BigEndian.PutUint64(a[64:], uint64(r.Stats.Left))
	binary.BigEndian.PutUint64(a[72:], uint64(r.Stats.Uploaded))
	ev := uint32(0)
	switch r.Event {
	case emu.EventStarted:
		ev = 2
	case emu.EventStopped:
		ev = 3
	}
	binary.BigEndian.PutUint32(a[80:], ev)
	binary.BigEndian.PutUint32(a[84:], 0) // ip: default
	binary.BigEndian.PutUint32(a[88:], keyToUint32(r.Client.Key(r.InfoHash, r.Event)))
	binary.BigEndian.PutUint32(a[92:], uint32(int32(r.Client.Numwant(r.Event))))
	binary.BigEndian.PutUint16(a[96:], uint16(r.Port))
	resp, err = udpRoundTrip(ctx, conn, a, tx, 20)
	if err != nil {
		return nil, err
	}
	return &Response{
		Interval: int(binary.BigEndian.Uint32(resp[8:])),
		Leechers: int(binary.BigEndian.Uint32(resp[12:])),
		Seeders:  int(binary.BigEndian.Uint32(resp[16:])),
	}, nil
}

func udpRoundTrip(ctx context.Context, conn net.Conn, pkt []byte, tx uint32, minLen int) ([]byte, error) {
	buf := make([]byte, 2048)
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := conn.Write(pkt); err != nil {
			return nil, err
		}
		deadline := time.Now().Add(time.Duration(5*(attempt+1)) * time.Second)
		if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
			deadline = dl
		}
		_ = conn.SetReadDeadline(deadline)
		n, err := conn.Read(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() && ctx.Err() == nil {
				continue
			}
			return nil, fmt.Errorf("UDP: %w", err)
		}
		if n < 8 || binary.BigEndian.Uint32(buf[4:]) != tx {
			continue
		}
		if binary.BigEndian.Uint32(buf[0:]) == 3 {
			return nil, fmt.Errorf("refus du tracker: %s", strings.TrimSpace(string(buf[8:n])))
		}
		if n < minLen {
			return nil, errors.New("UDP: réponse trop courte")
		}
		return buf[:n], nil
	}
	return nil, errors.New("UDP: pas de réponse du tracker")
}

func keyToUint32(k string) uint32 {
	if k == "" {
		return randUint32()
	}
	if v, err := strconv.ParseUint(k, 16, 32); err == nil {
		return uint32(v)
	}
	return crc32.ChecksumIEEE([]byte(k))
}

func randUint32() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}
