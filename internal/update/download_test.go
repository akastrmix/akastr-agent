package update

import (
	"bytes"
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"
)

// A node on a slow link loses its connection part way; the next attempt must
// continue from the bytes it kept instead of starting over forever.
func TestInterruptedDownloadResumesFromKeptBytes(t *testing.T) {
	binary := make([]byte, 256*1024)
	_, _ = rand.Read(binary)
	half := len(binary) / 2
	var ranges []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranges = append(ranges, r.Header.Get("Range"))
		if len(ranges) == 1 {
			w.Header().Set("Content-Length", strconv.Itoa(len(binary)))
			_, _ = w.Write(binary[:half])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
		http.ServeContent(w, r, "agent", time.Time{}, bytes.NewReader(binary))
	}))
	defer server.Close()
	partial := partialPath(t.TempDir(), digest(binary))

	if _, err := download(context.Background(), server.Client(), server.URL, "test", partial, digest(binary)); err == nil {
		t.Fatal("an interrupted download succeeded")
	}
	if info, err := os.Stat(partial); err != nil || info.Size() != int64(half) {
		t.Fatalf("kept bytes %v %v", info, err)
	}
	got, err := download(context.Background(), server.Client(), server.URL, "test", partial, digest(binary))
	if err != nil || !bytes.Equal(got, binary) {
		t.Fatalf("resumed download: %v", err)
	}
	if ranges[1] != "bytes="+strconv.Itoa(half)+"-" {
		t.Fatalf("second request asked for %q", ranges[1])
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatal("finished download left its partial file")
	}
}
