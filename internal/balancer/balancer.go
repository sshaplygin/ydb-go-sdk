package balancer

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	"github.com/ydb-platform/ydb-go-sdk/v3/config"
	"github.com/ydb-platform/ydb-go-sdk/v3/discovery"
	balancerConfig "github.com/ydb-platform/ydb-go-sdk/v3/internal/balancer/config"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/closer"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/conn"
	internalDiscovery "github.com/ydb-platform/ydb-go-sdk/v3/internal/discovery"
	discoveryConfig "github.com/ydb-platform/ydb-go-sdk/v3/internal/discovery/config"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/endpoint"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/repeater"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/xerrors"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/xsync"
	"github.com/ydb-platform/ydb-go-sdk/v3/retry"
	"github.com/ydb-platform/ydb-go-sdk/v3/trace"
)

var ErrNoEndpoints = xerrors.Wrap(fmt.Errorf("no endpoints"))

type discoveryClient interface {
	discovery.Client
	closer.Closer
}

type Balancer struct {
	driverConfig      config.Config
	balancerConfig    balancerConfig.Config
	pool              *conn.Pool
	discoveryClient   func(ctx context.Context) (discoveryClient, error)
	discoveryRepeater repeater.Repeater
	localDCDetector   func(ctx context.Context, endpoints []endpoint.Endpoint) (string, error)

	mu               xsync.RWMutex
	connectionsState *connectionsState

	onDiscovery []func(ctx context.Context, endpoints []endpoint.Info)
}

func (b *Balancer) OnUpdate(onDiscovery func(ctx context.Context, endpoints []endpoint.Info)) {
	b.mu.WithLock(func() {
		b.onDiscovery = append(b.onDiscovery, onDiscovery)
	})
}

func (b *Balancer) clusterDiscovery(ctx context.Context) (err error) {
	if err = retry.Retry(ctx, func(ctx context.Context) (err error) {
		if err = b.clusterDiscoveryAttempt(ctx); err != nil {
			return xerrors.WithStackTrace(err)
		}
		return nil
	}, retry.WithIdempotent(true)); err != nil {
		return xerrors.WithStackTrace(err)
	}
	return nil
}

func (b *Balancer) clusterDiscoveryAttempt(ctx context.Context) (err error) {
	var (
		onDone = trace.DriverOnBalancerUpdate(
			b.driverConfig.Trace(),
			&ctx,
			b.balancerConfig.DetectlocalDC,
		)
		endpoints []endpoint.Endpoint
		localDC   string
	)

	defer func() {
		// if got err but parent context is not done - mark error as retryable
		if err != nil && ctx.Err() == nil && xerrors.Is(err,
			context.DeadlineExceeded,
			context.Canceled,
		) {
			err = xerrors.WithStackTrace(xerrors.Retryable(err))
		}
		nodes := make([]trace.EndpointInfo, 0, len(endpoints))
		for _, e := range endpoints {
			nodes = append(nodes, e.Copy())
		}
		onDone(
			nodes,
			localDC,
			err,
		)
	}()

	var (
		childCtx context.Context
		cancel   context.CancelFunc
	)
	if dialTimeout := b.driverConfig.DialTimeout(); dialTimeout > 0 {
		childCtx, cancel = context.WithTimeout(ctx, dialTimeout)
	} else {
		childCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	client, err := b.discoveryClient(childCtx)
	if err != nil {
		return xerrors.WithStackTrace(err)
	}
	defer func() {
		_ = client.Close(ctx)
	}()

	endpoints, err = client.Discover(childCtx)
	if err != nil {
		return xerrors.WithStackTrace(err)
	}

	if b.balancerConfig.DetectlocalDC {
		localDC, err = b.localDCDetector(childCtx, endpoints)
		if err != nil {
			return xerrors.WithStackTrace(err)
		}
	}

	b.applyDiscoveredEndpoints(childCtx, endpoints, localDC)

	return nil
}

func (b *Balancer) applyDiscoveredEndpoints(ctx context.Context, endpoints []endpoint.Endpoint, localDC string) {
	connections := endpointsToConnections(b.pool, endpoints)
	for _, c := range connections {
		b.pool.Allow(ctx, c)
		c.Endpoint().Touch()
	}

	info := balancerConfig.Info{SelfLocation: localDC}
	state := newConnectionsState(connections, b.balancerConfig.IsPreferConn, info, b.balancerConfig.AllowFalback)

	endpointsInfo := make([]endpoint.Info, len(endpoints))
	for i, e := range endpoints {
		endpointsInfo[i] = e
	}

	b.mu.WithLock(func() {
		b.connectionsState = state
		for _, onDiscovery := range b.onDiscovery {
			onDiscovery(ctx, endpointsInfo)
		}
	})
}

func (b *Balancer) Close(ctx context.Context) (err error) {
	onDone := trace.DriverOnBalancerClose(
		b.driverConfig.Trace(),
		&ctx,
	)
	defer func() {
		onDone(err)
	}()

	if b.discoveryRepeater != nil {
		b.discoveryRepeater.Stop()
	}

	return nil
}

func New(
	ctx context.Context,
	driverConfig config.Config,
	pool *conn.Pool,
	opts ...discoveryConfig.Option,
) (b *Balancer, err error) {
	var (
		onDone = trace.DriverOnBalancerInit(
			driverConfig.Trace(),
			&ctx,
		)
		discoveryConfig = discoveryConfig.New(opts...)
	)
	defer func() {
		onDone(err)
	}()

	b = &Balancer{
		driverConfig:    driverConfig,
		pool:            pool,
		localDCDetector: detectLocalDC,
		discoveryClient: func(ctx context.Context) (_ discoveryClient, err error) {
			cc, err := grpc.DialContext(ctx,
				"dns:///"+b.driverConfig.Endpoint(),
				b.driverConfig.GrpcDialOptions()...,
			)
			if err != nil {
				return nil, xerrors.WithStackTrace(err)
			}
			return internalDiscovery.New(cc, discoveryConfig), nil
		},
	}

	if config := driverConfig.Balancer(); config == nil {
		b.balancerConfig = balancerConfig.Config{}
	} else {
		b.balancerConfig = *config
	}

	if b.balancerConfig.SingleConn {
		b.connectionsState = newConnectionsState(
			endpointsToConnections(pool, []endpoint.Endpoint{
				endpoint.New(driverConfig.Endpoint()),
			}),
			nil, balancerConfig.Info{}, false)
	} else {
		// initialization of balancer state
		if err = b.clusterDiscovery(ctx); err != nil {
			return nil, xerrors.WithStackTrace(err)
		}
		// run background discovering
		if d := discoveryConfig.Interval(); d > 0 {
			b.discoveryRepeater = repeater.New(d, b.clusterDiscoveryAttempt,
				repeater.WithName("discovery"),
				repeater.WithTrace(b.driverConfig.Trace()),
			)
		}
	}

	return b, nil
}

func (b *Balancer) Invoke(
	ctx context.Context,
	method string,
	args interface{},
	reply interface{},
	opts ...grpc.CallOption,
) error {
	return b.wrapCall(ctx, func(ctx context.Context, cc conn.Conn) error {
		return cc.Invoke(ctx, method, args, reply, opts...)
	})
}

func (b *Balancer) NewStream(
	ctx context.Context,
	desc *grpc.StreamDesc,
	method string,
	opts ...grpc.CallOption,
) (_ grpc.ClientStream, err error) {
	var client grpc.ClientStream
	err = b.wrapCall(ctx, func(ctx context.Context, cc conn.Conn) error {
		client, err = cc.NewStream(ctx, desc, method, opts...)
		return err
	})
	if err == nil {
		return client, nil
	}
	return nil, err
}

func (b *Balancer) wrapCall(ctx context.Context, f func(ctx context.Context, cc conn.Conn) error) (err error) {
	cc, err := b.getConn(ctx)
	if err != nil {
		return xerrors.WithStackTrace(err)
	}

	defer func() {
		if err == nil {
			if cc.GetState() == conn.Banned {
				b.pool.Allow(ctx, cc)
			}
		} else {
			if xerrors.MustPessimizeEndpoint(err, b.driverConfig.ExcludeGRPCCodesForPessimization()...) {
				b.pool.Ban(ctx, cc, err)
			}
		}
	}()

	if ctx, err = b.driverConfig.Meta().Context(ctx); err != nil {
		return xerrors.WithStackTrace(err)
	}

	if err = f(ctx, cc); err != nil {
		if conn.UseWrapping(ctx) {
			return xerrors.WithStackTrace(err)
		}
		return err
	}

	return nil
}

func (b *Balancer) connections() *connectionsState {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.connectionsState
}

func (b *Balancer) getConn(ctx context.Context) (c conn.Conn, err error) {
	onDone := trace.DriverOnBalancerChooseEndpoint(
		b.driverConfig.Trace(),
		&ctx,
	)
	defer func() {
		if err == nil {
			onDone(c.Endpoint(), nil)
		} else {
			onDone(nil, err)
		}
	}()

	if err = ctx.Err(); err != nil {
		return nil, xerrors.WithStackTrace(err)
	}

	var (
		state       = b.connections()
		failedCount int
	)

	defer func() {
		if failedCount*2 > state.PreferredCount() && b.discoveryRepeater != nil {
			b.discoveryRepeater.Force()
		}
	}()

	c, failedCount = state.GetConnection(ctx)
	if c == nil {
		return nil, xerrors.WithStackTrace(
			fmt.Errorf("%w: cannot get connection from Balancer after %d attempts", ErrNoEndpoints, failedCount),
		)
	}
	return c, nil
}

func endpointsToConnections(p *conn.Pool, endpoints []endpoint.Endpoint) []conn.Conn {
	conns := make([]conn.Conn, 0, len(endpoints))
	for _, e := range endpoints {
		conns = append(conns, p.Get(e))
	}
	return conns
}
