package peermap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rkonfj/peerguard/peer"
	"github.com/rkonfj/peerguard/peermap/auth"
	"github.com/rkonfj/peerguard/peermap/exporter"
	exporterauth "github.com/rkonfj/peerguard/peermap/exporter/auth"
	"github.com/rkonfj/peerguard/peermap/oidc"
	"golang.org/x/time/rate"
)

var (
	_ io.ReadWriter = (*Peer)(nil)
)

type Peer struct {
	conn    *websocket.Conn
	exitSig chan struct{}
	peerMap *PeerMap

	networkSecret  auth.JSONSecret
	networkContext *networkContext

	metadata   url.Values
	activeTime time.Time
	id         peer.ID
	nonce      byte
	wMut       sync.Mutex

	connRRL  *rate.Limiter
	connWRL  *rate.Limiter
	connData chan []byte
	connBuf  []byte
}

func (p *Peer) write(b []byte) error {
	for i, v := range b {
		b[i] = v ^ p.nonce
	}
	return p.writeWS(websocket.BinaryMessage, b)
}

func (p *Peer) writeWS(messageType int, b []byte) error {
	p.wMut.Lock()
	defer p.wMut.Unlock()
	return p.conn.WriteMessage(messageType, b)
}

func (p *Peer) close() error {
	p.peerMap.removePeer(p.networkSecret.Network, p.id)
	_ = p.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(2*time.Second))
	return p.conn.Close()
}

func (p *Peer) Read(b []byte) (n int, err error) {
	defer func() {
		if p.connRRL != nil && n > 0 {
			p.connRRL.WaitN(context.Background(), n)
		}
	}()
	if p.connBuf != nil {
		n = copy(b, p.connBuf)
		if n < len(p.connBuf) {
			p.connBuf = p.connBuf[n:]
		} else {
			p.connBuf = nil
		}
		return
	}

	wsb, ok := <-p.connData
	if !ok {
		return 0, io.EOF
	}
	n = copy(b, wsb)
	if n < len(wsb) {
		p.connBuf = wsb[n:]
	}
	return
}

func (p *Peer) Write(b []byte) (n int, err error) {
	if p.connWRL != nil && len(b) > 0 {
		p.connWRL.WaitN(context.Background(), len(b))
	}
	err = p.write(append(append([]byte(nil), peer.CONTROL_CONN), b...))
	if err != nil {
		return
	}
	return len(b), nil
}

func (p *Peer) Close() error {
	close(p.exitSig)
	close(p.connData)
	return p.close()
}

func (p *Peer) String() string {
	return (&url.URL{
		Scheme:   "pg",
		Host:     string(p.id),
		RawQuery: p.metadata.Encode(),
	}).String()
}

func (p *Peer) Start() {
	p.activeTime = time.Now()
	go p.readMessageLoop()
	go p.keepalive()
	if p.metadata.Has("silenceMode") {
		return
	}

	if p.peerMap.cfg.PublicNetwork == p.networkSecret.Network {
		return
	}

	ctx, _ := p.peerMap.getNetwork(p.networkSecret.Network)
	ctx.peersMutex.RLock()
	defer ctx.peersMutex.RUnlock()
	for k, v := range ctx.peers {
		if k == string(p.id) {
			continue
		}

		if v.metadata.Has("silenceMode") {
			continue
		}
		p.leadDisco(v)
	}
}

func (p *Peer) leadDisco(target *Peer) {
	myMeta := []byte(p.metadata.Encode())
	b := make([]byte, 2+len(p.id)+len(myMeta))
	b[0] = peer.CONTROL_NEW_PEER
	b[1] = p.id.Len()
	copy(b[2:], p.id.Bytes())
	copy(b[len(p.id)+2:], myMeta)
	target.write(b)

	peerMeta := []byte(target.metadata.Encode())
	b1 := make([]byte, 2+len(target.id)+len(peerMeta))
	b1[0] = peer.CONTROL_NEW_PEER
	b1[1] = target.id.Len()
	copy(b1[2:], target.id.Bytes())
	copy(b1[len(target.id)+2:], peerMeta)
	p.write(b1)
}

func (p *Peer) readMessageLoop() {
	for {
		select {
		case <-p.exitSig:
			return
		default:
		}
		mt, b, err := p.conn.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err,
				websocket.CloseGoingAway,
				websocket.CloseNoStatusReceived,
				websocket.CloseNormalClosure) {
				slog.Debug("ReadLoopExited", "details", err.Error())
			}
			p.Close()
			return
		}
		p.activeTime = time.Now()
		switch mt {
		case websocket.BinaryMessage:
		default:
			continue
		}
		for i, v := range b {
			b[i] = v ^ p.nonce
		}
		if slices.Contains([]byte{peer.CONTROL_LEAD_DISCO, peer.CONTROL_NEW_PEER_UDP_ADDR}, b[0]) {
			p.networkContext.disoRatelimiter.WaitN(context.Background(), len(b))
		} else if p.networkContext.ratelimiter != nil {
			p.networkContext.ratelimiter.WaitN(context.Background(), len(b))
		}
		tgtPeerID := peer.ID(b[2 : b[1]+2])
		slog.Debug("PeerEvent", "op", b[0], "from", p.id, "to", tgtPeerID)
		tgtPeer, err := p.peerMap.getPeer(p.networkSecret.Network, tgtPeerID)
		if err != nil {
			slog.Debug("FindPeer failed", "detail", err)
			continue
		}
		switch b[0] {
		case peer.CONTROL_LEAD_DISCO:
			p.leadDisco(tgtPeer)
		case peer.CONTROL_CONN:
			p.connData <- b[1:]
		default:
			data := b[b[1]+2:]
			bb := make([]byte, 2+len(p.id)+len(data))
			bb[0] = b[0]
			bb[1] = p.id.Len()
			copy(bb[2:p.id.Len()+2], p.id.Bytes())
			copy(bb[p.id.Len()+2:], data)
			_ = tgtPeer.write(bb)
		}
	}
}

func (p *Peer) keepalive() {
	ticker := time.NewTicker(12 * time.Second)
	for {
		select {
		case <-p.exitSig:
			ticker.Stop()
			return
		case <-ticker.C:
		}
		if err := p.writeWS(websocket.TextMessage, nil); err != nil {
			break
		}
		if time.Since(p.activeTime) > 25*time.Second {
			slog.Debug("Closing inactive connection", "peer", p.id)
			break
		}

		if time.Until(time.Unix(p.networkSecret.Deadline, 0)) <
			p.peerMap.cfg.SecretValidityPeriod-p.peerMap.cfg.SecretRotationPeriod {
			p.updateSecret()
		}
	}
	p.close()
}

func (p *Peer) updateSecret() error {
	secret, err := p.peerMap.generateSecret(auth.Net{
		ID:        p.networkSecret.Network,
		Alias:     p.networkContext.alias,
		Neighbors: p.networkContext.neighbors,
	})
	if err != nil {
		slog.Error("NetworkSecretRefresh", "err", err)
		return err
	}
	b, err := json.Marshal(secret)
	if err != nil {
		slog.Error("NetworkSecretRefresh", "err", err)
		return err
	}
	data := make([]byte, 1+len(b))
	data[0] = peer.CONTROL_UPDATE_NETWORK_SECRET
	copy(data[1:], b)
	if err = p.write(data); err != nil {
		slog.Error("NetworkSecretRefresh", "err", err)
		return err
	}
	p.networkSecret, _ = p.peerMap.authenticator.ParseSecret(secret.Secret)
	return nil
}

type networkContext struct {
	peersMutex      sync.RWMutex
	peers           map[string]*Peer
	ratelimiter     *rate.Limiter
	disoRatelimiter *rate.Limiter
	createTime      time.Time
	updateTime      time.Time

	id        string
	metaMutex sync.Mutex
	alias     string
	neighbors []string
}

func (ctx *networkContext) removePeer(id peer.ID) {
	ctx.peersMutex.Lock()
	defer ctx.peersMutex.Unlock()
	delete(ctx.peers, string(id))
}

func (ctx *networkContext) getPeer(id peer.ID) (*Peer, bool) {
	ctx.peersMutex.RLock()
	defer ctx.peersMutex.RUnlock()
	p, ok := ctx.peers[id.String()]
	return p, ok
}

func (ctx *networkContext) peerCount() int {
	ctx.peersMutex.RLock()
	defer ctx.peersMutex.RUnlock()
	return len(ctx.peers)
}

func (ctx *networkContext) SetIfAbsent(peerID string, p *Peer) bool {
	ctx.peersMutex.Lock()
	defer ctx.peersMutex.Unlock()
	if _, ok := ctx.peers[peerID]; ok {
		return false
	}
	ctx.peers[peerID] = p
	return true
}

func (ctx *networkContext) initMeta(n auth.Net, updateTime time.Time) {
	ctx.metaMutex.Lock()
	defer ctx.metaMutex.Unlock()
	if ctx.updateTime.After(updateTime) {
		return
	}
	ctx.updateTime = updateTime
	ctx.alias = n.Alias
	ctx.neighbors = n.Neighbors
}

func (ctx *networkContext) updateMeta(n auth.Net) error {
	ctx.metaMutex.Lock()
	defer ctx.metaMutex.Unlock()
	if ctx.alias == n.Alias && slices.Equal(ctx.neighbors, n.Neighbors) {
		return nil
	}
	ctx.updateTime = time.Now()
	ctx.neighbors = n.Neighbors
	ctx.alias = n.Alias
	ctx.peersMutex.RLock()
	defer ctx.peersMutex.RUnlock()
	for _, v := range ctx.peers {
		v.updateSecret()
	}
	return nil
}

type NetState struct {
	ID         string    `json:"id"`
	Alias      string    `json:"alias"`
	Neighbors  []string  `json:"neighbors"`
	CreateTime time.Time `json:"createTime"`
	UpdateTime time.Time `json:"updateTime"`
}

type PeerMap struct {
	httpServer            *http.Server
	wsUpgrader            *websocket.Upgrader
	networkMapMutex       sync.RWMutex
	networkMap            map[string]*networkContext
	peerMapMutex          sync.RWMutex
	peerMap               map[string]*networkContext
	cfg                   Config
	authenticator         *auth.Authenticator
	exporterAuthenticator *exporterauth.Authenticator
}

func (pm *PeerMap) removePeer(network string, id peer.ID) {
	if ctx, ok := pm.getNetwork(network); ok {
		slog.Debug("PeerRemoved", "network", network, "peer", id)
		ctx.removePeer(id)
		pm.peerMapMutex.Lock()
		delete(pm.peerMap, id.String())
		pm.peerMapMutex.Unlock()
	}
}

func (pm *PeerMap) getNetwork(network string) (*networkContext, bool) {
	pm.networkMapMutex.RLock()
	defer pm.networkMapMutex.RUnlock()
	ctx, ok := pm.networkMap[network]
	return ctx, ok
}

func (pm *PeerMap) getPeer(network string, peerID peer.ID) (*Peer, error) {
	if ctx, ok := pm.getNetwork(network); ok {
		if peer, ok := ctx.getPeer(peerID); ok {
			return peer, nil
		}
		pm.peerMapMutex.RLock()
		neighNet, ok := pm.peerMap[peerID.String()]
		pm.peerMapMutex.RUnlock()
		if ok && slices.Contains(ctx.neighbors, neighNet.id) {
			if peer, ok := neighNet.getPeer(peerID); ok {
				return peer, nil
			}
		}
	}
	return nil, fmt.Errorf("peer(%s/%s) not found", network, peerID)
}

func (pm *PeerMap) FindPeer(network string, filter func(url.Values) bool) ([]*Peer, error) {
	if ctx, ok := pm.getNetwork(network); ok {
		var ret []*Peer
		ctx.peersMutex.RLock()
		defer ctx.peersMutex.RUnlock()
		for _, v := range ctx.peers {
			if filter(v.metadata) {
				ret = append(ret, v)
			}
		}
		return ret, nil
	}
	return nil, fmt.Errorf("peer(%s/metafilter) not found", network)
}

func (pm *PeerMap) Serve(ctx context.Context) error {
	slog.Debug("ApplyConfig", "cfg", pm.cfg)
	// watch sigterm for exit
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		slog.Info("Graceful shutdown")
		pm.httpServer.Shutdown(context.Background())
		if err := pm.save(); err != nil {
			slog.Error("Save networks", "err", err)
		}
	}()
	// load networks
	if err := pm.load(); err != nil {
		slog.Error("Load networks", "err", err)
	}
	// watch sighup for save networks
	go pm.watchSaveCycle(ctx)
	// serving http
	slog.Info("Serving for http now", "listen", pm.cfg.Listen)
	err := pm.httpServer.ListenAndServe()
	wg.Wait()
	return err
}

func (pm *PeerMap) HandleQueryNetworks(w http.ResponseWriter, r *http.Request) {
	exporterToken := r.Header.Get("X-Token")
	_, err := pm.exporterAuthenticator.CheckToken(exporterToken)
	if err != nil {
		slog.Debug("ExporterAuthFailed", "details", err)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var networks []exporter.NetworkHead
	pm.networkMapMutex.RLock()
	for k, v := range pm.networkMap {
		networks = append(networks, exporter.NetworkHead{
			ID:         k,
			PeersCount: v.peerCount(),
			CreateTime: fmt.Sprintf("%d", v.createTime.UnixNano()),
		})
	}
	pm.networkMapMutex.RUnlock()
	json.NewEncoder(w).Encode(networks)
}

func (pm *PeerMap) HandleQueryNetworkPeers(w http.ResponseWriter, r *http.Request) {
	exporterToken := r.Header.Get("X-Token")
	_, err := pm.exporterAuthenticator.CheckToken(exporterToken)
	if err != nil {
		slog.Debug("ExporterAuthFailed", "details", err)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var networks []exporter.Network
	pm.networkMapMutex.RLock()
	for k, v := range pm.networkMap {
		var peers []string
		v.peersMutex.RLock()
		for _, peer := range v.peers {
			peers = append(peers, peer.String())
		}
		v.peersMutex.RUnlock()
		networks = append(networks, exporter.Network{ID: k, Peers: peers})
	}
	pm.networkMapMutex.RUnlock()
	json.NewEncoder(w).Encode(networks)
}

func (pm *PeerMap) HandlePutNetworkMeta(w http.ResponseWriter, r *http.Request) {
	exporterToken := r.Header.Get("X-Token")
	_, err := pm.exporterAuthenticator.CheckToken(exporterToken)
	if err != nil {
		slog.Debug("ExporterAuthFailed", "details", err)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	network := r.PathValue("network")
	var request exporter.PutNetworkMetaRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ctx, ok := pm.getNetwork(network)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err := ctx.updateMeta(auth.Net{
		Alias:     request.Alias,
		Neighbors: request.Neighbors,
	}); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func (pm *PeerMap) HandleOIDCAuthorize(w http.ResponseWriter, r *http.Request) {
	providerName := path.Base(r.URL.Path)
	provider, ok := oidc.Provider(providerName)
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	email, _, err := provider.UserInfo(r.URL.Query().Get("code"))
	if err != nil {
		slog.Error("OIDC get userInfo error", "err", err)
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(fmt.Sprintf("oidc: %s", err)))
		return
	}
	if email == "" {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("odic: email is required"))
		return
	}
	n := auth.Net{ID: email}
	if ctx, ok := pm.getNetwork(email); ok {
		n.Alias = ctx.alias
		n.Neighbors = ctx.neighbors
	}
	secret, err := pm.generateSecret(n)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	err = oidc.NotifyToken(r.URL.Query().Get("state"), secret)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Write([]byte("ok"))
}

func (pm *PeerMap) HandlePeerPacketConnect(w http.ResponseWriter, r *http.Request) {
	networkSecrest := r.Header.Get("X-Network")
	jsonSecret := auth.JSONSecret{
		Network:  networkSecrest,
		Deadline: math.MaxInt64,
	}
	if len(pm.cfg.PublicNetwork) == 0 || pm.cfg.PublicNetwork != networkSecrest {
		secret, err := pm.authenticator.ParseSecret(networkSecrest)
		if err != nil {
			slog.Debug("Authenticate failed", "err", err, "network", jsonSecret.Network, "secret", r.Header.Get("X-Network"))
			w.WriteHeader(http.StatusForbidden)
			return
		}
		jsonSecret = secret
	}

	peerID := r.Header.Get("X-PeerID")
	nonce := peer.MustParseNonce(r.Header.Get("X-Nonce"))

	pm.networkMapMutex.RLock()
	networkCtx, ok := pm.networkMap[jsonSecret.Network]
	pm.networkMapMutex.RUnlock()
	if !ok {
		pm.networkMapMutex.Lock()
		networkCtx, ok = pm.networkMap[jsonSecret.Network]
		if !ok {
			networkCtx = pm.newNetworkContext(NetState{
				ID:         jsonSecret.Network,
				CreateTime: time.Now(),
			})
			pm.networkMap[jsonSecret.Network] = networkCtx
		}
		pm.networkMapMutex.Unlock()
	}

	networkCtx.initMeta(
		auth.Net{Alias: jsonSecret.Alias, Neighbors: jsonSecret.Neighbors},
		time.Unix(jsonSecret.Deadline, 0).Add(-pm.cfg.SecretValidityPeriod))

	peer := Peer{
		exitSig:        make(chan struct{}),
		peerMap:        pm,
		networkSecret:  jsonSecret,
		networkContext: networkCtx,
		id:             peer.ID(peerID),
		nonce:          nonce,
		connData:       make(chan []byte, 128),
	}

	metadata := r.Header.Get("X-Metadata")
	if len(metadata) > 0 {
		_, err := base64.StdEncoding.DecodeString(metadata)
		if err == nil {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		meta, err := url.ParseQuery(metadata)
		if err != nil {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		peer.metadata = meta
	}

	if ok := networkCtx.SetIfAbsent(peerID, &peer); !ok {
		slog.Debug("Address is already in used", "addr", peerID)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	pm.peerMapMutex.Lock()
	pm.peerMap[peerID] = networkCtx
	pm.peerMapMutex.Unlock()
	upgradeHeader := http.Header{}
	upgradeHeader.Set("X-Nonce", r.Header.Get("X-Nonce"))
	stuns, _ := json.Marshal(pm.cfg.STUNs)
	upgradeHeader.Set("X-STUNs", base64.StdEncoding.EncodeToString(stuns))
	if pm.cfg.RateLimiter != nil {
		upgradeHeader.Set("X-Limiter-Burst", fmt.Sprintf("%d", pm.cfg.RateLimiter.Burst))
		upgradeHeader.Set("X-Limiter-Limit", fmt.Sprintf("%d", pm.cfg.RateLimiter.Limit))
	}
	wsConn, err := pm.wsUpgrader.Upgrade(w, r, upgradeHeader)
	if err != nil {
		slog.Error(err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	peer.conn = wsConn
	peer.Start()
	slog.Debug("PeerConnected", "network", jsonSecret.Network, "peer", peerID)
}

func (pm *PeerMap) watchSaveCycle(ctx context.Context) {
	for {
		sig := make(chan os.Signal, 2)
		signal.Notify(sig, syscall.SIGHUP)
		select {
		case <-ctx.Done():
			close(sig)
			return
		case <-sig:
			close(sig)
			if err := pm.save(); err != nil {
				slog.Error("Save networks", "err", err)
			}
		}
	}
}

func (pm *PeerMap) newNetworkContext(state NetState) *networkContext {
	var rateLimiter *rate.Limiter
	if pm.cfg.RateLimiter != nil && pm.cfg.RateLimiter.Limit > 0 {
		rateLimiter = rate.NewLimiter(
			rate.Limit(pm.cfg.RateLimiter.Limit),
			pm.cfg.RateLimiter.Burst)
	}
	return &networkContext{
		id:              state.ID,
		peers:           make(map[string]*Peer),
		ratelimiter:     rateLimiter,
		disoRatelimiter: rate.NewLimiter(rate.Limit(10*1024), 128*1024),
		createTime:      state.CreateTime,
		updateTime:      state.UpdateTime,
		alias:           state.Alias,
		neighbors:       state.Neighbors,
	}
}

func (pm *PeerMap) load() error {
	f, err := os.Open(pm.cfg.StateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("load: open state file: %w", err)
	}
	defer f.Close()
	var nets []NetState
	if err := json.NewDecoder(f).Decode(&nets); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("load: decode state: %w", err)
	}
	pm.networkMapMutex.Lock()
	defer pm.networkMapMutex.Unlock()
	for _, n := range nets {
		pm.networkMap[n.ID] = pm.newNetworkContext(n)
	}
	slog.Info("Load networks", "count", len(nets))
	return nil
}

func (pm *PeerMap) save() error {
	var nets []NetState
	pm.networkMapMutex.RLock()
	for _, v := range pm.networkMap {
		nets = append(nets, NetState{
			ID:         v.id,
			Alias:      v.alias,
			Neighbors:  v.neighbors,
			CreateTime: v.createTime,
			UpdateTime: v.updateTime})
	}
	pm.networkMapMutex.RUnlock()
	if nets == nil {
		return nil
	}
	f, err := os.Create(pm.cfg.StateFile)
	if err != nil {
		return fmt.Errorf("save: open state file: %w", err)
	}
	if err := json.NewEncoder(f).Encode(nets); err != nil {
		return fmt.Errorf("save: encode state: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("save: close state file: %w", err)
	}
	slog.Info("Save networks", "count", len(nets))
	return nil
}

func (pm *PeerMap) generateSecret(n auth.Net) (peer.NetworkSecret, error) {
	secret, err := auth.NewAuthenticator(pm.cfg.SecretKey).GenerateSecret(n, pm.cfg.SecretValidityPeriod)
	if err != nil {
		return peer.NetworkSecret{}, err
	}
	return peer.NetworkSecret{
		Network: n.ID,
		Secret:  secret,
		Expire:  time.Now().Add(pm.cfg.SecretValidityPeriod - 10*time.Second),
	}, nil
}

func New(server *http.Server, cfg Config) (*PeerMap, error) {
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}

	pm := PeerMap{
		wsUpgrader:            &websocket.Upgrader{},
		networkMap:            make(map[string]*networkContext),
		peerMap:               make(map[string]*networkContext),
		authenticator:         auth.NewAuthenticator(cfg.SecretKey),
		exporterAuthenticator: exporterauth.New(cfg.SecretKey),
		cfg:                   cfg,
	}

	if server == nil {
		mux := http.NewServeMux()
		server = &http.Server{Handler: mux, Addr: cfg.Listen}
		mux.HandleFunc("/", pm.HandlePeerPacketConnect)
		mux.HandleFunc("/networks", pm.HandleQueryNetworks)
		mux.HandleFunc("/peers", pm.HandleQueryNetworkPeers)
		mux.HandleFunc("PUT /network/{network}/meta", pm.HandlePutNetworkMeta)

		mux.HandleFunc("/network/token", oidc.HandleNotifyToken)
		mux.HandleFunc("/oidc/", oidc.RedirectAuthURL)
		mux.HandleFunc("/oidc/authorize/", pm.HandleOIDCAuthorize)
	}
	pm.httpServer = server
	return &pm, nil
}
