//go:build with_cloudflared

package cloudflare

import (
	"context"
	"net"
	"sync"
	"time"

	cloudflared "github.com/sagernet/sing-cloudflared"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	boxDialer "github.com/sagernet/sing-box/common/dialer"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/bufio"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json/badoption"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/pipe"
	tun "github.com/sagernet/sing-tun"
)

func RegisterInbound(registry *inbound.Registry) {
	inbound.Register[option.CloudflaredInboundOptions](registry, C.TypeCloudflared, NewInbound)
}

func NewInbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.CloudflaredInboundOptions) (adapter.Inbound, error) {
	controlDialer, err := boxDialer.NewWithOptions(boxDialer.Options{
		Context:        ctx,
		Options:        options.ControlDialer,
		RemoteIsDomain: true,
	})
	if err != nil {
		return nil, E.Cause(err, "build cloudflared control dialer")
	}
	tunnelDialer, err := boxDialer.NewWithOptions(boxDialer.Options{
		Context: ctx,
		Options: options.TunnelDialer,
	})
	if err != nil {
		return nil, E.Cause(err, "build cloudflared tunnel dialer")
	}

	service, err := cloudflared.NewService(cloudflared.ServiceOptions{
		Logger:           logger,
		ConnectionDialer: &routerDialer{router: router, tag: tag},
		ControlDialer:    controlDialer,
		TunnelDialer:     tunnelDialer,
		ICMPHandler:      &icmpRouterHandler{router: router, tag: tag},
		ConnContext: func(ctx context.Context) context.Context {
			return adapter.WithContext(ctx, &adapter.InboundContext{
				Inbound:     tag,
				InboundType: C.TypeCloudflared,
			})
		},
		Token:           options.Token,
		HAConnections:   options.HAConnections,
		Protocol:        options.Protocol,
		PostQuantum:     options.PostQuantum,
		EdgeIPVersion:   options.EdgeIPVersion,
		DatagramVersion: options.DatagramVersion,
		GracePeriod:     resolveGracePeriod(options.GracePeriod),
		Region:          options.Region,
	})
	if err != nil {
		return nil, err
	}

	return &Inbound{
		Adapter: inbound.NewAdapter(C.TypeCloudflared, tag),
		service: service,
	}, nil
}

type Inbound struct {
	inbound.Adapter
	service *cloudflared.Service
}

func (i *Inbound) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	return i.service.Start()
}

func (i *Inbound) Close() error {
	return i.service.Close()
}

func resolveGracePeriod(value *badoption.Duration) time.Duration {
	if value == nil {
		return 0
	}
	return time.Duration(*value)
}

// routerDialer bridges N.Dialer to the sing-box router for origin connections.
type routerDialer struct {
	router adapter.Router
	tag    string
}

func (d *routerDialer) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	input, output := pipe.Pipe()
	done := make(chan struct{})
	metadata := adapter.InboundContext{
		Inbound:     d.tag,
		InboundType: C.TypeCloudflared,
		Network:     N.NetworkTCP,
		Destination: destination,
	}
	var closeOnce sync.Once
	closePipe := func() {
		closeOnce.Do(func() {
			common.Close(input, output)
		})
	}
	go d.router.RouteConnectionEx(ctx, output, metadata, N.OnceClose(func(it error) {
		closePipe()
		close(done)
	}))
	return input, nil
}

func (d *routerDialer) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	originDialer, ok := d.router.(routedOriginPacketDialer)
	if !ok {
		return nil, E.New("router does not support cloudflare routed packet dialing")
	}
	packetConn, err := originDialer.DialRoutePacketConnection(ctx, adapter.InboundContext{
		Inbound:     d.tag,
		InboundType: C.TypeCloudflared,
		Network:     N.NetworkUDP,
		Destination: destination,
		UDPConnect:  true,
	})
	if err != nil {
		return nil, err
	}
	return bufio.NewNetPacketConn(packetConn), nil
}

type routedOriginPacketDialer interface {
	DialRoutePacketConnection(ctx context.Context, metadata adapter.InboundContext) (N.PacketConn, error)
}

// icmpRouterHandler bridges cloudflared.ICMPHandler to router.PreMatch.
type icmpRouterHandler struct {
	router adapter.Router
	tag    string
}

func (h *icmpRouterHandler) RouteICMPConnection(ctx context.Context, session tun.DirectRouteSession, routeContext tun.DirectRouteContext, timeout time.Duration) (tun.DirectRouteDestination, error) {
	var ipVersion uint8
	if session.Source.Is4() {
		ipVersion = 4
	} else {
		ipVersion = 6
	}
	metadata := adapter.InboundContext{
		Inbound:           h.tag,
		InboundType:       C.TypeCloudflared,
		IPVersion:         ipVersion,
		Network:           N.NetworkICMP,
		Source:            M.SocksaddrFrom(session.Source, 0),
		Destination:       M.SocksaddrFrom(session.Destination, 0),
		OriginDestination: M.SocksaddrFrom(session.Destination, 0),
	}
	return h.router.PreMatch(metadata, routeContext, timeout, false)
}
