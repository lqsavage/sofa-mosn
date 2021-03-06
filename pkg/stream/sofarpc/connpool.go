/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package sofarpc

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alipay/sofa-mosn/pkg/network"
	"github.com/alipay/sofa-mosn/pkg/protocol"
	str "github.com/alipay/sofa-mosn/pkg/stream"
	"github.com/alipay/sofa-mosn/pkg/types"
	"github.com/rcrowley/go-metrics"
)

func init() {
	network.RegisterNewPoolFactory(protocol.SofaRPC, NewConnPool)
	types.RegisterConnPoolFactory(protocol.SofaRPC, true)
}

var defaultSubProtocol byte = 0x00

// types.ConnectionPool
// activeClient used as connected client
// host is the upstream
type connPool struct {
	activeClients map[byte]*activeClient //sub protocol -> activeClient
	host          types.Host

	mux sync.Mutex
}

// NewConnPool
func NewConnPool(host types.Host) types.ConnectionPool {
	return &connPool{
		activeClients: make(map[byte]*activeClient),
		host:          host,
	}
}

func (p *connPool) Protocol() types.Protocol {
	return protocol.SofaRPC
}

func (p *connPool) NewStream(ctx context.Context,
	responseDecoder types.StreamReceiveListener, listener types.PoolEventListener) {
	subProtocol := getSubProtocol(ctx)

	p.mux.Lock()
	if p.activeClients[subProtocol] == nil {
		p.activeClients[subProtocol] = newActiveClient(ctx, subProtocol, p)
	}
	activeClient := p.activeClients[subProtocol]
	p.mux.Unlock()

	if activeClient == nil {
		listener.OnFailure(types.ConnectionFailure, p.host)
		return
	}

	if !p.host.ClusterInfo().ResourceManager().Requests().CanCreate() {
		listener.OnFailure(types.Overflow, p.host)
		p.host.HostStats().UpstreamRequestPendingOverflow.Inc(1)
		p.host.ClusterInfo().Stats().UpstreamRequestPendingOverflow.Inc(1)
	} else {
		atomic.AddUint64(&activeClient.totalStream, 1)
		p.host.HostStats().UpstreamRequestTotal.Inc(1)
		p.host.HostStats().UpstreamRequestActive.Inc(1)
		p.host.ClusterInfo().Stats().UpstreamRequestTotal.Inc(1)
		p.host.ClusterInfo().Stats().UpstreamRequestActive.Inc(1)
		p.host.ClusterInfo().ResourceManager().Requests().Increase()
		streamEncoder := activeClient.client.NewStream(ctx, responseDecoder)
		streamEncoder.GetStream().AddEventListener(activeClient)

		listener.OnReady(streamEncoder, p.host)
	}

	return
}

func (p *connPool) Close() {
	p.mux.Lock()
	defer p.mux.Unlock()

	for _, ac := range p.activeClients {
		ac.client.Close()
	}
}

func (p *connPool) onConnectionEvent(client *activeClient, event types.ConnectionEvent) {
	// event.ConnectFailure() contains types.ConnectTimeout and types.ConnectTimeout
	if event.IsClose() {
		p.host.HostStats().UpstreamConnectionClose.Inc(1)
		p.host.HostStats().UpstreamConnectionActive.Dec(1)

		p.host.ClusterInfo().Stats().UpstreamConnectionClose.Inc(1)
		p.host.ClusterInfo().Stats().UpstreamConnectionActive.Dec(1)

		switch event {
		case types.LocalClose:
			p.host.HostStats().UpstreamConnectionLocalClose.Inc(1)
			p.host.ClusterInfo().Stats().UpstreamConnectionLocalClose.Inc(1)

			if client.closeWithActiveReq {
				p.host.HostStats().UpstreamConnectionLocalCloseWithActiveRequest.Inc(1)
				p.host.ClusterInfo().Stats().UpstreamConnectionLocalCloseWithActiveRequest.Inc(1)
			}

		case types.RemoteClose:
			p.host.HostStats().UpstreamConnectionRemoteClose.Inc(1)
			p.host.ClusterInfo().Stats().UpstreamConnectionRemoteClose.Inc(1)

			if client.closeWithActiveReq {
				p.host.HostStats().UpstreamConnectionRemoteCloseWithActiveRequest.Inc(1)
				p.host.ClusterInfo().Stats().UpstreamConnectionRemoteCloseWithActiveRequest.Inc(1)

			}
		default:
			// do nothing
		}
		p.mux.Lock()
		p.activeClients[client.subProtocol] = nil
		p.mux.Unlock()
	} else if event == types.ConnectTimeout {
		p.host.HostStats().UpstreamRequestTimeout.Inc(1)
		p.host.ClusterInfo().Stats().UpstreamRequestTimeout.Inc(1)
		client.client.Close()
		p.activeClients[client.subProtocol] = nil
	} else if event == types.ConnectFailed {
		p.host.HostStats().UpstreamConnectionConFail.Inc(1)
		p.host.ClusterInfo().Stats().UpstreamConnectionConFail.Inc(1)
		p.activeClients[client.subProtocol] = nil
	}
}

func (p *connPool) onStreamDestroy(client *activeClient) {
	p.host.HostStats().UpstreamRequestActive.Dec(1)
	p.host.ClusterInfo().Stats().UpstreamRequestActive.Dec(1)
	p.host.ClusterInfo().ResourceManager().Requests().Decrease()
}

func (p *connPool) onStreamReset(client *activeClient, reason types.StreamResetReason) {
	if reason == types.StreamConnectionTermination || reason == types.StreamConnectionFailed {
		p.host.HostStats().UpstreamRequestFailureEject.Inc(1)
		p.host.ClusterInfo().Stats().UpstreamRequestFailureEject.Inc(1)
		client.closeWithActiveReq = true
	} else if reason == types.StreamLocalReset {
		p.host.HostStats().UpstreamRequestLocalReset.Inc(1)
		p.host.ClusterInfo().Stats().UpstreamRequestLocalReset.Inc(1)
	} else if reason == types.StreamRemoteReset {
		p.host.HostStats().UpstreamRequestRemoteReset.Inc(1)
		p.host.ClusterInfo().Stats().UpstreamRequestRemoteReset.Inc(1)
	}
}

func (p *connPool) createStreamClient(context context.Context, connData types.CreateConnectionData) str.Client {
	return str.NewStreamClient(context, protocol.SofaRPC, connData.Connection, connData.HostInfo)
}

// keepAliveListener is a types.ConnectionEventListener
type keepAliveListener struct {
	keepAlive types.KeepAlive
}

func (l *keepAliveListener) OnEvent(event types.ConnectionEvent) {
	if event == types.OnReadTimeout {
		l.keepAlive.SendKeepAlive()
	}
}

// types.StreamEventListener
// types.ConnectionEventListener
// types.StreamConnectionEventListener
type activeClient struct {
	subProtocol        byte
	pool               *connPool
	keepAlive          *keepAliveListener
	client             str.Client
	host               types.CreateConnectionData
	closeWithActiveReq bool
	totalStream        uint64
}

func newActiveClient(ctx context.Context, subProtocol byte, pool *connPool) *activeClient {
	ac := &activeClient{
		subProtocol: subProtocol,
		pool:        pool,
	}

	data := pool.host.CreateConnection(ctx)
	connCtx := context.WithValue(context.Background(), types.ContextKeyConnectionID, data.Connection.ID())
	codecClient := pool.createStreamClient(connCtx, data)
	codecClient.AddConnectionEventListener(ac)
	codecClient.SetStreamConnectionEventListener(ac)

	ac.client = codecClient
	ac.host = data

	if err := ac.client.Connect(true); err != nil {
		return nil
	}
	// Add Keep Alive
	// protocol is from onNewDetectStream
	// TODO: support protocol convert
	protocolValue := ctx.Value(types.ContextSubProtocol)
	if proto, ok := protocolValue.(byte); ok {
		// TODO: support config
		ac.keepAlive = &keepAliveListener{
			keepAlive: NewSofaRPCKeepAlive(codecClient, proto, time.Second, 6),
		}
		ac.client.AddConnectionEventListener(ac.keepAlive)
		go ac.keepAlive.keepAlive.Start()
	}
	// stats
	pool.host.HostStats().UpstreamConnectionTotal.Inc(1)
	pool.host.HostStats().UpstreamConnectionActive.Inc(1)
	pool.host.ClusterInfo().Stats().UpstreamConnectionTotal.Inc(1)
	pool.host.ClusterInfo().Stats().UpstreamConnectionActive.Inc(1)

	// bytes total adds all connections data together, but buffered data not
	codecClient.SetConnectionStats(&types.ConnectionStats{
		ReadTotal:     pool.host.ClusterInfo().Stats().UpstreamBytesReadTotal,
		ReadBuffered:  metrics.NewGauge(),
		WriteTotal:    pool.host.ClusterInfo().Stats().UpstreamBytesWriteTotal,
		WriteBuffered: metrics.NewGauge(),
	})

	return ac
}

func (ac *activeClient) OnEvent(event types.ConnectionEvent) {
	ac.pool.onConnectionEvent(ac, event)
}

// types.StreamEventListener
func (ac *activeClient) OnDestroyStream() {
	ac.pool.onStreamDestroy(ac)
}

func (ac *activeClient) OnResetStream(reason types.StreamResetReason) {
	ac.pool.onStreamReset(ac, reason)
}

// types.StreamConnectionEventListener
func (ac *activeClient) OnGoAway() {}

func getSubProtocol(ctx context.Context) byte {
	if ctx != nil {
		if val := ctx.Value(types.ContextSubProtocol); val != nil {
			if code, ok := val.(byte); ok {
				return code
			}
		}
	}
	return defaultSubProtocol
}
