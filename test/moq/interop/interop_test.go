// Copyright 2026 LiveKit, Inc.
// SPDX-License-Identifier: Apache-2.0

package interop

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/trickstercache/trickster/v2/pkg/moq/codec"
	"github.com/trickstercache/trickster/v2/pkg/moq/message"
	"github.com/trickstercache/trickster/v2/pkg/moq/session"
)

// One authenticated QUIC connection carries a publication from Rust and a
// subscription to its echo. The server streams only the bytes it received.
func TestDraft23RoundTrip(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certFile := filepath.Join(t.TempDir(), "server.der")
	if err := os.WriteFile(certFile, der, 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := quic.ListenAddr("127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		NextProtos:   []string{"moqt-23"},
	}, &quic.Config{EnableDatagrams: true})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	peerDone := make(chan error, 1)
	go func() { peerDone <- serve(ctx, listener) }()
	client := exec.CommandContext(ctx, "cargo", "run", "--locked", "--quiet", "--", listener.Addr().String(), certFile)
	output, clientErr := client.CombinedOutput()
	if clientErr != nil {
		cancel()
	}
	select {
	case serverErr := <-peerDone:
		if serverErr != nil || clientErr != nil {
			t.Fatalf("server: %v, Rust client: %v\n%s", serverErr, clientErr, output)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("server did not finish; Rust client: %v\n%s", clientErr, output)
	}
}

func serve(ctx context.Context, listener *quic.Listener) error {
	conn, err := listener.Accept(ctx)
	if err != nil {
		return err
	}
	defer conn.CloseWithError(0, "test ended")
	if proto := conn.ConnectionState().TLS.NegotiatedProtocol; proto != "moqt-23" {
		return fmt.Errorf("unexpected ALPN %q", proto)
	}
	c, ok := codec.ForDraft(23)
	if !ok {
		return errors.New("draft 23 unavailable")
	}
	echo := &echoHandler{payload: make(chan []byte, 1), errs: make(chan error, 1)}
	s, err := session.New(session.FromQUIC(conn), session.Options{Role: session.RoleServer, Codec: c, Handler: echo})
	if err != nil {
		return err
	}
	if err = s.Start(ctx); err != nil {
		return err
	}
	select {
	case err = <-echo.errs:
		return err
	case <-s.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type echoHandler struct {
	payload chan []byte
	errs    chan error
}

func (h *echoHandler) fail(err error) {
	select {
	case h.errs <- err:
	default:
	}
}

func (h *echoHandler) HandleRequest(r *session.Request) {
	switch m := r.Message().(type) {
	case *message.Publish:
		if !matches(m.Track, "up") {
			break
		}
		r.SetHandlers(nil, h)
		if err := r.Send(&message.RequestOK{}); err != nil {
			h.fail(err)
		}
		return
	case *message.Subscribe:
		if !matches(m.Track, "down") {
			break
		}
		if err := h.sendEcho(r); err != nil {
			h.fail(err)
		}
		return
	}
	if err := r.Reject(message.RequestDoesNotExist, "unknown test track", 0); err != nil {
		h.fail(err)
	}
}

func matches(track message.FullTrackName, name string) bool {
	return len(track.Namespace) == 2 &&
		string(track.Namespace[0]) == "livekit" &&
		string(track.Namespace[1]) == "interop" &&
		string(track.Name) == name
}

func (h *echoHandler) sendEcho(r *session.Request) error {
	if err := r.Send(&message.SubscribeOK{TrackAlias: r.Session().NewTrackAlias()}); err != nil {
		return err
	}
	var payload []byte
	select {
	case payload = <-h.payload:
	case <-r.Context().Done():
		return r.Context().Err()
	}
	stream, err := r.OpenSubgroup(r.Context(), message.SubgroupHeader{Group: 0, EndOfGroup: true, Priority: 128}, 3, false)
	if err != nil {
		return err
	}
	if err = stream.WriteObject(&message.SubgroupObject{PayloadLen: uint64(len(payload))}); err != nil {
		return err
	}
	if _, err = stream.Write(payload); err != nil {
		return err
	}
	return stream.Close()
}

func (h *echoHandler) HandleSubgroup(r *session.Request, in *session.IncomingSubgroup) {
	obj, err := in.Next()
	if err != nil {
		h.fail(err)
		return
	}
	if obj.PayloadLen > 1024 || obj.ID != 0 {
		h.fail(fmt.Errorf("unexpected object ID %d and length %d", obj.ID, obj.PayloadLen))
		return
	}
	payload := make([]byte, int(obj.PayloadLen))
	if _, err = io.ReadFull(in, payload); err != nil {
		h.fail(err)
		return
	}
	if !bytes.Equal(payload, []byte("LiveKit MoQ interoperability\x00\xff")) {
		h.fail(errors.New("incorrect upstream payload"))
		return
	}
	select {
	case h.payload <- payload:
	case <-r.Context().Done():
	}
}

func (*echoHandler) HandleGoAway(*session.Session, *message.GoAway) {}

func (h *echoHandler) HandleDatagram(*session.Request, *message.Datagram) {
	h.fail(errors.New("unexpected datagram"))
}

func (h *echoHandler) HandleFetch(*session.Request, *session.IncomingFetch) {
	h.fail(errors.New("unexpected fetch"))
}
