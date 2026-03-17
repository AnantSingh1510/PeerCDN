# PeerCDN

P2P content delivery network written in Go. Files are split into chunks and served directly between peers, the central server only handles peer discovery and never sees file data. Built this as a side project in my 3rd year of BTech after getting frustrated reading about how CDNs work without any good hands-on examples.

The architecture is closer to a stripped-down BitTorrent than a traditional CDN. A tracker maintains a registry of which peers hold which chunks. Downloaders query the tracker, get a list of peers, and fetch directly over HTTP. Chunks are SHA-256 verified before being written to disk. The tracker itself is just a WebSocket server, lightweight enough to run on a $5 VPS.

### This project supports all mime types and works like BitTorrent and Cloudflare combined, I built this as a side project during my college 3rd year, would try my best to improve upon this in coming time.

---

## Architecture


### Overall system flow
```mermaid
  flowchart TD
    subgraph Origin["Origin Server"]
        OF[File Store\n/chunks/manifestID/hash]
    end

    subgraph Tracker["Tracker :8080"]
        WS[WebSocket Handler]
        REG[(Peer Registry\nmanifestID → chunkIndex → peerIDs)]
        REAP[Reaper\nevery 30s, TTL 60s]
        WS --> REG
        REAP -->|evict stale peers| REG
    end

    subgraph Seeder["Seeder Peer"]
        SC[Chunker\n512KB blocks + SHA-256]
        SS[Chunk Store\n/chunks/manifestID/hash]
        SH[HTTP Server :9001\nGET /peer/chunk/manifestID/hash]
        SC -->|writes| SS
        SS --> SH
    end

    subgraph Leecher["Leecher Peer"]
        LQ[swarm_query\nrarest-first sort]
        LP[4x parallel workers]
        LV[SHA-256 verify]
        LS[Chunk Store]
        LA[Assemble\noutput file]
        LQ --> LP
        LP --> LV
        LV -->|pass| LS
        LS --> LA
    end

    Seeder -->|announce chunks| Tracker
    Leecher -->|swarm_query + chunk queries| Tracker
    Tracker -->|peer list + addresses| Leecher
    Leecher -->|GET chunk| Seeder
    Leecher -->|fallback if no peers| Origin
    LA -->|announce| Tracker
```


### Signaling protocol in detail

```mermaid

sequenceDiagram
    participant S as Seeder
    participant T as Tracker
    participant L as Leecher
    participant O as Origin

    Note over S,T: Seeder connects
    S->>T: WS connect
    T-->>S: welcome {peerId: "abc123"}
    S->>T: announce {manifestId, chunks: [0,1,2..10], listenAddr: "host:9001"}
    T->>T: registry[manifestId][chunkN] = {"abc123"}

    Note over L,T: Leecher connects
    L->>T: WS connect
    T-->>L: welcome {peerId: "def456"}

    Note over L,T: Rarest-first scheduling
    L->>T: swarm_query {manifestId}
    T-->>L: swarm_result {"0":1, "1":1, "2":1 ...}
    L->>L: sort chunks by peer count ascending

    Note over L,S: Parallel chunk download (4 workers)
    par worker 1
        L->>T: query {manifestId, chunkIndex: 0}
        T-->>L: query_result {peers: [{id, addr: "host:9001"}]}
        L->>S: GET /peer/chunk/manifestId/hash0
        S-->>L: 512KB raw bytes
        L->>L: SHA-256 verify ✓
        L->>L: write to store
    and worker 2
        L->>T: query {manifestId, chunkIndex: 1}
        T-->>L: query_result {peers: [{id, addr: "host:9001"}]}
        L->>S: GET /peer/chunk/manifestId/hash1
        S-->>L: 512KB raw bytes
        L->>L: SHA-256 verify ✓
    and worker 3 (no peers)
        L->>T: query {manifestId, chunkIndex: 2}
        T-->>L: query_result {peers: []}
        L->>O: GET /chunks/manifestId/hash2
        O-->>L: 512KB raw bytes
        L->>L: SHA-256 verify ✓
    end

    Note over L,T: Leecher becomes seeder
    L->>L: assemble all chunks → output file
    L->>T: announce {manifestId, chunks: [0,1..10], listenAddr}
    T->>T: registry updated — leecher now in swarm

    Note over T: Heartbeat / TTL
    loop every 25s
        S->>T: heartbeat
        L->>T: heartbeat
    end
    T->>T: reap peers silent for 60s
```


### Chunk store internals

```mermaid
flowchart LR
    subgraph Input
        F[Original File\nmovie.mp4 500MB]
    end

    subgraph Chunker
        R[Read 512KB block]
        H[SHA-256 hash]
        M[Build Manifest]
        R --> H --> M
    end

    subgraph Store["Chunk Store (disk)"]
        direction TB
        MJ[manifest\nabc123...json]
        subgraph Chunks["chunks/abc123/"]
            C0[a3f9....\n512KB]
            C1[b7c2....\n512KB]
            C2[e91d....\n512KB]
            CN[last chunk\n≤512KB]
        end
    end

    subgraph Verify
        V{SHA-256\nmatch?}
        OK[write to store]
        BAD[discard +\nre-request]
        V -->|yes| OK
        V -->|no| BAD
    end

    F --> Chunker
    Chunker --> Store
    Store -->|on download| Verify
```

Chunk transfer between Go peers is plain HTTP — the seeder runs a lightweight HTTP server on `--listen`, exposes chunks at `/peer/chunk/<manifestID>/<hash>`. No WebSocket, no WebRTC for Go↔Go. Browser peers use the same HTTP endpoints, with WebRTC as a future path for browser↔browser.

---

## How files move

**Chunking** — the chunker reads the file in 512KB blocks, SHA-256 hashes each block, and writes a manifest JSON + one file per chunk on disk. The manifest ID is the SHA-256 of the full file, so you can verify the whole thing at the end.

**Announcing** — when a seeder starts, it sends an `announce` message to the tracker with the manifest ID and a list of chunk indexes it holds. The tracker stores this in an in-memory registry.

**Downloading** — the leecher first sends a `swarm_query` to get a chunk→peerCount map, sorts missing chunks by ascending count (rarest-first), then fires up to 4 parallel workers. Each worker queries the tracker for peers holding that chunk and fetches via HTTP. Chunks that fail integrity checks are discarded and retried. If no peer has a chunk, falls back to origin.

**Heartbeat** — peers send a heartbeat every 25s. The tracker reaps any peer that goes silent for 60s. This keeps the registry clean when peers crash or disconnect without a clean close.

---

## Running it

```bash
go mod tidy && make all
```

```bash
./bin/tracker --addr :8080

./bin/chunker --file ./movie.mp4 --out ./chunks --mime video/mp4

./bin/peer seed \
  --tracker ws://localhost:8080/ws \
  --manifest ./chunks/<id>.json \
  --store ./chunks \
  --listen :9001

./bin/peer get \
  --tracker ws://localhost:8080/ws \
  --manifest ./chunks/<id>.json \
  --store ./dl-chunks \
  --origin http://your-origin:9000 \
  --out ./movie-copy.mp4
```

---

## Signaling protocol

WebSocket + JSON frames. The tracker is purely a coordination layer.

| type | direction | description |
|---|---|---|
| `announce` | C→S | declare chunk ownership for a manifest |
| `query` | C→S | get peers holding chunk N of manifest M |
| `swarm_query` | C→S | get peer counts per chunk (for rarest-first) |
| `heartbeat` | C→S | keep-alive |
| `offer/answer/ice` | C↔S | WebRTC relay (browser peers) |
| `welcome` | S→C | assigned peerID on connect |
| `query_result` | S→C | peer list with addresses |
| `swarm_result` | S→C | `{"0": 3, "1": 1, "2": 4, ...}` |

---

## Manifest format

```json
{
  "version": 1,
  "id": "<sha256 of whole file>",
  "name": "movie.mp4",
  "size": 524288000,
  "chunkSize": 524288,
  "mimeType": "video/mp4",
  "createdAt": 1710000000,
  "chunks": [
    { "index": 0, "hash": "<sha256>", "offset": 0,      "size": 524288 },
    { "index": 1, "hash": "<sha256>", "offset": 524288, "size": 524288 }
  ]
}
```

Distribute the manifest out-of-band (HTTP, paste, whatever). The manifest ID doubles as a content address — if the file changes the ID changes.

---

## What works

- file chunking + SHA-256 integrity at chunk and whole-file level
- tracker with in-memory peer registry and TTL eviction
- rarest-first scheduling via swarm size queries
- parallel chunk downloads with semaphore (default 4)
- direct HTTP peer-to-peer transfer
- origin HTTP fallback
- browser client (same HTTP chunk fetching, rarest-first, announces after download)
- seed-after-download — leecher automatically becomes seeder on completion

## Workflow
```mermaid
graph LR
    File -->|512KB blocks + SHA-256| Manifest
    Manifest -->|announce| Tracker
    Tracker -->|who has chunk N?| Peers
    Peers -->|HTTP GET /peer/chunk/...| Downloader
    Downloader -->|verify + store| Disk
    Disk -->|assemble| Output
    Downloader -->|no peers?| Origin
    Output -->|announce| Tracker
```

## Known gaps

- no origin server binary yet — you need to bring your own HTTP server for the fallback
- Go↔Go peer address resolution works but browser peers can't serve chunks back (browsers can't run HTTP servers), so browser→browser transfer needs WebRTC which isn't wired up yet
- no resume — if a download is interrupted you start over
- tracker has no auth, rate limiting, or persistence — it's purely in-memory
- TURN server integration missing, so peers behind strict NATs will silently fall back to origin
