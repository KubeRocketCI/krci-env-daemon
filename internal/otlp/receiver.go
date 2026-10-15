// Package otlp receives Envoy Gateway's access log over OTLP. A request to a host of a
// managed environment counts as use; one to a sleeping environment wakes it.
package otlp

import (
	"context"
	"strconv"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// maxAuthority is the longest authority the receiver looks up: a hostname and a port.
const maxAuthority = validation.DNS1123SubdomainMaxLength + len(":65535")

// Hosts maps a request host to the namespace of its environment.
type Hosts interface {
	HostEnv(host string) string
}

// Toucher records a use of an environment.
type Toucher interface {
	Touch(ns, why string)
}

// Receiver implements the OTLP logs service.
type Receiver struct {
	collogs.UnimplementedLogsServiceServer
	Hosts Hosts
	State Toucher
}

// Export reads the authority and the response code of every record and touches each
// environment once per batch. The request path is not read: it can carry tokens. Nothing
// else of a record reaches the state or the log: the reason of a wake is "request" and
// the code when it is one.
func (r *Receiver) Export(_ context.Context, req *collogs.ExportLogsServiceRequest) (*collogs.ExportLogsServiceResponse, error) {
	touched := map[string]string{}

	for _, rl := range req.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				r.note(touched, lr)
			}
		}
	}

	for ns, why := range touched {
		r.State.Touch(ns, why)
	}

	return &collogs.ExportLogsServiceResponse{}, nil
}

// note records the environment of the request in touched, with the first reason seen.
func (r *Receiver) note(touched map[string]string, lr *logsv1.LogRecord) {
	host, code := requestOf(lr)
	if len(host) > maxAuthority {
		return
	}

	ns := r.Hosts.HostEnv(host)
	if ns == "" {
		return
	}

	if _, seen := touched[ns]; seen {
		return
	}

	why := "request"
	if isStatusCode(code) {
		why += " " + code
	}

	touched[ns] = why
}

// requestOf reads the authority and the response code of a record.
func requestOf(lr *logsv1.LogRecord) (host, code string) {
	for _, kv := range lr.GetAttributes() {
		switch kv.GetKey() {
		case "authority", ":authority":
			host = stringValue(kv.GetValue())
		case "response_code":
			code = stringValue(kv.GetValue())
		}
	}

	return host, code
}

func isStatusCode(code string) bool {
	n, err := strconv.Atoi(code)
	return err == nil && len(code) == 3 && n >= 100
}

func stringValue(v *commonv1.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonv1.AnyValue_StringValue:
		return x.StringValue
	case *commonv1.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	default:
		return ""
	}
}
