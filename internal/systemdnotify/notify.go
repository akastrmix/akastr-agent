package systemdnotify

import (
	"errors"
	"net"
	"os"
	"strings"
)

func Ready() error {
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
	written, err := connection.Write([]byte("READY=1\nSTATUS=Control connection ready"))
	if err != nil {
		return err
	}
	if written != len("READY=1\nSTATUS=Control connection ready") {
		return errors.New("short systemd notification write")
	}
	return nil
}
