package p2p

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	cmap "github.com/orcaman/concurrent-map/v2"
	"github.com/rkonfj/peerguard/peer"
	"tailscale.com/net/stun"
)

const (
	OP_PEER_DISCO1      = 0
	OP_PEER_DISCO2      = 1
	OP_PEER_CONFIRM     = 2
	OP_PEER_HEALTHCHECK = 10
)

type STUNBindContext struct {
	PeerID peer.PeerID
	CTime  time.Time
}

type PeerContext struct {
	Addr          *net.UDPAddr
	Conn          *net.UDPConn
	LastValidTime time.Time
	UpdateTime    time.Time
}

type PeerEvent struct {
	Op     int
	Addr   *net.UDPAddr
	Conn   *net.UDPConn
	PeerID peer.PeerID
}

type PeerPacketConn struct {
	peersMap         map[peer.PeerID]*PeerContext
	peerEvent        chan PeerEvent
	inbound          chan []byte
	stunChan         chan []byte
	wsConn           *websocket.Conn
	node             *Node
	peerID           peer.PeerID
	nonce            byte
	stunTxIDMap      cmap.ConcurrentMap[string, STUNBindContext]
	healthcheckTimer time.Timer
	ctx              context.Context
	ctxCancel        context.CancelFunc

	peersMapMutex sync.RWMutex
	stunServers   []string
}

// ReadFrom reads a packet from the connection,
// copying the payload into p. It returns the number of
// bytes copied into p and the return address that
// was on the packet.
// It returns the number of bytes read (0 <= n <= len(p))
// and any error encountered. Callers should always process
// the n > 0 bytes returned before considering the error err.
// ReadFrom can be made to time out and return an error after a
// fixed time limit; see SetDeadline and SetReadDeadline.
func (c *PeerPacketConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	select {
	case <-c.ctx.Done():
		err = io.ErrClosedPipe
		return
	case <-c.node.closedSig:
		err = io.ErrClosedPipe
		c.Close()
		return
	default:
	}
	b := <-c.inbound
	addr = peer.PeerID(b[2 : b[1]+2])
	n = copy(p, b[b[1]+2:])
	return
}

// WriteTo writes a packet with payload p to addr.
// WriteTo can be made to time out and return an Error after a
// fixed time limit; see SetDeadline and SetWriteDeadline.
// On packet-oriented connections, write timeouts are rare.
func (c *PeerPacketConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	if _, ok := addr.(peer.PeerID); !ok {
		return 0, errors.New("not a p2p address")
	}
	tgtPeer := addr.(peer.PeerID)
	if c.peerConnected(tgtPeer) {
		return c.writeToUDP(tgtPeer, p)
	}
	slog.Debug("[Relay] WriteTo", "addr", tgtPeer)
	return len(p), c.writeTo(p, tgtPeer, 0)
}

func (c *PeerPacketConn) writeTo(p []byte, tgtPeer peer.PeerID, action byte) error {
	b := make([]byte, 0, 2+len(tgtPeer)+len(p))
	b = append(b, action)             // relay
	b = append(b, tgtPeer.Len())      // addr length
	b = append(b, tgtPeer.Bytes()...) // addr
	b = append(b, p...)               // data
	for i, v := range b {
		b[i] = v ^ c.nonce
	}
	return c.wsConn.WriteMessage(websocket.BinaryMessage, b)
}

// Close closes the connection.
// Any blocked ReadFrom or WriteTo operations will be unblocked and return errors.
func (c *PeerPacketConn) Close() error {
	c.ctxCancel()
	c.healthcheckTimer.Stop()
	close(c.stunChan)
	close(c.inbound)
	close(c.peerEvent)
	_ = c.wsConn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(2*time.Second))
	return c.wsConn.Close()
}

// LocalAddr returns the local network address, if known.
func (c *PeerPacketConn) LocalAddr() net.Addr {
	return c.peerID
}

// SetDeadline sets the read and write deadlines associated
// with the connection. It is equivalent to calling both
// SetReadDeadline and SetWriteDeadline.
//
// A deadline is an absolute time after which I/O operations
// fail instead of blocking. The deadline applies to all future
// and pending I/O, not just the immediately following call to
// Read or Write. After a deadline has been exceeded, the
// connection can be refreshed by setting a deadline in the future.
//
// If the deadline is exceeded a call to Read or Write or to other
// I/O methods will return an error that wraps os.ErrDeadlineExceeded.
// This can be tested using errors.Is(err, os.ErrDeadlineExceeded).
// The error's Timeout method will return true, but note that there
// are other possible errors for which the Timeout method will
// return true even if the deadline has not been exceeded.
//
// An idle timeout can be implemented by repeatedly extending
// the deadline after successful ReadFrom or WriteTo calls.
//
// A zero value for t means I/O operations will not time out.
func (c *PeerPacketConn) SetDeadline(t time.Time) error {
	return nil
}

// SetReadDeadline sets the deadline for future ReadFrom calls
// and any currently-blocked ReadFrom call.
// A zero value for t means ReadFrom will not time out.
func (c *PeerPacketConn) SetReadDeadline(t time.Time) error {
	return nil
}

// SetWriteDeadline sets the deadline for future WriteTo calls
// and any currently-blocked WriteTo call.
// Even if write times out, it may return n > 0, indicating that
// some of the data was successfully written.
// A zero value for t means WriteTo will not time out.
func (c *PeerPacketConn) SetWriteDeadline(t time.Time) error {
	return nil
}

// keepState keep p2p connection
func (c *PeerPacketConn) keepState() {
	go c.runPeerEventEngine()
	go c.runNATTraversalLoop()
	go c.runPeersHealthcheck()
	for {
		mt, b, err := c.wsConn.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err,
				websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				slog.Error(err.Error())
			}
			c.wsConn.Close()
			return
		}
		switch mt {
		case websocket.PingMessage:
			c.wsConn.WriteMessage(websocket.PongMessage, nil)
			continue
		case websocket.BinaryMessage:
		default:
			continue
		}
		for i, v := range b {
			b[i] = v ^ c.nonce
		}
		switch b[0] {
		case peer.CONTROL_RELAY:
			c.inbound <- b
		case peer.CONTROL_PRE_NAT_TRAVERSAL:
			go c.requestSTUN(b)
		case peer.CONTROL_NAT_TRAVERSAL:
			go c.natTraversal(b)
		}
	}
}

func (c *PeerPacketConn) runPeerEventEngine() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case e := <-c.peerEvent:
			c.handlePeerEvent(e)
		}
	}
}

func (c *PeerPacketConn) handlePeerEvent(e PeerEvent) {
	c.peersMapMutex.Lock()
	defer c.peersMapMutex.Unlock()
	switch e.Op {
	case OP_PEER_DISCO1: // 创建发现 peer 事务
		c.healthcheckTimer.Reset(c.node.peerKeepaliveInterval/2 + time.Second)
		peerCtx := PeerContext{Conn: e.Conn}
		if peer, ok := c.peersMap[e.PeerID]; ok {
			peer.Conn.Close()
			slog.Info("[UDP] Clean peer", "peer", e.PeerID, "addr", peer.Addr)
			peerCtx.Addr = peer.Addr
			delete(c.peersMap, e.PeerID)
		}
		c.peersMap[e.PeerID] = &peerCtx
	case OP_PEER_DISCO2: // 收到 peer addr
		if peerCtx, ok := c.peersMap[e.PeerID]; ok {
			peerCtx.Addr = e.Addr
			break
		}
		time.AfterFunc(100*time.Millisecond, func() {
			slog.Debug("Retry OP_PEER_DISCO2", "peer", e.PeerID, "addr", e.Addr)
			c.peerEvent <- e
		})
	case OP_PEER_CONFIRM: // 确认事务
		slog.Debug("[UDP] Heartbeat", "peer", e.PeerID, "addr", e.Addr)
		if peer, ok := c.peersMap[e.PeerID]; ok {
			updated := time.Since(peer.LastValidTime) > 2*c.node.peerKeepaliveInterval
			if updated {
				slog.Info("[UDP] Add peer", "peer", e.PeerID, "addr", e.Addr)
			}
			peer.LastValidTime = time.Now()
			peer.Addr = e.Addr
		}
	case OP_PEER_HEALTHCHECK:
		for k, v := range c.peersMap {
			if time.Since(v.LastValidTime) > 2*c.node.peerKeepaliveInterval {
				v.Conn.Close()
				slog.Info("[UDP] Remove peer", "peer", k, "addr", v.Addr)
				delete(c.peersMap, k)
			}
		}
	}
}

func (c *PeerPacketConn) runReadUDPLoop(udpConn *net.UDPConn) {
	buf := make([]byte, 65535)
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}
		n, peerAddr, err := udpConn.ReadFromUDP(buf)
		if err != nil {
			if !strings.Contains(err.Error(), "use of closed network connection") {
				slog.Error("read from udp error", "err", err)
			}
			return
		}

		// ping
		if n > 4 && string(buf[:5]) == "_ping" && n <= 260 {
			peerID := string(buf[5:n])
			c.peerEvent <- PeerEvent{
				Op:     OP_PEER_CONFIRM,
				PeerID: peer.PeerID(peerID),
				Addr:   peerAddr,
			}
			continue
		}

		// stun
		if stun.Is(buf[:n]) {
			b := make([]byte, n)
			copy(b, buf[:n])
			c.stunChan <- b
			continue
		}

		// other
		peerID := c.getPeerID(peerAddr)
		b := make([]byte, 2+len(peerID)+n)
		b[1] = peerID.Len()
		copy(b[2:], peerID.Bytes())
		copy(b[2+len(peerID):], buf[:n])
		c.inbound <- b
	}
}

func (c *PeerPacketConn) requestSTUN(b []byte) {
	peerID := peer.PeerID(b[2 : b[1]+2])

	udpConn, err := net.ListenUDP("udp", nil)
	if err != nil {
		slog.Error("listen udp error", "err", err)
		return
	}
	go c.runReadUDPLoop(udpConn)

	c.peerEvent <- PeerEvent{
		Op:     OP_PEER_DISCO1,
		PeerID: peerID,
		Conn:   udpConn,
	}

	json.Unmarshal(b[b[1]+2:], &c.stunServers)
	txID := stun.NewTxID()
	c.stunTxIDMap.Set(string(txID[:]), STUNBindContext{PeerID: peerID, CTime: time.Now()})
	for _, stunServer := range c.stunServers {
		uaddr, err := net.ResolveUDPAddr("udp", stunServer)
		if err != nil {
			slog.Error(err.Error())
			continue
		}
		_, err = udpConn.WriteToUDP(stun.Request(txID), uaddr)
		if err != nil {
			slog.Error(err.Error())
			continue
		}
		time.Sleep(3 * time.Second)
		if c.peerConnected(peerID) {
			break
		}
	}
}

func (c *PeerPacketConn) runNATTraversalLoop() {
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}
		stunResp := <-c.stunChan

		txid, saddr, err := stun.ParseResponse(stunResp)
		if err != nil {
			slog.Error("Skipped invalid stun response", "err", err.Error())
			continue
		}

		tx, ok := c.stunTxIDMap.Get(string(txid[:]))
		if !ok {
			slog.Error("Skipped unknown stun response", "txid", hex.EncodeToString(txid[:]))
			continue
		}

		if !saddr.IsValid() {
			slog.Error("Skipped invalid UDP addr", "addr", saddr)
			continue
		}
		for i := 0; i < 3; i++ {
			err := c.writeTo([]byte(saddr.String()), tx.PeerID, peer.CONTROL_NAT_TRAVERSAL)
			if err == nil {
				slog.Info("[UDP] Node public address found", "addr", saddr.String())
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		c.stunTxIDMap.Remove(string(txid[:]))
	}
}

func (c *PeerPacketConn) natTraversal(b []byte) {
	targetPeerID := peer.PeerID(b[2 : b[1]+2])
	targetUDPAddr := b[b[1]+2:]
	udpAddr, err := net.ResolveUDPAddr("udp", string(targetUDPAddr))
	if err != nil {
		slog.Error("Resolve udp addr error", "err", err)
		return
	}
	c.peerEvent <- PeerEvent{
		Op:     OP_PEER_DISCO2,
		PeerID: targetPeerID,
		Addr:   udpAddr,
	}
	interval := 300 * time.Millisecond
	for i := 0; ; i++ {
		select {
		case <-c.ctx.Done():
			slog.Info("[UDP] Ping exit", "peer", targetPeerID)
			return
		default:
		}
		peerDiscovered := c.getPeerID(udpAddr) != ""
		if interval == c.node.peerKeepaliveInterval && !peerDiscovered {
			break
		}
		if peerDiscovered || i >= 24 {
			interval = c.node.peerKeepaliveInterval
		}
		slog.Debug("[UDP] Ping", "peer", targetPeerID, "addr", udpAddr)
		c.writeToUDP(targetPeerID, []byte("_ping"+c.peerID))
		time.Sleep(interval)
	}
}

func (c *PeerPacketConn) writeToUDP(peerID peer.PeerID, p []byte) (int, error) {
	c.peersMapMutex.RLock()
	defer c.peersMapMutex.RUnlock()
	if peerCtx, ok := c.peersMap[peerID]; ok && peerCtx.Addr != nil {
		slog.Debug("[UDP] WriteTo", "peer", peerID, "addr", peerCtx.Addr)
		return peerCtx.Conn.WriteToUDP(p, peerCtx.Addr)
	}
	return 0, io.ErrClosedPipe
}

func (c *PeerPacketConn) peerConnected(peerID peer.PeerID) bool {
	c.peersMapMutex.RLock()
	defer c.peersMapMutex.RUnlock()
	peerCtx, ok := c.peersMap[peerID]
	return ok && time.Since(peerCtx.LastValidTime) <= 2*c.node.peerKeepaliveInterval
}

func (c *PeerPacketConn) getPeerID(udpAddr *net.UDPAddr) peer.PeerID {
	if udpAddr == nil {
		return ""
	}
	c.peersMapMutex.RLock()
	defer c.peersMapMutex.RUnlock()
	for k, v := range c.peersMap {
		if v.Addr == nil {
			continue
		}
		if time.Since(v.LastValidTime) > 2*c.node.peerKeepaliveInterval {
			continue
		}
		if v.Addr.IP.Equal(udpAddr.IP) && v.Addr.Port == udpAddr.Port {
			return peer.PeerID(k)
		}
	}
	return ""
}

func (c *PeerPacketConn) runPeersHealthcheck() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.healthcheckTimer.C:
			c.peerEvent <- PeerEvent{
				Op: OP_PEER_HEALTHCHECK,
			}
			c.healthcheckTimer.Reset(c.node.peerKeepaliveInterval/2 + time.Second)
		}
	}
}