//go:build windows

package hub

import "fmt"

// streams is a stub on Windows: live pane streaming needs tmux
// pipe-pane, which the classic console backend has no equivalent for.
type streams struct{}

func newStreams(dir string) *streams { return &streams{} }

func (s *streams) Subscribe(pane string) (<-chan []byte, func(), error) {
	return nil, nil, fmt.Errorf("pane streaming is not supported on Windows")
}
