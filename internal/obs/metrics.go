// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package obs

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds every metric nbsandboxd exports.
// All are declared up front; later phases wire them to real values.
type Metrics struct {
	Registry *prometheus.Registry

	RunsTotal         *prometheus.CounterVec // by final status
	RunDuration       prometheus.Histogram
	RunPhase          *prometheus.HistogramVec // where a run's time went, by phase
	QueueLength       prometheus.Gauge
	ActiveRuns        prometheus.Gauge
	ActiveLogStreams  prometheus.Gauge
	DockerUp          prometheus.Gauge
	HTTPRequestsTotal *prometheus.CounterVec // by route pattern and status code
}

// NewMetrics creates the registry with Go and process collectors.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		RunsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "nbsandbox_runs_total",
			Help: "Finished runs by final status (succeeded, failed, oom, timeout, ...).",
		}, []string{"status"}),
		RunDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "nbsandbox_run_duration_seconds",
			Help:    "Duration of finished runs, from start to end.",
			Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600},
		}),
		RunPhase: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "nbsandbox_run_phase_seconds",
			Help:    "Where finished runs spent their time, by phase (queued, prepare, container_start, runner_start, kernel_start, execute, render, exit, teardown, record).",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300},
		}, []string{"phase"}),
		QueueLength: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "nbsandbox_queue_length",
			Help: "Runs waiting in the queue.",
		}),
		ActiveRuns: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "nbsandbox_active_runs",
			Help: "Runs currently executing.",
		}),
		ActiveLogStreams: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "nbsandbox_log_streams_active",
			Help: "Open live log streams (SSE).",
		}),
		DockerUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "nbsandbox_docker_up",
			Help: "1 when Docker is reachable, 0 when not.",
		}),
		HTTPRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "nbsandbox_http_requests_total",
			Help: "HTTP requests by route pattern and status code.",
		}, []string{"route", "code"}),
	}
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.RunsTotal, m.RunDuration, m.RunPhase, m.QueueLength, m.ActiveRuns,
		m.ActiveLogStreams, m.DockerUp, m.HTTPRequestsTotal,
	)
	return m
}

// Handler serves the registry in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}
