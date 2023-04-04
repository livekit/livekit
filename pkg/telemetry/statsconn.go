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

	return NewConn(conn, prometheus.Incoming), nil
}

type Conn struct {
	net.Conn
	direction prometheus.Direction
}

func NewConn(c net.Conn, direction prometheus.Direction) *Conn {
	prometheus.AddConnection(direction)
	return &Conn{Conn: c, direction: direction}
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

func (c *Conn) Close() error {
	prometheus.SubConnection(c.direction)
	return c.Conn.Close()
}

type PacketConn struct {
	net.PacketConn
	direction prometheus.Direction
}

func NewPacketConn(c net.PacketConn, direction prometheus.Direction) *PacketConn {
	prometheus.AddConnection(direction)
	return &PacketConn{PacketConn: c, direction: direction}
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

func (c *PacketConn) Close() error {
	prometheus.SubConnection(c.direction)
	return c.PacketConn.Close()
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

	return NewPacketConn(conn, prometheus.Outgoing), addr, err
}

func (g *RelayAddressGenerator) AllocateConn(network string, requestedPort int) (net.Conn, net.Addr, error) {
	conn, addr, err := g.RelayAddressGenerator.AllocateConn(network, requestedPort)
	if err != nil {
		return nil, addr, err
	}

	return NewConn(conn, prometheus.Outgoing), addr, err
}
