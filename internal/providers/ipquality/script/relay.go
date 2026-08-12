package script

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	xproxy "golang.org/x/net/proxy"
)

type socksRelay struct {
	listener net.Listener
	cancel   context.CancelFunc
	wait     sync.WaitGroup
}

func startSOCKSRelay(parent context.Context, upstream string, credentials Profile) (*socksRelay, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen local SOCKS relay: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	relay := &socksRelay{listener: listener, cancel: cancel}
	relay.wait.Add(1)
	go func() {
		defer relay.wait.Done()
		for {
			connection, acceptError := listener.Accept()
			if acceptError != nil {
				return
			}
			relay.wait.Add(1)
			go func() {
				defer relay.wait.Done()
				defer connection.Close()
				_ = relayConnection(ctx, connection, upstream, credentials)
			}()
		}
	}()
	return relay, nil
}

func (r *socksRelay) URL() string {
	return "socks5://" + r.listener.Addr().String()
}

func (r *socksRelay) Close() {
	r.cancel()
	_ = r.listener.Close()
	r.wait.Wait()
}

func relayConnection(ctx context.Context, client net.Conn, upstream string, credentials Profile) error {
	_ = client.SetDeadline(time.Now().Add(30 * time.Second))
	header := make([]byte, 2)
	if _, err := io.ReadFull(client, header); err != nil || header[0] != 5 || header[1] == 0 {
		return errors.New("invalid SOCKS greeting")
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(client, methods); err != nil {
		return err
	}
	noAuth := false
	for _, method := range methods {
		if method == 0 {
			noAuth = true
		}
	}
	if !noAuth {
		_, _ = client.Write([]byte{5, 0xff})
		return errors.New("local SOCKS client did not offer no-auth")
	}
	if _, err := client.Write([]byte{5, 0}); err != nil {
		return err
	}
	request := make([]byte, 4)
	if _, err := io.ReadFull(client, request); err != nil || request[0] != 5 || request[1] != 1 || request[2] != 0 {
		return errors.New("invalid SOCKS connect request")
	}
	host, err := readSOCKSHost(client, request[3])
	if err != nil {
		return err
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(client, portBytes); err != nil {
		return err
	}
	destination := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(portBytes))))
	base := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	dialer, err := xproxy.SOCKS5("tcp", upstream, &xproxy.Auth{
		User: credentials.Username, Password: credentials.Password,
	}, base)
	if err != nil {
		return err
	}
	contextDialer, ok := dialer.(xproxy.ContextDialer)
	if !ok {
		return errors.New("upstream SOCKS dialer has no context support")
	}
	upstreamConnection, err := contextDialer.DialContext(ctx, "tcp", destination)
	if err != nil {
		_, _ = client.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return err
	}
	defer upstreamConnection.Close()
	if _, err := client.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}); err != nil {
		return err
	}
	_ = client.SetDeadline(time.Time{})
	copyDone := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstreamConnection, client); copyDone <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstreamConnection); copyDone <- struct{}{} }()
	select {
	case <-ctx.Done():
	case <-copyDone:
	}
	return nil
}

func readSOCKSHost(reader io.Reader, addressType byte) (string, error) {
	switch addressType {
	case 1:
		value := make([]byte, 4)
		if _, err := io.ReadFull(reader, value); err != nil {
			return "", err
		}
		return net.IP(value).String(), nil
	case 3:
		length := []byte{0}
		if _, err := io.ReadFull(reader, length); err != nil || length[0] == 0 {
			return "", errors.New("invalid SOCKS domain length")
		}
		value := make([]byte, int(length[0]))
		if _, err := io.ReadFull(reader, value); err != nil {
			return "", err
		}
		return string(value), nil
	case 4:
		value := make([]byte, 16)
		if _, err := io.ReadFull(reader, value); err != nil {
			return "", err
		}
		return net.IP(value).String(), nil
	default:
		return "", errors.New("unsupported SOCKS address type")
	}
}
