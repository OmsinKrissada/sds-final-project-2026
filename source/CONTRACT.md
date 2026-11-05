# Quizbox service contract

Single source of truth for how the six containers talk. If code and this file
disagree, this file wins; change it deliberately.

## Global rules

- One Go module at the repo root: `github.com/quizbox/quizbox` (Go 1.24).
  Services live in `services/<name>/` as `package main`.
- Allowed deps (already in go.mod): `github.com/redis/go-redis/v9`,
  `github.com/lib/pq` (via `database/sql`), `github.com/coder/websocket`,
  `github.com/alicebob/miniredis/v2` (tests only). Stdlib otherwise.
  Passwords use stdlib `crypto/pbkdf2` (Go 1.24). Do NOT add other modules and
  do NOT edit go.mod / go.sum or run `go mod tidy`.
- Every service `main()` starts with `common.HandleNoop()` and ends with
  `common.Run("<name>", mux)`. Use `internal/common` for JSON, health, env,
  tracing and service-to-service calls.
- Every service listens on `PORT` (default 8080) and serves `GET /healthz`
  via `common.Health`.
- HTTP routing: use Go 1.22+ `http.ServeMux` patterns (`"GET /api/quizzes/{id}"`).
- Errors: JSON `{"error": "..."}` via `common.WriteError`.
- Logging: `log/slog`.

## Tracing (demo requirement)

- `common.Run` wraps every handler in `TraceMiddleware`, which sets the
  response header `X-Trace: <svc>@<node>,<hops...>`.
- Call other services only through `common.Client` (merges their X-Trace).
- After touching Redis call `common.Note(ctx, "redis")`; after touching
  Postgres call `common.Note(ctx, "postgres")`.
- Gateway sets `X-App-Hops` = `common.AppHops(trace)` (distinct services,
  excluding gateway and postgres). Every client request must reach ≥ 2.
- Browsers cannot read headers of a WebSocket 101 response, so game sends the
  trace in its first WS message (`hello`).

## Environment

| Var | Used by | Default (local dev) |
|---|---|---|
| `PORT` | all | 8080 |
| `NODE_NAME`, `POD_NAME` | all (downward API) | hostname |
| `ACCOUNT_URL` | gateway, frontend, game | http://localhost:8082 |
| `QUIZ_URL` | gateway, game | http://localhost:8083 |
| `GAME_URL` | gateway, frontend | http://localhost:8084 |
| `FRONTEND_URL` | gateway | http://localhost:8081 |
| `REDIS_ADDR` | account, game | localhost:6379 |
| `REDIS_USERNAME`, `REDIS_PASSWORD` | account, game | empty |
| `DATABASE_URL` | account, quiz, game | postgres://quizbox:quizbox@localhost:5432/quizbox?sslmode=disable |
| `COOKIE_SECURE` | account | false |

In Kubernetes all services listen on 8080 and the URLs are
`http://<svc>:8080`; Redis is `redis:6379`; Postgres is `postgres:5432`.
Local dev ports: gateway 8080, frontend 8081, account 8082, quiz 8083, game 8084.

## Auth model

- Opaque session token in cookie `qb_session` (HttpOnly, Path=/, SameSite=Lax,
  Max-Age 7 days, Secure only if `COOKIE_SECURE=true`).
- Token: 32 random bytes, base64url. Stored as hex sha256:
  Redis `sess:<hash>` HASH {userId, name} with TTL 7d, and Postgres
  `account.sessions`. Redis is a cache; on miss or Redis error, read Postgres
  and repopulate Redis (best-effort).
- Gateway strips any client-sent `X-User-Id` / `X-User-Name`, calls
  `GET {ACCOUNT_URL}/internal/authz` with the client's `Cookie` header, and on
  200 copies `X-User-Id` / `X-User-Name` from that response onto the upstream
  request.

## Gateway routes (public surface, port 8080)

| Route | Auth | Upstream |
|---|---|---|
| `/api/auth/*` | none | account |
| `/api/quizzes`, `/api/quizzes/*` | required | quiz |
| `POST /api/games` | required | game |
| `GET /api/games/{pin}` | none | game |
| `GET /api/leaderboard` | none | game |
| `GET /api/me/history` | required | game |
| `/ws/*` (WebSocket) | optional (inject user if session valid, else anonymous) | game |
| `/healthz` | none | gateway itself |
| everything else | none | frontend |

- Required auth + authz 401 → respond `401 {"error":"login required"}`.
- Upstream unreachable after one retry → `503 {"error":"service unavailable","recovering":"<svc>"}`
  (also when account is down during authz: `"recovering":"account"`).
- Gateway response headers: `X-Trace` (its own token first, then authz hops,
  then upstream hops) and `X-App-Hops`.
- `/internal/*` paths are never routed by gateway (return 404).

## account (port 8082 locally)

- `POST /api/auth/register` `{email, password, displayName}` → 201
  `{user:{id,email,displayName}}` + Set-Cookie. Validation: email contains
  "@", password ≥ 6 chars, displayName 1..40. Duplicate email → 409.
- `POST /api/auth/login` `{email, password}` → 200 `{user}` + Set-Cookie; bad
  credentials → 401.
- `POST /api/auth/logout` → 204, deletes session in Redis + PG, clears cookie.
- `GET /api/auth/me` → 200 `{user}` or 401.
- `GET /internal/authz` → 200 with headers `X-User-Id`, `X-User-Name`
  (body `{user}`) or 401.
- `GET /internal/session` → 200 `{user: {...} | null}`; 503 only if both Redis and Postgres fail (frontend SSR shows the recovering banner).
- `GET /internal/users?ids=1,2,3` → `{users:[{id,displayName}]}`.
- Password hash: `pbkdf2-sha256$<iter>$<b64 salt>$<b64 key>`, iter 60000,
  16-byte salt, 32-byte key. Constant-time compare.
- Must call `common.Note(ctx,"redis")` on every request that creates/reads/
  deletes a session so register/login/logout/authz touch account + redis.
- Optional env `SEED_DEMO=true`: on startup ensure user
  `demo@quizbox.local` / `demo1234` / "Demo Host" exists with id 1 when free (quiz seeds sample quizzes for `SEED_OWNER_ID=1`).

## quiz (port 8083 locally)

All `/api/*` routes require header `X-User-Id` (set by gateway); else 401.

Quiz JSON:
```json
{"id":1,"ownerId":1,"title":"Planets","createdAt":"...","updatedAt":"...",
 "questions":[{"text":"Largest planet?","choices":["Mars","Jupiter","Venus","Earth"],"correct":1,"timeLimit":20}]}
```
- `GET /api/quizzes` → `{quizzes:[{id,title,questionCount,updatedAt}]}` (own only, newest first).
- `POST /api/quizzes` body `{title, questions}` → 201 full quiz.
  Validation: title 1..120; 1..50 questions; text 1..300; 2..4 choices each
  1..80 chars; correct in range; timeLimit 5..120 (default 20).
- `GET /api/quizzes/{id}` → full quiz (owner only, else 404).
- `PUT /api/quizzes/{id}` → replace title + questions (owner only) → 200 full quiz.
- `DELETE /api/quizzes/{id}` → 204 (owner only).
- `GET /internal/quizzes/{id}` → full quiz, any owner (used by game).
- On first start with `SEED_DEMO=true`: if user id given by `SEED_OWNER_ID`
  (default 1) owns no quizzes, insert two sample quizzes (8 questions each).

## game (port 8084 locally)

### HTTP
- `POST /api/games` `{quizId}` (needs X-User-Id) → game fetches
  `GET {QUIZ_URL}/internal/quizzes/{id}`; 403 if `ownerId != X-User-Id`;
  creates 6-digit PIN (retry on collision, SETNX), stores state, snapshots to
  PG → 201 `{pin, hostToken}`.
- `GET /api/games/{pin}` → `{pin,status,quizTitle,playerCount}` or 404
  (restores from PG if Redis lost it).
- `GET /api/leaderboard?limit=10` → `{entries:[{userId,displayName,score}]}`
  from `lb:alltime`; names from `GET {ACCOUNT_URL}/internal/users?ids=...`
  (if account is down, use "player #id"). Rebuild `lb:alltime` from
  `game.results` if key `lb:alltime:built` is missing.
- `GET /api/me/history` (needs X-User-Id) →
  `{played:[{pin,quizTitle,score,rank,finishedAt}], hosted:[{pin,quizTitle,status,createdAt}]}`.

### Redis keys (TTL 3h on all game keys)
```
game:{pin}:meta       HASH  quizId quizTitle hostId hostToken status q deadline qStart sessionId v
game:{pin}:questions  STRING JSON array (with correct + timeLimit)
game:{pin}:players    HASH  playerId -> JSON {nick,userId|null}
game:{pin}:tokens     HASH  playerToken -> playerId
game:{pin}:q:{i}:ans  HASH  playerId -> JSON {choice,ms,points}
game:{pin}:scores     ZSET  playerId -> points
lb:alltime            ZSET  userId -> total points
lb:alltime:built      STRING "1"
channel game:events   PUBLISH "<pin>" whenever a game changes
```
`status` ∈ lobby | question | reveal | finished. `v` = HINCRBY on every change.
Scoring: correct answer → `round(1000 * (1 - elapsed/limit/2))`, wrong → 0.
Answers accepted only while status=question and now ≤ deadline + 500ms; one
per player per question (HSETNX).

### Persistence
- On every status transition, upsert `game.sessions` (status, current_q,
  snapshot = all Redis keys of the game as JSON).
- If `game:{pin}:meta` is missing but `game.sessions` has a non-finished row
  for the PIN updated in the last 3h → restore Redis from snapshot (lazy, on
  any access). Answers to the in-flight question since the last snapshot may
  be lost; acceptable.
- On finish: insert `game.results` per player (rank by score), ZINCRBY
  `lb:alltime` for players with userId, mark session finished.

### WebSocket `GET /ws/games/{pin}`
Query: `role=host&token=<hostToken>` or `role=player&nick=<nick>` or
`role=player&resume=<playerToken>`. Optional `X-User-Id` (gateway) links a
logged-in player. Invalid → close with code 4404 (no game) / 4401 (bad token)
/ 4400 (bad request).

Server → client (JSON text frames):
- `{"type":"hello","trace":"game@node,redis","hops":2,"playerId":"...","playerToken":"..."}` first message.
- `{"type":"state","v":12,"game":{...}}` — full snapshot after connect and after
  every change (broadcast via `game:events` to all replicas, each pushes to
  its local sockets). `game`:
  ```json
  {"pin":"482913","status":"question","quizTitle":"Planets","qIndex":2,"qCount":8,
   "question":{"text":"...","choices":["..."],"timeLimit":20,"deadline":1731..., "serverNow":1731...},
   "answered":5,"playerCount":7,
   "players":[{"id":"p1","nick":"ann"}],
   "reveal":{"correct":1,"counts":[0,5,1,1]},
   "scoreboard":[{"id":"p1","nick":"ann","score":1840}],
   "you":{"id":"p1","nick":"ann","score":1840,"answered":true,"lastPoints":920,"lastCorrect":true}}
  ```
  `question.correct` is never sent before reveal. `reveal` only when
  status=reveal|finished. `scoreboard` top 10 (all at finished). `you` only for players.
- `{"type":"pong"}` reply to ping; `{"type":"error","error":"..."}`.

Client → server:
- any: `{"type":"ping"}`
- host: `{"type":"start"}` (lobby→question 0), `{"type":"reveal"}`
  (question→reveal), `{"type":"next"}` (reveal→next question or finished),
  `{"type":"end"}` (→finished).
- player: `{"type":"answer","q":2,"choice":1}`.

Server also sends WS ping frames every 10s and drops sockets that miss 2.
On SIGTERM: close all sockets with code 4503 "reconnect" (clients reconnect).

## frontend (port 8081 locally)

Go server in `services/frontend` embedding `services/frontend/dist/index.html`
(single-file Vite build from `frontend/web`). For every non-API GET it renders
index.html replacing `<!--BOOT-->` with
`<script>window.__BOOT__={...}</script>`:
- always: `session` from `GET {ACCOUNT_URL}/internal/session` (forward Cookie)
- path `/`: `hallOfFame` from `GET {GAME_URL}/api/leaderboard?limit=5`
- path `/play` with `?pin=`: `game` from `GET {GAME_URL}/api/games/{pin}`
- failures degrade to null fields plus `degraded:["game"]`; if account is
  down, return 503 page with `recovering":"account"` banner (still render HTML).
No other static files exist; icon is `data:,`.

## Ownership of state
| Data | Owner | Store |
|---|---|---|
| users, sessions | account | PG `account`, Redis `sess:*` |
| quizzes, questions | quiz | PG `quiz` |
| live games, leaderboards, results | game | Redis `game:*`, `lb:*`, PG `game` |
