// Rôle : lecture/écriture du format bencode (fichiers .torrent et réponses des trackers).

// Package bencode implements a minimal bencode decoder/encoder (BEP 3).
package bencode

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

// Decode parses one bencoded value from data.
// Strings are returned as string (raw bytes), integers as int64,
// lists as []any and dictionaries as map[string]any.
func Decode(data []byte) (any, error) {
	d := &decoder{data: data}
	v, err := d.value()
	if err != nil {
		return nil, err
	}
	return v, nil
}

// RawValue returns the exact bytes of the value stored under key in the
// top-level dictionary (used to compute the info_hash).
func RawValue(data []byte, key string) ([]byte, error) {
	d := &decoder{data: data}
	if d.pos >= len(d.data) || d.data[d.pos] != 'd' {
		return nil, errors.New("bencode: top level is not a dictionary")
	}
	d.pos++
	for d.pos < len(d.data) && d.data[d.pos] != 'e' {
		k, err := d.str()
		if err != nil {
			return nil, err
		}
		start := d.pos
		if _, err := d.value(); err != nil {
			return nil, err
		}
		if k == key {
			return d.data[start:d.pos], nil
		}
	}
	return nil, fmt.Errorf("bencode: key %q not found", key)
}

type decoder struct {
	data []byte
	pos  int
}

func (d *decoder) value() (any, error) {
	if d.pos >= len(d.data) {
		return nil, errors.New("bencode: unexpected end of data")
	}
	switch c := d.data[d.pos]; {
	case c == 'i':
		return d.integer()
	case c == 'l':
		d.pos++
		list := []any{}
		for {
			if d.pos >= len(d.data) {
				return nil, errors.New("bencode: unterminated list")
			}
			if d.data[d.pos] == 'e' {
				d.pos++
				return list, nil
			}
			v, err := d.value()
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
	case c == 'd':
		d.pos++
		dict := map[string]any{}
		for {
			if d.pos >= len(d.data) {
				return nil, errors.New("bencode: unterminated dictionary")
			}
			if d.data[d.pos] == 'e' {
				d.pos++
				return dict, nil
			}
			k, err := d.str()
			if err != nil {
				return nil, err
			}
			v, err := d.value()
			if err != nil {
				return nil, err
			}
			dict[k] = v
		}
	case c >= '0' && c <= '9':
		return d.str()
	default:
		return nil, fmt.Errorf("bencode: invalid character %q at %d", c, d.pos)
	}
}

func (d *decoder) integer() (int64, error) {
	end := bytes.IndexByte(d.data[d.pos:], 'e')
	if end < 0 {
		return 0, errors.New("bencode: unterminated integer")
	}
	n, err := strconv.ParseInt(string(d.data[d.pos+1:d.pos+end]), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bencode: bad integer: %w", err)
	}
	d.pos += end + 1
	return n, nil
}

func (d *decoder) str() (string, error) {
	colon := bytes.IndexByte(d.data[d.pos:], ':')
	if colon < 0 {
		return "", errors.New("bencode: bad string length")
	}
	n, err := strconv.Atoi(string(d.data[d.pos : d.pos+colon]))
	if err != nil || n < 0 {
		return "", errors.New("bencode: bad string length")
	}
	start := d.pos + colon + 1
	if start+n > len(d.data) {
		return "", errors.New("bencode: string exceeds data")
	}
	d.pos = start + n
	return string(d.data[start:d.pos]), nil
}

// Encode serialises strings, ints, lists and maps (keys sorted).
func Encode(v any) []byte {
	var b bytes.Buffer
	encode(&b, v)
	return b.Bytes()
}

func encode(b *bytes.Buffer, v any) {
	switch t := v.(type) {
	case string:
		b.WriteString(strconv.Itoa(len(t)))
		b.WriteByte(':')
		b.WriteString(t)
	case []byte:
		encode(b, string(t))
	case int:
		fmt.Fprintf(b, "i%de", t)
	case int64:
		fmt.Fprintf(b, "i%de", t)
	case []any:
		b.WriteByte('l')
		for _, e := range t {
			encode(b, e)
		}
		b.WriteByte('e')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('d')
		for _, k := range keys {
			encode(b, k)
			encode(b, t[k])
		}
		b.WriteByte('e')
	default:
		panic(fmt.Sprintf("bencode: unsupported type %T", v))
	}
}
