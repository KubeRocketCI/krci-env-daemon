// Package grpcserver builds the daemon's gRPC servers with the limits that bound what a
// client can make them hold: the request size, the streams in flight, the metadata size,
// the open connections, and connections that are idle or stall in the handshake. Neither
// listener authenticates its peer; the limits are what stands between a pod that reaches a
// port and the daemon's memory limit.
package grpcserver

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/netutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/tap"
)

// handshakeTimeout closes a connection that has not finished the HTTP/2 handshake; until
// then it holds a MaxConns slot. grpc waits 120 s by default. A variable for the tests.
var handshakeTimeout = 10 * time.Second

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
	// MaxConns is the most open connections; one above it waits to be accepted. Zero sets
	// no limit.
	MaxConns int
}

// Server is a gRPC server with the limits. Register the services, then call Serve.
type Server struct {
	*grpc.Server
	gate     *gate
	maxConns int
}

// New returns a server with the limits.
func New(l Limits) *Server {
	g := &gate{limit: int64(l.MaxStreams)}

	return &Server{
		gate:     g,
		maxConns: l.MaxConns,
		Server: grpc.NewServer(
			grpc.ConnectionTimeout(handshakeTimeout),
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

// Serve serves lis, limited to MaxConns open connections. It admits streams only for the
// methods registered before the call.
func (s *Server) Serve(lis net.Listener) error {
	s.gate.methods = map[string]bool{}

	for name, info := range s.GetServiceInfo() {
		for _, m := range info.Methods {
			s.gate.methods["/"+name+"/"+m.Name] = true
		}
	}

	if s.maxConns > 0 {
		lis = netutil.LimitListener(lis, s.maxConns)
	}

	return s.Server.Serve(lis)
}

// gate counts the streams in flight across every connection. admit runs before a stream
// exists and refuses one above the limit, and one for a method no service serves: grpc
// answers that without an End event, so its slot would never come back. An admitted stream
// gives its slot back on its End event or, when grpc drops it before its handler because
// its deadline had already passed on arrival, when that deadline fires.
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

	release := sync.OnceFunc(func() { g.open.Add(-1) })
	expired := context.AfterFunc(ctx, func() {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			release()
		}
	})

	return context.WithValue(ctx, slotKey{}, &slot{release: release, expired: expired}), nil
}

type slotKey struct{}

type slot struct {
	release func()
	expired func() bool // stops the deadline watch once the handler holds the stream
}

func (g *gate) HandleRPC(ctx context.Context, s stats.RPCStats) {
	sl, _ := ctx.Value(slotKey{}).(*slot)
	if sl == nil {
		return
	}

	switch s.(type) {
	case *stats.InHeader:
		sl.expired()
	case *stats.End:
		sl.release()
	}
}

func (g *gate) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context { return ctx }

func (g *gate) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context { return ctx }

func (g *gate) HandleConn(context.Context, stats.ConnStats) {
	// Connections carry nothing to count: the gate counts streams.
}
