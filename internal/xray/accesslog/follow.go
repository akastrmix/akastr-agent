package accesslog

import (
	"bytes"
	"io"
	"math"
	"os"
)

const (
	// maxRead bounds one poll; a longer backlog is read over later polls,
	// and a first open starts this far before the end.
	maxRead = 4 << 20
	maxLine = 4096
)

// follower reads what is appended to one file, starting again from zero when
// the file is truncated or replaced.
//
// 3x-ui truncates the log only after appending all of it to a copy, so the
// follower reads back from the copy what it had not read yet. A truncation
// then loses only what Xray writes between the copy and the truncation. When
// the file is opened from its start, the end of the copy comes right before
// it, and is read first.
type follower struct {
	path, copy string
	file       *os.File
	info       os.FileInfo
	off        int64
	partial    []byte
	// skip drops text up to the next newline: a line whose start was not read.
	skip bool
	// tail is the last text read. A truncation followed within one poll by
	// more text than was read before keeps the size growing; the text at the
	// old offset then differs from tail.
	tail []byte
	// mark is the copy as it was before the last read. Once 3x-ui appends the
	// file to it, the text not read yet starts at its size plus off.
	mark os.FileInfo
	buf  []byte
}

func (f *follower) close() {
	if f != nil && f.file != nil {
		f.file.Close()
		f.file = nil
	}
}

// poll passes every complete line appended since the last poll; backlog is
// set for the lines read on opening the file, which may be old.
func (f *follower) poll(emit func(line []byte, backlog bool)) error {
	info, err := os.Stat(f.path)
	if err != nil {
		return err
	}
	if f.file != nil && !os.SameFile(f.info, info) {
		f.close()
	}
	backlog := f.file == nil
	if backlog {
		if f.file, err = os.Open(f.path); err != nil {
			return err
		}
		if info, err = f.file.Stat(); err != nil {
			f.close()
			return err
		}
		f.info, f.off, f.partial, f.skip, f.tail, f.mark = info, max(0, info.Size()-maxRead), nil, info.Size() > maxRead, nil, nil
		if f.off == 0 {
			f.readCopyTail(emit)
		}
	} else if info.Size() < f.off || !f.unchanged() || f.cleared(info.Size()) {
		f.recover(emit)
		f.off, f.partial, f.skip, f.tail = 0, f.partial[:0], false, f.tail[:0]
	}
	if f.copy != "" {
		f.mark, _ = os.Stat(f.copy)
	}
	f.off, err = f.read(f.file, f.off, math.MaxInt64, emit, backlog)
	return err
}

// readCopyTail passes the last lines of the copy.
func (f *follower) readCopyTail(emit func([]byte, bool)) {
	if f.copy == "" {
		return
	}
	c, err := os.Open(f.copy)
	if err != nil {
		return
	}
	defer c.Close()
	info, err := c.Stat()
	if err != nil {
		return
	}
	from := max(0, info.Size()-maxRead)
	f.skip = from > 0
	_, _ = f.read(c, from, info.Size(), emit, true)
	f.partial, f.skip, f.tail = f.partial[:0], false, f.tail[:0]
}

// cleared reports a truncation that the offset cannot show: one while
// nothing had been read since the file was last truncated or opened. The
// copy grew, and the file no longer starts with the text appended to it.
func (f *follower) cleared(size int64) bool {
	if f.off != 0 || f.mark == nil {
		return false
	}
	c, err := os.Open(f.copy)
	if err != nil {
		return false
	}
	defer c.Close()
	info, err := c.Stat()
	if err != nil || !os.SameFile(info, f.mark) || info.Size() <= f.mark.Size() {
		return false
	}
	n := min(size, info.Size()-f.mark.Size(), tailSize)
	head, appended := make([]byte, n), make([]byte, n)
	if _, err := f.file.ReadAt(head, 0); err != nil && err != io.EOF {
		return false
	}
	if _, err := c.ReadAt(appended, f.mark.Size()); err != nil && err != io.EOF {
		return false
	}
	return size == 0 || !bytes.Equal(head, appended)
}

// recover passes what the truncated file held past the offset, read back
// from the copy. The tail read last must sit right before that point in the
// copy; otherwise the copy was rotated or changed in another way, and the
// lines are lost.
func (f *follower) recover(emit func([]byte, bool)) {
	if f.mark == nil {
		return
	}
	c, err := os.Open(f.copy)
	if err != nil {
		return
	}
	defer c.Close()
	info, err := c.Stat()
	from := f.mark.Size() + f.off
	if err != nil || !os.SameFile(info, f.mark) || info.Size() < from {
		return
	}
	got := make([]byte, len(f.tail))
	if n, _ := c.ReadAt(got, from-int64(len(got))); n != len(got) || !bytes.Equal(got, f.tail) {
		return
	}
	_, _ = f.read(c, from, info.Size(), emit, false)
}

// read passes the lines of r from off up to limit, at most maxRead bytes,
// and returns where it stopped.
func (f *follower) read(r io.ReaderAt, off, limit int64, emit func([]byte, bool), backlog bool) (int64, error) {
	if f.buf == nil {
		f.buf = make([]byte, 64<<10)
	}
	line := func(b []byte) { emit(b, backlog) }
	for read := int64(0); read < maxRead && off < limit; {
		n, err := r.ReadAt(f.buf[:min(int64(len(f.buf)), limit-off)], off)
		off += int64(n)
		read += int64(n)
		f.split(f.buf[:n], line)
		f.remember(f.buf[:n])
		if err == io.EOF || n == 0 {
			return off, nil
		}
		if err != nil {
			return off, err
		}
	}
	return off, nil
}

func (f *follower) split(data []byte, emit func([]byte)) {
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			if f.skip || len(f.partial)+len(data) > maxLine {
				f.partial, f.skip = f.partial[:0], true
			} else {
				f.partial = append(f.partial, data...)
			}
			return
		}
		if !f.skip && len(f.partial)+i <= maxLine {
			emit(append(f.partial, data[:i]...))
		}
		f.partial, f.skip = f.partial[:0], false
		data = data[i+1:]
	}
}

const tailSize = 64

func (f *follower) remember(data []byte) {
	if len(data) >= tailSize {
		f.tail = append(f.tail[:0], data[len(data)-tailSize:]...)
		return
	}
	f.tail = append(f.tail, data...)
	if len(f.tail) > tailSize {
		f.tail = append(f.tail[:0], f.tail[len(f.tail)-tailSize:]...)
	}
}

// unchanged reports whether the text before the offset is still what was read.
func (f *follower) unchanged() bool {
	if len(f.tail) == 0 {
		return true
	}
	got := make([]byte, len(f.tail))
	n, _ := f.file.ReadAt(got, f.off-int64(len(f.tail)))
	return n == len(got) && bytes.Equal(got, f.tail)
}
