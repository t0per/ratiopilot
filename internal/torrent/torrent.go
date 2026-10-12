// Rôle : extrait d'un .torrent l'info_hash, la taille et la liste des trackers.

// Package torrent reads .torrent metainfo files.
package torrent

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"

	"ratiopilot/internal/bencode"
)

// Torrent holds the fields RatioPilot needs from a .torrent file.
type Torrent struct {
	Name     string
	InfoHash [20]byte
	Size     int64
	Trackers []string // announce URLs, tier order flattened, de-duplicated
	Private  bool
}

// HexHash returns the info hash as lowercase hex.
func (t *Torrent) HexHash() string { return hex.EncodeToString(t.InfoHash[:]) }

// Parse decodes a .torrent file.
func Parse(data []byte) (*Torrent, error) {
	raw, err := bencode.RawValue(data, "info")
	if err != nil {
		return nil, errors.New("fichier .torrent invalide (pas de section info)")
	}
	top, err := bencode.Decode(data)
	if err != nil {
		return nil, err
	}
	root, ok := top.(map[string]any)
	if !ok {
		return nil, errors.New("fichier .torrent invalide")
	}
	info, ok := root["info"].(map[string]any)
	if !ok {
		return nil, errors.New("fichier .torrent invalide (info)")
	}
	t := &Torrent{InfoHash: sha1.Sum(raw)}
	t.Name, _ = info["name"].(string)
	if p, ok := info["private"].(int64); ok && p == 1 {
		t.Private = true
	}
	if l, ok := info["length"].(int64); ok {
		t.Size = l
	} else if files, ok := info["files"].([]any); ok {
		for _, f := range files {
			if fm, ok := f.(map[string]any); ok {
				if l, ok := fm["length"].(int64); ok {
					t.Size += l
				}
			}
		}
	}
	seen := map[string]bool{}
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			return
		}
		pu, err := url.Parse(u)
		if err != nil {
			return
		}
		switch pu.Scheme {
		case "http", "https", "udp":
			seen[u] = true
			t.Trackers = append(t.Trackers, u)
		}
	}
	if al, ok := root["announce-list"].([]any); ok {
		for _, tier := range al {
			if tl, ok := tier.([]any); ok {
				for _, u := range tl {
					if s, ok := u.(string); ok {
						add(s)
					}
				}
			}
		}
	}
	if a, ok := root["announce"].(string); ok {
		add(a)
	}
	if len(t.Trackers) == 0 {
		return nil, errors.New("aucun tracker HTTP/HTTPS/UDP dans ce torrent")
	}
	if t.Name == "" {
		t.Name = t.HexHash()
	}
	return t, nil
}
