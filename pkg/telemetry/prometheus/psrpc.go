package prometheus

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/psrpc"
	"github.com/livekit/psrpc/middleware"
)

var (
	psrpcUnaryRequestTime   *prometheus.HistogramVec
	psrpcUnaryResponseTime  *prometheus.HistogramVec
	psrpcMultiRequestTime   *prometheus.HistogramVec
	psrpcMultiResponseTime  *prometheus.HistogramVec
	psrpcStreamSendTime     *prometheus.HistogramVec
	psrpcStreamReceiveTotal *prometheus.CounterVec
	psrpcStreamCurrent      *prometheus.GaugeVec
)

func initPSRPCStats(nodeID string, nodeType livekit.NodeType, env string) {
	labels := []string{"service", "rpc"}
	streamLabels := []string{"role", "service", "rpc"}

	psrpcUnaryRequestTime = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "psrpc",
		Name:        "unary_request_time_ms",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String(), "env": env},
		Buckets:     []float64{100, 200, 300, 400, 500, 1000, 1500, 2000, 5000, 10000},
	}, labels)
	psrpcUnaryResponseTime = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "psrpc",
		Name:        "unary_response_time_ms",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String(), "env": env},
		Buckets:     []float64{10, 50, 100, 300, 500, 1000, 1500, 2000, 5000, 10000},
	}, labels)
	psrpcMultiRequestTime = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "psrpc",
		Name:        "multi_request_time_ms",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String(), "env": env},
		Buckets:     []float64{100, 200, 300, 400, 500, 1000, 1500, 2000, 5000, 10000},
	}, labels)
	psrpcMultiResponseTime = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "psrpc",
		Name:        "multi_response_time_ms",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String(), "env": env},
		Buckets:     []float64{10, 50, 100, 300, 500, 1000, 1500, 2000, 5000, 10000},
	}, labels)
	psrpcStreamSendTime = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   livekitNamespace,
		Subsystem:   "psrpc",
		Name:        "stream_send_time_ms",
		ConstLabels: prometheus.Labels{"node_id": nodeID, "node_type": nodeType.String(), "env": env},
		Buckets:     []float64{100, 200, 300, 400, 500, 1000, 1500, 2000, 5000, 10000},
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

	prometheus.MustRegister(psrpcUnaryRequestTime)
	prometheus.MustRegister(psrpcUnaryResponseTime)
	prometheus.MustRegister(psrpcMultiRequestTime)
	prometheus.MustRegister(psrpcMultiResponseTime)
	prometheus.MustRegister(psrpcStreamSendTime)
	prometheus.MustRegister(psrpcStreamReceiveTotal)
	prometheus.MustRegister(psrpcStreamCurrent)
}

var _ middleware.MetricsObserver = PSRPCMetricsObserver{}

type PSRPCMetricsObserver struct{}

func (o PSRPCMetricsObserver) OnUnaryRequest(role middleware.MetricRole, info psrpc.RPCInfo, duration time.Duration, err error) {
	if role == middleware.ClientRole {
		psrpcUnaryRequestTime.WithLabelValues(info.Service, info.Method).Observe(float64(duration.Milliseconds()))
	} else {
		psrpcUnaryResponseTime.WithLabelValues(info.Service, info.Method).Observe(float64(duration.Milliseconds()))
	}
}

func (o PSRPCMetricsObserver) OnMultiRequest(role middleware.MetricRole, info psrpc.RPCInfo, duration time.Duration, responseCount int, errorCount int) {
	if role == middleware.ClientRole {
		psrpcMultiRequestTime.WithLabelValues(info.Service, info.Method).Observe(float64(duration.Milliseconds()))
	} else {
		psrpcMultiResponseTime.WithLabelValues(info.Service, info.Method).Observe(float64(duration.Milliseconds()))
	}
}

func (o PSRPCMetricsObserver) OnStreamSend(role middleware.MetricRole, info psrpc.RPCInfo, duration time.Duration, err error) {
	psrpcStreamSendTime.WithLabelValues(role.String(), info.Service, info.Method).Observe(float64(duration.Milliseconds()))
}

func (o PSRPCMetricsObserver) OnStreamRecv(role middleware.MetricRole, info psrpc.RPCInfo, err error) {
	psrpcStreamReceiveTotal.WithLabelValues(role.String(), info.Service, info.Method).Inc()
}

func (o PSRPCMetricsObserver) OnStreamOpen(role middleware.MetricRole, info psrpc.RPCInfo) {
	psrpcStreamCurrent.WithLabelValues(role.String(), info.Service, info.Method).Inc()
}

func (o PSRPCMetricsObserver) OnStreamClose(role middleware.MetricRole, info psrpc.RPCInfo) {
	psrpcStreamCurrent.WithLabelValues(role.String(), info.Service, info.Method).Dec()
}
