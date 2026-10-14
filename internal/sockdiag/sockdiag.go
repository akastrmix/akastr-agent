// Package sockdiag reads the host's TCP connections from the kernel through
// netlink sock_diag, the interface ss uses, without running any program.
package sockdiag

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"syscall"
)

// Conn is one TCP connection as the kernel counts it.
type Conn struct {
	// Cookie tells connections apart for their whole life, even when an
	// address pair is reused.
	Cookie        uint64
	Local, Remote netip.AddrPort
	// BytesAcked is what the peer acknowledged of the bytes sent and
	// BytesReceived what arrived. Both include protocol overhead and only grow.
	BytesAcked, BytesReceived uint64
}

const (
	netlinkSockDiag  = 4  // NETLINK_SOCK_DIAG
	sockDiagByFamily = 20 // SOCK_DIAG_BY_FAMILY
	inetDiagInfo     = 2  // INET_DIAG_INFO: struct tcp_info
	diagRequestSize  = 56 // struct inet_diag_req_v2
	diagMessageSize  = 72 // struct inet_diag_msg
	// tcp_info offsets of tcpi_bytes_acked and tcpi_bytes_received (Linux 4.1).
	bytesAckedAt    = 120
	bytesReceivedAt = 128
	// Multicast groups SKNLGRP_INET_TCP_DESTROY (1) and SKNLGRP_INET6_TCP_DESTROY
	// (3), as a bind mask.
	tcpDestroyGroups = 1<<0 | 1<<2
	// Connections that still carry data: ESTABLISHED, FIN_WAIT1, FIN_WAIT2,
	// CLOSE_WAIT, LAST_ACK and CLOSING. TIME_WAIT has no tcp_info.
	carryingStates = 1<<1 | 1<<4 | 1<<5 | 1<<8 | 1<<9 | 1<<11
)

// TCP lists the host's TCP connections that still carry data, with their
// byte counters.
func TCP() ([]Conn, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, netlinkSockDiag)
	if err != nil {
		return nil, fmt.Errorf("open sock_diag: %w", err)
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("bind sock_diag: %w", err)
	}
	// A dump that never finishes must not stall the caller.
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &syscall.Timeval{Sec: 5}); err != nil {
		return nil, fmt.Errorf("configure sock_diag: %w", err)
	}
	var out []Conn
	for seq, family := range []uint8{syscall.AF_INET, syscall.AF_INET6} {
		if err := dumpFamily(fd, family, uint32(seq+1), &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Closed calls f with the final counters of each TCP connection the kernel
// destroys, until ctx ends. Polling cannot see what a connection carried after
// its last poll; this can. When the kernel cannot queue a notice, it is lost.
func Closed(ctx context.Context, f func(Conn)) error {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, netlinkSockDiag)
	if err != nil {
		return fmt.Errorf("open sock_diag: %w", err)
	}
	defer syscall.Close(fd)
	// Bursts of closing connections queue here; a larger buffer loses fewer.
	_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUFFORCE, 4<<20)
	// Wake up regularly to notice ctx ending.
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &syscall.Timeval{Sec: 1}); err != nil {
		return fmt.Errorf("configure sock_diag: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK, Groups: tcpDestroyGroups}); err != nil {
		return fmt.Errorf("subscribe sock_diag: %w", err)
	}
	buf := make([]byte, 1<<16)
	for ctx.Err() == nil {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		switch {
		case errors.Is(err, syscall.EAGAIN), errors.Is(err, syscall.EINTR), errors.Is(err, syscall.ENOBUFS):
			continue
		case err != nil:
			return fmt.Errorf("read sock_diag: %w", err)
		}
		messages, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			continue
		}
		for _, m := range messages {
			if m.Header.Type != sockDiagByFamily {
				continue
			}
			if c, ok := parse(m.Data); ok {
				f(c)
			}
		}
	}
	return ctx.Err()
}

func dumpFamily(fd int, family uint8, seq uint32, out *[]Conn) error {
	request := make([]byte, syscall.NLMSG_HDRLEN+diagRequestSize)
	binary.NativeEndian.PutUint32(request[0:], uint32(len(request)))
	binary.NativeEndian.PutUint16(request[4:], sockDiagByFamily)
	binary.NativeEndian.PutUint16(request[6:], syscall.NLM_F_REQUEST|syscall.NLM_F_DUMP)
	binary.NativeEndian.PutUint32(request[8:], seq)
	body := request[syscall.NLMSG_HDRLEN:]
	body[0], body[1], body[2] = family, syscall.IPPROTO_TCP, 1<<(inetDiagInfo-1)
	binary.NativeEndian.PutUint32(body[4:], carryingStates)
	if err := syscall.Sendto(fd, request, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return fmt.Errorf("request sock_diag: %w", err)
	}
	buf := make([]byte, 1<<16)
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read sock_diag: %w", err)
		}
		messages, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return fmt.Errorf("parse sock_diag: %w", err)
		}
		for _, m := range messages {
			if m.Header.Seq != seq {
				continue
			}
			switch m.Header.Type {
			case syscall.NLMSG_DONE:
				return nil
			case syscall.NLMSG_ERROR:
				if len(m.Data) >= 4 {
					if errno := -int32(binary.NativeEndian.Uint32(m.Data)); errno != 0 {
						return fmt.Errorf("sock_diag: %w", syscall.Errno(errno))
					}
				}
				return errors.New("sock_diag reported an error")
			}
			if c, ok := parse(m.Data); ok {
				*out = append(*out, c)
			}
		}
	}
}

// parse reads one inet_diag_msg and its INET_DIAG_INFO attribute. A message
// without tcp_info long enough for the byte counters is skipped.
func parse(data []byte) (Conn, bool) {
	if len(data) < diagMessageSize {
		return Conn{}, false
	}
	family := data[0]
	size := 4
	if family == syscall.AF_INET6 {
		size = 16
	} else if family != syscall.AF_INET {
		return Conn{}, false
	}
	local, _ := netip.AddrFromSlice(data[8 : 8+size])
	remote, _ := netip.AddrFromSlice(data[24 : 24+size])
	c := Conn{
		Cookie: uint64(binary.NativeEndian.Uint32(data[44:])) | uint64(binary.NativeEndian.Uint32(data[48:]))<<32,
		Local:  netip.AddrPortFrom(local.Unmap(), binary.BigEndian.Uint16(data[4:])),
		Remote: netip.AddrPortFrom(remote.Unmap(), binary.BigEndian.Uint16(data[6:])),
	}
	for attrs := data[diagMessageSize:]; len(attrs) >= syscall.SizeofRtAttr; {
		length := int(binary.NativeEndian.Uint16(attrs[0:]))
		if length < syscall.SizeofRtAttr || length > len(attrs) {
			return Conn{}, false
		}
		if binary.NativeEndian.Uint16(attrs[2:]) == inetDiagInfo {
			info := attrs[syscall.SizeofRtAttr:length]
			if len(info) < bytesReceivedAt+8 {
				return Conn{}, false
			}
			c.BytesAcked = binary.NativeEndian.Uint64(info[bytesAckedAt:])
			c.BytesReceived = binary.NativeEndian.Uint64(info[bytesReceivedAt:])
			return c, true
		}
		attrs = attrs[min((length+3)&^3, len(attrs)):]
	}
	return Conn{}, false
}
