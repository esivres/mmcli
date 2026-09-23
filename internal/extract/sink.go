package extract

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// errOutputLimit stops an extractor whose output keeps growing (e.g. one
// shared string referenced millions of times).
var errOutputLimit = errors.New("output limit reached")

// sink keeps the first and the last bytes of what is written, so memory stays
// bounded whatever the input expands to. Logs usually matter at both ends.
type sink struct {
	head, tail []byte // tail is a ring buffer once full
	headMax    int
	tailMax    int
	tailStart  int
	total      int64
	stopAt     int64 // total bytes after which writes fail
	onStop     func()
}

func newSink(keep int, stopAt int64) *sink {
	return &sink{headMax: keep * 2 / 3, tailMax: keep - keep*2/3, stopAt: stopAt}
}

func (s *sink) Write(p []byte) (int, error) {
	n := len(p)
	if s.stopAt > 0 && s.total+int64(n) > s.stopAt {
		n = int(s.stopAt - s.total)
	}
	s.add(p[:n])
	if n < len(p) {
		if s.onStop != nil {
			s.onStop()
		}
		return n, errOutputLimit
	}
	return n, nil
}

func (s *sink) WriteString(str string) { _, _ = s.Write([]byte(str)) }
func (s *sink) WriteByte(b byte) error { _, err := s.Write([]byte{b}); return err }

func (s *sink) add(p []byte) {
	s.total += int64(len(p))
	if room := s.headMax - len(s.head); room > 0 {
		k := min(room, len(p))
		s.head = append(s.head, p[:k]...)
		p = p[k:]
	}
	for len(p) > 0 && s.tailMax > 0 {
		if len(s.tail) < s.tailMax {
			k := min(s.tailMax-len(s.tail), len(p))
			s.tail = append(s.tail, p[:k]...)
			p = p[k:]
			continue
		}
		if len(p) >= s.tailMax {
			copy(s.tail, p[len(p)-s.tailMax:])
			s.tailStart = 0
			return
		}
		for _, b := range p {
			s.tail[s.tailStart] = b
			s.tailStart = (s.tailStart + 1) % s.tailMax
		}
		return
	}
}

func (s *sink) failed() bool { return s.stopAt > 0 && s.total >= s.stopAt }

// result assembles the kept text; cut runes at the seams are dropped.
func (s *sink) result(stopped bool) Result {
	tail := append(append([]byte(nil), s.tail[s.tailStart:]...), s.tail[:s.tailStart]...)
	kept := int64(len(s.head) + len(tail))
	if s.total <= kept && !stopped {
		return Result{Text: toValid(append(s.head, tail...))}
	}
	head := s.head
	for len(head) > 0 && !utf8.Valid(head) {
		head = head[:len(head)-1]
	}
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	omitted := s.total - int64(len(head)) - int64(len(tail))
	marker := fmt.Sprintf("\n[... %d bytes omitted ...]\n", omitted)
	if stopped {
		marker = fmt.Sprintf("\n[... %d bytes omitted; extraction stopped at the %d-byte output limit ...]\n", omitted, s.stopAt)
	}
	return Result{Text: toValid(head) + marker + toValid(tail), Truncated: true, Omitted: omitted}
}

func toValid(b []byte) string { return string([]rune(string(b))) }
