package accesslog

import (
	"bytes"
	"io"
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
type follower struct {
	path    string
	file    *os.File
	info    os.FileInfo
	off     int64
	partial []byte
	// skip drops text up to the next newline: a line whose start was not read.
	skip bool
	// tail is the last text read. A truncation followed within one poll by
	// more text than was read before keeps the size growing; the text at the
	// old offset then differs from tail.
	tail []byte
	buf  []byte
}

func (f *follower) close() {
	if f != nil && f.file != nil {
		f.file.Close()
		f.file = nil
	}
}

// poll passes every complete line appended since the last poll.
func (f *follower) poll(emit func([]byte)) error {
	info, err := os.Stat(f.path)
	if err != nil {
		return err
	}
	if f.file != nil && !os.SameFile(f.info, info) {
		f.close()
	}
	if f.file == nil {
		if f.file, err = os.Open(f.path); err != nil {
			return err
		}
		if info, err = f.file.Stat(); err != nil {
			f.close()
			return err
		}
		f.info, f.off, f.partial, f.skip = info, max(0, info.Size()-maxRead), nil, info.Size() > maxRead
	}
	if info.Size() < f.off || !f.unchanged() {
		f.off, f.partial, f.skip, f.tail = 0, nil, false, nil
	}
	if f.buf == nil {
		f.buf = make([]byte, 64<<10)
	}
	for read := int64(0); read < maxRead; {
		n, err := f.file.ReadAt(f.buf, f.off)
		f.off += int64(n)
		read += int64(n)
		f.split(f.buf[:n], emit)
		f.remember(f.buf[:n])
		if err == io.EOF || n == 0 {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
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
