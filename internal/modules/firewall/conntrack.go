package firewall

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"syscall"
	"time"
)

// Tracked connections of a forward keep their translation until they end
// (UDP about two minutes, TCP five days), so after a forward is removed or
// its target changes, the old connections are deleted over netlink: nodes
// have no conntrack program (Cloud ADR 0110). The IPv4 table is dumped and
// each entry this module translated is deleted by its original tuple and id,
// so an entry recycled in between is left alone.
const (
	ctSubsystem = 1 // NFNL_SUBSYS_CTNETLINK
	ctGet       = 1
	ctDelete    = 2

	ctaTupleOrig  = 1
	ctaTupleReply = 2
	ctaStatus     = 3
	ctaID         = 12
	ctaZone       = 18
	ctaTupleIP    = 1
	ctaTupleProto = 2
	ctaIPv4Src    = 1
	ctaProtoNum   = 1
	ctaProtoSrc   = 2
	ctaProtoDst   = 3

	// ipsDstNAT marks an entry whose destination was translated: only those
	// are a forward's. The node's own connection to the same target and port
	// is not.
	ipsDstNAT = 1 << 5

	nlaNested   = 0x8000
	nlaTypeMask = 0x3fff
	netlinkWait = 10 * time.Second
)

func openNetlink() (int, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_NETFILTER)
	if err != nil {
		return -1, err
	}
	timeout := syscall.NsecToTimeval(netlinkWait.Nanoseconds())
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &timeout); err != nil {
		syscall.Close(fd)
		return -1, err
	}
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		syscall.Close(fd)
		return -1, err
	}
	return fd, nil
}

type tracked struct {
	protocol   uint8
	port       uint16 // original destination
	reply      netip.AddrPort
	status     uint32
	id         uint32
	orig, zone []byte
}

func (s *system) forget(ctx context.Context, protocol string, port int, to netip.AddrPort) error {
	number := map[string]uint8{"tcp": syscall.IPPROTO_TCP, "udp": syscall.IPPROTO_UDP}[protocol]
	fd, err := s.netlink()
	if err != nil {
		return fmt.Errorf("connection tracking socket: %w", err)
	}
	defer syscall.Close(fd)
	n := &ctNetlink{fd: fd}
	var matches []tracked
	err = n.exchange(ctGet, syscall.NLM_F_REQUEST|syscall.NLM_F_DUMP, nil, func(payload []byte) {
		if e, ok := parseTracked(payload); ok && e.protocol == number && int(e.port) == port && e.reply == to && e.status&ipsDstNAT != 0 {
			matches = append(matches, e)
		}
	})
	if err != nil {
		return fmt.Errorf("list tracked connections: %w", err)
	}
	for _, e := range matches {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		id := binary.BigEndian.AppendUint32(nil, e.id)
		attrs := append(attribute(ctaTupleOrig|nlaNested, e.orig), attribute(ctaID, id)...)
		if e.zone != nil {
			attrs = append(attrs, attribute(ctaZone, e.zone)...)
		}
		err := n.exchange(ctDelete, syscall.NLM_F_REQUEST|syscall.NLM_F_ACK, attrs, func([]byte) {})
		if err != nil && !errors.Is(err, syscall.ENOENT) {
			return fmt.Errorf("delete a tracked connection: %w", err)
		}
	}
	return nil
}

type ctNetlink struct {
	fd  int
	seq uint32
}

// exchange sends one request and hands each answer to each until the dump
// ends or the acknowledgement arrives.
func (n *ctNetlink) exchange(kind, flags uint16, attrs []byte, each func([]byte)) error {
	n.seq++
	message := make([]byte, 20, 20+len(attrs))
	binary.NativeEndian.PutUint32(message[0:], uint32(20+len(attrs)))
	binary.NativeEndian.PutUint16(message[4:], ctSubsystem<<8|kind)
	binary.NativeEndian.PutUint16(message[6:], flags)
	binary.NativeEndian.PutUint32(message[8:], n.seq)
	message[16] = syscall.AF_INET // nfgenmsg: family, version 0, resource 0
	message = append(message, attrs...)
	if err := syscall.Sendto(n.fd, message, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return err
	}
	buf := make([]byte, 1<<16)
	for {
		size, _, err := syscall.Recvfrom(n.fd, buf, 0)
		if err != nil {
			return err
		}
		for b := buf[:size]; len(b) >= 16; {
			length := int(binary.NativeEndian.Uint32(b[0:]))
			if length < 16 || length > len(b) {
				return errors.New("malformed netlink message")
			}
			kind, seq, payload := binary.NativeEndian.Uint16(b[4:]), binary.NativeEndian.Uint32(b[8:]), b[16:length]
			b = b[min(align(length), len(b)):]
			if seq != n.seq {
				continue
			}
			switch kind {
			case syscall.NLMSG_DONE:
				return nil
			case syscall.NLMSG_ERROR:
				if len(payload) < 4 {
					return errors.New("short netlink error")
				}
				if errno := -int32(binary.NativeEndian.Uint32(payload)); errno != 0 {
					return syscall.Errno(errno)
				}
				return nil
			default:
				if len(payload) >= 4 {
					each(payload[4:])
				}
			}
		}
	}
}

func align(n int) int { return (n + 3) &^ 3 }

func attribute(kind uint16, payload []byte) []byte {
	b := make([]byte, align(4+len(payload)))
	binary.NativeEndian.PutUint16(b[0:], uint16(4+len(payload)))
	binary.NativeEndian.PutUint16(b[2:], kind)
	copy(b[4:], payload)
	return b
}

func attributes(b []byte) map[uint16][]byte {
	result := map[uint16][]byte{}
	for len(b) >= 4 {
		length := int(binary.NativeEndian.Uint16(b[0:]))
		if length < 4 || length > len(b) {
			break
		}
		result[binary.NativeEndian.Uint16(b[2:])&nlaTypeMask] = b[4:length]
		b = b[min(align(length), len(b)):]
	}
	return result
}

func parseTracked(payload []byte) (tracked, bool) {
	a := attributes(payload)
	orig, reply := attributes(a[ctaTupleOrig]), attributes(a[ctaTupleReply])
	origProto := attributes(orig[ctaTupleProto])
	replyIP, replyProto := attributes(reply[ctaTupleIP]), attributes(reply[ctaTupleProto])
	if len(origProto[ctaProtoNum]) != 1 || len(origProto[ctaProtoDst]) != 2 || len(replyIP[ctaIPv4Src]) != 4 ||
		len(replyProto[ctaProtoSrc]) != 2 || len(a[ctaStatus]) != 4 || len(a[ctaID]) != 4 {
		return tracked{}, false
	}
	e := tracked{
		protocol: origProto[ctaProtoNum][0],
		port:     binary.BigEndian.Uint16(origProto[ctaProtoDst]),
		reply:    netip.AddrPortFrom(netip.AddrFrom4([4]byte(replyIP[ctaIPv4Src])), binary.BigEndian.Uint16(replyProto[ctaProtoSrc])),
		status:   binary.BigEndian.Uint32(a[ctaStatus]),
		id:       binary.BigEndian.Uint32(a[ctaID]),
		orig:     append([]byte(nil), a[ctaTupleOrig]...),
	}
	if zone := a[ctaZone]; zone != nil {
		e.zone = append([]byte(nil), zone...)
	}
	return e, true
}
