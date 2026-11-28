// Package connectionrecords reports to Cloud a record of each request the
// access log names for an owned client, and once a bound TCP connection ends,
// the same record again with when it ended and what it carried. Cloud merges
// the two by the line's time and source, so a record sent twice changes
// nothing.
//
// Records wait on disk until Cloud acknowledges them (see spool), so neither
// a restart nor a long time without Cloud loses them. What is lost all the
// same is reported as a gap: records dropped as the oldest when the queue is
// full, or as the newest when the disk is nearly so, and log lines a restart
// could not read back. On opening the log, its lines from the last one
// queued on are recorded again, so the requests made while the Agent was
// stopped or replaced are not lost either. Connections open across a restart
// end with no record (see ledger).
package connectionrecords

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/akastrmix/akastr-agent/internal/module"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/akastrmix/akastr-agent/internal/state"
	"github.com/akastrmix/akastr-agent/internal/xray"
	"github.com/akastrmix/akastr-agent/internal/xray/accesslog"
	"github.com/akastrmix/akastr-agent/internal/xray/ledger"
)

const (
	Name      = "connection_records"
	KindBatch = "connection_records.batch"

	// Records made since are written to disk at least this often.
	batchInterval = 10 * time.Second
	maxBatch      = 1000
	// maxGaps go in one batch; beyond them a gap extends the last of its kind.
	maxGaps = 100
	// Records waiting to be written while the disk refuses them: about two
	// hours of the busiest node.
	maxBuffered = 20000
	// Bounds a record so that a full batch fits one protocol message; Xray
	// configurations name outbounds briefly.
	maxOutbound   = 128
	timeLayout    = "2006-01-02T15:04:05.000000Z"
	leftoverRetry = 30 * time.Second

	backlogFull = "backlog_full"
	logMissed   = "log_missed"
)

// Config has no fields: the module is on or off.
type Config struct{}

func ParseConfig(raw json.RawMessage) (Config, error) {
	return protocol.DecodeStrict[Config](raw, Name)
}

// Directory is where the module keeps its queue under the Agent's state
// directory.
func Directory(stateDir string) string { return filepath.Join(stateDir, Name) }

// Leftovers removes the queue a node kept while the module was on: it holds
// customers' browsing history.
func Leftovers(stateDir string) *module.Cleanup {
	return module.NewCleanup("connection records queue", leftoverRetry, func(context.Context) error {
		return os.RemoveAll(Directory(stateDir))
	})
}

type record struct {
	At       string `json:"at"`
	Email    string `json:"email"`
	Source   string `json:"source"`
	Network  string `json:"network"`
	Host     string `json:"host"`
	Port     uint16 `json:"port"`
	Outbound string `json:"outbound"`
	Blocked  bool   `json:"blocked"`
	*ending
}

// ending is what an end record adds: all four fields or none. Its line was
// bound to a TCP connection, so its network is TCP whatever the line says.
type ending struct {
	EndedAt  string `json:"ended_at"`
	Up       uint64 `json:"up"`
	Down     uint64 `json:"down"`
	Requests int    `json:"requests"`
}

// gap is a stretch of time whose records are known to be incomplete: records
// dropped, from the first of their times to the last, and how many; or log
// lines not read, from the last line queued to the first read after.
type gap struct {
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at"`
	Reason    string `json:"reason"`
	Lost      int    `json:"lost,omitempty"`
}

// widen makes the gap cover start to end; the times are all in timeLayout,
// so they sort as text.
func (g *gap) widen(start, end string) {
	if g.StartedAt == "" || start < g.StartedAt {
		g.StartedAt = start
	}
	if end > g.EndedAt {
		g.EndedAt = end
	}
}

type batch struct {
	Records []json.RawMessage `json:"records"`
	Gaps    []gap             `json:"gaps,omitempty"`
}

// Records queues the records on disk and sends them in batches, one batch
// waiting for its acknowledgement at a time.
type Records struct {
	dir  string
	wake chan struct{}
	// saved is what state.json held when the process started.
	saved saved
	// Only Run uses the spool; it is nil until it opens.
	spool *spool
	// openFailed and writeFailed keep a lasting failure to one log line.
	openFailed, writeFailed bool
	limit                   int64
	free                    func(string) (int64, error)

	mu sync.Mutex
	// buffer holds the records made since the last flush.
	buffer []record
	// seen is the time of the latest line taken in: a line read on opening
	// the log is new from it on.
	seen time.Time
	// checking is set until the first line read after a start with a saved
	// time says whether the log still held every line since.
	checking bool
	// gaps wait for Cloud; the first pendingGaps went with the batch waiting.
	gaps        []gap
	gapsChanged bool
	pending     *protocol.ReportBody
	// pendingSize is how many bytes of the queue the batch waiting fills, and
	// acked how many the batches acknowledged since the last flush fill.
	pendingSize, acked int64
	pendingGaps        int
	pendingFull        bool
}

var _ module.Reporter = (*Records)(nil)

// New reads what an earlier process left in dir, and only reads it: an update
// candidate builds its runtime before it takes over.
func New(dir string) *Records {
	r := &Records{dir: dir, wake: make(chan struct{}, 1), limit: maxQueued, free: freeSpace}
	found, err := state.NewJSONFile(filepath.Join(dir, stateFile)).Load(&r.saved)
	if err != nil {
		// The queue is sent again from its start, which repeats only what
		// Cloud has.
		slog.Warn("connection records state unreadable", "error", err.Error())
		r.saved = saved{}
	}
	if at, err := time.Parse(timeLayout, r.saved.LastLineAt); found && err == nil {
		r.seen, r.checking = at, true
	}
	r.gaps = append([]gap(nil), r.saved.Gaps...)
	return r
}

func format(t time.Time) string { return t.UTC().Format(timeLayout) }

// Lines takes in the access log's lines.
func (r *Records) Lines(lines []accesslog.Line) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// A line read on opening the log is new from the latest line taken in
	// on: an earlier process, or this one before the log was opened again,
	// queued the older ones. With no such line, as when the module was just
	// turned on, none is.
	from := r.seen
	for _, line := range lines {
		if line.Written.IsZero() {
			continue
		}
		// Times are kept to the microsecond, as the log writes them.
		at := line.Written.Truncate(time.Microsecond)
		if r.checking {
			r.checking = false
			if at.After(from) {
				r.addGap(gap{StartedAt: format(from), EndedAt: format(at), Reason: logMissed})
			}
		}
		if line.Backlog && (from.IsZero() || at.Before(from)) {
			continue
		}
		r.add(line, nil)
		if at.After(r.seen) {
			r.seen = at
		}
	}
}

// Ended takes in a bound connection that ended.
func (r *Records) Ended(e ledger.Ended) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// network is the destination's, as in the start record; a line that does
	// not say, as Shadowsocks 2022 writes none, was a TCP request on its TCP
	// connection.
	line := e.Line
	if line.Dest.Network == "" {
		line.Dest.Network = "tcp"
	}
	r.add(line, &ending{EndedAt: format(e.At), Up: e.Up, Down: e.Down, Requests: e.Requests})
}

// add buffers the record of a line of an owned client, which only a line
// with its time, destination and outbound can make: Cloud refuses a batch
// with one record it cannot read.
func (r *Records) add(line accesslog.Line, end *ending) {
	if !xray.Owned(line.Email) || line.Written.IsZero() || line.Dest.Host == "" || line.Outbound == "" || len(line.Outbound) > maxOutbound {
		return
	}
	source := netip.AddrPortFrom(line.Conn.Remote.Addr().WithZone(""), line.Conn.Remote.Port())
	r.buffer = append(r.buffer, record{At: format(line.Written), Email: line.Email, Source: source.String(),
		Network: line.Dest.Network, Host: line.Dest.Host, Port: line.Dest.Port, Outbound: line.Outbound, Blocked: line.Blocked, ending: end})
	if len(r.buffer) > maxBuffered {
		// The disk is not taking them. Dropping a tenth at once keeps a flood
		// from copying the buffer on every line.
		drop := len(r.buffer) - maxBuffered*9/10
		r.addGap(lost(r.buffer[:drop]))
		r.buffer = append(r.buffer[:0], r.buffer[drop:]...)
	}
	if len(r.buffer) >= maxBatch {
		r.signal()
	}
}

func lost(records []record) gap {
	g := gap{Reason: backlogFull, Lost: len(records)}
	for _, rec := range records {
		g.widen(rec.At, rec.At)
	}
	return g
}

// addGap keeps a gap until Cloud acknowledges it. Records dropped extend the
// last such gap not sent yet, as any gap does once there are maxGaps, so the
// list stays short however long Cloud is away.
func (r *Records) addGap(g gap) {
	r.gapsChanged = true
	for i := len(r.gaps) - 1; i >= r.pendingGaps; i-- {
		if old := &r.gaps[i]; old.Reason == g.Reason && (g.Reason == backlogFull || len(r.gaps) >= maxGaps) {
			old.widen(g.StartedAt, g.EndedAt)
			old.Lost += g.Lost
			return
		}
	}
	r.gaps = append(r.gaps, g)
}

func (r *Records) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Records) Run(ctx context.Context, publish module.Publish) error {
	ticker := time.NewTicker(batchInterval)
	defer ticker.Stop()
	for {
		r.step(publish)
		select {
		case <-ctx.Done():
			// A stop loses nothing taken in.
			if r.spool != nil {
				r.flush()
				r.spool.close()
			}
			return ctx.Err()
		case <-ticker.C:
		case <-r.wake:
		}
	}
}

// step writes what was made since the last step and sends the waiting batch
// again, or the next one. Until the queue opens, records wait in memory.
func (r *Records) step(publish module.Publish) {
	if r.spool == nil {
		s, err := openSpool(r.dir, r.saved, r.limit, r.free)
		if err != nil {
			if !r.openFailed {
				slog.Warn("connection records queue unavailable; retrying", "error", err.Error())
			}
			r.openFailed = true
			return
		}
		r.spool, r.openFailed = s, false
	}
	r.flush()
	r.send(publish)
}

// flush applies acknowledgements to the queue on disk, writes the records
// made since, and saves where the queue starts, the last line queued and the
// gaps.
func (r *Records) flush() {
	s := r.spool
	r.mu.Lock()
	acked := r.acked
	records, seen := r.buffer, r.seen
	r.acked, r.buffer = 0, nil
	r.mu.Unlock()
	changed := acked > 0
	if acked > 0 {
		if err := s.ack(acked); err != nil {
			slog.Warn("connection records queue not shortened", "error", err.Error())
		}
	}
	if len(records) > 0 {
		changed = true
		var data bytes.Buffer
		encoder := json.NewEncoder(&data)
		for _, rec := range records {
			_ = encoder.Encode(rec)
		}
		if over := s.queued() + int64(data.Len()) - s.limit; over > 0 {
			// The oldest go, and the batch waiting is among them.
			r.mu.Lock()
			r.pending, r.pendingSize, r.pendingGaps, r.acked = nil, 0, 0, 0
			r.mu.Unlock()
			g, err := s.drop(max(over, s.limit/10))
			if err != nil {
				slog.Warn("connection records queue not shortened", "error", err.Error())
			}
			if g.Lost > 0 {
				r.mu.Lock()
				r.addGap(g)
				r.mu.Unlock()
			}
		}
		err := s.append(data.Bytes())
		if err != nil {
			if !r.writeFailed {
				slog.Warn("connection records not written; dropping them until they are", "error", err.Error())
			}
			r.mu.Lock()
			r.addGap(lost(records))
			r.mu.Unlock()
		}
		r.writeFailed = err != nil
	}
	r.mu.Lock()
	if !seen.IsZero() && format(seen) != s.saved.LastLineAt {
		s.saved.LastLineAt, changed = format(seen), true
	}
	if r.gapsChanged {
		s.saved.Gaps, r.gapsChanged, changed = append([]gap(nil), r.gaps...), false, true
	}
	r.mu.Unlock()
	if changed {
		if err := s.save(); err != nil {
			slog.Warn("connection records state not saved", "error", err.Error())
		}
	}
}

// send sends the waiting batch again, or the next one from the queue with the
// gaps.
func (r *Records) send(publish module.Publish) {
	r.mu.Lock()
	waiting := r.pending != nil
	r.mu.Unlock()
	if !waiting {
		records, size, err := r.spool.read(maxBatch)
		if err != nil {
			slog.Warn("connection records queue unreadable", "error", err.Error())
			return
		}
		if len(records) == 0 && size > 0 {
			// Only lines that are not records.
			_ = r.spool.ack(size)
			size = 0
		}
		r.mu.Lock()
		gaps := r.gaps[:min(len(r.gaps), maxGaps)]
		if len(records) > 0 || len(gaps) > 0 {
			if data, err := json.Marshal(batch{Records: records, Gaps: gaps}); err == nil {
				r.pending = &protocol.ReportBody{ReportID: protocol.NewUUID(), Kind: KindBatch, Data: json.RawMessage(data)}
				r.pendingSize, r.pendingGaps, r.pendingFull = size, len(gaps), len(records) == maxBatch
			}
		}
		r.mu.Unlock()
	}
	r.mu.Lock()
	if r.pending == nil {
		r.mu.Unlock()
		return
	}
	body := *r.pending
	r.mu.Unlock()
	// Without a session the next step or ready session sends it again.
	_ = publish(body)
}

func (r *Records) ControlReady() { r.signal() }

// Acknowledge settles the batch waiting; its records leave the queue on the
// next step.
func (r *Records) Acknowledge(reportID string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil || r.pending.ReportID != reportID {
		return false, nil
	}
	r.acked += r.pendingSize
	if r.pendingGaps > 0 {
		r.gaps = append(r.gaps[:0:0], r.gaps[r.pendingGaps:]...)
		r.gapsChanged = true
	}
	r.pending, r.pendingSize, r.pendingGaps = nil, 0, 0
	if r.pendingFull || len(r.buffer) >= maxBatch {
		r.signal()
	}
	return true, nil
}

// UpdateSafe allows a replacement at any time. Records not written yet go
// with the process, but the next one reads their lines again; only the end
// records of the last seconds are lost.
func (r *Records) UpdateSafe() error { return nil }
