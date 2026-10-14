package otlp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
)

type hosts map[string]string

func (h hosts) HostEnv(host string) string { return h[host] }

type touches []string

func (t *touches) Touch(ns, why string) { *t = append(*t, ns+": "+why) }

func str(k, v string) *commonv1.KeyValue {
	return &commonv1.KeyValue{Key: k, Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: v}}}
}

func num(k string, v int64) *commonv1.KeyValue {
	return &commonv1.KeyValue{Key: k, Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: v}}}
}

func batch(records ...[]*commonv1.KeyValue) *collogs.ExportLogsServiceRequest {
	var lrs []*logsv1.LogRecord
	for _, attrs := range records {
		lrs = append(lrs, &logsv1.LogRecord{Attributes: attrs})
	}

	return &collogs.ExportLogsServiceRequest{ResourceLogs: []*logsv1.ResourceLogs{{
		ScopeLogs: []*logsv1.ScopeLogs{{LogRecords: lrs}},
	}}}
}

func TestExportTouchesEachEnvironmentOncePerBatch(t *testing.T) {
	var got touches

	r := &Receiver{Hosts: hosts{"portal.team-a.example.com": "team-a", "portal.team-b.example.com": "team-b"}, State: &got}

	_, err := r.Export(t.Context(), batch(
		[]*commonv1.KeyValue{str("authority", "portal.team-a.example.com"), num("response_code", 503), str("path", "/auth/callback?code=secret")},
		[]*commonv1.KeyValue{str("authority", "portal.team-a.example.com"), str("response_code", "200")},
		[]*commonv1.KeyValue{str(":authority", "portal.team-b.example.com"), str("response_code", "200")},
		[]*commonv1.KeyValue{str("authority", "unknown.example.com"), str("response_code", "404")},
		nil,
	))
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"team-a: request 503", "team-b: request 200"}, got)
}

func TestExportKeepsOnlyWhatLooksLikeARequest(t *testing.T) {
	var got touches

	r := &Receiver{Hosts: hosts{"portal.team-a.example.com": "team-a", "portal.team-b.example.com": "team-b"}, State: &got}

	_, err := r.Export(t.Context(), batch(
		[]*commonv1.KeyValue{str("authority", "portal.team-a.example.com"), str("response_code", strings.Repeat("5", 4000))},
		[]*commonv1.KeyValue{str("authority", strings.Repeat("a", 300)+".example.com"), str("response_code", "200")},
		[]*commonv1.KeyValue{str("authority", "portal.team-b.example.com"), str("response_code", "+42")},
	))
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"team-a: request", "team-b: request"}, got, "a code that is not one is dropped, a host longer than a hostname is skipped")
}
