package minecraft

import (
	"github.com/sandertv/gophertunnel/minecraft"
	"strings"
)

func Connect(target string) (*minecraft.Conn, error) {
	if len(strings.Split(target, ":")) < 2 {
		target = target + ":19132"
	}

	serverConn, err := minecraft.Dialer{
		TokenSource: src,
	}.Dial("raknet", target)
	if err != nil {
		return nil, err
	}

	return serverConn, serverConn.DoSpawn()
}
