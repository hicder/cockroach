// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package cli

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/rpc"
	"github.com/cockroachdb/cockroach/pkg/server"
	"github.com/cockroachdb/cockroach/pkg/server/serverpb"
	"github.com/cockroachdb/cockroach/pkg/util/netutil/addr"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/errors"
	"google.golang.org/grpc"
)

// getClientGRPCConn returns a ClientConn and a method that blocks
// until the connection (and its associated goroutines) have terminated.
func getClientGRPCConn(ctx context.Context, cfg server.Config) (*grpc.ClientConn, func(), error) {
	if ctx.Done() == nil {
		return nil, nil, errors.New("context must be cancellable")
	}
	// 0 to disable max offset checks; this RPC context is not a member of the
	// cluster, so there's no need to enforce that its max offset is the same
	// as that of nodes in the cluster.
	clock := &timeutil.DefaultTimeSource{}
	tracer := cfg.Tracer
	if tracer == nil {
		tracer = tracing.NewTracer()
	}
	stopper := stop.NewStopper(stop.WithTracer(tracer))
	rpcContext := rpc.NewContext(ctx,
		rpc.ContextOptions{
			TenantID: roachpb.SystemTenantID,
			Config:   cfg.Config,
			Clock:    clock,
			Stopper:  stopper,
			Settings: cfg.Settings,

			ClientOnly: true,
		})
	if cfg.TestingKnobs.Server != nil {
		rpcContext.Knobs = cfg.TestingKnobs.Server.(*server.TestingKnobs).ContextTestingKnobs
	}
	addr, err := addr.AddrWithDefaultLocalhost(cfg.AdvertiseAddr)
	if err != nil {
		stopper.Stop(ctx)
		return nil, nil, err
	}
	// We use GRPCUnvalidatedDial() here because it does not matter
	// to which node we're talking to.
	conn, err := rpcContext.GRPCUnvalidatedDial(addr).Connect(ctx)
	if err != nil {
		stopper.Stop(ctx)
		return nil, nil, err
	}
	stopper.AddCloser(stop.CloserFn(func() {
		_ = conn.Close() // nolint:grpcconnclose
	}))

	closer := func() {
		// We use context.Background() here and not ctx because we
		// want to ensure that the closers always run to completion
		// even if the context used to create the client conn is
		// canceled.
		stopper.Stop(context.Background())
	}
	return conn, closer, nil
}

// getAdminClient returns an AdminClient and a closure that must be invoked
// to free associated resources.
func getAdminClient(ctx context.Context, cfg server.Config) (serverpb.AdminClient, func(), error) {
	conn, finish, err := getClientGRPCConn(ctx, cfg)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to connect to the node")
	}
	return serverpb.NewAdminClient(conn), finish, nil
}

// getStatusClient returns a StatusClient and a closure that must be invoked
// to free associated resources.
func getStatusClient(
	ctx context.Context, cfg server.Config,
) (serverpb.StatusClient, func(), error) {
	conn, finish, err := getClientGRPCConn(ctx, cfg)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to connect to the node")
	}
	return serverpb.NewStatusClient(conn), finish, nil
}
