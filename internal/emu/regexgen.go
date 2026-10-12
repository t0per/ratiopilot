// Rôle : génère les peer_id et clés aléatoires décrits par une expression régulière.

package emu

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// Tiny random-string generator for the regex subset used by .client
// profile files: literals, escapes, (groups), [classes with ranges], . and {n}/{n,m}.
// Every rune <= 0xFF is emitted as one byte (Latin-1).

type node interface{ gen(b *[]byte) }

type lit byte

func (l lit) gen(b *[]byte) { *b = append(*b, byte(l)) }

type class []byte

func (c class) gen(b *[]byte) { *b = append(*b, c[randInt(len(c))]) }

type seq []node

func (s seq) gen(b *[]byte) {
	for _, n := range s {
		n.gen(b)
	}
}

type repeat struct {
	n        node
	min, max int
}

func (r repeat) gen(b *[]byte) {
	count := r.min
	if r.max > r.min {
		count += randInt(r.max - r.min + 1)
	}
	for i := 0; i < count; i++ {
		r.n.gen(b)
	}
}

// Pattern is a compiled generator.
type Pattern struct{ root seq }

// Generate returns one random byte string matching the pattern.
func (p *Pattern) Generate() []byte {
	var b []byte
	p.root.gen(&b)
	return b
}

// CompilePattern parses the supported regex subset.
func CompilePattern(expr string) (*Pattern, error) {
	p := &parser{r: []rune(expr)}
	s, err := p.sequence(false)
	if err != nil {
		return nil, fmt.Errorf("motif %q: %w", expr, err)
	}
	return &Pattern{root: s}, nil
}

type parser struct {
	r   []rune
	pos int
}

func toByte(r rune) (byte, error) {
	if r > 0xFF || r < 0 {
		return 0, fmt.Errorf("caractère hors Latin-1: %U", r)
	}
	return byte(r), nil
}

func (p *parser) sequence(inGroup bool) (seq, error) {
	var out seq
	for p.pos < len(p.r) {
		c := p.r[p.pos]
		var n node
		switch c {
		case ')':
			if !inGroup {
				return nil, fmt.Errorf("parenthèse fermante inattendue")
			}
			p.pos++
			return out, nil
		case '(':
			p.pos++
			if p.pos+1 < len(p.r) && p.r[p.pos] == '?' && p.r[p.pos+1] == ':' {
				p.pos += 2
			}
			s, err := p.sequence(true)
			if err != nil {
				return nil, err
			}
			n = s
		case '[':
			cl, err := p.class()
			if err != nil {
				return nil, err
			}
			n = cl
		case '.':
			p.pos++
			var cl class
			for ch := byte(0x21); ch < 0x7f; ch++ {
				cl = append(cl, ch)
			}
			n = cl
		case '\\':
			p.pos++
			if p.pos >= len(p.r) {
				return nil, fmt.Errorf("échappement incomplet")
			}
			b, err := toByte(p.r[p.pos])
			if err != nil {
				return nil, err
			}
			p.pos++
			n = lit(b)
		default:
			b, err := toByte(c)
			if err != nil {
				return nil, err
			}
			p.pos++
			n = lit(b)
		}
		// quantifier
		if p.pos < len(p.r) && p.r[p.pos] == '{' {
			end := strings.IndexRune(string(p.r[p.pos:]), '}')
			if end < 0 {
				return nil, fmt.Errorf("quantificateur incomplet")
			}
			body := string(p.r[p.pos+1 : p.pos+end])
			p.pos += end + 1
			lo, hi := body, body
			if i := strings.IndexByte(body, ','); i >= 0 {
				lo, hi = body[:i], body[i+1:]
			}
			min, err1 := strconv.Atoi(lo)
			max, err2 := strconv.Atoi(hi)
			if err1 != nil || err2 != nil || max < min {
				return nil, fmt.Errorf("quantificateur invalide {%s}", body)
			}
			n = repeat{n: n, min: min, max: max}
		}
		out = append(out, n)
	}
	if inGroup {
		return nil, fmt.Errorf("parenthèse non fermée")
	}
	return out, nil
}

func (p *parser) class() (class, error) {
	p.pos++ // skip [
	var items []byte
	read := func() (byte, error) {
		c := p.r[p.pos]
		if c == '\\' {
			p.pos++
			if p.pos >= len(p.r) {
				return 0, fmt.Errorf("échappement incomplet")
			}
			c = p.r[p.pos]
		}
		p.pos++
		return toByte(c)
	}
	for p.pos < len(p.r) && p.r[p.pos] != ']' {
		lo, err := read()
		if err != nil {
			return nil, err
		}
		if p.pos+1 < len(p.r) && p.r[p.pos] == '-' && p.r[p.pos+1] != ']' {
			p.pos++
			hi, err := read()
			if err != nil {
				return nil, err
			}
			if hi < lo {
				return nil, fmt.Errorf("plage invalide")
			}
			for ch := int(lo); ch <= int(hi); ch++ {
				items = append(items, byte(ch))
			}
		} else {
			items = append(items, lo)
		}
	}
	if p.pos >= len(p.r) {
		return nil, fmt.Errorf("classe non fermée")
	}
	p.pos++ // skip ]
	if len(items) == 0 {
		return nil, fmt.Errorf("classe vide")
	}
	return class(items), nil
}

func randInt(n int) int {
	if n <= 1 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		panic(err)
	}
	return int(v.Int64())
}

func randInt64Between(lo, hi int64) int64 {
	if hi <= lo {
		return lo
	}
	v, err := rand.Int(rand.Reader, big.NewInt(hi-lo+1))
	if err != nil {
		panic(err)
	}
	return lo + v.Int64()
}
