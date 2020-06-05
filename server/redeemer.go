package server

// generate the go bindings for github.com/livepeer/go-livepeer/net/redeemer.proto first !
//go:generate mockgen -source github.com/livepeer/go-livepeer/net/redeemer.pb.go -destination github.com/livepeer/go-livepeer/net/redeemer_mock.pb.go -package net

import (
	"context"
	"fmt"
	"io"
	"math/big"
	gonet "net"
	"net/url"
	"sync"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/event"
	"github.com/golang/glog"
	"github.com/livepeer/go-livepeer/eth"
	"github.com/livepeer/go-livepeer/net"
	"github.com/livepeer/go-livepeer/pm"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

var cleanupLoopTime = 1 * time.Hour

type Redeemer struct {
	recipient   ethcommon.Address
	subs        sync.Map
	eth         eth.LivepeerEthClient
	sm          pm.SenderMonitor
	quit        chan struct{}
	liveSenders sync.Map // ethCommon.Address => time.Time lastAccess
}

// NewRedeemer creates a new ticket redemption service instance
func NewRedeemer(recipient ethcommon.Address, eth eth.LivepeerEthClient, sm pm.SenderMonitor) (*Redeemer, error) {

	if recipient == (ethcommon.Address{}) {
		return nil, fmt.Errorf("must provide a recipient")
	}

	if eth == nil {
		return nil, fmt.Errorf("must provide a LivepeerEthClient")
	}

	if sm == nil {
		return nil, fmt.Errorf("must provide a SenderMonitor")
	}

	return &Redeemer{
		recipient: recipient,
		eth:       eth,
		sm:        sm,
		quit:      make(chan struct{}),
	}, nil
}

func (r *Redeemer) Start(host *url.URL) error {
	listener, err := gonet.Listen("tcp", host.String())
	if err != nil {
		return err
	}
	defer listener.Close()
	if err != nil {
		return err
	}
	// slice of gRPC options
	// Here we can configure things like TLS
	opts := []grpc.ServerOption{}
	// var s *grpc.Server
	s := grpc.NewServer(opts...)
	defer s.Stop()

	net.RegisterTicketRedeemerServer(s, r)

	go r.startCleanupLoop()

	return s.Serve(listener)
}

func (r *Redeemer) Stop() {
	close(r.quit)
}

func (r *Redeemer) QueueTicket(ctx context.Context, ticket *net.Ticket) (*net.QueueTicketRes, error) {
	t := pmTicket(ticket)
	if err := r.sm.QueueTicket(t); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	glog.Infof("ticket queued sender=0x%x", ticket.Sender)

	go r.monitorMaxFloat(ethcommon.BytesToAddress(ticket.Sender))
	return &net.QueueTicketRes{}, nil
}

func (r *Redeemer) monitorMaxFloat(sender ethcommon.Address) {
	_, ok := r.liveSenders.Load(sender)
	if ok {
		// update last access
		r.liveSenders.Store(sender, time.Now())
		return
	}
	r.liveSenders.Store(sender, time.Now())
	sink := make(chan *big.Int, 10)
	sub := r.sm.SubscribeMaxFloat(sender, sink)
	defer sub.Unsubscribe()
	for {
		select {
		case <-r.quit:
			return
		case err := <-sub.Err():
			glog.Error(err)
		case mf := <-sink:
			r.sendMaxFloatUpdate(sender, mf)
		}
	}
}

func (r *Redeemer) sendMaxFloatUpdate(sender ethcommon.Address, maxFloat *big.Int) {
	r.subs.Range(
		func(key, value interface{}) bool {
			var maxFloatB []byte
			if maxFloat != nil {
				maxFloatB = maxFloat.Bytes()
			}
			value.(chan *net.MaxFloatUpdate) <- &net.MaxFloatUpdate{
				Sender:   sender.Bytes(),
				MaxFloat: maxFloatB,
			}
			return true
		},
	)
}

func (r *Redeemer) MonitorMaxFloat(req *net.MonitorMaxFloatReq, stream net.TicketRedeemer_MonitorMaxFloatServer) error {
	// The client address will serve as the ID for the stream
	p, ok := peer.FromContext(stream.Context())
	if !ok {
		return status.Error(codes.Internal, "context is nil")
	}

	// Make a channel to receive max float updates
	//  This check allows to overwrite the channel for testing purposes
	var maxFloatUpdates chan *net.MaxFloatUpdate
	maxFloatUpdatesI, ok := r.subs.Load(p.Addr.String())
	if !ok {
		maxFloatUpdates = make(chan *net.MaxFloatUpdate)
		r.subs.Store(p.Addr.String(), maxFloatUpdates)
		glog.Infof("new MonitorMaxFloat subscriber: %v", p.Addr.String())
	} else {
		maxFloatUpdates, ok = maxFloatUpdatesI.(chan *net.MaxFloatUpdate)
		if !ok {
			return status.Error(codes.Internal, "maxFloatUpdates is of the wrong type")
		}
	}

	// Block so that the stream is over a long-lived connection
	for {
		select {
		case maxFloatUpdate := <-maxFloatUpdates:
			if err := stream.Send(maxFloatUpdate); err != nil {
				if err == io.EOF {
					r.subs.Delete(p.Addr.String())
					return status.Error(codes.Internal, err.Error())
				}
				glog.Errorf("Unable to send maxFloat update to client=%v err=%v", p.Addr.String(), err)
			}
		case <-r.quit:
			return nil
		case <-stream.Context().Done():
			return nil
		}
	}
}

func (r *Redeemer) MaxFloat(ctx context.Context, req *net.MaxFloatReq) (*net.MaxFloatUpdate, error) {
	mf, err := r.sm.MaxFloat(ethcommon.BytesToAddress(req.Sender))
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Errorf("max float error: %v", err).Error())
	}
	return &net.MaxFloatUpdate{
		Sender:   req.Sender,
		MaxFloat: mf.Bytes(),
	}, nil
}

func (r *Redeemer) startCleanupLoop() {
	ticker := time.NewTicker(cleanupLoopTime)
	for {
		select {
		case <-ticker.C:
			// clean up map entries that haven't been cleared since the last cleanup loop ran
			r.liveSenders.Range(func(key, value interface{}) bool {
				if value.(time.Time).Add(cleanupLoopTime).Before(time.Now()) {
					r.liveSenders.Delete(key)
				}
				return true
			})
		case <-r.quit:
			return
		}
	}
}

type RedeemerClient struct {
	rpc     net.TicketRedeemerClient
	senders map[ethcommon.Address]*struct {
		maxFloat   *big.Int
		lastAccess time.Time
	}
	mu   sync.RWMutex
	quit chan struct{}
	sm   pm.SenderManager
	tm   pm.TimeManager
}

// NewRedeemerClient instantiates a new client for the ticket redemption service
// The client implements the pm.SenderMonitor interface
func NewRedeemerClient(uri *url.URL, sm pm.SenderManager, tm pm.TimeManager) (*RedeemerClient, *grpc.ClientConn, error) {
	conn, err := grpc.Dial(
		uri.String(),
		grpc.WithBlock(),
		grpc.WithTimeout(GRPCConnectTimeout),
		grpc.WithInsecure(),
	)

	//TODO: PROVIDE KEEPALIVE SETTINGS
	if err != nil {
		glog.Errorf("Did not connect to orch=%v err=%v", uri, err)
		return nil, nil, fmt.Errorf("Did not connect to orch=%v err=%v", uri, err)
	}
	return &RedeemerClient{
		rpc: net.NewTicketRedeemerClient(conn),
		sm:  sm,
		tm:  tm,
		senders: make(map[ethcommon.Address]*struct {
			maxFloat   *big.Int
			lastAccess time.Time
		}),
		quit: make(chan struct{}),
	}, conn, nil
}

func (r *RedeemerClient) Start() {
	go r.monitorMaxFloat(context.Background())
}

func (r *RedeemerClient) Stop() {
	close(r.quit)
}

func (r *RedeemerClient) QueueTicket(ticket *pm.SignedTicket) error {
	ctx, cancel := context.WithTimeout(context.Background(), GRPCTimeout)
	defer cancel()
	_, err := r.rpc.QueueTicket(ctx, protoTicket(ticket))
	return err
}

func (r *RedeemerClient) MaxFloat(sender ethcommon.Address) (*big.Int, error) {
	r.mu.Lock()
	if mf, ok := r.senders[sender]; ok && mf.maxFloat != nil {
		mf.lastAccess = time.Now()
		return mf.maxFloat, nil
	}
	r.mu.Unlock()

	// request max float from redeemer if not locally available
	ctx, cancel := context.WithTimeout(context.Background(), GRPCTimeout)
	defer cancel()

	mfu, err := r.rpc.MaxFloat(ctx, &net.MaxFloatReq{Sender: sender.Bytes()})
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(mfu.MaxFloat), nil
}

func (r *RedeemerClient) ValidateSender(sender ethcommon.Address) error {
	info, err := r.sm.GetSenderInfo(sender)
	if err != nil {
		return fmt.Errorf("could not get sender info for %v: %v", sender.Hex(), err)
	}
	maxWithdrawRound := new(big.Int).Add(r.tm.LastInitializedRound(), big.NewInt(1))
	if info.WithdrawRound.Int64() != 0 && info.WithdrawRound.Cmp(maxWithdrawRound) != 1 {
		return fmt.Errorf("deposit and reserve for sender %v is set to unlock soon", sender.Hex())
	}
	return nil
}

func (r *RedeemerClient) SubscribeMaxFloat(sender ethcommon.Address, sink chan<- *big.Int) event.Subscription {
	return nil
}

func (r *RedeemerClient) monitorMaxFloat(ctx context.Context) {
	stream, err := r.rpc.MonitorMaxFloat(ctx, &net.MonitorMaxFloatReq{})
	if err != nil {
		glog.Errorf("Unable to get MonitorMaxFloat stream")
		return
	}

	updateC := make(chan *net.MaxFloatUpdate)
	errC := make(chan error)
	go func() {
		for {
			update, err := stream.Recv()
			if err != nil {
				errC <- err
			} else {
				updateC <- update
			}
		}
	}()

	for {
		select {
		case <-r.quit:
			glog.Infof("closing redeemer service")
			return
		case <-ctx.Done():
			glog.Infof("closing redeemer service")
			return
		case update := <-updateC:
			r.mu.Lock()
			r.senders[ethcommon.BytesToAddress(update.Sender)] = &struct {
				maxFloat   *big.Int
				lastAccess time.Time
			}{new(big.Int).SetBytes(update.MaxFloat), time.Now()}
			r.mu.Unlock()
		case err := <-errC:
			glog.Error(err)
		}
	}
}

func (r *RedeemerClient) startCleanupLoop() {
	ticker := time.NewTicker(cleanupLoopTime)
	for {
		select {
		case <-ticker.C:
			// clean up map entries that haven't been cleared since the last cleanup loop ran
			r.mu.Lock()
			for sender, mf := range r.senders {
				if mf.lastAccess.Add(cleanupLoopTime).Before(time.Now()) {
					delete(r.senders, sender)
					r.sm.Clear(sender)
				}
			}
			r.mu.Unlock()
		case <-r.quit:
			return
		}
	}
}

func pmTicket(ticket *net.Ticket) *pm.SignedTicket {
	return &pm.SignedTicket{
		Ticket: &pm.Ticket{
			Recipient:              ethcommon.BytesToAddress(ticket.TicketParams.Recipient),
			Sender:                 ethcommon.BytesToAddress(ticket.Sender),
			FaceValue:              new(big.Int).SetBytes(ticket.TicketParams.FaceValue),
			WinProb:                new(big.Int).SetBytes(ticket.TicketParams.WinProb),
			SenderNonce:            ticket.SenderParams.SenderNonce,
			RecipientRandHash:      ethcommon.BytesToHash(ticket.TicketParams.RecipientRandHash),
			CreationRound:          ticket.ExpirationParams.CreationRound,
			CreationRoundBlockHash: ethcommon.BytesToHash(ticket.ExpirationParams.CreationRoundBlockHash),
			ParamsExpirationBlock:  new(big.Int).SetBytes(ticket.TicketParams.ExpirationBlock),
		},
		RecipientRand: new(big.Int).SetBytes(ticket.RecipientRand),
		Sig:           ticket.SenderParams.Sig,
	}
}

func protoTicket(ticket *pm.SignedTicket) *net.Ticket {
	return &net.Ticket{
		Sender:        ticket.Sender.Bytes(),
		RecipientRand: ticket.Recipient.Bytes(),
		TicketParams: &net.TicketParams{
			Recipient:         ticket.Recipient.Bytes(),
			FaceValue:         ticket.FaceValue.Bytes(),
			WinProb:           ticket.WinProb.Bytes(),
			RecipientRandHash: ticket.RecipientRandHash.Bytes(),
			ExpirationBlock:   ticket.ParamsExpirationBlock.Bytes(),
		},
		SenderParams: &net.TicketSenderParams{
			SenderNonce: ticket.SenderNonce,
			Sig:         ticket.Sig,
		},
		ExpirationParams: &net.TicketExpirationParams{
			CreationRound:          ticket.CreationRound,
			CreationRoundBlockHash: ticket.CreationRoundBlockHash.Bytes(),
		},
	}
}
