// Package scaler serves KEDA's external-push scaler protocol. A push starts a workload at
// once; KEDA's polls keep it up while its environment is in use.
package scaler

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/KubeRocketCI/krci-env-daemon/internal/externalscaler"
)

const metricName = "env-active"

// State answers whether a ScaledObject's workload should run.
type State interface {
	// IsActive reports managed=false for a ScaledObject that is not a target of a
	// managed environment.
	IsActive(ns, scaledObject string) (active, managed bool)
	Subscribe(ns, scaledObject string) (<-chan struct{}, func())
}

// Server implements the KEDA external scaler service. For a ScaledObject the daemon does
// not manage it answers NotFound, so KEDA keeps the replicas and reports the trigger error
// instead of scaling the workload to zero.
type Server struct {
	externalscaler.UnimplementedExternalScalerServer
	State State
	// Stopping closes when the daemon shuts down; open streams end then.
	Stopping <-chan struct{}
}

// validate rejects a reference that does not name a namespace and a ScaledObject, before
// anything is stored under it.
func validate(ref *externalscaler.ScaledObjectRef) error {
	if len(validation.IsDNS1123Label(ref.GetNamespace())) > 0 || len(validation.IsDNS1123Subdomain(ref.GetName())) > 0 {
		return status.Error(codes.InvalidArgument, "scaledObjectRef must carry a namespace and a name")
	}

	return nil
}

func (s *Server) isActive(ref *externalscaler.ScaledObjectRef) (bool, error) {
	if err := validate(ref); err != nil {
		return false, err
	}

	active, managed := s.State.IsActive(ref.GetNamespace(), ref.GetName())
	if !managed {
		return false, status.Errorf(codes.NotFound, "ScaledObject %s/%s is not a target of an environment krci-env-daemon manages",
			ref.GetNamespace(), ref.GetName())
	}

	return active, nil
}

// IsActive answers KEDA's polls.
func (s *Server) IsActive(_ context.Context, ref *externalscaler.ScaledObjectRef) (*externalscaler.IsActiveResponse, error) {
	active, err := s.isActive(ref)
	if err != nil {
		return nil, err
	}

	return &externalscaler.IsActiveResponse{Result: active}, nil
}

// StreamIsActive pushes true to KEDA whenever the ScaledObject becomes active.
func (s *Server) StreamIsActive(ref *externalscaler.ScaledObjectRef, stream externalscaler.ExternalScaler_StreamIsActiveServer) error {
	if err := validate(ref); err != nil {
		return err
	}

	ch, cancel := s.State.Subscribe(ref.GetNamespace(), ref.GetName())
	defer cancel()

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-s.Stopping:
			return status.Error(codes.Unavailable, "krci-env-daemon is shutting down")
		case <-ch:
			if err := stream.Send(&externalscaler.IsActiveResponse{Result: true}); err != nil {
				return err
			}
		}
	}
}

// GetMetricSpec declares one metric with target 1, so an active workload runs one replica.
func (s *Server) GetMetricSpec(_ context.Context, _ *externalscaler.ScaledObjectRef) (*externalscaler.GetMetricSpecResponse, error) {
	return &externalscaler.GetMetricSpecResponse{MetricSpecs: []*externalscaler.MetricSpec{{MetricName: metricName, TargetSize: 1}}}, nil
}

// GetMetrics reports 1 while the workload is active and 0 otherwise.
func (s *Server) GetMetrics(_ context.Context, req *externalscaler.GetMetricsRequest) (*externalscaler.GetMetricsResponse, error) {
	active, err := s.isActive(req.GetScaledObjectRef())
	if err != nil {
		return nil, err
	}

	var v int64
	if active {
		v = 1
	}

	return &externalscaler.GetMetricsResponse{MetricValues: []*externalscaler.MetricValue{{MetricName: metricName, MetricValue: v}}}, nil
}
