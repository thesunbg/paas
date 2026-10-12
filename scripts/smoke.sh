#!/bin/sh
# End-to-end smoke test against a running installation.
#
#   PAAS_SERVER=https://paas.example.com PAAS_EMAIL=admin@example.com PAAS_PASSWORD=... \
#   EDGE=https://  ./scripts/smoke.sh
#
# Creates a throw-away project, deploys the public example repo (or REPO=...),
# checks the app through the edge proxy, provisions a shared Postgres,
# runs a backup, and deletes everything again.
set -eu
API="${PAAS_SERVER:-http://127.0.0.1:8080}/api/v1"
REPO="${REPO:-https://github.com/thesunbg/paas}"
ROOT="${ROOT-examples/go-http}"
EDGE_HOST="${EDGE_HOST:-127.0.0.1}"
j() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)"; }
req() { m=$1; p=$2; shift 2; curl -fsS -X "$m" "$API$p" -H "Authorization: Bearer $TOKEN" -H 'content-type: application/json' "$@"; }
step() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }

TOKEN=$(curl -fsS -X POST "$API/auth/login" -H 'content-type: application/json' \
  -d "{\"email\":\"$PAAS_EMAIL\",\"password\":\"$PAAS_PASSWORD\"}" | j 'd["token"]')
TEAM=$(req GET /me | j 'd["teams"][0]["id"]')
step "create project"
PROJ=$(req POST /teams/$TEAM/projects -d '{"name":"smoke test"}' | j 'd["id"]')
trap 'step "cleanup"; req DELETE /projects/$PROJ >/dev/null || true' EXIT

step "deploy $REPO ($ROOT)"
OUT=$(req POST /projects/$PROJ/services -d "{\"name\":\"smoke\",\"source\":{\"repo\":\"$REPO\",\"branch\":\"main\",\"root_dir\":\"$ROOT\"},\"size\":\"nano\"}")
SVC=$(echo "$OUT" | j 'd["service"]["id"]'); DEP=$(echo "$OUT" | j 'd["deployment"]["id"]')
for i in $(seq 1 150); do
  ST=$(req GET /deployments/$DEP | j 'd["deployment"]["status"]')
  case $ST in active) break;; failed|cancelled) req GET /deployments/$DEP/logs | j '"\n".join(l["line"] for l in d[-30:])'; exit 1;; esac
  sleep 2
done
HOST=$(req GET /services/$SVC | j 'd["domains"][0]["hostname"]')
step "GET http://$HOST"
curl -fsS -H "Host: $HOST" "http://$EDGE_HOST/" | head -c 200; echo

step "shared postgres"
DB=$(req POST /projects/$PROJ/databases -d "{\"name\":\"smoke\",\"engine\":\"postgres\",\"link_services\":[\"$SVC\"]}" | j 'd["id"]')
for i in $(seq 1 150); do
  ST=$(req GET /databases/$DB | j 'd["database"]["status"]')
  [ "$ST" = ready ] && break; [ "$ST" = failed ] && { req GET /databases/$DB; exit 1; }; sleep 2
done
step "backup"
req POST /databases/$DB/backups | j '"%s, %d bytes" % (d["status"], d["size_bytes"])'
step "all good"
