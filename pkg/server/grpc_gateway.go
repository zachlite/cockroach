// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package server

import (
	"context"
	"fmt"
	"net"

	"github.com/cockroachdb/cockroach/pkg/rpc"
	"github.com/cockroachdb/cockroach/pkg/server/telemetry"
	"github.com/cockroachdb/cockroach/pkg/ts"
	"github.com/cockroachdb/cockroach/pkg/util/httputil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/netutil"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	gwruntime "github.com/grpc-ecosystem/grpc-gateway/runtime"
	"google.golang.org/grpc"
)

// grpcGatewayServer represents a grpc service with HTTP endpoints through GRPC
// gateway.
type grpcGatewayServer interface { // XXX: interops between http+grpc
	RegisterService(g *grpc.Server)
	RegisterGateway(
		ctx context.Context,
		mux *gwruntime.ServeMux,
		conn *grpc.ClientConn,
	) error
}

var _ grpcGatewayServer = (*adminServer)(nil)
var _ grpcGatewayServer = (*statusServer)(nil)
var _ grpcGatewayServer = (*authenticationServer)(nil)
var _ grpcGatewayServer = (*ts.Server)(nil)

// configureGRPCGateway initializes services necessary for running the
// GRPC Gateway services proxied against the server at `grpcSrv`.
//
// The connection between the reverse proxy provided by grpc-gateway
// and our grpc server uses a loopback-based listener to create
// connections between the two.
//
// The function returns 3 arguments that are necessary to call
// `RegisterGateway` which generated for each of your gRPC services
// by grpc-gateway.
func configureGRPCGateway(
	ctx, workersCtx context.Context,
	ambientCtx log.AmbientContext,
	rpcContext *rpc.Context,
	stopper *stop.Stopper,
	grpcSrv *grpcServer,
	GRPCAddr string,
) (*gwruntime.ServeMux, context.Context, *grpc.ClientConn, error) {
	jsonpb := &protoutil.JSONPb{
		EnumsAsInts:  true,
		EmitDefaults: true,
		Indent:       "  ",
	}
	protopb := new(protoutil.ProtoPb)
	gwMux := gwruntime.NewServeMux(
		gwruntime.WithMarshalerOption(gwruntime.MIMEWildcard, jsonpb),
		gwruntime.WithMarshalerOption(httputil.JSONContentType, jsonpb),
		gwruntime.WithMarshalerOption(httputil.AltJSONContentType, jsonpb),
		gwruntime.WithMarshalerOption(httputil.ProtoContentType, protopb),
		gwruntime.WithMarshalerOption(httputil.AltProtoContentType, protopb),
		gwruntime.WithOutgoingHeaderMatcher(authenticationHeaderMatcher),
		gwruntime.WithMetadata(forwardAuthenticationMetadata),
	)
	gwCtx, gwCancel := context.WithCancel(ambientCtx.AnnotateCtx(context.Background()))
	stopper.AddCloser(stop.CloserFn(gwCancel))

	// loopback handles the HTTP <-> RPC loopback connection.
	loopback := newLoopbackListener(workersCtx, stopper)

	waitQuiesce := func(context.Context) {
		<-stopper.ShouldQuiesce()
		_ = loopback.Close()
	}
	if err := stopper.RunAsyncTask(workersCtx, "gw-quiesce", waitQuiesce); err != nil {
		waitQuiesce(workersCtx)
	}

	_ = stopper.RunAsyncTask(workersCtx, "serve-loopback", func(context.Context) {
		netutil.FatalIfUnexpected(grpcSrv.Serve(loopback))
	})

	// Eschew `(*rpc.Context).GRPCDial` to avoid unnecessary moving parts on the
	// uniquely in-process connection.
	dialOpts, err := rpcContext.GRPCDialOptions()
	if err != nil {
		return nil, nil, nil, err
	}

	callCountInterceptor := func(
		ctx context.Context,
		method string,
		req, reply interface{},
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		telemetry.Inc(getServerEndpointCounter(method))
		return invoker(ctx, method, req, reply, cc, opts...)
	}
	conn, err := grpc.DialContext(ctx, GRPCAddr, append(append(
		dialOpts,
		grpc.WithUnaryInterceptor(callCountInterceptor)),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return loopback.Connect(ctx)
		}),
	)...)
	if err != nil {
		return nil, nil, nil, err
	}
	{
		waitQuiesce := func(workersCtx context.Context) {
			<-stopper.ShouldQuiesce()
			// NB: we can't do this as a Closer because (*Server).ServeWith is
			// running in a worker and usually sits on accept() which unblocks
			// only when the listener closes. In other words, the listener needs
			// to close when quiescing starts to allow that worker to shut down.
			err := conn.Close() // nolint:grpcconnclose
			if err != nil {
				log.Ops.Fatalf(workersCtx, "%v", err)
			}
		}
		if err := stopper.RunAsyncTask(workersCtx, "wait-quiesce", waitQuiesce); err != nil {
			waitQuiesce(workersCtx)
		}
	}
	return gwMux, gwCtx, conn, nil
}

// getServerEndpointCounter returns a telemetry Counter corresponding to the
// given grpc method.
func getServerEndpointCounter(method string) telemetry.Counter {
	const counterPrefix = "http.grpc-gateway"
	return telemetry.GetCounter(fmt.Sprintf("%s.%s", counterPrefix, method))
}
