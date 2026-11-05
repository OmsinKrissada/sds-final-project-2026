# Quizbox

A live, Kahoot-style quiz game built as six container images on a Raspberry Pi
k3s cluster. Final project for 2110415 Software-Defined Systems.

A host logs in, writes a quiz and starts a game. Players join from their phones
with a 6-digit PIN, answer against a timer, and see scores after each question.
Every request is served by **at least two application containers, not counting
the gateway**, and the app recovers on its own when any single Pi is unplugged.

- [What it does](#what-it-does)
- [Architecture](#architecture)
- [Set up the cluster](#1-set-up-the-cluster)
- [Set up the database](#2-set-up-the-database-on-the-vm)
- [Build the images](#3-build-the-images)
- [Deploy](#4-deploy-the-application)
- [Fault-tolerance demo](#fault-tolerance-demo)
- [Local development and tests](#local-development-and-tests)
- [Troubleshooting](#troubleshooting)

## What it does

| Who | Can |
|---|---|
| Host (account required) | Register / log in, create and edit quizzes (2–4 choices, time limit per question), host a live game, see history |
| Player (no account needed) | Join with PIN + nickname, answer on a full-screen four-button pad, see points and rank after each question |
| Everyone | Hall of fame (all-time points of logged-in players) on the home page |

Scoring: a correct answer gives `round(1000 × (1 − elapsed / limit / 2))`
points, so faster answers score more; wrong answers score 0.

A demo account and two sample quizzes are seeded on first start:
**`demo@quizbox.local` / `demo1234`**.

## Architecture

```
                  ┌──────────────────────────── k3s cluster (pods on the 5 Pis) ───────────────────────────┐
 browsers ──VIP──▶│ gateway ──▶ frontend ──▶ account ──▶ redis                                              │
192.168.0.240     │    │           └──────▶ game ────▶ redis                                                │
 (MetalLB)        │    ├─ authz ─▶ account ─▶ redis                                                        │──▶ Postgres 16
                  │    ├─────────▶ quiz                                                                    │    (Docker on the
                  │    └─────────▶ game ───▶ quiz, redis, account                                          │     Ubuntu VM)
                  └────────────────────────────────────────────────────────────────────────────────────────┘
```

| Image | Job | State |
|---|---|---|
| `gateway` | Single entry point on the VIP. Routes by path, asks account to validate the session, proxies WebSockets. Adds `X-Trace` and `X-App-Hops`. Not counted as a hop. | none |
| `frontend` | Server-renders every page. Each load calls account (who is logged in) and game (hall of fame / PIN check). The Svelte SPA is inlined into the HTML, so there are no separate asset requests. | none |
| `account` | Register, login, logout, session validation (forward-auth for gateway), display names | Redis `sess:*`, PG schema `account` |
| `quiz` | Quiz and question CRUD; internal endpoint for game | PG schema `quiz` |
| `game` | Game PINs, WebSocket hub, answers, scoring, leaderboards, history. Fan-out between replicas via Redis pub/sub. | Redis `game:*`, `lb:*`, PG schema `game` |
| `redis` | Sessions cache, live game state, sorted-set leaderboards, pub/sub | `emptyDir`, rebuildable from PG |

**Redis is a cache, Postgres is the truth.** Sessions fall back to Postgres if
Redis is gone. Live games are snapshotted to Postgres on every status change and
restored automatically if Redis restarts empty.

### Every request touches ≥ 2 app containers

| Request | App containers (gateway excluded) | Hops |
|---|---|---|
| `GET /` (page load) | frontend, account, game, redis | 4 |
| `GET /play?pin=…` | frontend, account, game, redis | 4 |
| `POST /api/auth/login` / `register` / `logout` | account, redis | 2 |
| `GET/POST /api/quizzes` | account (authz), redis, quiz | 3 |
| `POST /api/games` | account (authz), redis, game, quiz | 4 |
| `GET /api/games/{pin}`, `/api/leaderboard` | game, redis | 2 |
| `GET /api/me/history` | account (authz), redis, game | 3 |
| `WS /ws/games/{pin}` | game, redis | 2 |

Every response carries `X-Trace` (e.g.
`gateway@pi-2,account@pi-3,redis,quiz@pi-1,postgres`) and `X-App-Hops`.
Postgres is outside the cluster and is not counted. In the app, tap the
**trace** button (bottom right) to see the hops of the last 20 requests,
including the page load and the WebSocket connection.

Full API, Redis keys and the WebSocket protocol are in [CONTRACT.md](CONTRACT.md).

### Repository layout

```
cluster/       k3s server/agent install, Pi prep, MetalLB
db/            Postgres on the VM: docker-compose.yml, init.sql (schemas), roles.sh (per-service roles)
deploy/base    Kustomize: 1 replica of each service (default)
deploy/overlays/ha   2 replicas of the five stateless services
deploy/prepull.yaml  DaemonSet that caches all images on every Pi
services/      gateway, account, quiz, game, frontend (Go) + Dockerfiles
frontend/web   Svelte SPA (single-file Vite build → services/frontend/dist/index.html)
internal/common  shared tracing, HTTP client with retry, health, server runner
test/e2e       end-to-end tests against a running stack
scripts/       render, secrets, images, dev runner
```

## Hardware and network

- 1 notebook running an **Ubuntu 22.04/24.04 VM** (≥ 2 vCPU, 4 GB RAM) with a
  **bridged** network adapter on the router's LAN. It is the k3s server and runs
  Postgres. Disable sleep on the notebook.
- 5 × **Raspberry Pi 3B+** with a **64-bit** OS (Raspberry Pi OS Lite 64-bit or
  Ubuntu Server arm64), wired to the TP-Link router.
- TP-Link TL-WR841N, LAN `192.168.0.0/24`. Reserve DHCP leases for the VM and
  the Pis, and keep `192.168.0.240–250` **out of the DHCP pool** (MetalLB uses
  them).
- Players' phones must join the router's WiFi to reach `192.168.0.240`.

The examples below assume the VM is `192.168.0.10`. Adjust to your network.

## 1. Set up the cluster

On the **VM**:

```bash
git clone <this repo> && cd quizbox
cp .env.example .env            # edit REGISTRY, DB_HOST, passwords
sudo -E cluster/server.sh       # prints the join command + token
mkdir -p ~/.kube && sudo cp /etc/rancher/k3s/k3s.yaml ~/.kube/config && sudo chown $USER ~/.kube/config
```

`server.sh` installs k3s with traefik and servicelb disabled, taints the server
so no app pod lands on it, and sets `node-monitor-grace-period=20s` so dead Pis
are detected quickly.

On **each Pi** (copy the `cluster/` folder over, or clone the repo):

```bash
sudo cluster/pi-prep.sh pi1     # memory cgroups, swap off, hostname; reboot if told
sudo cluster/agent.sh https://192.168.0.10:6443 <token>
```

Back on the VM:

```bash
kubectl get nodes -o wide       # 1 control plane + 5 Pis, all Ready
make metallb                    # MetalLB + pool 192.168.0.240-250
```

## 2. Set up the database on the VM

Docker must be installed on the VM.

```bash
make db-up                      # postgres:16 on :5432, data in db/pgdata
```

On first start, `db/init.sql` creates the `account`, `quiz` and `game` schemas,
and `db/roles.sh` creates one role per service that can only use its own schema.
These scripts run only when `db/pgdata` is empty; delete it to re-initialize.

Check that the Pis can reach it: `nc -zv 192.168.0.10 5432` from a Pi. If
`ufw` is enabled on the VM, `sudo ufw allow from 192.168.0.0/24 to any port 5432`.

## 3. Build the images

Images are multi-arch (`linux/arm64` for the Pis, `linux/amd64` for laptops),
cross-compiled without QEMU.

**Option A: GitHub Actions** (recommended). Pushing to `main` runs
`.github/workflows/images.yml`, which tests the code and pushes
`ghcr.io/<owner>/quizbox-<svc>:latest` and `:sha-<short>`. Then make the five
packages **public** in GitHub → Packages (or add an imagePullSecret), otherwise
the Pis cannot pull them. Set `REGISTRY=ghcr.io/<owner>` in `.env`.

**Option B: from your machine**: `docker login ghcr.io`, then `make images`.

## 4. Deploy the application

```bash
make prepull      # cache all images on every Pi, so replacement pods start fast
make deploy       # creates the DB secret, renders kustomize, applies, waits for all six rollouts
make where        # which pod runs on which Pi
```

Open **http://192.168.0.240** and log in with `demo@quizbox.local` /
`demo1234`. To run the end-to-end tests against the cluster:

```bash
make e2e BASE_URL=http://192.168.0.240
```

`make deploy` uses `deploy/base` (1 replica each). `make deploy-ha` uses 2
replicas of the stateless services instead. Use an immutable `TAG` (e.g.
`sha-abc1234`) when rolling out new code, because pods use
`imagePullPolicy: IfNotPresent`.

`make undeploy` deletes the namespace.

## Fault-tolerance demo

Each service runs **one replica**, spread so every Pi hosts at least one pod.
Pulling any Pi therefore takes something down, and you can watch Kubernetes
recreate it on another Pi.

| Mechanism | Setting |
|---|---|
| Fast node failure detection | `node-monitor-grace-period=20s` (server), `node-status-update-frequency=5s` (agents) |
| Fast eviction | `tolerationSeconds: 10` for `not-ready` / `unreachable` (default is 300) |
| Spread | `topologySpreadConstraints` on hostname over label `app.kubernetes.io/part-of: quizbox` |
| Floating entry point | MetalLB L2 VIP `192.168.0.240` moves to a live Pi |
| Readiness | `/healthz` on every service, checked every 3 s |
| Fast restart | images pre-pulled on every Pi |
| Clear failure | gateway returns `503 {"recovering":"<svc>"}`; the UI shows a banner and retries |
| WebSocket recovery | 5 s app-level heartbeat, auto-reconnect, resume token; game state lives in Redis |

Expected timeline after unplugging a Pi: node `NotReady` after ~20 s, pods
evicted ~10 s later, replacements Ready ~15–25 s after that.

**Demo script**

1. `kubectl get nodes -o wide` and `make where`.
2. Host a game with the demo account; classmates join from their phones.
3. Open the **trace** drawer to show ≥ 2 app hops per request.
4. The instructor picks a Pi. Read out what `make where` shows on it.
5. Unplug it and run `make watch`. Requests to the affected service fail with
   the "recovering" banner until its replacement is Running on another Pi.
6. Repeat the requests (reload page, quiz list, answer a question,
   leaderboard). All succeed.

Rehearse without unplugging: `make kill-pod SVC=quiz` deletes a pod and times
its replacement.

What happens per service while it is down:

| Down | Effect until replaced |
|---|---|
| gateway | everything (single entry point) |
| account | login and API calls fail; running games continue |
| quiz | editing and new games fail; running games continue |
| game | live games pause and reconnect; home page drops the hall of fame |
| frontend | page loads fail; open tabs keep working |
| redis | live games pause, then restore from Postgres; logins fall back to Postgres |

Pods on the unplugged Pi stay in `Terminating` until it comes back. This is
expected: Kubernetes cannot confirm they stopped, but it still creates the
replacements.

## Local development and tests

Requirements: Go 1.24, Node 22, Docker.

```bash
make dev          # Postgres + Redis in Docker, all five services natively
                  # → http://localhost:8080 (gateway), stop with make dev-stop
make test         # unit tests for all services
make e2e BASE_URL=http://localhost:8080
make web          # rebuild the SPA after changing frontend/web
```

Or the full stack in containers: `docker compose -f docker-compose.dev.yml up --build`.

Frontend hot reload: `cd frontend/web && npm install && npm run dev` (proxies
`/api` and `/ws` to the gateway on :8080).

### Tests

| Suite | Covers |
|---|---|
| `internal/common` | trace merging, hop counting, client retry |
| `services/gateway` | routing, forward-auth, header stripping, merged trace and hop count, 503 on dead upstream, WebSocket passthrough |
| `services/account` | password hashing, register/login/authz/logout, Redis-down fallback to Postgres |
| `services/quiz` | validation, owner isolation, CRUD, internal fetch, seeding |
| `services/game` | full game over WebSocket, scoring, duplicate and late answers, no answer leak before reveal, resume, Redis loss and restore, two replicas fanning out, concurrent host actions |
| `services/frontend` | boot data injection and escaping, degraded mode, trace |
| `test/e2e` | full stack over HTTP and WebSocket; asserts `X-App-Hops ≥ 2` on every response |

Unit tests that touch Postgres expect one at
`postgres://quizbox:quizbox@localhost:5432/quizbox` with `db/init.sql`
applied (`make dev` provides it).

## Troubleshooting

| Symptom | Fix |
|---|---|
| Pods `ImagePullBackOff` | GHCR packages are private; make them public. Check `REGISTRY` is lowercase. |
| Replacement takes ~5 min | Tolerations missing or overridden; check `kubectl get pod -o yaml`. |
| Node flaps `NotReady` | WiFi or a weak power supply. Use wired Ethernet; raise grace period to 30 s in `cluster/server.sh`. |
| `account`/`quiz`/`game` not Ready | Postgres unreachable: check `DB_HOST`, VM firewall, `kubectl -n quizbox get endpointslice postgres`. |
| No external IP on gateway | `make metallb`; the pool range must be outside the router's DHCP range. |
| Phones can't connect | They must be on the router's WiFi, not mobile data. |
| k3s agent fails to start on a Pi | Memory cgroups not enabled: run `cluster/pi-prep.sh` and reboot. |

## After the demo

Reflash every Pi with Raspberry Pi OS and factory-reset the router, per the
course instructions.
