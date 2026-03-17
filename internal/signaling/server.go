package signaling

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

type MsgType string

const (
	MsgAnnounce   MsgType = "announce"
	MsgQuery      MsgType = "query" 
	MsgSwarmQuery MsgType = "swarm_query"
	MsgHeartbeat  MsgType = "heartbeat" 

	MsgOffer     MsgType = "offer"
	MsgAnswer    MsgType = "answer"
	MsgICE       MsgType = "ice"
	MsgByeTarget MsgType = "bye"

	MsgQueryResult MsgType = "query_result"
	MsgSwarmResult MsgType = "swarm_result"
	MsgRelay       MsgType = "relay"
	MsgError       MsgType = "error"
	MsgWelcome     MsgType = "welcome"

	peerTTL = 60 * time.Second
)

type Message struct {
	Type MsgType `json:"type"`

	ManifestID string `json:"manifestId,omitempty"`
	Chunks     []int  `json:"chunks,omitempty"`
	ChunkIndex int    `json:"chunkIndex,omitempty"`

	Peers      []PeerInfo     `json:"peers,omitempty"`
	SwarmSizes map[string]int `json:"swarmSizes,omitempty"`

	TargetID string          `json:"targetId,omitempty"`
	FromID   string          `json:"fromId,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`

	PeerID  string `json:"peerId,omitempty"`
	ListenAddr string `json:"listenAddr,omitempty"`
	Message string `json:"message,omitempty"`
}

type PeerInfo struct {
    ID   string `json:"id"`
    Addr string `json:"addr,omitempty"`
}


type availability map[int]map[string]struct{}

type registry struct {
	mu    sync.RWMutex
	data  map[string]availability
	addrs map[string]string
}

func newRegistry() *registry {
	return &registry{data: make(map[string]availability), addrs: make(map[string]string)}
}

func (r *registry) announce(peerID, manifestID string, chunks []int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.data[manifestID] == nil {
		r.data[manifestID] = make(availability)
	}
	for _, idx := range chunks {
		if r.data[manifestID][idx] == nil {
			r.data[manifestID][idx] = make(map[string]struct{})
		}
		r.data[manifestID][idx][peerID] = struct{}{}
	}
}

func (r *registry) setAddr(peerID, addr string) {
    r.mu.Lock()
    r.addrs[peerID] = addr
    r.mu.Unlock()
}

func (r *registry) getAddr(peerID string) string {
    r.mu.RLock()
    defer r.mu.RUnlock()
    return r.addrs[peerID]
}

func (r *registry) query(manifestID string, chunkIndex, maxPeers int) []PeerInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	av, ok := r.data[manifestID]
	if !ok {
		return nil
	}
	result := make([]PeerInfo, 0, maxPeers)
	for id := range av[chunkIndex] {
		result = append(result, PeerInfo{ID: id})
		if len(result) >= maxPeers {
			break
		}
	}
	return result
}

func (r *registry) swarmSizes(manifestID string) map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	av, ok := r.data[manifestID]
	if !ok {
		return nil
	}
	out := make(map[string]int, len(av))
	for idx, peers := range av {
		out[fmt.Sprintf("%d", idx)] = len(peers)
	}
	return out
}

func (r *registry) removePeer(peerID string) {
    r.mu.Lock()
    defer r.mu.Unlock()
    delete(r.addrs, peerID)
    for _, av := range r.data {
		for idx, peers := range av {
			delete(peers, peerID)
			if len(peers) == 0 {
				delete(av, idx)
			}
		}
	}
}

type hub struct {
	mu    sync.RWMutex
	conns map[string]*peerConn
}

func newHub() *hub { return &hub{conns: make(map[string]*peerConn)} }

func (h *hub) add(p *peerConn) {
	h.mu.Lock()
	h.conns[p.id] = p
	h.mu.Unlock()
}

func (h *hub) remove(id string) {
	h.mu.Lock()
	delete(h.conns, id)
	h.mu.Unlock()
}

func (h *hub) get(id string) (*peerConn, bool) {
	h.mu.RLock()
	p, ok := h.conns[id]
	h.mu.RUnlock()
	return p, ok
}

func (h *hub) count() int {
	h.mu.RLock()
	n := len(h.conns)
	h.mu.RUnlock()
	return n
}


type peerConn struct {
	id       string
	conn     *websocket.Conn
	send     chan Message
	seenMu   sync.Mutex
	lastSeen time.Time
}

func newPeerConn(id string, conn *websocket.Conn) *peerConn {
	return &peerConn{
		id:       id,
		conn:     conn,
		send:     make(chan Message, 64),
		lastSeen: time.Now(),
	}
}

func (p *peerConn) touch() {
	p.seenMu.Lock()
	p.lastSeen = time.Now()
	p.seenMu.Unlock()
}

func (p *peerConn) isStale() bool {
	p.seenMu.Lock()
	stale := time.Since(p.lastSeen) > peerTTL
	p.seenMu.Unlock()
	return stale
}

func (p *peerConn) enqueue(m Message) {
	select {
	case p.send <- m:
	default:
		slog.Warn("send channel full, dropping message", "peer", p.id, "type", m.Type)
	}
}


type Server struct {
	reg *registry
	hub *hub
	log *slog.Logger
}

func New(log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		reg: newRegistry(),
		hub: newHub(),
		log: log,
	}
	go s.reapLoop()
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"status":"ok","peers":%d}`, s.hub.count())
	})
	return mux
}

func (s *Server) reapLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.hub.mu.Lock()
		for id, p := range s.hub.conns {
			if p.isStale() {
				s.log.Info("reaping stale peer", "id", id)
				delete(s.hub.conns, id)
				s.reg.removePeer(id)
				p.conn.Close(websocket.StatusGoingAway, "ttl expired")
			}
		}
		s.hub.mu.Unlock()
	}
}

// Fix later: WS Connection dropping
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.Error("websocket accept", "err", err)
		return
	}

	peerID := generateID()
	p := newPeerConn(peerID, conn)
	s.hub.add(p)
	s.log.Info("peer connected", "id", peerID, "total", s.hub.count())

	go p.writePump()
	p.enqueue(Message{Type: MsgWelcome, PeerID: peerID})

	s.readPump(p)

	s.hub.remove(peerID)
	s.reg.removePeer(peerID)
	conn.Close(websocket.StatusGoingAway, "disconnected")
	s.log.Info("peer disconnected", "id", peerID, "total", s.hub.count())
}

func (s *Server) readPump(p *peerConn) {
	ctx := context.Background()
	for {
		var msg Message
		if err := wsjson.Read(ctx, p.conn, &msg); err != nil {
			return
		}
		p.touch()
		s.handle(p, msg)
	}
}

func (s *Server) handle(p *peerConn, msg Message) {
	switch msg.Type {

	case MsgHeartbeat:
		// touch() already called nothing more is needed

	case MsgAnnounce:
		if msg.ManifestID == "" || len(msg.Chunks) == 0 {
			p.enqueue(Message{Type: MsgError, Message: "announce: manifestId and chunks required"})
			return
		}
		if msg.ListenAddr != "" {
			s.reg.setAddr(p.id, msg.ListenAddr)
		}
		// Remove this later
		s.reg.announce(p.id, msg.ManifestID, msg.Chunks)
		s.log.Info("announce", "peer", p.id, "manifest", msg.ManifestID[:8], "chunks", len(msg.Chunks))

	case MsgQuery:
		if msg.ManifestID == "" {
			p.enqueue(Message{Type: MsgError, Message: "query: manifestId required"})
			return
		}
		peers := s.reg.query(msg.ManifestID, msg.ChunkIndex, 10)
		for i := range peers {
			peers[i].Addr = s.reg.getAddr(peers[i].ID)
		}
		p.enqueue(Message{
			Type:       MsgQueryResult,
			ManifestID: msg.ManifestID,
			ChunkIndex: msg.ChunkIndex,
			Peers:      peers,
		})

	case MsgSwarmQuery:
		if msg.ManifestID == "" {
			p.enqueue(Message{Type: MsgError, Message: "swarm_query: manifestId required"})
			return
		}
		sizes := s.reg.swarmSizes(msg.ManifestID)
		p.enqueue(Message{
			Type:       MsgSwarmResult,
			ManifestID: msg.ManifestID,
			SwarmSizes: sizes,
		})

	case MsgOffer, MsgAnswer, MsgICE:
		if msg.TargetID == "" {
			p.enqueue(Message{Type: MsgError, Message: "relay: targetId required"})
			return
		}
		target, ok := s.hub.get(msg.TargetID)
		if !ok {
			p.enqueue(Message{Type: MsgError, Message: fmt.Sprintf("peer %s not found", msg.TargetID)})
			return
		}
		target.enqueue(Message{
			Type:    MsgRelay,
			FromID:  p.id,
			Payload: msg.Payload,
		})

	default:
		p.enqueue(Message{Type: MsgError, Message: fmt.Sprintf("unknown message type: %s", msg.Type)})
	}
}

func (p *peerConn) writePump() {
	ctx := context.Background()
	for msg := range p.send {
		if err := wsjson.Write(ctx, p.conn, msg); err != nil {
			return
		}
	}
}
