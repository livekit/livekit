// Code generated by protoc-gen-psrpc v0.0.1, DO NOT EDIT.
// source: pkg/service/rpc/egress.proto

package rpc

import context "context"

import psrpc1 "github.com/livekit/psrpc"
import livekit "github.com/livekit/protocol/livekit"
import livekit3 "github.com/livekit/protocol/livekit"

// ===============================
// EgressInternal Client Interface
// ===============================

type EgressInternalClient interface {
	StartEgress(context.Context, *livekit3.StartEgressRequest, ...psrpc1.RequestOpt) (*livekit.EgressInfo, error)

	UpdateStream(context.Context, string, *livekit.UpdateStreamRequest, ...psrpc1.RequestOpt) (*livekit.EgressInfo, error)

	StopEgress(context.Context, string, *livekit.StopEgressRequest, ...psrpc1.RequestOpt) (*livekit.EgressInfo, error)

	SubscribeInfoUpdate(context.Context) (psrpc1.Subscription[*livekit.EgressInfo], error)
}

// ===================================
// EgressInternal ServerImpl Interface
// ===================================

type EgressInternalServerImpl interface {
	StartEgress(context.Context, *livekit3.StartEgressRequest) (*livekit.EgressInfo, error)

	UpdateStream(context.Context, *livekit.UpdateStreamRequest) (*livekit.EgressInfo, error)

	StopEgress(context.Context, *livekit.StopEgressRequest) (*livekit.EgressInfo, error)
}

// ===============================
// EgressInternal Server Interface
// ===============================

type EgressInternalServer interface {
	RegisterUpdateStreamTopic(string) error
	DeregisterUpdateStreamTopic(string) error

	RegisterStopEgressTopic(string) error
	DeregisterStopEgressTopic(string) error

	PublishInfoUpdate(context.Context, *livekit.EgressInfo) error
}

// =====================
// EgressInternal Client
// =====================

type egressInternalClient struct {
	client psrpc1.RPCClient
}

// NewEgressInternalClient creates a psrpc client that implements the EgressInternalClient interface.
func NewEgressInternalClient(clientID string, bus psrpc1.MessageBus, opts ...psrpc1.ClientOpt) (EgressInternalClient, error) {
	rpcClient, err := psrpc1.NewRPCClient("EgressInternal", clientID, bus, opts...)
	if err != nil {
		return nil, err
	}

	return &egressInternalClient{
		client: rpcClient,
	}, nil
}

func (c *egressInternalClient) StartEgress(ctx context.Context, req *livekit3.StartEgressRequest, opts ...psrpc1.RequestOpt) (*livekit.EgressInfo, error) {
	return psrpc1.RequestTopicSingle[*livekit.EgressInfo](ctx, c.client, "StartEgress", "", req, opts...)
}

func (c *egressInternalClient) UpdateStream(ctx context.Context, topic string, req *livekit.UpdateStreamRequest, opts ...psrpc1.RequestOpt) (*livekit.EgressInfo, error) {
	return psrpc1.RequestTopicSingle[*livekit.EgressInfo](ctx, c.client, "UpdateStream", topic, req, opts...)
}

func (c *egressInternalClient) StopEgress(ctx context.Context, topic string, req *livekit.StopEgressRequest, opts ...psrpc1.RequestOpt) (*livekit.EgressInfo, error) {
	return psrpc1.RequestTopicSingle[*livekit.EgressInfo](ctx, c.client, "StopEgress", topic, req, opts...)
}

func (c *egressInternalClient) SubscribeInfoUpdate(ctx context.Context) (psrpc1.Subscription[*livekit.EgressInfo], error) {
	return psrpc1.SubscribeTopicQueue[*livekit.EgressInfo](ctx, c.client, "InfoUpdate", "")
}

// =====================
// EgressInternal Server
// =====================

type egressInternalServer struct {
	svc EgressInternalServerImpl
	rpc psrpc1.RPCServer
}

// NewEgressInternalServer builds a RPCServer that can be used to handle
// requests that are routed to the right method in the provided svc implementation.
func NewEgressInternalServer(serverID string, svc EgressInternalServerImpl, bus psrpc1.MessageBus, opts ...psrpc1.ServerOpt) (EgressInternalServer, error) {
	rpcServer := psrpc1.NewRPCServer("EgressInternal", serverID, bus, opts...)

	var err error
	err = rpcServer.RegisterHandler(psrpc1.NewHandler("StartEgress", svc.StartEgress))
	if err != nil {
		rpcServer.Close()
		return nil, err
	}

	return &egressInternalServer{
		svc: svc,
		rpc: rpcServer,
	}, nil
}

func (s *egressInternalServer) RegisterUpdateStreamTopic(topic string) error {
	return s.rpc.RegisterHandler(psrpc1.NewTopicHandler("UpdateStream", topic, s.svc.UpdateStream))
}

func (s *egressInternalServer) DeregisterUpdateStreamTopic(topic string) error {
	return s.rpc.DeregisterTopic("UpdateStream", topic)
}

func (s *egressInternalServer) RegisterStopEgressTopic(topic string) error {
	return s.rpc.RegisterHandler(psrpc1.NewTopicHandler("StopEgress", topic, s.svc.StopEgress))
}

func (s *egressInternalServer) DeregisterStopEgressTopic(topic string) error {
	return s.rpc.DeregisterTopic("StopEgress", topic)
}

func (s *egressInternalServer) PublishInfoUpdate(ctx context.Context, msg *livekit.EgressInfo) error {
	return s.rpc.PublishTopic(ctx, "InfoUpdate", "", msg)
}

var psrpcFileDescriptor0 = []byte{
	// 249 bytes of a gzipped FileDescriptorProto
	0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0xff, 0xe2, 0x92, 0x29, 0xc8, 0x4e, 0xd7,
	0x2f, 0x4e, 0x2d, 0x2a, 0xcb, 0x4c, 0x4e, 0xd5, 0x2f, 0x2a, 0x48, 0xd6, 0x4f, 0x4d, 0x2f, 0x4a,
	0x2d, 0x2e, 0xd6, 0x2b, 0x28, 0xca, 0x2f, 0xc9, 0x17, 0x62, 0x2e, 0x2a, 0x48, 0x96, 0xe2, 0xcd,
	0x2f, 0x28, 0xc9, 0xcc, 0xcf, 0x83, 0x8a, 0x49, 0x49, 0xe5, 0x64, 0x96, 0xa5, 0x66, 0x67, 0x96,
	0xc4, 0x17, 0x15, 0x24, 0xc7, 0x67, 0xe6, 0x95, 0xa4, 0x16, 0xe5, 0x25, 0xe6, 0x40, 0xe5, 0x44,
	0x60, 0x72, 0xc8, 0xa6, 0x28, 0xb1, 0x73, 0xb1, 0xba, 0xe6, 0x16, 0x94, 0x54, 0x1a, 0xcd, 0x62,
	0xe2, 0xe2, 0x73, 0x05, 0xcb, 0x78, 0x42, 0xf5, 0x09, 0xd9, 0x73, 0x71, 0x07, 0x97, 0x24, 0x16,
	0x95, 0x40, 0x84, 0x85, 0xa4, 0xf5, 0xa0, 0x26, 0xe8, 0x21, 0x89, 0x06, 0xa5, 0x16, 0x96, 0xa6,
	0x16, 0x97, 0x48, 0x09, 0xc3, 0x25, 0x61, 0x86, 0xa4, 0xe5, 0x0b, 0x79, 0x72, 0xf1, 0x84, 0x16,
	0xa4, 0x24, 0x96, 0xa4, 0x06, 0x97, 0x14, 0xa5, 0x26, 0xe6, 0x0a, 0xc9, 0xc0, 0x15, 0x21, 0x0b,
	0xe3, 0x33, 0x42, 0x89, 0x6d, 0x53, 0x27, 0x23, 0x93, 0x04, 0xa3, 0x90, 0x2b, 0x17, 0x57, 0x70,
	0x49, 0x7e, 0x01, 0xd4, 0x29, 0x52, 0x48, 0x4e, 0x81, 0x09, 0x12, 0x65, 0x8c, 0x29, 0x17, 0x17,
	0x88, 0x0f, 0xb1, 0x5e, 0x88, 0x4b, 0xaf, 0xa8, 0x20, 0x59, 0x0f, 0xec, 0x7f, 0x3c, 0xda, 0x04,
	0x18, 0x9d, 0x74, 0xa2, 0xb4, 0xd2, 0x33, 0x4b, 0x32, 0x4a, 0x93, 0xf4, 0x92, 0xf3, 0x73, 0xf5,
	0xa1, 0x0a, 0xe1, 0x34, 0x5a, 0x34, 0x25, 0xb1, 0x81, 0x83, 0xd6, 0x18, 0x10, 0x00, 0x00, 0xff,
	0xff, 0x43, 0x80, 0x51, 0x6f, 0xc0, 0x01, 0x00, 0x00,
}
