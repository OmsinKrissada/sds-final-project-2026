#!/bin/bash
# Runs once, on first start of an empty data dir, AFTER 01-init.sql (the
# official image executes /docker-entrypoint-initdb.d/* in name order), so the
# schemas and tables already exist when the grants below run.
# Creates one LOGIN role per service with privileges on its own schema only.
# The database itself ($POSTGRES_DB, default quizbox) is created by the image.
set -euo pipefail

: "${ACCOUNT_DB_PASSWORD:?ACCOUNT_DB_PASSWORD is required}"
: "${QUIZ_DB_PASSWORD:?QUIZ_DB_PASSWORD is required}"
: "${GAME_DB_PASSWORD:?GAME_DB_PASSWORD is required}"

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
  -v account_pw="$ACCOUNT_DB_PASSWORD" \
  -v quiz_pw="$QUIZ_DB_PASSWORD" \
  -v game_pw="$GAME_DB_PASSWORD" \
  -v dbname="$POSTGRES_DB" <<'SQL'
CREATE ROLE account_svc LOGIN PASSWORD :'account_pw';
CREATE ROLE quiz_svc    LOGIN PASSWORD :'quiz_pw';
CREATE ROLE game_svc    LOGIN PASSWORD :'game_pw';

-- Nobody but the owner gets implicit access.
REVOKE ALL ON SCHEMA public FROM PUBLIC;
REVOKE ALL ON SCHEMA account, quiz, game FROM PUBLIC;
GRANT CONNECT ON DATABASE :"dbname" TO account_svc, quiz_svc, game_svc;

-- account_svc -> schema account only
GRANT USAGE ON SCHEMA account TO account_svc;
GRANT ALL ON ALL TABLES    IN SCHEMA account TO account_svc;
GRANT ALL ON ALL SEQUENCES IN SCHEMA account TO account_svc;
ALTER DEFAULT PRIVILEGES IN SCHEMA account GRANT ALL ON TABLES    TO account_svc;
ALTER DEFAULT PRIVILEGES IN SCHEMA account GRANT ALL ON SEQUENCES TO account_svc;

-- quiz_svc -> schema quiz only
GRANT USAGE ON SCHEMA quiz TO quiz_svc;
GRANT ALL ON ALL TABLES    IN SCHEMA quiz TO quiz_svc;
GRANT ALL ON ALL SEQUENCES IN SCHEMA quiz TO quiz_svc;
ALTER DEFAULT PRIVILEGES IN SCHEMA quiz GRANT ALL ON TABLES    TO quiz_svc;
ALTER DEFAULT PRIVILEGES IN SCHEMA quiz GRANT ALL ON SEQUENCES TO quiz_svc;

-- game_svc -> schema game only
GRANT USAGE ON SCHEMA game TO game_svc;
GRANT ALL ON ALL TABLES    IN SCHEMA game TO game_svc;
GRANT ALL ON ALL SEQUENCES IN SCHEMA game TO game_svc;
ALTER DEFAULT PRIVILEGES IN SCHEMA game GRANT ALL ON TABLES    TO game_svc;
ALTER DEFAULT PRIVILEGES IN SCHEMA game GRANT ALL ON SEQUENCES TO game_svc;
SQL
echo "roles.sh: account_svc, quiz_svc, game_svc created and granted"
