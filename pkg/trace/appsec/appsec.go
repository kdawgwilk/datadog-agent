// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package appsec

import (
	"encoding/json"
	"fmt"
	stdlog "log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/pkg/errors"

	"github.com/DataDog/datadog-agent/pkg/trace/api/apiutil"
	"github.com/DataDog/datadog-agent/pkg/trace/config"
	"github.com/DataDog/datadog-agent/pkg/trace/log"
	"github.com/DataDog/datadog-agent/pkg/trace/metrics"
)

// NewIntakeReverseProxy returns the AppSec Intake Proxy handler according to
// the agent configuration.
func NewIntakeReverseProxy(conf *config.AgentConfig) (http.Handler, error) {
	disabled := func(reason string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add("Content-Type", "application/json")
			w.WriteHeader(http.StatusMethodNotAllowed)
			if err := json.NewEncoder(w).Encode(reason); err != nil {
				log.Error(err)
			}
		})
	}
	cfg, err := newConfig(conf)
	if err != nil {
		return disabled(fmt.Sprintf("appsec agent disabled due to a configuration error: %v", err)), errors.Wrap(err, "configuration: ")
	}
	if !cfg.Enabled {
		log.Info("AppSec proxy disabled by configuration")
		return disabled("appsec agent disabled by configuration"), nil
	}
	return newIntakeReverseProxy(cfg.IntakeURL, cfg.APIKey, cfg.MaxPayloadSize, conf.NewHTTPTransport(), conf.AgentVersion), nil
}

// newIntakeReverseProxy creates a reverse proxy to the intake backend using the
// given transport round-tripper.
// The reverse proxy handler also limits the request body size and adds extra
// headers such as Dd-Api-Key and Via.
func newIntakeReverseProxy(target *url.URL, apiKey string, maxPayloadSize int64, transport http.RoundTripper, agentVersion string) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(target)
	via := fmt.Sprintf("trace-agent %s", agentVersion)
	// Wrap and overwrite the returned director to add extra headers
	director := proxy.Director
	proxy.Director = func(req *http.Request) {
		// Call the original director changing the request target
		director(req)
		// Set extra headers
		req.Header.Set("Via", via)
		req.Header.Set("Dd-Api-Key", apiKey)
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		w.Header().Add("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		if err := json.NewEncoder(w).Encode(err); err != nil {
			log.Error(err)
		}
	}
	proxy.Transport = withMetrics(transport, maxPayloadSize)
	proxy.ErrorLog = stdlog.New(log.NewThrottled(5, 10*time.Second), "Appsec backend proxy: ", 0)
	return proxy
}

const (
	appSecRequestMetricsPrefix     = "datadog.trace_agent.appsec."
	appSecRequestCountMetricsID    = appSecRequestMetricsPrefix + "request"
	appSecRequestDurationMetricsID = appSecRequestMetricsPrefix + "request_duration_ms"
	appSecRequestErrorMetricsID    = appSecRequestMetricsPrefix + "request_error"
)

// metricsTags returns the metrics tags of a request.
func metricsTags(req *http.Request) []string {
	tags := []string{"path:" + req.URL.Path}
	if ct := req.Header.Get("Content-Type"); ct != "" {
		tags = append(tags, "content_type:"+ct)
	}
	return tags
}

type roundTripper struct {
	http.RoundTripper
	maxPayloadSize int64
}

func withMetrics(rt http.RoundTripper, maxPayloadSize int64) http.RoundTripper {
	return &roundTripper{
		RoundTripper:   rt,
		maxPayloadSize: maxPayloadSize,
	}
}

// RoundTrip limits the request body size that can be read and performs internal monitoring metrics
func (r *roundTripper) RoundTrip(req *http.Request) (res *http.Response, err error) {
	if req.Body != nil && r.maxPayloadSize > 0 {
		req.Body = apiutil.NewLimitedReader(req.Body, r.maxPayloadSize)
	}

	now := time.Now()
	defer func() {
		tags := metricsTags(req)
		metrics.Count(appSecRequestCountMetricsID, 1, tags, 1)
		metrics.Timing(appSecRequestDurationMetricsID, time.Since(now), tags, 1)

		if err != nil {
			var kind string
			switch err {
			case apiutil.ErrLimitedReaderLimitReached:
				kind = "ErrLimitedReaderLimitReached"
			default:
				kind = fmt.Sprintf("%T", err)
			}
			tags = append(tags, fmt.Sprintf("error:%s", kind))
			if res != nil {
				tags = append(tags, fmt.Sprintf("status:%d", res.StatusCode))
			}
			metrics.Count(appSecRequestErrorMetricsID, 1, tags, 1)
		}
	}()
	return r.RoundTripper.RoundTrip(req)
}
