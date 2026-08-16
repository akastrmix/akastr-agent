package systemdnotify

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestReadySendsSystemdNotification(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix datagram sockets are not available on Windows")
	}
	socketPath := filepath.Join(t.TempDir(), "notify.sock")
	listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: socketPath, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("NOTIFY_SOCKET", socketPath)
	if err := Ready(); err != nil {
		t.Fatal(err)
	}
	if err := listener.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 128)
	read, _, err := listener.ReadFromUnix(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if string(buffer[:read]) != "READY=1\nSTATUS=Control connection ready" {
		t.Fatalf("unexpected notification %q", buffer[:read])
	}
}

func TestReadyIsNoopOutsideSystemd(t *testing.T) {
	_ = os.Unsetenv("NOTIFY_SOCKET")
	if err := Ready(); err != nil {
		t.Fatal(err)
	}
}
