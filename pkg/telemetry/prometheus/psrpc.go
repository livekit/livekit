package prometheus

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/psrpc"
	"github.com/livekit/psrpc/pkg/middleware"
)

var (
	psrpcRequestTime        *prometheus.HistogramVec
	psrpcStreamSendTime     *prometheus.HistogramVec
	psrpcStreamReceiveTotal *prometheus.CounterVec
	psrpcStreamCurrent      *prometheus.GaugeVec
	psrpcErrorTotal         *prometheus.CounterVec
)

func initPSRPCStats(nodeID string, nodeType livekit.NodeType, env string) {
	labels := []string{"role", "kind", "service", "method"}
	streamLabels := []string{"role", "service", "method"}

	psrpcRequestTime = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "psrpc",
		Name:        "request_time_ms",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String(), "env": env},
		Buckets:     []float64{10, 50, 100, 300, 500, 1000, 1500, 2000, 5000, 10000},
	}, labels)
	psrpcStreamSendTime = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "psrpc",
		Name:        "stream_send_time_ms",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String(), "env": env},
		Buckets:     []float64{10, 50, 100, 300, 500, 1000, 1500, 2000, 5000, 10000},
	}, streamLabels)
	psrpcStreamReceiveTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "psrpc",
		Name:        "stream_receive_total",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String(), "env": env},
	}, streamLabels)
	psrpcStreamCurrent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "psrpc",
		Name:        "stream_count",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String(), "env": env},
	}, streamLabels)
	psrpcErrorTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "psrpc",
		Name:        "error_total",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String(), "env": env},
	}, labels)

	prometheus.MustRegister(psrpcRequestTime)
	prometheus.MustRegister(psrpcStreamSendTime)
	prometheus.MustRegister(psrpcStreamReceiveTotal)
	prometheus.MustRegister(psrpcStreamCurrent)
	prometheus.MustRegister(psrpcErrorTotal)
}

var _ middleware.MetricsObserver = PSRPCMetricsObserver{}

type PSRPCMetricsObserver struct{}

func (o PSRPCMetricsObserver) OnUnaryRequest(role middleware.MetricRole, info psrpc.RPCInfo, duration time.Duration, err error) {
	if err != nil {
		psrpcErrorTotal.WithLabelValues(role.String(), "rpc", info.Service, info.Method).Inc()
	} else if role == middleware.ClientRole {
		psrpcRequestTime.WithLabelValues(role.String(), "rpc", info.Service, info.Method).Observe(float64(duration.Milliseconds()))
	} else {
		psrpcRequestTime.WithLabelValues(role.String(), "rpc", info.Service, info.Method).Observe(float64(duration.Milliseconds()))
	}
}

func (o PSRPCMetricsObserver) OnMultiRequest(role middleware.MetricRole, info psrpc.RPCInfo, duration time.Duration, responseCount int, errorCount int) {
	if responseCount == 0 {
		psrpcErrorTotal.WithLabelValues(role.String(), "multirpc", info.Service, info.Method).Inc()
	} else if role == middleware.ClientRole {
		psrpcRequestTime.WithLabelValues(role.String(), "multirpc", info.Service, info.Method).Observe(float64(duration.Milliseconds()))
	} else {
		psrpcRequestTime.WithLabelValues(role.String(), "multirpc", info.Service, info.Method).Observe(float64(duration.Milliseconds()))
	}
}

func (o PSRPCMetricsObserver) OnStreamSend(role middleware.MetricRole, info psrpc.RPCInfo, duration time.Duration, err error) {
	if err != nil {
		psrpcErrorTotal.WithLabelValues(role.String(), "stream", info.Service, info.Method).Inc()
	} else {
		psrpcStreamSendTime.WithLabelValues(role.String(), info.Service, info.Method).Observe(float64(duration.Milliseconds()))
	}
}

func (o PSRPCMetricsObserver) OnStreamRecv(role middleware.MetricRole, info psrpc.RPCInfo, err error) {
	if err != nil {
		psrpcErrorTotal.WithLabelValues(role.String(), "stream", info.Service, info.Method).Inc()
	} else {
		psrpcStreamReceiveTotal.WithLabelValues(role.String(), info.Service, info.Method).Inc()
	}
}

func (o PSRPCMetricsObserver) OnStreamOpen(role middleware.MetricRole, info psrpc.RPCInfo) {
	psrpcStreamCurrent.WithLabelValues(role.String(), info.Service, info.Method).Inc()
}

func (o PSRPCMetricsObserver) OnStreamClose(role middleware.MetricRole, info psrpc.RPCInfo) {
	psrpcStreamCurrent.WithLabelValues(role.String(), info.Service, info.Method).Dec()
}
