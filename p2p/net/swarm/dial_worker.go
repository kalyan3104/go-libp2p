package swarm

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/canonicallog"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"

	ma "github.com/multiformats/go-multiaddr"
)

// /////////////////////////////////////////////////////////////////////////////////
// lo and behold, The Dialer
// TODO explain how all this works
// ////////////////////////////////////////////////////////////////////////////////

// dialRequest is structure used to request dials to the peer associated with a
// worker loop
type dialRequest struct {
	// ctx is the context that may be used for the request
	// if another concurrent request is made, any of the concurrent request's ctx may be used for
	// dials to the peer's addresses
	// ctx for simultaneous connect requests have higher priority than normal requests
	ctx context.Context
	// resch is the channel used to send the response for this query
	resch chan dialResponse
}

// dialResponse is the response sent to dialRequests on the request's resch channel
type dialResponse struct {
	// conn is the connection to the peer on success
	conn *Conn
	// err is the error in dialing the peer
	// nil on connection success
	err error
}

// pendRequest is used to track progress on a dialRequest.
type pendRequest struct {
	// req is the original dialRequest
	req dialRequest
	// err comprises errors of all failed dials
	err *DialError
	// addrs are the addresses on which we are waiting for pending dials
	// At the time of creation addrs is initialised to all the addresses of the peer. On a failed dial,
	// the addr is removed from the map and err is updated. On a successful dial, the dialRequest is
	// completed and response is sent with the connection
	addrs map[string]bool
}

// addrDial tracks dials to a particular multiaddress.
type addrDial struct {
	// addr is the address dialed
	addr ma.Multiaddr
	// ctx is the context used for dialing the address
	ctx context.Context
	// conn is the established connection on success
	conn *Conn
	// err is the err on dialing the address
	err error
	// dialed indicates whether we have triggered the dial to the address
	dialed bool
	// createdAt is the time this struct was created
	createdAt time.Time
	// dialRankingDelay is the delay in dialing this address introduced by the ranking logic
	dialRankingDelay time.Duration
	// startTime is the dialStartTime
	startTime time.Time
}

// dialWorker synchronises concurrent dials to a peer. It ensures that we make at most one dial to a
// peer's address
type dialWorker struct {
	s    *Swarm
	peer peer.ID
	// reqch is used to send dial requests to the worker. close reqch to end the worker loop
	reqch <-chan dialRequest
	// pendingRequests is the set of pendingRequests
	pendingRequests map[*pendRequest]bool
	// trackedDials tracks dials to the peer's addresses. An entry here is used to ensure that
	// we dial an address at most once
	trackedDials map[string]*addrDial
	// resch is used to receive response for dials to the peers addresses.
	resch chan dialResult
	// connected is true when a connection has been successfully established
	connected bool
	// dq is used to pace dials to different addresses of the peer
	dq *dialQueue
	// dialsInFlight are the addresses with dials pending completion.
	dialsInFlight int
	// totalDials is used to track number of dials made by this worker for metrics
	totalDials int

	// for testing
	wg sync.WaitGroup
	cl Clock
}

func newDialWorker(s *Swarm, p peer.ID, reqch <-chan dialRequest, cl Clock) *dialWorker {
	if cl == nil {
		cl = RealClock{}
	}
	return &dialWorker{
		s:               s,
		peer:            p,
		reqch:           reqch,
		pendingRequests: make(map[*pendRequest]bool),
		trackedDials:    make(map[string]*addrDial),
		resch:           make(chan dialResult),
		cl:              cl,
	}
}

// loop implements the core dial worker loop. Requests are received on w.reqch.
// The loop exits when w.reqch is closed.
func (w *dialWorker) loop() {
	w.wg.Add(1)
	defer w.wg.Done()
	defer w.s.limiter.clearAllPeerDials(w.peer)
	defer w.cleanup()

	w.dq = newDialQueue()
	startTime := w.cl.Now()
	// dialTimer is the dialTimer used to trigger dials
	dialTimer := w.cl.InstantTimer(startTime.Add(math.MaxInt64))
	timerRunning := true
	// scheduleNextDial updates timer for triggering the next dial
	scheduleNextDial := func() {
		if timerRunning && !dialTimer.Stop() {
			<-dialTimer.Ch()
		}
		timerRunning = false
		if w.dq.Len() > 0 {
			if w.dialsInFlight == 0 && !w.connected {
				// if there are no dials in flight, trigger the next dials immediately
				dialTimer.Reset(startTime)
			} else {
				dialTimer.Reset(startTime.Add(w.dq.top().Delay))
			}
			timerRunning = true
		}
	}

loop:
	for {
		// The loop has three parts
		//  1. Input requests are received on w.reqch. If a suitable connection is not available we create
		//     a pendRequest object to track the dialRequest and add the addresses to w.dq.
		//  2. Addresses from the dialQueue are dialed at appropriate time intervals depending on delay logic.
		//     We are notified of the completion of these dials on w.resch.
		//  3. Responses for dials are received on w.resch. On receiving a response, we updated the pendRequests
		//     interested in dials on this address.

		select {
		case req, ok := <-w.reqch:
			if !ok {
				return
			}
			// We have received a new request. If we do not have a suitable connection,
			// track this dialRequest with a pendRequest.
			// Enqueue the peer's addresses relevant to this request in dq and
			// track dials to the addresses relevant to this request.

			c, err := w.s.bestAcceptableConnToPeer(req.ctx, w.peer)
			if c != nil || err != nil {
				req.resch <- dialResponse{conn: c, err: err}
				continue loop
			}

			addrs, addrErrs, err := w.s.addrsForDial(req.ctx, w.peer)
			if err != nil {
				req.resch <- dialResponse{err: &DialError{Peer: w.peer, DialErrors: addrErrs, Cause: err}}
				continue loop
			}

			// TODO(sukunrt): remove this check. This should never happen given the call
			// to w.s.bestAcceptableConnToPeer above, but I think this happens for circuit v2 addresses
			for _, addr := range addrs {
				if ad, ok := w.trackedDials[string(addr.Bytes())]; ok {
					if ad.conn != nil {
						// dial to this addr was successful, complete the request
						req.resch <- dialResponse{conn: ad.conn}
						continue loop
					}
				}
			}

			w.addNewRequest(req, addrs, addrErrs)
			scheduleNextDial()

		case <-dialTimer.Ch():
			// It's time to dial the next batch of addresses.
			// We don't check the delay of the addresses received from the queue here
			// because if the timer triggered before the delay, it means that all
			// the inflight dials have errored and we should dial the next batch of
			// addresses
			now := time.Now()
			for _, adelay := range w.dq.NextBatch() {
				// spawn the dial
				ad, ok := w.trackedDials[string(adelay.Addr.Bytes())]
				if !ok {
					log.Errorf("SWARM BUG: no entry for address %s in trackedDials", adelay.Addr)
					continue
				}
				ad.dialed = true
				ad.dialRankingDelay = now.Sub(ad.createdAt)
				err := w.s.dialNextAddr(ad.ctx, w.peer, ad.addr, w.resch)
				if err != nil {
					// Errored without attempting a dial. This happens in case of backoff.
					w.dispatchError(ad, err)
				} else {
					w.dialsInFlight++
					w.totalDials++
				}
			}
			timerRunning = false
			// schedule more dials
			scheduleNextDial()

		case res := <-w.resch:
			// A dial to an address has completed.
			// Update all requests waiting on this address. On success, complete the request.
			// On error, record the error

			ad, ok := w.trackedDials[string(res.Addr.Bytes())]
			if !ok {
				log.Errorf("SWARM BUG: no entry for address %s in trackedDials", res.Addr)
				if res.Conn != nil {
					res.Conn.Close()
				}
				// It is better to decrement the dials in flight and schedule one extra dial
				// than risking not closing the worker loop on cleanup
				w.dialsInFlight--
				continue
			}

			if res.Kind == DialStarted {
				ad.startTime = w.cl.Now()
				scheduleNextDial()
				continue
			}

			w.dialsInFlight--
			// We're recording any error as a failure here.
			// Notably, this also applies to cancelations (i.e. if another dial attempt was faster).
			// This is ok since the black hole detector uses a very low threshold (5%).
			w.s.bhd.RecordResult(ad.addr, res.Err == nil)

			if res.Conn != nil {
				w.handleSuccess(ad, res)
			} else {
				w.handleError(ad, res)
			}
			scheduleNextDial()
		}
	}
}

// addNewRequest adds a new dial request to the worker loop. If the request has no pending dials, a response
// is sent immediately otherwise it is tracked in pendingRequests
func (w *dialWorker) addNewRequest(req dialRequest, addrs []ma.Multiaddr, addrErrs []TransportError) {
	// get the delays to dial these addrs from the swarms dialRanker
	simConnect, _, _ := network.GetSimultaneousConnect(req.ctx)
	addrRanking := w.rankAddrs(addrs, simConnect)

	// create the pending request object
	pr := &pendRequest{
		req:   req,
		err:   &DialError{Peer: w.peer, DialErrors: addrErrs},
		addrs: make(map[string]bool, len(addrRanking)),
	}
	for _, adelay := range addrRanking {
		pr.addrs[string(adelay.Addr.Bytes())] = true
	}

	for _, adelay := range addrRanking {
		ad, ok := w.trackedDials[string(adelay.Addr.Bytes())]
		if !ok {
			// new address, track and enqueue
			now := time.Now()
			w.trackedDials[string(adelay.Addr.Bytes())] = &addrDial{
				addr:      adelay.Addr,
				ctx:       req.ctx,
				createdAt: now,
			}
			w.dq.Add(network.AddrDelay{Addr: adelay.Addr, Delay: adelay.Delay})
			continue
		}

		if ad.err != nil {
			// dial to this addr errored, accumulate the error
			pr.err.recordErr(ad.addr, ad.err)
			delete(pr.addrs, string(ad.addr.Bytes()))
			continue
		}

		if !ad.dialed {
			// we haven't dialed this address. update the ad.ctx to have simultaneous connect values
			// set correctly
			if isSimConnect, isClient, reason := network.GetSimultaneousConnect(req.ctx); isSimConnect {
				if wasSimConnect, _, _ := network.GetSimultaneousConnect(ad.ctx); !wasSimConnect {
					ad.ctx = network.WithSimultaneousConnect(ad.ctx, isClient, reason)
					// update the element in dq to use the simultaneous connect delay.
					w.dq.Add(network.AddrDelay{
						Addr:  ad.addr,
						Delay: adelay.Delay,
					})
				}
			}
		}
	}

	if len(pr.addrs) == 0 {
		// all request applicable addrs have been dialed, we must have errored
		pr.err.Cause = ErrAllDialsFailed
		req.resch <- dialResponse{err: pr.err}
	} else {
		// The request has some pending or new dials
		w.pendingRequests[pr] = true
	}
}

func (w *dialWorker) handleSuccess(ad *addrDial, res dialResult) {
	// Ensure we connected to the correct peer.
	// This was most likely already checked by the security protocol, but it doesn't hurt do it again here.
	if res.Conn.RemotePeer() != w.peer {
		res.Conn.Close()
		tpt := w.s.TransportForDialing(res.Addr)
		err := fmt.Errorf("BUG in transport %T: tried to dial %s, dialed %s", w.peer, res.Conn.RemotePeer(), tpt)
		log.Error(err)
		w.dispatchError(ad, err)
		return
	}

	canonicallog.LogPeerStatus(100, res.Conn.RemotePeer(), res.Conn.RemoteMultiaddr(), "connection_status", "established", "dir", "outbound")
	if w.s.metricsTracer != nil {
		connWithMetrics := wrapWithMetrics(res.Conn, w.s.metricsTracer, ad.startTime, network.DirOutbound)
		connWithMetrics.completedHandshake()
		res.Conn = connWithMetrics
	}

	// we got a connection, add it to the swarm
	conn, err := w.s.addConn(res.Conn, network.DirOutbound)
	if err != nil {
		// oops no, we failed to add it to the swarm
		res.Conn.Close()
		w.dispatchError(ad, err)
		return
	}
	ad.conn = conn

	for pr := range w.pendingRequests {
		if pr.addrs[string(ad.addr.Bytes())] {
			pr.req.resch <- dialResponse{conn: conn}
			delete(w.pendingRequests, pr)
		}
	}

	if !w.connected {
		w.connected = true
		if w.s.metricsTracer != nil {
			w.s.metricsTracer.DialRankingDelay(ad.dialRankingDelay)
		}
	}
}

func (w *dialWorker) handleError(ad *addrDial, res dialResult) {
	if res.Err != nil && w.s.metricsTracer != nil {
		w.s.metricsTracer.FailedDialing(res.Addr, res.Err)
	}
	// add backoff if applicable and dispatch
	// ErrDialRefusedBlackHole shouldn't end up here, just a safety check
	if res.Err != ErrDialRefusedBlackHole && res.Err != context.Canceled && !w.connected {
		// we only add backoff if there has not been a successful connection
		// for consistency with the old dialer behavior.
		w.s.backf.AddBackoff(w.peer, res.Addr)
	} else if res.Err == ErrDialRefusedBlackHole {
		log.Errorf("SWARM BUG: unexpected ErrDialRefusedBlackHole while dialing peer %s to addr %s",
			w.peer, res.Addr)
	}
	w.dispatchError(ad, res.Err)
}

// dispatches an error to a specific addr dial
func (w *dialWorker) dispatchError(ad *addrDial, err error) {
	ad.err = err
	for pr := range w.pendingRequests {
		// accumulate the error
		if pr.addrs[string(ad.addr.Bytes())] {
			pr.err.recordErr(ad.addr, err)
			delete(pr.addrs, string(ad.addr.Bytes()))
			if len(pr.addrs) == 0 {
				// all addrs have erred, dispatch dial error
				// but first do a last one check in case an acceptable connection has landed from
				// a simultaneous dial that started later and added new acceptable addrs
				c, _ := w.s.bestAcceptableConnToPeer(pr.req.ctx, w.peer)
				if c != nil {
					pr.req.resch <- dialResponse{conn: c}
				} else {
					pr.err.Cause = ErrAllDialsFailed
					pr.req.resch <- dialResponse{err: pr.err}
				}
				delete(w.pendingRequests, pr)
			}
		}
	}

	// if it was a backoff, clear the address dial so that it doesn't inhibit new dial requests.
	// this is necessary to support active listen scenarios, where a new dial comes in while
	// another dial is in progress, and needs to do a direct connection without inhibitions from
	// dial backoff.
	if err == ErrDialBackoff {
		delete(w.trackedDials, string(ad.addr.Bytes()))
	}
}

// rankAddrs ranks addresses for dialing. if it's a simConnect request we
// dial all addresses immediately without any delay
func (w *dialWorker) rankAddrs(addrs []ma.Multiaddr, isSimConnect bool) []network.AddrDelay {
	if isSimConnect {
		return NoDelayDialRanker(addrs)
	}
	return w.s.dialRanker(addrs)
}

// cleanup is called on workerloop close
func (w *dialWorker) cleanup() {
	if w.s.metricsTracer != nil {
		w.s.metricsTracer.DialCompleted(w.connected, w.totalDials)
	}
	for w.dialsInFlight > 0 {
		res := <-w.resch
		// We're recording any error as a failure here.
		// Notably, this also applies to cancelations (i.e. if another dial attempt was faster).
		// This is ok since the black hole detector uses a very low threshold (5%).
		w.s.bhd.RecordResult(res.Addr, res.Err == nil)
		if res.Conn != nil {
			res.Conn.Close()
		}
		w.dialsInFlight--
	}
}

// dialQueue is a priority queue used to schedule dials
type dialQueue struct {
	// q contains dials ordered by delay
	q []network.AddrDelay
}

// newDialQueue returns a new dialQueue
func newDialQueue() *dialQueue {
	return &dialQueue{q: make([]network.AddrDelay, 0, 16)}
}

// Add adds adelay to the queue. If another element exists in the queue with
// the same address, it replaces that element.
func (dq *dialQueue) Add(adelay network.AddrDelay) {
	for i := 0; i < dq.Len(); i++ {
		if dq.q[i].Addr.Equal(adelay.Addr) {
			if dq.q[i].Delay == adelay.Delay {
				// existing element is the same. nothing to do
				return
			}
			// remove the element
			copy(dq.q[i:], dq.q[i+1:])
			dq.q = dq.q[:len(dq.q)-1]
			break
		}
	}

	for i := 0; i < dq.Len(); i++ {
		if dq.q[i].Delay > adelay.Delay {
			dq.q = append(dq.q, network.AddrDelay{}) // extend the slice
			copy(dq.q[i+1:], dq.q[i:])
			dq.q[i] = adelay
			return
		}
	}
	dq.q = append(dq.q, adelay)
}

// NextBatch returns all the elements in the queue with the highest priority
func (dq *dialQueue) NextBatch() []network.AddrDelay {
	if dq.Len() == 0 {
		return nil
	}

	// i is the index of the second highest priority element
	var i int
	for i = 0; i < dq.Len(); i++ {
		if dq.q[i].Delay != dq.q[0].Delay {
			break
		}
	}
	res := dq.q[:i]
	dq.q = dq.q[i:]
	return res
}

// top returns the top element of the queue
func (dq *dialQueue) top() network.AddrDelay {
	return dq.q[0]
}

// Len returns the number of elements in the queue
func (dq *dialQueue) Len() int {
	return len(dq.q)
}
