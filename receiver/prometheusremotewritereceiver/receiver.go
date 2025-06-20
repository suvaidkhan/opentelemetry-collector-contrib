// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0
package prometheusremotewritereceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/prometheusremotewritereceiver"

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/gogo/protobuf/proto"
	lru "github.com/hashicorp/golang-lru/v2"
	promconfig "github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/labels"
	writev2 "github.com/prometheus/prometheus/prompb/io/prometheus/write/v2"
	promremote "github.com/prometheus/prometheus/storage/remote"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componentstatus"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/receiver/receiverhelper"
	"go.uber.org/zap/zapcore"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/exp/metrics/identity"
)

func newRemoteWriteReceiver(settings receiver.Settings, cfg *Config, nextConsumer consumer.Metrics) (receiver.Metrics, error) {
	cache, err := lru.New[uint64, pmetric.ResourceMetrics](1000)
	if err != nil {
		return nil, fmt.Errorf("failed to create LRU cache: %w", err)
	}

	return &prometheusRemoteWriteReceiver{
		settings:     settings,
		nextConsumer: nextConsumer,
		config:       cfg,
		server: &http.Server{
			ReadTimeout: 60 * time.Second,
		},
		rmCache: cache,
	}, nil
}

type prometheusRemoteWriteReceiver struct {
	settings     receiver.Settings
	nextConsumer consumer.Metrics

	config *Config
	server *http.Server
	wg     sync.WaitGroup

	rmCache *lru.Cache[uint64, pmetric.ResourceMetrics]
	obsrecv *receiverhelper.ObsReport
}

// metricIdentity contains all the components that uniquely identify a metric
// according to the OpenTelemetry Protocol data model.
// The definition of the metric uniqueness is based on the following document. Ref: https://opentelemetry.io/docs/specs/otel/metrics/data-model/#opentelemetry-protocol-data-model
type metricIdentity struct {
	ResourceID   string
	ScopeName    string
	ScopeVersion string
	MetricName   string
	Unit         string
	Type         writev2.Metadata_MetricType
}

// createMetricIdentity creates a metricIdentity struct from the required components
func createMetricIdentity(resourceID, scopeName, scopeVersion, metricName, unit string, metricType writev2.Metadata_MetricType) metricIdentity {
	return metricIdentity{
		ResourceID:   resourceID,
		ScopeName:    scopeName,
		ScopeVersion: scopeVersion,
		MetricName:   metricName,
		Unit:         unit,
		Type:         metricType,
	}
}

// Hash generates a unique hash for the metric identity
func (mi metricIdentity) Hash() uint64 {
	const separator = "\xff"

	combined := strings.Join([]string{
		mi.ResourceID,
		mi.ScopeName,
		mi.ScopeVersion,
		mi.MetricName,
		mi.Unit,
		fmt.Sprintf("%d", mi.Type),
	}, separator)

	return xxhash.Sum64String(combined)
}

func (prw *prometheusRemoteWriteReceiver) Start(ctx context.Context, host component.Host) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/write", prw.handlePRW)
	var err error
	prw.obsrecv, err = receiverhelper.NewObsReport(receiverhelper.ObsReportSettings{
		ReceiverID:             prw.settings.ID,
		ReceiverCreateSettings: prw.settings,
		Transport:              "http",
	})
	if err != nil {
		return fmt.Errorf("failed to create obsreport: %w", err)
	}

	prw.server, err = prw.config.ToServer(ctx, host, prw.settings.TelemetrySettings, mux)
	if err != nil {
		return fmt.Errorf("failed to create server definition: %w", err)
	}
	listener, err := prw.config.ToListener(ctx)
	if err != nil {
		return fmt.Errorf("failed to create prometheus remote-write listener: %w", err)
	}

	prw.wg.Add(1)
	go func() {
		defer prw.wg.Done()
		if err := prw.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			componentstatus.ReportStatus(host, componentstatus.NewFatalErrorEvent(fmt.Errorf("error starting prometheus remote-write receiver: %w", err)))
		}
	}()
	return nil
}

func (prw *prometheusRemoteWriteReceiver) Shutdown(ctx context.Context) error {
	if prw.server == nil {
		return nil
	}
	err := prw.server.Shutdown(ctx)
	if err == nil {
		// Only wait if Shutdown returns successfully,
		// otherwise we may block indefinitely.
		prw.wg.Wait()
	}
	return err
}

func (prw *prometheusRemoteWriteReceiver) handlePRW(w http.ResponseWriter, req *http.Request) {
	contentType := req.Header.Get("Content-Type")
	if contentType == "" {
		prw.settings.Logger.Warn("message received without Content-Type header, rejecting")
		http.Error(w, "Content-Type header is required", http.StatusUnsupportedMediaType)
		return
	}

	msgType, err := prw.parseProto(contentType)
	if err != nil {
		prw.settings.Logger.Warn("Error decoding remote-write request", zapcore.Field{Key: "error", Type: zapcore.ErrorType, Interface: err})
		http.Error(w, err.Error(), http.StatusUnsupportedMediaType)
		return
	}
	if msgType != promconfig.RemoteWriteProtoMsgV2 {
		prw.settings.Logger.Warn("message received with unsupported proto version, rejecting")
		http.Error(w, "Unsupported proto version", http.StatusUnsupportedMediaType)
		return
	}

	// After parsing the content-type header, the next step would be to handle content-encoding.
	// Luckly confighttp's Server has middleware that already decompress the request body for us.

	body, err := io.ReadAll(req.Body)
	if err != nil {
		prw.settings.Logger.Warn("Error decoding remote write request", zapcore.Field{Key: "error", Type: zapcore.ErrorType, Interface: err})
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var prw2Req writev2.Request
	if err = proto.Unmarshal(body, &prw2Req); err != nil {
		prw.settings.Logger.Warn("Error decoding remote write request", zapcore.Field{Key: "error", Type: zapcore.ErrorType, Interface: err})
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	m, stats, err := prw.translateV2(req.Context(), &prw2Req)
	stats.SetHeaders(w)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest) // Following instructions at https://prometheus.io/docs/specs/remote_write_spec_2_0/#invalid-samples
		return
	}

	w.WriteHeader(http.StatusNoContent)

	obsrecvCtx := prw.obsrecv.StartMetricsOp(req.Context())
	err = prw.nextConsumer.ConsumeMetrics(req.Context(), m)
	if err != nil {
		prw.settings.Logger.Error("Error consuming metrics", zapcore.Field{Key: "error", Type: zapcore.ErrorType, Interface: err})
	}
	prw.obsrecv.EndMetricsOp(obsrecvCtx, "prometheusremotewritereceiver", m.ResourceMetrics().Len(), err)
}

// parseProto parses the content-type header and returns the version of the remote-write protocol.
// We can't expect that senders of remote-write v1 will add the "proto=" parameter since it was not
// a requirement in v1. So, if the parameter is not found, we assume v1.
func (prw *prometheusRemoteWriteReceiver) parseProto(contentType string) (promconfig.RemoteWriteProtoMsg, error) {
	contentType = strings.TrimSpace(contentType)

	parts := strings.Split(contentType, ";")
	if parts[0] != "application/x-protobuf" {
		return "", fmt.Errorf("expected %q as the first (media) part, got %v content-type", "application/x-protobuf", contentType)
	}

	for _, part := range parts[1:] {
		parameter := strings.Split(part, "=")
		if len(parameter) != 2 {
			return "", fmt.Errorf("as per https://www.rfc-editor.org/rfc/rfc9110#parameter expected parameters to be key-values, got %v in %v content-type", part, contentType)
		}

		if strings.TrimSpace(parameter[0]) == "proto" {
			ret := promconfig.RemoteWriteProtoMsg(parameter[1])
			if err := ret.Validate(); err != nil {
				return "", fmt.Errorf("got %v content type; %w", contentType, err)
			}
			return ret, nil
		}
	}

	// No "proto=" parameter found, assume v1.
	return promconfig.RemoteWriteProtoMsgV1, nil
}

// translateV2 translates a v2 remote-write request into OTLP metrics.
// translate is not feature complete.
func (prw *prometheusRemoteWriteReceiver) translateV2(_ context.Context, req *writev2.Request) (pmetric.Metrics, promremote.WriteResponseStats, error) {
	var (
		badRequestErrors error
		otelMetrics      = pmetric.NewMetrics()
		labelsBuilder    = labels.NewScratchBuilder(0)
		// More about stats: https://github.com/prometheus/docs/blob/main/docs/specs/prw/remote_write_spec_2_0.md#required-written-response-headers
		// TODO: add histograms and exemplars to the stats. Histograms can be added after this PR be merged. Ref #39864
		// Exemplars should be implemented to add them to the stats.
		stats = promremote.WriteResponseStats{
			Confirmed: true,
		}
		// The key is composed by: resource_hash:scope_name:scope_version:metric_name:unit:type
		metricCache = make(map[uint64]pmetric.Metric)
	)

	for _, ts := range req.Timeseries {
		ls := ts.ToLabels(&labelsBuilder, req.Symbols)
		if !ls.Has(labels.MetricName) {
			badRequestErrors = errors.Join(badRequestErrors, errors.New("missing metric name in labels"))
			continue
		} else if duplicateLabel, hasDuplicate := ls.HasDuplicateLabelNames(); hasDuplicate {
			badRequestErrors = errors.Join(badRequestErrors, fmt.Errorf("duplicate label %q in labels", duplicateLabel))
			continue
		}

		// If the metric name is equal to target_info, we use its labels as attributes of the resource
		// Ref: https://opentelemetry.io/docs/specs/otel/compatibility/prometheus_and_openmetrics/#resource-attributes-1
		if ls.Get(labels.MetricName) == "target_info" {
			var rm pmetric.ResourceMetrics
			hashedLabels := xxhash.Sum64String(ls.Get("job") + string([]byte{'\xff'}) + ls.Get("instance"))

			if existingRM, ok := prw.rmCache.Get(hashedLabels); ok {
				rm = existingRM
			} else {
				rm = otelMetrics.ResourceMetrics().AppendEmpty()
			}

			attrs := rm.Resource().Attributes()
			parseJobAndInstance(attrs, ls.Get("job"), ls.Get("instance"))

			// Add the remaining labels as resource attributes
			for _, l := range ls {
				if l.Name != "job" && l.Name != "instance" && l.Name != labels.MetricName {
					attrs.PutStr(l.Name, l.Value)
				}
			}
			prw.rmCache.Add(hashedLabels, rm)
			continue
		}

		// For metrics other than target_info, we need to follow the standard process of creating a metric.
		var rm pmetric.ResourceMetrics
		hashedLabels := xxhash.Sum64String(ls.Get("job") + string([]byte{'\xff'}) + ls.Get("instance"))
		existingRM, ok := prw.rmCache.Get(hashedLabels)
		if ok {
			rm = existingRM
		} else {
			rm = otelMetrics.ResourceMetrics().AppendEmpty()
			parseJobAndInstance(rm.Resource().Attributes(), ls.Get("job"), ls.Get("instance"))
			prw.rmCache.Add(hashedLabels, rm)
		}

		scopeName, scopeVersion := prw.extractScopeInfo(ls)
		metricName := ls.Get(labels.MetricName)
		if ts.Metadata.UnitRef >= uint32(len(req.Symbols)) {
			badRequestErrors = errors.Join(badRequestErrors, fmt.Errorf("unit ref %d is out of bounds of symbolsTable", ts.Metadata.UnitRef))
			continue
		}

		if ts.Metadata.HelpRef >= uint32(len(req.Symbols)) {
			badRequestErrors = errors.Join(badRequestErrors, fmt.Errorf("help ref %d is out of bounds of symbolsTable", ts.Metadata.HelpRef))
			continue
		}

		unit := req.Symbols[ts.Metadata.UnitRef]
		description := req.Symbols[ts.Metadata.HelpRef]

		resourceID := identity.OfResource(rm.Resource())

		metricIdentity := createMetricIdentity(
			resourceID.String(), // Resource identity
			scopeName,           // Scope name
			scopeVersion,        // Scope version
			metricName,          // Metric name
			unit,                // Unit
			ts.Metadata.Type,    // Metric type
		)

		metricKey := metricIdentity.Hash()

		var scope pmetric.ScopeMetrics
		var foundScope bool
		for i := 0; i < rm.ScopeMetrics().Len(); i++ {
			s := rm.ScopeMetrics().At(i)
			if s.Scope().Name() == scopeName && s.Scope().Version() == scopeVersion {
				scope = s
				foundScope = true
				break
			}
		}
		if !foundScope {
			scope = rm.ScopeMetrics().AppendEmpty()
			scope.Scope().SetName(scopeName)
			scope.Scope().SetVersion(scopeVersion)
		}

		metric, exists := metricCache[metricKey]
		// If the metric does not exist, we create an empty metric and add it to the cache.
		if !exists {
			metric = scope.Metrics().AppendEmpty()
			metric.SetName(metricName)
			metric.SetUnit(unit)
			metric.SetDescription(description)

			switch ts.Metadata.Type {
			case writev2.Metadata_METRIC_TYPE_GAUGE:
				metric.SetEmptyGauge()
			case writev2.Metadata_METRIC_TYPE_COUNTER:
				sum := metric.SetEmptySum()
				sum.SetIsMonotonic(true)
				sum.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
			case writev2.Metadata_METRIC_TYPE_HISTOGRAM:
				metric.SetEmptyHistogram()
			case writev2.Metadata_METRIC_TYPE_SUMMARY:
				metric.SetEmptySummary()
			}

			metricCache[metricKey] = metric
		}

		// When the new description is longer than the existing one, we should update the metric description.
		// Reference to this behavior: https://opentelemetry.io/docs/specs/otel/metrics/data-model/#opentelemetry-protocol-data-model-producer-recommendations
		if len(metric.Description()) < len(description) {
			metric.SetDescription(description)
		}

		// Otherwise, we append the samples to the existing metric.
		switch ts.Metadata.Type {
		case writev2.Metadata_METRIC_TYPE_GAUGE:
			addNumberDatapoints(metric.Gauge().DataPoints(), ls, ts, &stats)
		case writev2.Metadata_METRIC_TYPE_COUNTER:
			addNumberDatapoints(metric.Sum().DataPoints(), ls, ts, &stats)
		case writev2.Metadata_METRIC_TYPE_HISTOGRAM:
			addHistogramDatapoints(metric.Histogram().DataPoints(), ls, ts)
		case writev2.Metadata_METRIC_TYPE_SUMMARY:
			addSummaryDatapoints(metric.Summary().DataPoints(), ls, ts)
		default:
			badRequestErrors = errors.Join(badRequestErrors, fmt.Errorf("unsupported metric type %q for metric %q", ts.Metadata.Type, metricName))
		}
	}

	return otelMetrics, stats, badRequestErrors
}

// parseJobAndInstance turns the job and instance labels service resource attributes.
// Following the specification at https://opentelemetry.io/docs/specs/otel/compatibility/prometheus_and_openmetrics/
func parseJobAndInstance(dest pcommon.Map, job, instance string) {
	if instance != "" {
		dest.PutStr("service.instance.id", instance)
	}
	if job != "" {
		parts := strings.Split(job, "/")
		if len(parts) == 2 {
			dest.PutStr("service.namespace", parts[0])
			dest.PutStr("service.name", parts[1])
			return
		}
		dest.PutStr("service.name", job)
	}
}

// addNumberDatapoints adds the labels to the datapoints attributes.
func addNumberDatapoints(datapoints pmetric.NumberDataPointSlice, ls labels.Labels, ts writev2.TimeSeries, stats *promremote.WriteResponseStats) {
	// Add samples from the timeseries
	for _, sample := range ts.Samples {
		dp := datapoints.AppendEmpty()
		dp.SetStartTimestamp(pcommon.Timestamp(ts.CreatedTimestamp * int64(time.Millisecond)))
		// Set timestamp in nanoseconds (Prometheus uses milliseconds)
		dp.SetTimestamp(pcommon.Timestamp(sample.Timestamp * int64(time.Millisecond)))
		dp.SetDoubleValue(sample.Value)

		attributes := dp.Attributes()
		for _, l := range ls {
			if l.Name == "instance" || l.Name == "job" || // Become resource attributes
				l.Name == labels.MetricName || // Becomes metric name
				l.Name == "otel_scope_name" || l.Name == "otel_scope_version" { // Becomes scope name and version
				continue
			}
			attributes.PutStr(l.Name, l.Value)
		}
		stats.Samples++
	}
}

func addSummaryDatapoints(_ pmetric.SummaryDataPointSlice, _ labels.Labels, _ writev2.TimeSeries) {
	// TODO: Implement this function
}

func addHistogramDatapoints(_ pmetric.HistogramDataPointSlice, _ labels.Labels, _ writev2.TimeSeries) {
	// TODO: Implement this function
}

// extractScopeInfo extracts the scope name and version from the labels. If the labels do not contain the scope name/version,
// it will use the default values from the settings.
func (prw *prometheusRemoteWriteReceiver) extractScopeInfo(ls labels.Labels) (string, string) {
	scopeName := prw.settings.BuildInfo.Description
	scopeVersion := prw.settings.BuildInfo.Version

	if sName := ls.Get("otel_scope_name"); sName != "" {
		scopeName = sName
	}

	if sVersion := ls.Get("otel_scope_version"); sVersion != "" {
		scopeVersion = sVersion
	}
	return scopeName, scopeVersion
}
