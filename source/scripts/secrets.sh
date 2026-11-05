#!/usr/bin/env bash
# Creates/updates Secret quizbox-db (keys account, quiz, game = full DATABASE_URL).
set -euo pipefail
. "$(dirname "$0")/lib.sh"
load_env
need ACCOUNT_DB_PASSWORD QUIZ_DB_PASSWORD GAME_DB_PASSWORD
DB_NAME="${DB_NAME:-quizbox}"

urlenc() { python3 -c 'import sys,urllib.parse as u; print(u.quote(sys.argv[1], safe=""))' "$1"; }
url() { echo "postgres://$1:$(urlenc "$2")@postgres:5432/$DB_NAME?sslmode=disable"; }

kubectl create namespace quizbox --dry-run=client -o yaml | kubectl apply -f -
kubectl -n quizbox create secret generic quizbox-db \
  --from-literal=account="$(url account_svc "$ACCOUNT_DB_PASSWORD")" \
  --from-literal=quiz="$(url quiz_svc "$QUIZ_DB_PASSWORD")" \
  --from-literal=game="$(url game_svc "$GAME_DB_PASSWORD")" \
  --dry-run=client -o yaml | kubectl apply -f -
