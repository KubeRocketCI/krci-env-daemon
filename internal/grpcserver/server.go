// Package grpcserver builds the daemon's gRPC servers with the limits that bound what a
// client can make them hold: the request size, the streams in flight, the metadata size,
// and idle connections. Neither listener authenticates its peer; the limits are what
// stands between a pod that reaches a port and the daemon's memory limit.
package grpcserver

import (
	"context"
	"net"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/tap"
)

const (
	maxHeaderList = 8 << 10
	// connectionIdle closes a connection without streams, such as one that was opened and
	// left; Envoy and KEDA reconnect.
	connectionIdle = 5 * time.Minute
	pingEvery      = 2 * time.Minute
	pingTimeout    = 20 * time.Second
	// minPeerPing is the shortest interval a client may ping at before its connection is
	// closed. KEDA and Envoy do not ping.
	minPeerPing = time.Minute
)

// Limits bounds one listener.
type Limits struct {
	// MaxMessage is the largest request in bytes; a larger one is refused before it is
	// decoded.
	MaxMessage int
	// MaxStreams is the most streams one connection may open, and the most in flight
	// across all connections; a stream above it is refused before its request is read.
	MaxStreams uint32
}

// Server is a gRPC server with the limits. Register the services, then call Serve.
type Server struct {
	*grpc.Server
	gate *gate
}

// New returns a server with the limits.
func New(l Limits) *Server {
	g := &gate{limit: int64(l.MaxStreams)}

	return &Server{
		gate: g,
		Server: grpc.NewServer(
			grpc.MaxRecvMsgSize(l.MaxMessage),
			grpc.MaxConcurrentStreams(l.MaxStreams),
			grpc.MaxHeaderListSize(maxHeaderList),
			grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionIdle: connectionIdle, Time: pingEvery, Timeout: pingTimeout}),
			grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: minPeerPing}),
			grpc.InTapHandle(g.admit),
			grpc.StatsHandler(g),
		),
	}
}

// Serve serves the registered services on lis.
func (s *Server) Serve(lis net.Listener) error {
	s.gate.methods = map[string]bool{}

	for name, info := range s.GetServiceInfo() {
		for _, m := range info.Methods {
			s.gate.methods["/"+name+"/"+m.Name] = true
		}
	}

	return s.Server.Serve(lis)
}

// gate counts the streams in flight across every connection. admit runs before a stream
// exists and refuses one above the limit; the stats End event of an admitted stream
// releases its slot. A stream for a method no service serves is refused in admit too,
// because grpc answers such a stream without an End event.
type gate struct {
	limit   int64
	open    atomic.Int64
	methods map[string]bool // set by Serve, read by admit afterwards
}

func (g *gate) admit(ctx context.Context, info *tap.Info) (context.Context, error) {
	if !g.methods[info.FullMethodName] {
		return nil, status.Errorf(codes.Unimplemented, "unknown method %s", info.FullMethodName)
	}

	if g.open.Add(1) > g.limit {
		g.open.Add(-1)
		return nil, status.Error(codes.ResourceExhausted, "too many streams in flight")
	}

	return ctx, nil
}

func (g *gate) HandleRPC(_ context.Context, s stats.RPCStats) {
	if _, ok := s.(*stats.End); ok {
		g.open.Add(-1)
	}
}

func (g *gate) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context { return ctx }

func (g *gate) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context { return ctx }

func (g *gate) HandleConn(context.Context, stats.ConnStats) {}
