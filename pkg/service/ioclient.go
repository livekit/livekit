package service

import (
	"context"

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
)

type ioClient struct {
	io *IOInfoService
}

func NewIOClient(io *IOInfoService) rpc.IOInfoClient {
	return &ioClient{io: io}
}

func (c *ioClient) CreateEgress(ctx context.Context, req *livekit.EgressInfo, _ ...psrpc.RequestOption) (*emptypb.Empty, error) {
	return c.io.CreateEgress(ctx, req)
}

func (c *ioClient) UpdateEgress(ctx context.Context, req *livekit.EgressInfo, _ ...psrpc.RequestOption) (*emptypb.Empty, error) {
	return c.io.UpdateEgress(ctx, req)
}

func (c *ioClient) GetEgress(ctx context.Context, req *rpc.GetEgressRequest, _ ...psrpc.RequestOption) (*livekit.EgressInfo, error) {
	return c.io.GetEgress(ctx, req)
}

func (c *ioClient) ListEgress(ctx context.Context, req *livekit.ListEgressRequest, _ ...psrpc.RequestOption) (*livekit.ListEgressResponse, error) {
	return c.io.ListEgress(ctx, req)
}

func (c *ioClient) GetIngressInfo(ctx context.Context, req *rpc.GetIngressInfoRequest, _ ...psrpc.RequestOption) (*rpc.GetIngressInfoResponse, error) {
	return c.io.GetIngressInfo(ctx, req)
}

func (c *ioClient) UpdateIngressState(ctx context.Context, req *rpc.UpdateIngressStateRequest, _ ...psrpc.RequestOption) (*emptypb.Empty, error) {
	return c.io.UpdateIngressState(ctx, req)
}

func (c *ioClient) UpdateEgressInfo(ctx context.Context, req *livekit.EgressInfo, _ ...psrpc.RequestOption) (*emptypb.Empty, error) {
	return c.io.UpdateEgressInfo(ctx, req)
}
