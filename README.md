# P2P Dependency Distribution for CI/CD

## The Problem

Every CI build downloads its dependencies from a central registry — npm, PyPI, crates.io — independently, from scratch, every single time. It doesn't matter that the machine running next to it just finished downloading the exact same packages five minutes ago. There is no sharing. Each machine asks the registry on its own, and the registry answers each one individually.

At small scale, this is an annoyance. At team scale, it becomes a structural waste problem.

### What the waste looks like

A team running 100 builds per day, with an average dependency payload of 500MB per build, is pulling **50GB/day** from upstream registries. Of that, roughly 48GB is byte-for-byte identical packages that already exist on other machines in the same datacenter. At typical registry download speeds of 20–50 MB/s, that translates to 10–25 minutes of pure download time baked into every single build — not compilation, not testing, just waiting for files that are already nearby.

The cost compounds in three dimensions:

- **Time** — Build queues grow. Developer feedback loops lengthen. Deploys slow down.
- **Bandwidth** — Cloud egress fees, registry rate limits, and throttling all become real operational concerns at scale.
- **Reliability** — Every build has a hard dependency on the registry being available. A PyPI outage or an npm incident breaks your entire CI fleet, even for packages you've downloaded a thousand times before.

### Why existing tools don't fully solve it

| Tool | What it does | Where it falls short |
|---|---|---|
| npm / pip local cache | Caches per machine | Machine B can't use machine A's cache |
| Artifactory / Nexus | Private mirror registry | Requires dedicated servers, maintenance, becomes a single point of failure, costs money to scale |
| GitHub Actions cache | Per-repo centralized cache | Size limits, centralized, no multi-source parallel download |
| Bazel remote cache | Powerful build artifact cache | Requires migrating your entire build system to Bazel — a months-long effort |

None of them provide decentralized, self-organizing, multi-source parallel downloads without a server you have to manage. The moment your central cache server goes down, you're back to hitting the registry.

---

## The Solution

A peer-to-peer distribution layer that sits transparently between your package manager and the upstream registry. Every CI machine runs a lightweight proxy daemon. That proxy intercepts package manager requests, checks the local cache, checks the peer swarm, and only reaches out to the registry if no one in the fleet has the bundle yet.

Once a single machine downloads a dependency bundle, it becomes a seeder. Every subsequent build in the fleet — regardless of which machine runs it — pulls from the swarm instead of the registry.

### How it works

**1. Bundle identification via lockfile hashing**

The proxy SHA256-hashes the build's lockfile (`package-lock.json`, `requirements.txt`, `Cargo.lock`) to generate a deterministic bundle ID. Because lockfiles are deterministic, two builds with the same lockfile need byte-for-byte identical dependencies. This makes the cached bundle perfectly safe to share — there's no ambiguity about whether two machines are asking for "the same thing."

**2. Three-tier resolution**

When a build triggers a dependency install, the proxy resolves in order:

1. **Local disk cache** — content-addressed storage keyed by bundle ID. Hits serve in under 5ms.
2. **Peer swarm** — query peers via mDNS (LAN) or DHT (cross-datacenter). Chunks download in parallel from multiple nodes simultaneously.
3. **Registry fallback** — if no peer has the bundle, fetch from npm/PyPI/crates.io as normal, then immediately begin seeding to the swarm.

**3. Peer discovery**

- **mDNS** handles discovery on the same LAN or datacenter subnet. No configuration needed. Machines find each other automatically at startup and share near-instantly at LAN speeds (~1 GB/s vs ~50 MB/s from the registry — a 20× difference).
- **DHT (Kademlia)** handles cross-datacenter discovery via a lightweight bootstrap node. One bootstrap node per organisation is sufficient; it doesn't store content, just facilitates peer handshakes.

**4. Zero workflow changes**

Developers keep running `npm install`, `pip install`, `cargo build` — exactly as before. The proxy is configured once at the infrastructure level via a single `.npmrc` line, `PIP_INDEX_URL` environment variable, or Cargo config entry. From the developer's perspective, nothing changes except builds get faster.

### Properties

- **Immutable and verified** — all bundles are content-addressed and cryptographically verified. A peer cannot serve you a corrupted or tampered bundle without the hash check failing.
- **Registry-resilient** — once a bundle exists in the swarm, builds that use it are completely independent of the upstream registry. A PyPI outage doesn't affect you.
- **Serverless** — no central cache server to provision, monitor, or scale. The swarm is the infrastructure.
- **Graceful degradation** — if the swarm has no peers (cold start, new dependency), the system transparently falls back to the registry and begins seeding. There is no failure mode that breaks a build.

### Expected outcomes

| Metric | Expectation |
|---|---|
| Build time reduction (warm fleet) | 50–80% |
| Registry traffic (warm fleet) | Near zero |
| Cold start penalty | One build — all subsequent builds are fast |
| Infrastructure cost | One lightweight bootstrap node per org |

---

## Architecture

### Core components

**Proxy daemon** — runs on every CI machine. Intercepts HTTP/HTTPS requests from package managers, manages the local content store, and participates in the P2P swarm. Exposes a local port that package managers are configured to target.

**Content store** — a content-addressed local filesystem store at `~/.p2p-cache/<sha256>/`. Chunks are stored individually so partial bundles can be served and resumed.

**libp2p node** — embedded in the proxy daemon. Handles peer identity (Ed25519 keypair), transport (QUIC + TCP), stream multiplexing, mDNS announcements, and DHT participation.

**Bootstrap node** — a minimal rendezvous service. Stateless. Does not store content. Helps DHT peers find each other across NAT boundaries.

### Technology choices

- **Language** — Go, using `go-libp2p`
- **Networking** — libp2p (peer identity, transport, DHT, mDNS)
- **Content store** — filesystem for Phase 1–2; BadgerDB for high-throughput fleets
- **Chunk transfer** — Bitswap (libp2p's content exchange protocol)

### Build phases

**Phase 1 — Local caching proxy (weeks 1–2)**
Build the HTTP proxy and local content store. No P2P yet. Delivers immediate bandwidth savings and generates baseline metrics.

**Phase 2 — LAN peer sharing via mDNS (weeks 3–4)**
Add the libp2p node and mDNS peer discovery. CI machines in the same datacenter now share dependencies automatically.

**Phase 3 — Cross-datacenter DHT (weeks 5–6)**
Add the DHT bootstrap node. Builds in different regions and datacenters find each other's bundles.


So far, Phase 1 and 2 have been implemented