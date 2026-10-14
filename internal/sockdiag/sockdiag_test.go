package sockdiag

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"syscall"
	"testing"
	"time"
)

// The kernel's counters for a real connection, found by its addresses, cover
// what each side sent, and the two ends are different connections.
func TestTCPCountsARealConnection(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		t.Run(network, func(t *testing.T) {
			host := "127.0.0.1:0"
			if network == "tcp6" {
				host = "[::1]:0"
			}
			listener, err := net.Listen(network, host)
			if err != nil {
				t.Skipf("no %s loopback: %v", network, err)
			}
			defer listener.Close()
			client, err := net.Dial(network, listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			server, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			const down, up = 3 << 20, 1 << 20
			go func() { _, _ = server.Write(make([]byte, down)) }()
			go func() { _, _ = client.Write(make([]byte, up)) }()
			if _, err := io.ReadFull(client, make([]byte, down)); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(server, make([]byte, up)); err != nil {
				t.Fatal(err)
			}
			local := netip.MustParseAddrPort(server.LocalAddr().String())
			remote := netip.MustParseAddrPort(server.RemoteAddr().String())
			deadline := time.Now().Add(2 * time.Second)
			for {
				conns, err := TCP()
				if err != nil {
					t.Fatal(err)
				}
				var ours, peer *Conn
				for i, c := range conns {
					if c.Local == local && c.Remote == remote {
						ours = &conns[i]
					}
					if c.Local == remote && c.Remote == local {
						peer = &conns[i]
					}
				}
				if ours == nil || peer == nil {
					t.Fatalf("connection not listed: %v %v", ours, peer)
				}
				if ours.Cookie == peer.Cookie {
					t.Fatal("both ends share a cookie")
				}
				if ours.BytesAcked >= down && ours.BytesReceived >= up {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("counters %d acked, %d received", ours.BytesAcked, ours.BytesReceived)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

// A closing connection reports its final counters, including what it carried
// after any poll could have seen it.
func TestClosedReportsFinalCounters(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closed := make(chan Conn, 64)
	done := make(chan error, 1)
	go func() { done <- Closed(ctx, func(c Conn) { closed <- c }) }()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	const down = 2 << 20
	// The subscription starts asynchronously; retry until a close is seen.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		client, err := net.Dial("tcp4", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		server, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		local := netip.MustParseAddrPort(server.LocalAddr().String())
		remote := netip.MustParseAddrPort(server.RemoteAddr().String())
		go func() { _, _ = server.Write(make([]byte, down)); server.Close() }()
		if _, err := io.ReadFull(client, make([]byte, down)); err != nil {
			t.Fatal(err)
		}
		client.Close()
		wait := time.After(300 * time.Millisecond)
	collect:
		for {
			select {
			case c := <-closed:
				if c.Local == local && c.Remote == remote {
					if c.BytesAcked < down {
						t.Fatalf("final counters %d acked", c.BytesAcked)
					}
					cancel()
					if err := <-done; !errors.Is(err, context.Canceled) {
						t.Fatalf("Closed returned %v", err)
					}
					return
				}
			case err := <-done:
				t.Skipf("no close notifications here: %v", err)
			case <-wait:
				break collect
			}
		}
	}
	t.Fatal("no close notification arrived")
}

// A kernel whose tcp_info stops before the byte counters, or a damaged
// attribute, yields no connection instead of a wrong count or a crash.
func TestParseSkipsIncompleteMessages(t *testing.T) {
	message := func(infoLength int) []byte {
		data := make([]byte, diagMessageSize+syscall.SizeofRtAttr+infoLength)
		data[0] = syscall.AF_INET
		attr := data[diagMessageSize:]
		binary.NativeEndian.PutUint16(attr[0:], uint16(syscall.SizeofRtAttr+infoLength))
		binary.NativeEndian.PutUint16(attr[2:], inetDiagInfo)
		return data
	}
	if _, ok := parse(message(bytesReceivedAt + 8)); !ok {
		t.Fatal("complete message rejected")
	}
	if _, ok := parse(message(bytesAckedAt)); ok {
		t.Fatal("short tcp_info accepted")
	}
	damaged := message(bytesReceivedAt + 8)
	binary.NativeEndian.PutUint16(damaged[diagMessageSize:], 0xffff)
	if _, ok := parse(damaged); ok {
		t.Fatal("damaged attribute accepted")
	}
	if _, ok := parse(damaged[:diagMessageSize-1]); ok {
		t.Fatal("short message accepted")
	}
}
