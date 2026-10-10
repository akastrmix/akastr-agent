package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// stallTimeout bounds how long a download may go without receiving a byte. A
// slow link may take as long as it needs, and what it has received is kept for
// the next attempt, so a node far from GitHub still finishes eventually.
const stallTimeout = time.Minute

func downloadClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: time.Minute,
	}}
}

// partialPath names the download kept between attempts after the binary it
// becomes, so a different target never continues another one's bytes.
func partialPath(stateDir, sha256 string) string {
	return filepath.Join(stateDir, "update-"+sha256+".partial")
}

// download fetches url into partial, continuing what an earlier attempt left,
// and returns the whole file once it has the approved digest. A file that does
// not match is discarded, so the next attempt starts over.
func download(ctx context.Context, client *http.Client, url, userAgent, partial, want string) ([]byte, error) {
	if err := os.MkdirAll(filepath.Dir(partial), 0o700); err != nil {
		return nil, err
	}
	removeOtherPartials(partial)
	file, err := os.OpenFile(partial, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	have := info.Size()
	if have > maxBinaryBytes {
		have = 0
	}
	if err := file.Truncate(have); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stall := time.AfterFunc(stallTimeout, cancel)
	defer stall.Stop()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", userAgent)
	if have > 0 {
		request.Header.Set("Range", "bytes="+strconv.FormatInt(have, 10)+"-")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusPartialContent && have > 0 && rangeStart(response) == have:
	case response.StatusCode == http.StatusRequestedRangeNotSatisfiable && have > 0:
		// Everything was received before; only the digest is left to check.
		return verified(file, partial, want)
	case response.StatusCode == http.StatusOK:
		have = 0
		if err := file.Truncate(0); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	if _, err := file.Seek(have, io.SeekStart); err != nil {
		return nil, err
	}
	body := &progress{reader: response.Body, stall: stall}
	if _, err := io.Copy(file, io.LimitReader(body, maxBinaryBytes-have+1)); err != nil {
		return nil, err
	}
	return verified(file, partial, want)
}

func verified(file *os.File, partial, want string) ([]byte, error) {
	contents, err := os.ReadFile(partial)
	if err != nil {
		return nil, err
	}
	if len(contents) > maxBinaryBytes || digest(contents) != want {
		_ = file.Truncate(0)
		return nil, errors.New("downloaded binary does not match the approved digest")
	}
	_ = os.Remove(partial)
	return contents, nil
}

// rangeStart reads the first byte position of a Content-Range response.
func rangeStart(response *http.Response) int64 {
	value, ok := strings.CutPrefix(response.Header.Get("Content-Range"), "bytes ")
	if !ok {
		return -1
	}
	start, _, _ := strings.Cut(value, "-")
	n, err := strconv.ParseInt(start, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

func removeOtherPartials(keep string) {
	others, _ := filepath.Glob(filepath.Join(filepath.Dir(keep), "update-*.partial"))
	for _, other := range others {
		if other != keep {
			_ = os.Remove(other)
		}
	}
}

// progress restarts the stall timer whenever bytes arrive.
type progress struct {
	reader io.Reader
	stall  *time.Timer
}

func (p *progress) Read(b []byte) (int, error) {
	n, err := p.reader.Read(b)
	if n > 0 {
		p.stall.Reset(stallTimeout)
	}
	return n, err
}
