# GhostProxy 👻

**Intelligent Traffic Proxy & Local-First Microservices Debugger**

GhostProxy is a lightweight, single-binary reverse proxy written in Go, specifically designed to solve the pain of local microservices development. It intercepts inbound and outbound HTTP payloads, acts as a self-configuring service virtualization layer, and enables seamless chaos engineering natively.

Instead of spinning up 5+ dependent Docker Compose services that drain your CPU and RAM, you spin up GhostProxy.

---

## ⚡ Core Features (The 3 Modes)

GhostProxy operates as a state machine with three distinct pipelines. You configure the mode globally, or override it on startup.

### 1. ⏺️ Record Mode (Default)
In this mode, GhostProxy acts as a transparent passthrough to your real upstream service. However, it secretly captures the full HTTP response (headers, status, JSON body) and persists it to a deterministic MD5-hashed snapshot file inside the `./mappings` directory.

### 2. 🔄 Replay Mode
When your real upstream service is down (or you don't want to run it to save RAM), switch to Replay mode. GhostProxy intercepts the network request, looks up the MD5 hash in the `./mappings` directory, and instantly returns the cached JSON snapshot. **Live network latency is completely eliminated.**

### 3. 💥 Chaos Mode
Testing how your frontend handles a 504 Gateway Timeout or a 2.5-second latency spike usually requires hardcoding mock errors or killing docker containers. In Chaos Mode, GhostProxy evaluates dynamic rules defined in your `config.yaml` to artificially degrade performance on a per-route basis.

---

## 🚀 Getting Started

### Prerequisites
- Go 1.22+

### 1. Build and Run

```bash
# Build the binary
go build -o ghostproxy.exe ./cmd/Ghost

# Run in default (Record) mode
./ghostproxy.exe --config config.yaml

# Run in Replay mode
./ghostproxy.exe --config config.yaml --mode replay

# Run in Chaos mode
./ghostproxy.exe --config config.yaml --mode chaos
```

### 2. Configure Your Client
Change your frontend application (or Postman/cURL) to point to GhostProxy instead of your real backend.

*If your backend runs on `localhost:3000`, configure GhostProxy to target `:3000`, and point your frontend to `localhost:8080`.*

---

## ⚙️ Configuration (`config.yaml`)

The entire engine is driven by a declarative YAML file.

```yaml
server:
  host: "0.0.0.0"
  port: 8080

mode: "record"

upstream:
  target: "http://localhost:3000"
  timeout_seconds: 30

storage:
  directory: "./mappings"

routes:
  # Example: 2.5s artificial delay + 503 Outage
  - path: "/api/v1/orders"
    methods: ["GET", "POST"]
    chaos:
      enabled: true
      latency_ms: 2500
      error_code: 503
      error_message: "Service Unavailable — Simulated downstream outage"

  # Example: Clean passthrough (no chaos)
  - path: "/health"
    methods: ["GET"]
    chaos:
      enabled: false
```

---

## 🛠️ Administrative Dashboard

GhostProxy ships with a built-in health and status endpoint. While the proxy is running, you can hit it to check the active mode and snapshot inventory.

```bash
curl http://localhost:8080/__ghostproxy/status
```

**Response:**
```json
{
  "status": "operational",
  "mode": "record",
  "upstream": "http://localhost:3000",
  "cached_snapshots": 14,
  "timestamp": "2026-05-31T15:25:00Z"
}
```

---

## 🐛 Debugging in VS Code / Antigravity

This repository includes a highly-optimized `.vscode/launch.json` file. 

Navigate to the Run/Debug panel in your IDE, and you will see 5 configurations ready to go:
- `GhostProxy — Debug` (Standard launch)
- `GhostProxy — Record Mode`
- `GhostProxy — Replay Mode`
- `GhostProxy — Chaos Mode`
- `GhostProxy — Attach to Process`

These configurations automatically disable compiler optimizations (`-gcflags='all=-N -l'`) allowing for perfect step-through variable inspection.
