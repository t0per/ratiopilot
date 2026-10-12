package emu

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAllBundledClientsLoad(t *testing.T) {
	files, _ := filepath.Glob("../../clients/*.client")
	if len(files) < 90 {
		t.Fatalf("expected ~95 client files, got %d", len(files))
	}
	var h [20]byte
	for i := range h {
		h[i] = byte(i * 13)
	}
	for _, f := range files {
		data, _ := os.ReadFile(f)
		c, err := Load(filepath.Base(f), data)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		for _, ev := range []Event{EventStarted, EventNone, EventStopped} {
			q, err := c.Query(h, ev, Stats{Uploaded: 1234}, 51413)
			if err != nil {
				t.Errorf("%s: %v", f, err)
			}
			if len(c.PeerID(h, ev)) != 20 {
				t.Errorf("%s: bad peer id length", f)
			}
			if ev == EventNone && strings.Contains(q, "event=") {
				t.Errorf("%s: event in regular announce: %s", f, q)
			}
			if strings.Contains(q, "{") && c.peerIDEncode {
				t.Errorf("%s: leftover placeholder: %s", f, q)
			}
		}
	}
}

func TestQbitQuery(t *testing.T) {
	data, _ := os.ReadFile("../../clients/qbittorrent-5.2.4.client")
	c, err := Load("q", data)
	if err != nil {
		t.Fatal(err)
	}
	var h [20]byte
	h[0], h[1], h[2] = 0xab, 'A', ' '
	q, _ := c.Query(h, EventStarted, Stats{Uploaded: 42}, 6881)
	t.Log(q)
	if !strings.HasPrefix(q, "info_hash=%abA%20%00") {
		t.Fatalf("bad infohash encoding: %s", q)
	}
	if !strings.Contains(q, "&peer_id=-qB5240-") || !strings.Contains(q, "&uploaded=42&") || !strings.Contains(q, "&event=started&") {
		t.Fatalf("bad query: %s", q)
	}
}

func TestTransmissionChecksum(t *testing.T) {
	data, _ := os.ReadFile("../../clients/transmission-3.00.client")
	c, _ := Load("t", data)
	var h [20]byte
	id := string(c.PeerID(h, EventStarted))
	t.Log(id)
	if !strings.HasPrefix(id, "-TR3000-") || len(id) != 20 {
		t.Fatalf("bad transmission id %q", id)
	}
}
