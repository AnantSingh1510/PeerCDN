package peer

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"
	"strings"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"

	"github.com/peercdn/peercdn/internal/chunk"
	"github.com/peercdn/peercdn/internal/manifest"
	"github.com/peercdn/peercdn/internal/signaling"
)

const (
	maxConcurrentDownloads = 4
	chunkRequestTimeout    = 30 * time.Second
	originFallbackTimeout  = 15 * time.Second
	heartbeatInterval      = 25 * time.Second
	dialPeerTimeout        = 10 * time.Second
)

type Config struct {
	TrackerURL string
	OriginURL  string 
	StoreDir   string
	ListenAddr string
}

type Peer struct {
    cfg    Config
    store  *chunk.Store
    tc     *trackerConn
    log    *slog.Logger
    peerID string
}

func New(cfg Config, log *slog.Logger) (*Peer, error) {
	if log == nil {
		log = slog.Default()
	}
	store, err := chunk.NewStore(cfg.StoreDir)
	if err != nil {
		return nil, fmt.Errorf("init store: %w", err)
	}
	return &Peer{
		cfg:   cfg,
		store: store,
		log:   log,
	}, nil
}

func (p *Peer) Start(ctx context.Context) error {
	tc, err := dialTracker(ctx, p.cfg.TrackerURL, p.log)
	if err != nil {
		return fmt.Errorf("dial tracker: %w", err)
	}
	p.tc = tc
	p.peerID = tc.peerID
	p.log.Info("connected to tracker", "peerID", p.peerID)

	go tc.readLoop(ctx, p.handleRelay)
	go p.heartbeatLoop(ctx)

	if p.cfg.ListenAddr != "" {
		go p.listenInbound(ctx)
	}
	return nil
}

func (p *Peer) Announce(m *manifest.Manifest) error {
	have := p.store.HaveChunks(m)
	var idxs []int
	for i, h := range have {
		if h {
			idxs = append(idxs, i)
		}
	}
	if len(idxs) == 0 {
		return nil
	}
	return p.tc.send(signaling.Message{
		Type:       signaling.MsgAnnounce,
		ManifestID: m.ID,
		Chunks:     idxs,
		ListenAddr: p.cfg.ListenAddr,
	})
}

func (p *Peer) Download(ctx context.Context, m *manifest.Manifest, manifestPath, outputPath string) error {
	lock, err := acquireLock(p.cfg.StoreDir, m.ID)
	if err != nil {
		return err
	}
	defer releaseLock(lock, p.cfg.StoreDir, m.ID)

	have := p.store.HaveChunks(m)
	missing := make([]int, 0, len(have))
	for i, h := range have {
		if !h {
			missing = append(missing, i)
		}
	}

	alreadyHave := len(m.Chunks) - len(missing)
	if alreadyHave > 0 && len(missing) > 0 {
		p.log.Info("resuming download", "have", alreadyHave, "missing", len(missing), "total", len(m.Chunks))
	}

	if len(missing) == 0 {
		p.log.Info("all chunks already present", "manifest", m.ID[:12])
		_ = markComplete(p.cfg.StoreDir, m.ID)
		return p.Announce(m)
	}

	if err := saveState(p.cfg.StoreDir, &downloadState{
		ManifestID:   m.ID,
		ManifestPath: manifestPath,
		OutputPath:   outputPath,
		TrackerURL:   p.cfg.TrackerURL,
		OriginURL:    p.cfg.OriginURL,
	}); err != nil {
		p.log.Warn("could not save resume state", "err", err)
	}

	order, err := p.rarestFirst(ctx, m.ID, missing)
	if err != nil {
		p.log.Warn("rarest-first fallback to sequential", "err", err)
		order = missing
	}
	sem := make(chan struct{}, maxConcurrentDownloads)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error

	for _, idx := range order {
		idx := idx
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := p.fetchChunk(ctx, m, idx); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("chunk %d: %w", idx, err))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(errs) > 0 {
		return fmt.Errorf("%d chunks failed, first: %w", len(errs), errs[0])
	}

	if err := markComplete(p.cfg.StoreDir, m.ID); err != nil {
		p.log.Warn("could not mark download complete", "err", err)
	}
	return p.Announce(m)
}

func (p *Peer) rarestFirst(ctx context.Context, manifestID string, missing []int) ([]int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	sizes, err := p.tc.swarmSizes(ctx, manifestID)
	if err != nil {
		return nil, err
	}

	type item struct {
		idx  int
		size int
	}
	items := make([]item, len(missing))
	for i, idx := range missing {
		key := fmt.Sprintf("%d", idx)
		items[i] = item{idx, sizes[key]}
	}
	sort.Slice(items, func(a, b int) bool {
		return items[a].size < items[b].size
	})

	out := make([]int, len(items))
	for i, it := range items {
		out[i] = it.idx
	}
	return out, nil
}

func (p *Peer) fetchChunk(ctx context.Context, m *manifest.Manifest, index int) error {
	c := m.Chunks[index]

	// Asking tracker who has this chunk, although might change this part later. - Short on time
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	peers, err := p.tc.query(cctx, m.ID, index)
	cancel()
	if err != nil {
		p.log.Warn("tracker query failed, going to origin", "chunk", index, "err", err)
		return p.fetchFromOrigin(ctx, m, index)
	}

	for _, pi := range peers {
		if pi.ID == p.peerID {
			continue
		}
		data, err := p.fetchFromGoPeer(ctx, pi.ID, pi.Addr, m.ID, c.Hash)
		if err != nil {
			p.log.Warn("peer fetch failed", "peer", pi.ID[:8], "chunk", index, "err", err)
			continue
		}
		if err := p.store.Put(m.ID, c.Hash, data); err != nil {
			p.log.Warn("store put failed", "chunk", index, "err", err)
			continue
		}
		p.log.Debug("chunk downloaded from peer", "chunk", index, "peer", pi.ID[:8])
		return nil
	}

	return p.fetchFromOrigin(ctx, m, index)
}

type chunkRequest struct {
	ManifestID string `json:"manifestId"`
	ChunkHash  string `json:"chunkHash"`
}

type chunkResponse struct {
	Error string `json:"error,omitempty"`
}

func (p *Peer) fetchFromGoPeer(ctx context.Context, remotePeerID, remoteAddr, manifestID, chunkHash string) ([]byte, error) {
    if remoteAddr == "" {
        return nil, fmt.Errorf("no address known for peer %s", remotePeerID[:8])
    }
    url := "http://" + remoteAddr + "/peer/chunk/" + manifestID + "/" + chunkHash
    rctx, cancel := context.WithTimeout(ctx, chunkRequestTimeout)
    defer cancel()
    req, _ := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        return nil, fmt.Errorf("http get: %w", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        return nil, fmt.Errorf("peer returned %d", resp.StatusCode)
    }
    data, err := io.ReadAll(resp.Body)
    if err != nil {
        return nil, fmt.Errorf("read body: %w", err)
    }
    return data, nil
}

// ---- Origin fallback -------------------------------------------------------

func (p *Peer) fetchFromOrigin(ctx context.Context, m *manifest.Manifest, index int) error {
	if p.cfg.OriginURL == "" {
		return fmt.Errorf("no origin URL configured and no peers available for chunk %d", index)
	}
	c := m.Chunks[index]
	url := fmt.Sprintf("%s/chunks/%s/%s", p.cfg.OriginURL, m.ID, c.Hash)

	rctx, cancel := context.WithTimeout(ctx, originFallbackTimeout)
	defer cancel()

	req, _ := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("origin GET: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("origin %d for chunk %d", resp.StatusCode, index)
	}

	data := make([]byte, c.Size)
	if _, err := io.ReadFull(resp.Body, data); err != nil {
		return fmt.Errorf("read origin body: %w", err)
	}
	if err := p.store.Put(m.ID, c.Hash, data); err != nil {
		return fmt.Errorf("store chunk %d from origin: %w", index, err)
	}
	p.log.Debug("chunk from origin", "chunk", index)
	return nil
}

// ---=--- Inbound server -----------------------

func (p *Peer) listenInbound(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/peer/chunk/", p.serveChunkHTTP)
	srv := &http.Server{Addr: p.cfg.ListenAddr, Handler: mux}

	p.log.Info("listening for inbound peer connections", "addr", p.cfg.ListenAddr)
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && ctx.Err() == nil {
		p.log.Error("inbound listener error", "err", err)
	}
}

func (p *Peer) serveChunkHTTP(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Access-Control-Allow-Origin", "*")  // ← add this
    parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/peer/chunk/"), "/")
    if len(parts) != 2 {
        http.Error(w, "bad request", http.StatusBadRequest)
        return
    }
    manifestID, chunkHash := parts[0], parts[1]
    data, err := p.store.Get(manifestID, chunkHash)
    if err != nil {
        http.Error(w, err.Error(), http.StatusNotFound)
        return
    }
    w.Header().Set("Content-Type", "application/octet-stream")
    w.Write(data)
}

func (p *Peer) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.tc.send(signaling.Message{Type: signaling.MsgHeartbeat}); err != nil {
				p.log.Warn("heartbeat send failed", "err", err)
			}
		}
	}
}


func (p *Peer) handleRelay(msg signaling.Message) {
	p.log.Debug("relay message", "from", msg.FromID)
	// WebRTC signaling relay for browser peers will be wired here - Anant
}

type trackerConn struct {
	conn   *websocket.Conn
	peerID string
	log    *slog.Logger

	mu          sync.Mutex
	queryPend   map[queryKey]chan []signaling.PeerInfo
	swarmPend   map[string]chan map[string]int
}

type queryKey struct {
	manifestID string
	chunkIndex int
}

func dialTracker(ctx context.Context, url string, log *slog.Logger) (*trackerConn, error) {
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return nil, err
	}
	tc := &trackerConn{
		conn:      conn,
		log:       log,
		queryPend: make(map[queryKey]chan []signaling.PeerInfo),
		swarmPend: make(map[string]chan map[string]int),
	}
	var welcome signaling.Message
	if err := wsjson.Read(ctx, conn, &welcome); err != nil {
		return nil, fmt.Errorf("read welcome: %w", err)
	}
	if welcome.Type != signaling.MsgWelcome {
		return nil, fmt.Errorf("expected welcome, got %s", welcome.Type)
	}
	tc.peerID = welcome.PeerID
	return tc, nil
}

func (tc *trackerConn) send(msg signaling.Message) error {
	return wsjson.Write(context.Background(), tc.conn, msg)
}

func (tc *trackerConn) query(ctx context.Context, manifestID string, chunkIndex int) ([]signaling.PeerInfo, error) {
	ch := make(chan []signaling.PeerInfo, 1)
	key := queryKey{manifestID, chunkIndex}
	tc.mu.Lock()
	tc.queryPend[key] = ch
	tc.mu.Unlock()

	if err := tc.send(signaling.Message{
		Type:       signaling.MsgQuery,
		ManifestID: manifestID,
		ChunkIndex: chunkIndex,
	}); err != nil {
		tc.mu.Lock()
		delete(tc.queryPend, key)
		tc.mu.Unlock()
		return nil, err
	}
	select {
	case peers := <-ch:
		return peers, nil
	case <-ctx.Done():
		tc.mu.Lock()
		delete(tc.queryPend, key)
		tc.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (tc *trackerConn) swarmSizes(ctx context.Context, manifestID string) (map[string]int, error) {
	ch := make(chan map[string]int, 1)
	tc.mu.Lock()
	tc.swarmPend[manifestID] = ch
	tc.mu.Unlock()

	if err := tc.send(signaling.Message{
		Type:       signaling.MsgSwarmQuery,
		ManifestID: manifestID,
	}); err != nil {
		tc.mu.Lock()
		delete(tc.swarmPend, manifestID)
		tc.mu.Unlock()
		return nil, err
	}
	select {
	case sizes := <-ch:
		return sizes, nil
	case <-ctx.Done():
		tc.mu.Lock()
		delete(tc.swarmPend, manifestID)
		tc.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (tc *trackerConn) readLoop(ctx context.Context, onRelay func(signaling.Message)) {
	for {
		var msg signaling.Message
		if err := wsjson.Read(ctx, tc.conn, &msg); err != nil {
			tc.log.Info("tracker read loop ended", "err", err)
			return
		}
		switch msg.Type {
		case signaling.MsgQueryResult:
			key := queryKey{msg.ManifestID, msg.ChunkIndex}
			tc.mu.Lock()
			ch, ok := tc.queryPend[key]
			if ok {
				delete(tc.queryPend, key)
			}
			tc.mu.Unlock()
			if ok {
				ch <- msg.Peers
			}

		case signaling.MsgSwarmResult:
			tc.mu.Lock()
			ch, ok := tc.swarmPend[msg.ManifestID]
			if ok {
				delete(tc.swarmPend, msg.ManifestID)
			}
			tc.mu.Unlock()
			if ok {
				ch <- msg.SwarmSizes
			}

		case signaling.MsgRelay:
			if onRelay != nil {
				onRelay(msg)
			}

		case signaling.MsgError:
			tc.log.Warn("tracker error", "msg", msg.Message)
		}
	}
}
