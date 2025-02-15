package middleware

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
)

// NewMetrics creates metrics middleware.
func NewMetrics(namespace string, registry *prometheus.Registry) gin.HandlerFunc {
	var requests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "http_requests_total",
			Help:      "HTTP requests.",
		},
		[]string{"status"},
	)
	var requestLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "http_request_latency_seconds",
			Help:      "HTTP request latency.",
			Buckets:   prometheus.ExponentialBuckets(0.01, 2, 10),
		},
		[]string{"status"},
	)

	registry.MustRegister(requests)
	registry.MustRegister(requestLatency)

	return func(c *gin.Context) {
		start := time.Now()

		// Process request.
		c.Next()

		requests.With(prometheus.Labels{
			"status": strconv.Itoa(c.Writer.Status()),
		}).Inc()
		requestLatency.With(prometheus.Labels{
			"status": strconv.Itoa(c.Writer.Status()),
		}).Observe(float64(time.Since(start).Milliseconds()) / 1000)
	}
}
