//go:build !windows

package hub

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/benhynes/hive/internal/control"
)

// streams multiplexes live pane output: tmux allows one pipe-pane per
// pane, so the first subscriber opens the pipe (a FIFO the daemon
// reads) and later subscribers share the broadcast. The last one out
// closes the pipe.
type streams struct {
	mu    sync.Mutex
	dir   string // FIFO directory (under the hub's home)
	panes map[string]*paneStream
}

type paneStream struct {
	pane string
	fifo string
	f    *os.File
	subs map[chan []byte]bool
}

func newStreams(dir string) *streams {
	return &streams{dir: dir, panes: map[string]*paneStream{}}
}

// subChanBuf bounds a subscriber's backlog. A subscriber that can't
// drain (dead TCP peer) gets closed rather than blocking the pane's
// reader or corrupting others' byte streams.
const subChanBuf = 256

// Subscribe attaches to the pane's live output. The returned channel
// yields raw output chunks and is closed when the pane's pipe ends or
// the subscriber falls too far behind. Call the returned cancel to
// detach.
func (s *streams) Subscribe(pane string) (<-chan []byte, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := s.panes[pane]
	if ps == nil {
		if err := os.MkdirAll(s.dir, 0o700); err != nil {
			return nil, nil, err
		}
		fifo := filepath.Join(s.dir, fmt.Sprintf("pipe-%s.fifo", sanitizePane(pane)))
		os.Remove(fifo)
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			return nil, nil, fmt.Errorf("mkfifo: %w", err)
		}
		// Open read+write so the open never blocks waiting for tmux's
		// writer and the FIFO never sees EOF while we hold the write
		// side — pipe-pane's `cat` may come and go.
		f, err := os.OpenFile(fifo, os.O_RDWR, 0)
		if err != nil {
			os.Remove(fifo)
			return nil, nil, err
		}
		if err := control.PipeOpen(pane, fifo); err != nil {
			f.Close()
			os.Remove(fifo)
			return nil, nil, err
		}
		ps = &paneStream{pane: pane, fifo: fifo, f: f, subs: map[chan []byte]bool{}}
		s.panes[pane] = ps
		go s.pump(ps)
	}
	ch := make(chan []byte, subChanBuf)
	ps.subs[ch] = true
	cancel := func() { s.unsubscribe(pane, ch) }
	return ch, cancel, nil
}

func (s *streams) unsubscribe(pane string, ch chan []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := s.panes[pane]
	if ps == nil || !ps.subs[ch] {
		return
	}
	delete(ps.subs, ch)
	if len(ps.subs) == 0 {
		s.teardownLocked(ps)
	}
}

// teardownLocked closes the pane's pipe and FIFO. Caller holds s.mu.
func (s *streams) teardownLocked(ps *paneStream) {
	delete(s.panes, ps.pane)
	control.PipeClose(ps.pane)
	ps.f.Close() // unblocks the pump's Read
	os.Remove(ps.fifo)
}

// pump reads the FIFO and fans chunks out to subscribers. A subscriber
// with a full channel is kicked (closed + removed): dropping bytes
// mid-escape-sequence would corrupt its terminal, so it must reconnect
// for a fresh snapshot instead.
func (s *streams) pump(ps *paneStream) {
	buf := make([]byte, 32*1024)
	for {
		n, err := ps.f.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			s.mu.Lock()
			for ch := range ps.subs {
				select {
				case ch <- chunk:
				default:
					delete(ps.subs, ch)
					close(ch)
				}
			}
			empty := len(ps.subs) == 0
			if empty && s.panes[ps.pane] == ps {
				s.teardownLocked(ps)
			}
			s.mu.Unlock()
			if empty {
				return
			}
		}
		if err != nil {
			s.mu.Lock()
			for ch := range ps.subs {
				close(ch)
			}
			ps.subs = map[chan []byte]bool{}
			if s.panes[ps.pane] == ps {
				s.teardownLocked(ps)
			}
			s.mu.Unlock()
			return
		}
	}
}

func sanitizePane(pane string) string {
	out := make([]rune, 0, len(pane))
	for _, r := range pane {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
