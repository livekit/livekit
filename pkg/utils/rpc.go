package utils

import (
	"fmt"
	"time"
)

type RpcErrorCode uint32

const (
	RpcApplicationError RpcErrorCode = 1500 + iota
	RpcConnectionTimeout
	RpcResponseTimeout
	RpcRecipientDisconnected
	RpcResponsePayloadTooLarge
	RpcSendFailed
)

const (
	RpcUnsupportedMethod RpcErrorCode = 1400 + iota
	RpcRecipientNotFound
	RpcRequestPayloadTooLarge
	RpcUnsupportedServer
	RpcUnsupportedVersion
)

const (
	RpcMaxRoundTripLatency = 2000 * time.Millisecond
	RpcMaxMessageBytes     = 256
	RpcMaxDataBytes        = 15360 // 15KiB
	RpcMaxPayloadBytes     = 15360 // 15KiB
)

type RpcError struct {
	Code    RpcErrorCode
	Message string
	Data    *string
}

func (e *RpcError) Error() string {
	return fmt.Sprintf("RpcError %d: %s", e.Code, e.Message)
}

type RpcPendingAckHandler struct {
	Resolve             func()
	ParticipantIdentity string
}

type RpcPendingResponseHandler struct {
	Resolve             func(payload *string, err *RpcError)
	ParticipantIdentity string
}
