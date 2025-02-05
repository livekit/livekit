/*
 * Copyright 2022 LiveKit, Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package service

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/twitchtv/twirp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/protocol/livekit"
)

type twirpRequestFields struct {
	service string
	method  string
	error   twirp.Error
}

// --------------------------------------------------------------------------

type twirpLoggerKey struct{}

// logging handling inspired by https://github.com/bakins/twirpzap
// License: Apache-2.0
func TwirpLogger() *twirp.ServerHooks {
	loggerPool := &sync.Pool{
		New: func() interface{} {
			return &twirpLogger{
				fieldsOrig: make([]interface{}, 0, 30),
			}
		},
	}
	return &twirp.ServerHooks{
		RequestReceived: func(ctx context.Context) (context.Context, error) {
			return loggerRequestReceived(ctx, loggerPool)
		},
		RequestRouted: loggerRequestRouted,
		Error:         loggerErrorReceived,
		ResponseSent: func(ctx context.Context) {
			loggerResponseSent(ctx, loggerPool)
		},
	}
}

type twirpLogger struct {
	twirpRequestFields

	fieldsOrig []interface{}
	fields     []interface{}
	startedAt  time.Time
}

func AppendLogFields(ctx context.Context, fields ...interface{}) {
	r, ok := ctx.Value(twirpLoggerKey{}).(*twirpLogger)
	if !ok || r == nil {
		return
	}

	r.fields = append(r.fields, fields...)
}

func loggerRequestReceived(ctx context.Context, twirpLoggerPool *sync.Pool) (context.Context, error) {
	r := twirpLoggerPool.Get().(*twirpLogger)
	r.startedAt = time.Now()
	r.fields = r.fieldsOrig
	r.error = nil

	if svc, ok := twirp.ServiceName(ctx); ok {
		r.service = svc
		r.fields = append(r.fields, "service", svc)
	}

	return context.WithValue(ctx, twirpLoggerKey{}, r), nil
}

func loggerRequestRouted(ctx context.Context) (context.Context, error) {
	if meth, ok := twirp.MethodName(ctx); ok {
		l, ok := ctx.Value(twirpLoggerKey{}).(*twirpLogger)
		if !ok || l == nil {
			return ctx, nil
		}
		l.method = meth
		l.fields = append(l.fields, "method", meth)
	}

	return ctx, nil
}

func loggerResponseSent(ctx context.Context, twirpLoggerPool *sync.Pool) {
	r, ok := ctx.Value(twirpLoggerKey{}).(*twirpLogger)
	if !ok || r == nil {
		return
	}

	r.fields = append(r.fields, "duration", time.Since(r.startedAt))

	if status, ok := twirp.StatusCode(ctx); ok {
		r.fields = append(r.fields, "status", status)
	}
	if r.error != nil {
		r.fields = append(r.fields, "error", r.error.Msg())
		r.fields = append(r.fields, "code", r.error.Code())
	}

	serviceMethod := "API " + r.service + "." + r.method
	utils.GetLogger(ctx).WithComponent(utils.ComponentAPI).Infow(serviceMethod, r.fields...)

	r.fields = r.fieldsOrig
	r.error = nil

	twirpLoggerPool.Put(r)
}

func loggerErrorReceived(ctx context.Context, e twirp.Error) context.Context {
	r, ok := ctx.Value(twirpLoggerKey{}).(*twirpLogger)
	if !ok || r == nil {
		return ctx
	}

	r.error = e
	return ctx
}

// --------------------------------------------------------------------------

type statusReporterKey struct{}

func TwirpRequestStatusReporter() *twirp.ServerHooks {
	return &twirp.ServerHooks{
		RequestReceived: statusReporterRequestReceived,
		RequestRouted:   statusReporterRequestRouted,
		Error:           statusReporterErrorReceived,
		ResponseSent:    statusReporterResponseSent,
	}
}

func statusReporterRequestReceived(ctx context.Context) (context.Context, error) {
	r := &twirpRequestFields{}

	if svc, ok := twirp.ServiceName(ctx); ok {
		r.service = svc
	}

	return context.WithValue(ctx, statusReporterKey{}, r), nil
}

func statusReporterRequestRouted(ctx context.Context) (context.Context, error) {
	if meth, ok := twirp.MethodName(ctx); ok {
		l, ok := ctx.Value(statusReporterKey{}).(*twirpRequestFields)
		if !ok || l == nil {
			return ctx, nil
		}
		l.method = meth
	}

	return ctx, nil
}

func statusReporterResponseSent(ctx context.Context) {
	r, ok := ctx.Value(statusReporterKey{}).(*twirpRequestFields)
	if !ok || r == nil {
		return
	}

	var statusFamily string
	if statusCode, ok := twirp.StatusCode(ctx); ok {
		if status, err := strconv.Atoi(statusCode); err == nil {
			switch {
			case status >= 400 && status <= 499:
				statusFamily = "4xx"
			case status >= 500 && status <= 599:
				statusFamily = "5xx"
			default:
				statusFamily = statusCode
			}
		}
	}

	var code twirp.ErrorCode
	if r.error != nil {
		code = r.error.Code()
	}

	prometheus.TwirpRequestStatusCounter.WithLabelValues(r.service, r.method, statusFamily, string(code)).Add(1)
}

func statusReporterErrorReceived(ctx context.Context, e twirp.Error) context.Context {
	r, ok := ctx.Value(statusReporterKey{}).(*twirpRequestFields)
	if !ok || r == nil {
		return ctx
	}

	r.error = e
	return ctx
}

// --------------------------------------------------------------------------

type twirpTelemetryKey struct{}

func TwirpTelemetry(nodeID livekit.NodeID, telemetry telemetry.TelemetryService) *twirp.ServerHooks {
	return &twirp.ServerHooks{
		RequestReceived: telemetryRequestReceived,
		Error:           telemetryErrorReceived,
		ResponseSent: func(ctx context.Context) {
			telemetryResponseSent(ctx, nodeID, telemetry)
		},
	}
}

func RecordRequest(ctx context.Context, request proto.Message) {
	if request == nil {
		return
	}

	a, ok := ctx.Value(twirpTelemetryKey{}).(*livekit.APICallInfo)
	if !ok || a == nil {
		return
	}

	switch request.(type) {
	case *livekit.CreateRoomRequest:
		a.Request = &livekit.APICallRequest{
			Message: &livekit.APICallRequest_CreateRoomRequest{
				CreateRoomRequest: request.(*livekit.CreateRoomRequest),
			},
		}
	}
}

func RecordResponse(ctx context.Context, response proto.Message) {
	if response == nil {
		return
	}

	a, ok := ctx.Value(twirpTelemetryKey{}).(*livekit.APICallInfo)
	if !ok || a == nil {
		return
	}

	switch response.(type) {
	case *livekit.Room:
		a.Response = &livekit.APICallResponse{
			Message: &livekit.APICallResponse_Room{
				Room: response.(*livekit.Room),
			},
		}
	}
}

func telemetryRequestReceived(ctx context.Context) (context.Context, error) {
	a := &livekit.APICallInfo{}
	a.StartedAt = timestamppb.Now()

	if svc, ok := twirp.ServiceName(ctx); ok {
		a.Service = svc
	}

	return context.WithValue(ctx, twirpTelemetryKey{}, a), nil
}

func telemetryRequestRouted(ctx context.Context) (context.Context, error) {
	if meth, ok := twirp.MethodName(ctx); ok {
		a, ok := ctx.Value(twirpTelemetryKey{}).(*livekit.APICallInfo)
		if !ok || a == nil {
			return ctx, nil
		}
		a.Method = meth
	}

	return ctx, nil
}

func telemetryResponseSent(ctx context.Context, nodeID livekit.NodeID, telemetry telemetry.TelemetryService) {
	a, ok := ctx.Value(twirpTelemetryKey{}).(*livekit.APICallInfo)
	if !ok || a == nil {
		return
	}

	a.NodeId = string(nodeID)
	if status, ok := twirp.StatusCode(ctx); ok {
		a.Status = status
	}
	a.DurationNs = time.Since(a.StartedAt.AsTime()).Nanoseconds()
	if telemetry != nil {
		telemetry.APICall(ctx, a)
	}
}

func telemetryErrorReceived(ctx context.Context, e twirp.Error) context.Context {
	a, ok := ctx.Value(twirpTelemetryKey{}).(*livekit.APICallInfo)
	if !ok || a == nil {
		return ctx
	}

	a.ErrorCode = string(e.Code())
	a.ErrorMessage = e.Msg()
	return ctx
}

// --------------------------------------------------------------------------
