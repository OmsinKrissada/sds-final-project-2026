-- Quizbox schema. One database, one schema per service.
-- Roles and grants are created by db/roles.sh (passwords come from env).

CREATE SCHEMA IF NOT EXISTS account;
CREATE SCHEMA IF NOT EXISTS quiz;
CREATE SCHEMA IF NOT EXISTS game;

-- account ---------------------------------------------------------------
CREATE TABLE IF NOT EXISTS account.users (
  id           BIGSERIAL PRIMARY KEY,
  email        TEXT NOT NULL UNIQUE,
  display_name TEXT NOT NULL,
  pw_hash      TEXT NOT NULL,              -- pbkdf2-sha256$<iter>$<b64 salt>$<b64 key>
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS account.sessions (
  token_hash TEXT PRIMARY KEY,              -- hex sha256 of the cookie token
  user_id    BIGINT NOT NULL REFERENCES account.users(id) ON DELETE CASCADE,
  expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_user_idx ON account.sessions(user_id);

-- quiz ------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS quiz.quizzes (
  id         BIGSERIAL PRIMARY KEY,
  owner_id   BIGINT NOT NULL,               -- account.users.id (no cross-schema FK by design)
  title      TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS quizzes_owner_idx ON quiz.quizzes(owner_id);

CREATE TABLE IF NOT EXISTS quiz.questions (
  id           BIGSERIAL PRIMARY KEY,
  quiz_id      BIGINT NOT NULL REFERENCES quiz.quizzes(id) ON DELETE CASCADE,
  idx          INT NOT NULL,
  text         TEXT NOT NULL,
  choices      JSONB NOT NULL,              -- ["a","b","c","d"], 2..4 entries
  correct      SMALLINT NOT NULL,           -- index into choices
  time_limit_s INT NOT NULL DEFAULT 20,
  UNIQUE (quiz_id, idx)
);

-- game ------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS game.sessions (
  id         BIGSERIAL PRIMARY KEY,
  pin        TEXT NOT NULL,
  quiz_id    BIGINT NOT NULL,
  host_id    BIGINT NOT NULL,
  status     TEXT NOT NULL,                 -- lobby | question | reveal | finished
  current_q  INT NOT NULL DEFAULT -1,
  snapshot   JSONB NOT NULL,                -- full redis state, used to restore after redis loss
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS sessions_live_pin_idx ON game.sessions(pin) WHERE status <> 'finished';

CREATE TABLE IF NOT EXISTS game.results (
  id          BIGSERIAL PRIMARY KEY,
  session_id  BIGINT NOT NULL REFERENCES game.sessions(id) ON DELETE CASCADE,
  pin         TEXT NOT NULL,
  quiz_title  TEXT NOT NULL,
  player_id   BIGINT NULL,                  -- account user id when the player was logged in
  nick        TEXT NOT NULL,
  score       INT NOT NULL,
  rank        INT NOT NULL,
  finished_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS results_player_idx ON game.results(player_id);
