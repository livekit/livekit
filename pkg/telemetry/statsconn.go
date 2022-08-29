package telemetry

import (
	"net"

	"github.com/pion/turn/v2"

	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
)

type Listener struct {
	net.Listener
}

func NewListener(l net.Listener) *Listener {
	return &Listener{Listener: l}
}

func (l *Listener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	return NewConn(conn), nil
}

type Conn struct {
	net.Conn
}

func NewConn(c net.Conn) *Conn {
	return &Conn{Conn: c}
}

func (c *Conn) Read(b []byte) (n int, err error) {
	n, err = c.Conn.Read(b)
	if n > 0 {
		prometheus.IncrementBytes(prometheus.Incoming, uint64(n), false)
	}
	return
}

func (c *Conn) Write(b []byte) (n int, err error) {
	n, err = c.Conn.Write(b)
	if n > 0 {
		prometheus.IncrementBytes(prometheus.Outgoing, uint64(n), false)
	}
	return
}

type PacketConn struct {
	net.PacketConn
}

func NewPacketConn(c net.PacketConn) *PacketConn {
	return &PacketConn{PacketConn: c}
}

func (c *PacketConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	n, addr, err = c.PacketConn.ReadFrom(p)
	if n > 0 {
		prometheus.IncrementBytes(prometheus.Incoming, uint64(n), false)
		prometheus.IncrementPackets(prometheus.Incoming, 1, false)
	}
	return
}

func (c *PacketConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	n, err = c.PacketConn.WriteTo(p, addr)
	if n > 0 {
		prometheus.IncrementBytes(prometheus.Outgoing, uint64(n), false)
		prometheus.IncrementPackets(prometheus.Outgoing, 1, false)
	}
	return
}

type RelayAddressGenerator struct {
	turn.RelayAddressGenerator
}

func NewRelayAddressGenerator(g turn.RelayAddressGenerator) *RelayAddressGenerator {
	return &RelayAddressGenerator{RelayAddressGenerator: g}
}

func (g *RelayAddressGenerator) AllocatePacketConn(network string, requestedPort int) (net.PacketConn, net.Addr, error) {
	conn, addr, err := g.RelayAddressGenerator.AllocatePacketConn(network, requestedPort)
	if err != nil {
		return nil, addr, err
	}

	return NewPacketConn(conn), addr, err
}

func (g *RelayAddressGenerator) AllocateConn(network string, requestedPort int) (net.Conn, net.Addr, error) {
	conn, addr, err := g.RelayAddressGenerator.AllocateConn(network, requestedPort)
	if err != nil {
		return nil, addr, err
	}

	return NewConn(conn), addr, err
}
