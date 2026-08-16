package lifecycle

import "sync"

type Gate struct {
	mu         sync.Mutex
	updating   bool
	operations int
}

type Lease struct {
	once    sync.Once
	release func()
}

func New() *Gate {
	return &Gate{}
}

func (g *Gate) TryOperation() (*Lease, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.updating {
		return nil, false
	}
	g.operations++
	return &Lease{release: func() {
		g.mu.Lock()
		g.operations--
		g.mu.Unlock()
	}}, true
}

func (g *Gate) TryUpdate() (*Lease, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.updating || g.operations != 0 {
		return nil, false
	}
	g.updating = true
	return &Lease{release: func() {
		g.mu.Lock()
		g.updating = false
		g.mu.Unlock()
	}}, true
}

func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(l.release)
}
