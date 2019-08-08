package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/skycoin/dmsg/cipher"
	"github.com/skycoin/dmsg/disc"
	"github.com/skycoin/skycoin/src/util/logging"

	"github.com/skycoin/skywire/pkg/metrics"
	"github.com/skycoin/skywire/pkg/routing"
	"github.com/skycoin/skywire/pkg/transport"
	"github.com/skycoin/skywire/pkg/transport/dmsg"
)

// Hop is a wrapper around transport hop to add functionality
type Hop struct {
	*routing.Hop
	routeID routing.RouteID
}

// Node performs routes setup operations over messaging channel.
type Node struct {
	Logger    *logging.Logger
	messenger *dmsg.Client
	srvCount  int
	metrics   metrics.Recorder
}

// NewNode constructs a new SetupNode.
func NewNode(conf *Config, metrics metrics.Recorder) (*Node, error) {
	pk := conf.PubKey
	sk := conf.SecKey

	logger := logging.NewMasterLogger()
	if lvl, err := logging.LevelFromString(conf.LogLevel); err == nil {
		logger.SetLevel(lvl)
	}
	messenger := dmsg.NewClient(pk, sk, disc.NewHTTP(conf.Messaging.Discovery), dmsg.SetLogger(logger.PackageLogger(dmsg.Type)))

	return &Node{
		Logger:    logger.PackageLogger("routesetup"),
		metrics:   metrics,
		messenger: messenger,
		srvCount:  conf.Messaging.ServerCount,
	}, nil
}

// Serve starts transport listening loop.
func (sn *Node) Serve(ctx context.Context) error {
	if sn.srvCount > 0 {
		if err := sn.messenger.InitiateServerConnections(ctx, sn.srvCount); err != nil {
			return fmt.Errorf("messaging: %s", err)
		}
		sn.Logger.Info("Connected to messaging servers")
	}

	sn.Logger.Info("Starting Setup Node")

	for {
		tp, err := sn.messenger.Accept(ctx)
		if err != nil {
			return err
		}
		go func(tp transport.Transport) {
			if err := sn.serveTransport(tp); err != nil {
				sn.Logger.Warnf("Failed to serve Transport: %s", err)
			}
		}(tp)
	}
}

func (sn *Node) serveTransport(tr transport.Transport) error {
	proto := NewSetupProtocol(tr)
	sp, data, err := proto.ReadPacket()
	if err != nil {
		return err
	}

	sn.Logger.Infof("Got new Setup request with type %s: %s", sp, string(data))
	defer sn.Logger.Infof("Completed Setup request with type %s: %s", sp, string(data))

	startTime := time.Now()
	switch sp {
	case PacketCreateLoop:
		var ld routing.LoopDescriptor
		if err = json.Unmarshal(data, &ld); err == nil {
			err = sn.createLoop(ld)
		}
	case PacketCloseLoop:
		var ld routing.LoopData
		if err = json.Unmarshal(data, &ld); err == nil {
			if _, ok := sn.remote(tr.Edges()); !ok {
				return errors.New("configured PubKey not found in edges")
			}
			err = sn.closeLoop(ld.Loop.Remote.PubKey, routing.LoopData{
				Loop: routing.Loop{
					Remote: ld.Loop.Local,
					Local:  ld.Loop.Remote,
				},
			})
		}
	default:
		err = errors.New("unknown foundation packet")
	}
	sn.metrics.Record(time.Since(startTime), err != nil)

	if err != nil {
		sn.Logger.Infof("Setup request with type %s failed: %s", sp, err)
		return proto.WritePacket(RespFailure, err)
	}

	return proto.WritePacket(RespSuccess, nil)
}

func (sn *Node) createLoop(ld routing.LoopDescriptor) error {
	sn.Logger.Infof("Creating new Loop %s", ld)
	rRouteID, err := sn.createRoute(ld.Expiry, ld.Reverse, ld.Loop.Local.Port, ld.Loop.Remote.Port)
	if err != nil {
		return err
	}

	fRouteID, err := sn.createRoute(ld.Expiry, ld.Forward, ld.Loop.Remote.Port, ld.Loop.Local.Port)
	if err != nil {
		return err
	}

	if len(ld.Forward) == 0 || len(ld.Reverse) == 0 {
		return nil
	}

	initiator := ld.Initiator()
	responder := ld.Responder()

	ldR := routing.LoopData{
		Loop: routing.Loop{
			Remote: routing.Addr{
				PubKey: initiator,
				Port:   ld.Loop.Local.Port,
			},
			Local: routing.Addr{
				PubKey: responder,
				Port:   ld.Loop.Remote.Port,
			},
		},
		RouteID: rRouteID,
	}
	if err := sn.connectLoop(responder, ldR); err != nil {
		sn.Logger.Warnf("Failed to confirm loop with responder: %s", err)
		return fmt.Errorf("loop connect: %s", err)
	}

	ldI := routing.LoopData{
		Loop: routing.Loop{
			Remote: routing.Addr{
				PubKey: responder,
				Port:   ld.Loop.Remote.Port,
			},
			Local: routing.Addr{
				PubKey: initiator,
				Port:   ld.Loop.Local.Port,
			},
		},
		RouteID: fRouteID,
	}
	if err := sn.connectLoop(initiator, ldI); err != nil {
		sn.Logger.Warnf("Failed to confirm loop with initiator: %s", err)
		if err := sn.closeLoop(responder, ldR); err != nil {
			sn.Logger.Warnf("Failed to close loop: %s", err)
		}
		return fmt.Errorf("loop connect: %s", err)
	}

	sn.Logger.Infof("Created Loop %s", ld)
	return nil
}

func (sn *Node) createRoute(expireAt time.Time, route routing.Route, rport, lport routing.Port) (routing.RouteID, error) {
	if len(route) == 0 {
		return 0, nil
	}

	sn.Logger.Infof("Creating new Route %s", route)
	r := make([]*Hop, len(route))

	initiator := route[0].From

	// indicate errors occurred during rules setup
	rulesSetupErrs := make(chan error, len(r))

	// context to cancel rule setup in case of errors
	ctx, cancel := context.WithCancel(context.Background())
	for idx := len(r) - 1; idx >= 0; idx-- {
		hop := &Hop{Hop: route[idx]}
		r[idx] = hop

		var nextHop *Hop
		if idx != len(r)-1 {
			nextHop = r[idx+1]
		}

		go func(idx int, hop, nextHop *Hop) {
			routeID, err := sn.requestRouteID(ctx, hop.To)
			if err != nil {
				// filter out context cancellation errors
				if err == context.Canceled {
					rulesSetupErrs <- err
				} else {
					rulesSetupErrs <- fmt.Errorf("rule setup: %s", err)
				}
				return
			}

			hop.routeID = routeID

			var rule routing.Rule
			if nextHop == nil {
				rule = routing.AppRule(expireAt, 0, initiator, lport, rport, routeID)
			} else {
				rule = routing.ForwardRule(expireAt, nextHop.routeID, nextHop.Transport, routeID)
			}

			err = sn.setupRule(ctx, hop.To, rule)
			if err != nil {
				// filter out context cancellation errors
				if err == context.Canceled {
					rulesSetupErrs <- err
				} else {
					rulesSetupErrs <- fmt.Errorf("rule setup: %s", err)
				}
				return
			}

			// put nil to avoid block
			rulesSetupErrs <- nil
		}(idx, hop, nextHop)
	}

	var rulesSetupErr error
	var cancelOnce sync.Once
	// check for any errors occurred so far
	for range r {
		// filter out context cancellation errors
		if err := <-rulesSetupErrs; err != nil && err != context.Canceled {
			// rules setup failed, cancel further setup
			cancelOnce.Do(cancel)
			rulesSetupErr = err
		}
	}

	// close chan to avoid leaks
	close(rulesSetupErrs)
	if rulesSetupErr != nil {
		return 0, rulesSetupErr
	}

	routeID, err := sn.requestRouteID(context.Background(), initiator)
	if err != nil {
		return 0, fmt.Errorf("request route id: %s", err)
	}

	rule := routing.ForwardRule(expireAt, r[0].routeID, r[0].Transport, routeID)
	if err := sn.setupRule(context.Background(), initiator, rule); err != nil {
		return 0, fmt.Errorf("rule setup: %s", err)
	}

	return routeID, nil
}

func (sn *Node) connectLoop(on cipher.PubKey, ld routing.LoopData) error {
	ctx := context.Background()

	tr, err := sn.messenger.Dial(ctx, on)
	if err != nil {
		return fmt.Errorf("transport: %s", err)
	}
	defer func() {
		if err := tr.Close(); err != nil {
			sn.Logger.Warnf("Failed to close transport: %s", err)
		}
	}()

	if err := ConfirmLoop(NewSetupProtocol(tr), ld); err != nil {
		return err
	}

	sn.Logger.Infof("Confirmed loop on %s with %s. RemotePort: %d. LocalPort: %d", on, ld.Loop.Remote.PubKey, ld.Loop.Remote.Port, ld.Loop.Local.Port)
	return nil
}

// Close closes underlying dmsg client.
func (sn *Node) Close() error {
	if sn == nil {
		return nil
	}
	return sn.messenger.Close()
}

func (sn *Node) remote(edges [2]cipher.PubKey) (cipher.PubKey, bool) {
	pubKey := sn.messenger.Local()
	if pubKey == edges[0] {
		return edges[1], true
	}
	if pubKey == edges[1] {
		return edges[0], true
	}
	return cipher.PubKey{}, false
}

func (sn *Node) closeLoop(on cipher.PubKey, ld routing.LoopData) error {
	fmt.Printf(">>> BEGIN: closeLoop(%s, ld)\n", on)
	defer fmt.Printf(">>>   END: closeLoop(%s, ld)\n", on)
	ctx := context.Background()

	tr, err := sn.messenger.Dial(ctx, on)
	fmt.Println(">>> *****: closeLoop() dialed:", err)
	if err != nil {
		return fmt.Errorf("transport: %s", err)
	}
	defer func() {
		if err := tr.Close(); err != nil {
			sn.Logger.Warnf("Failed to close transport: %s", err)
		}
	}()

	proto := NewSetupProtocol(tr)
	if err := LoopClosed(proto, ld); err != nil {
		return err
	}

	sn.Logger.Infof("Closed loop on %s. LocalPort: %d", on, ld.Loop.Local.Port)
	return nil
}

func (sn *Node) requestRouteID(ctx context.Context, pubKey cipher.PubKey) (routing.RouteID, error) {
	sn.Logger.Debugf("dialing to %s to request route ID\n", pubKey)
	tr, err := sn.messenger.Dial(ctx, pubKey)
	if err != nil {
		return 0, fmt.Errorf("transport: %s", err)
	}
	defer func() {
		if err := tr.Close(); err != nil {
			sn.Logger.Warnf("Failed to close transport: %s", err)
		}
	}()

	proto := NewSetupProtocol(tr)
	routeID, err := RequestRouteID(proto)
	if err != nil {
		return 0, err
	}

	sn.Logger.Infof("Received route ID %d from %s", routeID, pubKey)
	return routeID, nil
}

func (sn *Node) setupRule(ctx context.Context, pubKey cipher.PubKey, rule routing.Rule) error {
	sn.Logger.Debugf("dialing to %s to setup rule: %v\n", pubKey, rule)
	tr, err := sn.messenger.Dial(ctx, pubKey)
	if err != nil {
		return fmt.Errorf("transport: %s", err)
	}
	defer func() {
		if err := tr.Close(); err != nil {
			sn.Logger.Warnf("Failed to close transport: %s", err)
		}
	}()

	proto := NewSetupProtocol(tr)
	if err := AddRule(proto, rule); err != nil {
		return err
	}

	sn.Logger.Infof("Set rule of type %s on %s", rule.Type(), pubKey)
	return nil
}
