package daemon

import (
	"errors"
	"net"
	"os"
	"strings"
)

// notifyReady tells systemd the service started; status is what `systemctl status` shows.
func notifyReady(status string) error {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return nil
	}
	if strings.HasPrefix(socket, "@") {
		socket = "\x00" + strings.TrimPrefix(socket, "@")
	}
	connection, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer connection.Close()
	message := "READY=1\nSTATUS=" + status
	written, err := connection.Write([]byte(message))
	if err != nil {
		return err
	}
	if written != len(message) {
		return errors.New("short systemd notification write")
	}
	return nil
}
