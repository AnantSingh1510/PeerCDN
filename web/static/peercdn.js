/**
 * PeerCDN Browser Client
 *
 * Usage:
 *   const cdn = new PeerCDN({ trackerURL: 'ws://localhost:8080/ws', originURL: 'http://localhost:9000' });
 *   await cdn.connect();
 *   const blob = await cdn.download(manifest, (p) => console.log(p * 100 + '%'));
 */

const ICE_SERVERS = [
  { urls: 'stun:stun.l.google.com:19302' },
  // { urls: 'turn:...', username: '...', credential: '...' }  for production - Anant
];

const MAX_CONCURRENT      = 4;
const CHUNK_TIMEOUT_MS    = 20_000;
const HEARTBEAT_INTERVAL  = 25_000;
const QUERY_TIMEOUT_MS    = 5_000;

export class PeerCDN {
  constructor({ trackerURL, originURL = null }) {
    this.trackerURL = trackerURL;
    this.originURL  = originURL;
    this.ws         = null;
    this.peerID     = null;
    this._hbTimer   = null;

    /** @type {Map<string, RTCPeerConnection>} */
    this.peerConns = new Map();
    /** @type {Map<string, RTCDataChannel>} */
    this.channels  = new Map();

    this._queryPending  = new Map();
    this._swarmPending  = new Map();
    this._chunkPending  = new Map();
    this._iceBuf        = new Map();
    this._seedStore     = new Map();
    this._seedManifests = new Map();
  }

  connect() {
    return new Promise((resolve, reject) => {
      this.ws = new WebSocket(this.trackerURL);
      this.ws.onerror = () => reject(new Error('WebSocket error'));
      this.ws.onclose = () => {
        clearInterval(this._hbTimer);
        console.log('[peercdn] disconnected from tracker');
      };
      this.ws.onmessage = ({ data }) => {
        const msg = JSON.parse(data);
        if (msg.type === 'welcome') {
          this.peerID = msg.peerId;
          console.log('[peercdn] connected, peerID:', this.peerID);
          this.ws.onmessage = ({ data }) => this._onMessage(JSON.parse(data));
          this._startHeartbeat();
          resolve();
        } else {
          reject(new Error('expected welcome, got: ' + msg.type));
        }
      };
    });
  }

  _startHeartbeat() {
    this._hbTimer = setInterval(() => {
      if (this.ws.readyState === WebSocket.OPEN) {
        this._send({ type: 'heartbeat' });
      }
    }, HEARTBEAT_INTERVAL);
  }

  _send(msg) {
    this.ws.send(JSON.stringify(msg));
  }

  _onMessage(msg) {
    switch (msg.type) {
      case 'query_result': return this._onQueryResult(msg);
      case 'swarm_result': return this._onSwarmResult(msg);
      case 'relay':        return this._onRelay(msg);
      case 'error':        console.warn('[peercdn] tracker error:', msg.message); break;
      default:             console.debug('[peercdn] unknown msg type:', msg.type);
    }
  }

  _onQueryResult(msg) {
    const key = `${msg.manifestId}:${msg.chunkIndex}`;
    const p = this._queryPending.get(key);
    if (p) { this._queryPending.delete(key); p.resolve(msg.peers || []); }
  }

  _onSwarmResult(msg) {
    const p = this._swarmPending.get(msg.manifestId);
    if (p) { this._swarmPending.delete(msg.manifestId); p.resolve(msg.swarmSizes || {}); }
  }

  async _onRelay(msg) {
    const data = JSON.parse(msg.payload);
    if (data.type === 'offer')           await this._handleOffer(msg.fromId, data);
    else if (data.type === 'answer')     await this._handleAnswer(msg.fromId, data);
    else if (data.candidate !== undefined) await this._handleICE(msg.fromId, data);
  }

  querySwarm(manifestId) {
    return new Promise((resolve) => {
      const timeout = setTimeout(() => {
        this._swarmPending.delete(manifestId);
        resolve({});
      }, QUERY_TIMEOUT_MS);

      this._swarmPending.set(manifestId, {
        resolve: (sizes) => { clearTimeout(timeout); resolve(sizes); },
      });
      this._send({ type: 'swarm_query', manifestId });
    });
  }

  async rarestFirst(manifest) {
    const sizes = await this.querySwarm(manifest.id);
    return [...manifest.chunks].sort((a, b) => {
      const ca = sizes[String(a.index)] ?? 0;
      const cb = sizes[String(b.index)] ?? 0;
      return ca - cb;
    });
  }

  queryPeers(manifestId, chunkIndex) {
    return new Promise((resolve) => {
      const key = `${manifestId}:${chunkIndex}`;
      const timeout = setTimeout(() => {
        this._queryPending.delete(key);
        resolve([]);
      }, QUERY_TIMEOUT_MS);

      this._queryPending.set(key, {
        resolve: (peers) => { clearTimeout(timeout); resolve(peers); },
      });
      this._send({ type: 'query', manifestId, chunkIndex });
    });
  }

  async _connectToPeer(remotePeerID) {
    if (this.channels.has(remotePeerID)) {
      const dc = this.channels.get(remotePeerID);
      if (dc.readyState === 'open') return dc;
      this.channels.delete(remotePeerID);
    }

    const pc = new RTCPeerConnection({ iceServers: ICE_SERVERS });
    this.peerConns.set(remotePeerID, pc);

    const dc = pc.createDataChannel('peercdn', { ordered: true });
    dc.binaryType = 'arraybuffer';
    this._setupDataChannel(dc, remotePeerID);

    pc.onicecandidate = ({ candidate }) => {
      if (candidate) this._send({ type: 'ice', targetId: remotePeerID,
        payload: JSON.stringify(candidate) });
    };

    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
    this._send({ type: 'offer', targetId: remotePeerID,
      payload: JSON.stringify({ type: offer.type, sdp: offer.sdp }) });

    return new Promise((resolve, reject) => {
      const t = setTimeout(() => reject(new Error('DataChannel open timeout')), 15_000);
      dc.onopen  = () => { clearTimeout(t); resolve(dc); };
      dc.onerror = (e) => { clearTimeout(t); reject(e); };
    });
  }

  async _handleOffer(fromId, offer) {
    const pc = new RTCPeerConnection({ iceServers: ICE_SERVERS });
    this.peerConns.set(fromId, pc);

    pc.onicecandidate = ({ candidate }) => {
      if (candidate) this._send({ type: 'ice', targetId: fromId,
        payload: JSON.stringify(candidate) });
    };

    pc.ondatachannel = ({ channel }) => {
      channel.binaryType = 'arraybuffer';
      this._setupDataChannel(channel, fromId);
    };

    await pc.setRemoteDescription(offer);
    const answer = await pc.createAnswer();
    await pc.setLocalDescription(answer);
    this._send({ type: 'answer', targetId: fromId,
      payload: JSON.stringify({ type: answer.type, sdp: answer.sdp }) });

    for (const c of (this._iceBuf.get(fromId) || [])) await pc.addIceCandidate(c);
    this._iceBuf.delete(fromId);
  }

  async _handleAnswer(fromId, answer) {
    const pc = this.peerConns.get(fromId);
    if (pc) await pc.setRemoteDescription(answer);
  }

  async _handleICE(fromId, candidate) {
    const pc = this.peerConns.get(fromId);
    if (pc?.remoteDescription) {
      await pc.addIceCandidate(candidate);
    } else {
      if (!this._iceBuf.has(fromId)) this._iceBuf.set(fromId, []);
      this._iceBuf.get(fromId).push(candidate);
    }
  }

  _setupDataChannel(dc, remotePeerID) {
    dc.onclose = () => {
      this.channels.delete(remotePeerID);
      console.log('[peercdn] channel closed:', remotePeerID);
    };
    dc.onmessage = ({ data }) => {
      if (typeof data === 'string') {
        const msg = JSON.parse(data);
        if (msg.error) this._resolvePendingChunk(null, new Error(msg.error));
        return;
      }
      this._resolvePendingChunk(new Uint8Array(data), null);
    };

    dc.onmessage = async ({ data }) => {
      if (typeof data !== 'string') {
        this._resolvePendingChunk(new Uint8Array(data), null);
        return;
      }
      let req;
      try { req = JSON.parse(data); } catch { return; }

      if (req.manifestId && req.chunkHash) {
        const chunk = this._seedStore.get(req.chunkHash);
        if (!chunk) {
          dc.send(JSON.stringify({ error: 'chunk not found' }));
        } else {
          dc.send(chunk.buffer);
        }
      } else if ('error' in req) {
        this._resolvePendingChunk(null, new Error(req.error));
      }
    };

    this.channels.set(remotePeerID, dc);
  }

  _resolvePendingChunk(data, err) {
    const [entry] = this._chunkPending.entries();
    if (!entry) return;
    this._chunkPending.delete(entry[0]);
    if (err) entry[1].reject(err);
    else     entry[1].resolve(data);
  }

  async fetchFromPeer(peerID, manifestId, chunkHash) {
    const dc = await this._connectToPeer(peerID);
    return new Promise((resolve, reject) => {
      const t = setTimeout(() => {
        this._chunkPending.delete(chunkHash);
        reject(new Error('chunk request timeout'));
      }, CHUNK_TIMEOUT_MS);

      this._chunkPending.set(chunkHash, {
        resolve: (d) => { clearTimeout(t); resolve(d); },
        reject:  (e) => { clearTimeout(t); reject(e); },
      });
      dc.send(JSON.stringify({ manifestId, chunkHash }));
    });
  }

  async fetchFromOrigin(manifestId, chunk) {
    if (!this.originURL) throw new Error('no origin URL configured');
    const resp = await fetch(`${this.originURL}/chunks/${manifestId}/${chunk.hash}`,
      { signal: AbortSignal.timeout(15_000) });
    if (!resp.ok) throw new Error(`origin ${resp.status} for chunk ${chunk.index}`);
    return new Uint8Array(await resp.arrayBuffer());
  }

  async verify(data, expectedHash) {
    const buf  = await crypto.subtle.digest('SHA-256', data);
    const hex  = Array.from(new Uint8Array(buf)).map(b => b.toString(16).padStart(2, '0')).join('');
    return hex === expectedHash;
  }

  /**
   * Download all chunks for a manifest and return a Blob.
   * Automatically seeds after a successful download.
   *
   * @param {Object}   manifest
   * @param {Function} [onProgress]
   */
  async download(manifest, onProgress) {
    const total   = manifest.chunks.length;
    const results = new Array(total);
    let done = 0;

    const ordered = await this.rarestFirst(manifest);

    const sem = new _Semaphore(MAX_CONCURRENT);
    await Promise.all(ordered.map(async (chunk) => {
      await sem.acquire();
      try {
        results[chunk.index] = await this._fetchChunk(manifest.id, chunk);
        done++;
        onProgress?.(done / total);
      } finally {
        sem.release();
      }
    }));

    let offset = 0;
    const out = new Uint8Array(results.reduce((s, a) => s + a.byteLength, 0));
    for (const buf of results) { out.set(buf, offset); offset += buf.byteLength; }
    for (const chunk of manifest.chunks) {
      this._seedStore.set(chunk.hash, results[chunk.index]);
    }
    this._seedManifests.set(manifest.id, manifest);
    this._announce(manifest);

    return new Blob([out], { type: manifest.mimeType || 'application/octet-stream' });
  }

  async _fetchChunk(manifestId, chunk) {
    const peers = await this.queryPeers(manifestId, chunk.index);
    for (const peer of peers) {
      if (peer.id === this.peerID) continue;
      try {
        const data = await this.fetchFromPeer(peer.id, manifestId, chunk.hash);
        if (await this.verify(data, chunk.hash)) return data;
        console.warn('[peercdn] hash mismatch from peer', peer.id, 'chunk', chunk.index);
      } catch (e) {
        console.warn('[peercdn] peer failed:', peer.id.slice(0,8), e.message);
      }
    }
    // Origin fallback - Anant
    const data = await this.fetchFromOrigin(manifestId, chunk);
    if (!(await this.verify(data, chunk.hash)))
      throw new Error(`origin hash mismatch for chunk ${chunk.index}`);
    return data;
  }

  _announce(manifest) {
    this._send({
      type:       'announce',
      manifestId: manifest.id,
      chunks:     manifest.chunks.map(c => c.index),
    });
    console.log('[peercdn] announced', manifest.chunks.length, 'chunks for', manifest.id.slice(0,12));
  }

  /**
   * Manually seed a manifest from pre-loaded chunk data.
   * @param {Object}  manifest
   * @param {Map<string, Uint8Array>}
   */
  seed(manifest, chunksMap) {
    for (const [hash, data] of chunksMap) this._seedStore.set(hash, data);
    this._seedManifests.set(manifest.id, manifest);
    this._announce(manifest);
  }
}

class _Semaphore {
  constructor(n) { this._n = n; this._q = []; }
  acquire() {
    if (this._n > 0) { this._n--; return Promise.resolve(); }
    return new Promise(r => this._q.push(r));
  }
  release() {
    if (this._q.length > 0) this._q.shift()();
    else this._n++;
  }
}
