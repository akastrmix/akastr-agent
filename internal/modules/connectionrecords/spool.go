package connectionrecords

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akastrmix/akastr-agent/internal/layout"
	"github.com/akastrmix/akastr-agent/internal/state"
)

const (
	queueFile = "queue.jsonl"
	stateFile = "state.json"
	// compactFile is the queue being rewritten without what Cloud has.
	compactFile = ".queue.jsonl.new"
	// maxQueued bounds the records waiting on disk: days of the busiest node.
	maxQueued = 200 << 20
	// minFree is left to Xray, 3x-ui and the rest of the node.
	minFree = 1 << 30
)

var errLowSpace = errors.New("less than 1 GiB free on the file system")

// saved is state.json.
type saved struct {
	// LastLineAt is the time of the last log line whose records are on disk.
	LastLineAt string `json:"last_line_at,omitempty"`
	// Head is where the queue starts in queue.jsonl: the records before it
	// were acknowledged or dropped.
	Head int64 `json:"head"`
	// Gaps are those Cloud has not acknowledged yet.
	Gaps []gap `json:"gaps,omitempty"`
}

// spool is the queue of records waiting for Cloud on disk: queue.jsonl holds
// one record a line in the order they were made, and state.json where the
// queue starts in it, the last line queued and the gaps. A record leaves only
// once Cloud acknowledges its batch, or is dropped as the oldest when the
// queue would grow past its limit. Each write is synced before it counts, and
// state.json is written after what it describes, so a crash at any point
// loses no record that counted, at worst sending some again, which Cloud
// keeps once. Only the goroutine that sends uses it.
type spool struct {
	dir   string
	file  *os.File
	size  int64
	saved saved
	state *state.JSONFile
	// limit bounds the queue, and free tells the space left on its file
	// system.
	limit int64
	free  func(dir string) (int64, error)
}

func openSpool(dir string, from saved, limit int64, free func(string) (int64, error)) (*spool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// The queue is customers' browsing history.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Remove(filepath.Join(dir, compactFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, queueFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	s := &spool{dir: dir, file: file, saved: from, state: state.NewJSONFile(filepath.Join(dir, stateFile)), limit: limit, free: free}
	if err := s.recover(); err != nil {
		file.Close()
		return nil, err
	}
	return s, nil
}

// recover drops a line a crash left half written, which never counted, and
// starts the queue over when its saved start is not the start of a line.
func (s *spool) recover() error {
	if err := s.file.Chmod(0o600); err != nil {
		return err
	}
	info, err := s.file.Stat()
	if err != nil {
		return err
	}
	end, err := lineEnd(s.file, info.Size())
	if err != nil {
		return err
	}
	if end < info.Size() {
		if err := s.file.Truncate(end); err != nil {
			return err
		}
	}
	s.size = end
	if head := s.saved.Head; head < 0 || head > end || (head > 0 && !lineStart(s.file, head)) {
		s.saved.Head = 0
	}
	return nil
}

// lineEnd returns where the last complete line of f ends.
func lineEnd(f *os.File, size int64) (int64, error) {
	buf := make([]byte, 64<<10)
	for end := size; end > 0; {
		from := max(0, end-int64(len(buf)))
		chunk := buf[:end-from]
		if _, err := f.ReadAt(chunk, from); err != nil && err != io.EOF {
			return 0, err
		}
		if i := bytes.LastIndexByte(chunk, '\n'); i >= 0 {
			return from + int64(i) + 1, nil
		}
		end = from
	}
	return 0, nil
}

func lineStart(f *os.File, off int64) bool {
	b := make([]byte, 1)
	_, err := f.ReadAt(b, off-1)
	return err == nil && b[0] == '\n'
}

func freeSpace(dir string) (int64, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return 0, err
	}
	return int64(fs.Bavail) * fs.Bsize, nil
}

func (s *spool) close() { s.file.Close() }

func (s *spool) save() error { return s.state.Save(s.saved) }

// queued is how much of the file is waiting.
func (s *spool) queued() int64 { return s.size - s.saved.Head }

// append adds encoded lines to the queue, unless that would leave less than
// minFree on the file system: writing stops before the node's disk fills.
func (s *spool) append(data []byte) error {
	free, err := s.free(s.dir)
	if err != nil {
		return err
	}
	if free-int64(len(data)) < minFree {
		return errLowSpace
	}
	if _, err := s.file.WriteAt(data, s.size); err != nil {
		_ = s.file.Truncate(s.size)
		return err
	}
	if err := s.file.Sync(); err != nil {
		_ = s.file.Truncate(s.size)
		return err
	}
	s.size += int64(len(data))
	return nil
}

// scan passes each line from the queue's start, without its newline, until
// take returns false, and returns how many bytes the lines it passed fill.
func (s *spool) scan(take func(line []byte) bool) (int64, error) {
	r := bufio.NewReaderSize(io.NewSectionReader(s.file, s.saved.Head, s.queued()), 64<<10)
	var read int64
	for {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			return read, nil
		}
		if err != nil {
			return read, err
		}
		read += int64(len(line))
		if !take(line[:len(line)-1]) {
			return read, nil
		}
	}
}

// read returns up to n records from the queue's start, as they were written,
// and how many bytes they fill. A line that is not a JSON object, which only
// a damaged disk writes, is passed over.
func (s *spool) read(n int) ([]json.RawMessage, int64, error) {
	records := make([]json.RawMessage, 0, min(n, 64))
	size, err := s.scan(func(line []byte) bool {
		if bytes.HasPrefix(line, []byte("{")) && json.Valid(line) {
			records = append(records, line)
		}
		return len(records) < n
	})
	return records, size, err
}

// drop removes at least size bytes of the oldest records and returns their
// gap.
func (s *spool) drop(size int64) (gap, error) {
	g := gap{Reason: backlogFull}
	dropped, err := s.scan(func(line []byte) bool {
		var r struct {
			At string `json:"at"`
		}
		if json.Unmarshal(line, &r) == nil && r.At != "" {
			g.widen(r.At, r.At)
		}
		g.Lost++
		size -= int64(len(line)) + 1
		return size > 0
	})
	if err != nil {
		return gap{}, err
	}
	s.saved.Head += dropped
	return g, s.shrink()
}

// ack removes the size bytes at the queue's start that Cloud acknowledged.
func (s *spool) ack(size int64) error {
	s.saved.Head += size
	return s.shrink()
}

// shrink gives back the space before the queue's start once it is the whole
// file, or half of it: each byte is copied a bounded number of times however
// the queue grows and drains. The head is saved with the rest of the state
// after, but for a compaction (see compact).
func (s *spool) shrink() error {
	switch {
	case s.saved.Head == s.size:
		// A crash before the head is saved finds it past the end, and starts
		// over at zero.
		if err := s.file.Truncate(0); err != nil {
			return err
		}
		s.size, s.saved.Head = 0, 0
	case s.saved.Head*2 >= s.size:
		return s.compact()
	}
	return nil
}

// compact rewrites the queue without what is before its start. The start is
// saved as zero before the new file takes the old one's place: a crash in
// between sends again what Cloud has, never skips what it has not.
func (s *spool) compact() error {
	path := filepath.Join(s.dir, compactFile)
	out, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	head := s.saved.Head
	_, err = io.Copy(out, io.NewSectionReader(s.file, head, s.size-head))
	if err == nil {
		err = out.Sync()
	}
	if err == nil {
		s.saved.Head = 0
		if err = s.save(); err != nil {
			s.saved.Head = head
		}
	}
	if err == nil {
		err = os.Rename(path, filepath.Join(s.dir, queueFile))
	}
	if err != nil {
		out.Close()
		os.Remove(path)
		return err
	}
	s.file.Close()
	s.file, s.size = out, s.size-head
	return layout.SyncDirectory(s.dir)
}
