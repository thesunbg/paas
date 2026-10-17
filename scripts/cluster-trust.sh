#!/bin/sh
# Cluster trust check (plan 261017-cluster-trust, phase 6): manual, two VMs.
# Run from a workstation that can reach the control plane (VM1) and ssh to the node to join (VM2).
#   PAAS_SERVER=https://paas.example.com PAAS_TOKEN=<admin api token> PAAS_PROJECT=<project> \
#   CP_HOST=paas.example.com NODE_SSH=root@vm2 APP=<existing app service> ./scripts/cluster-trust.sh
# Needs: the `paas` CLI, nc, curl, ssh (key auth, root or passwordless sudo on VM2). VM2 must be a
# fresh machine that has never joined (or whose old node was deleted). Steps that need judgement
# (build, PITR database) are printed as MANUAL and do not change the exit code.
# Uses the internal node-name "ct-node2" and deletes that node at the end.
set -u
: "${PAAS_SERVER:?control plane URL, https://...}" "${PAAS_TOKEN:?admin API token}" "${CP_HOST:?control plane host (NATS on :4222)}"
: "${NODE_SSH:?ssh target of VM2, e.g. root@vm2}" "${APP:?name of an existing app service to deploy}"
export PAAS_SERVER PAAS_TOKEN
fail=0
ok() { echo "ok     $1"; }
bad() { echo "BROKEN $1"; fail=1; }
step() { echo; echo "== $1"; }
node() { ssh -o BatchMode=yes "$NODE_SSH" "$@"; }

step "NATS requires TLS"
INFO=$(printf '' | nc -w3 "$CP_HOST" 4222 | head -1)
case "$INFO" in *'"tls_required":true'*) ok "INFO has tls_required:true";; *) bad "INFO lacks tls_required:true: $INFO";; esac
# NATS sends INFO before the TLS handshake; a plain CONNECT must then get the connection closed.
REPLY=$(printf 'CONNECT {}\r\nPING\r\n' | nc -w3 "$CP_HOST" 4222 | tail -n +2)
case "$REPLY" in *PONG*) bad "plain CONNECT was answered: $REPLY";; *) ok "plain CONNECT gets no PONG";; esac

step "join node 2 with a new single-use token"
JOIN=$(paas join-command --roles worker | grep 'install.sh') || { echo "paas join-command failed"; exit 1; }
[ -n "$JOIN" ] || { echo "no join command printed"; exit 1; }
if node "$JOIN"; then ok "join command succeeded"; else bad "join command failed"; fi
sleep 5
paas nodes
NID=$(curl -fsS -H "Authorization: Bearer $PAAS_TOKEN" "$PAAS_SERVER/api/v1/nodes" | sed 's/},{/}\n{/g' | grep '"status":"online"' | grep -v '"legacy":true' | tail -1 | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
[ -n "$NID" ] && ok "node 2 online as $NID (verify in the table above that it is the new one)" || bad "no online node found"

step "workloads on the enrolled cluster"
if paas deploy "$APP" --no-follow; then ok "deploy queued"; else bad "deploy failed"; fi
if timeout 20 paas logs -f "$APP" 2>&1 | head -5 | grep -q .; then ok "logs -f streamed"; else bad "logs -f produced nothing in 20s"; fi
if paas exec "$APP" -- true; then ok "exec"; else bad "exec failed"; fi
echo "MANUAL: run one build (git deploy or paas run) and one database with PITR enabled that land on node 2 (cordon the others, or use node labels), and confirm both finish."

step "used join token is refused"
if node "$JOIN"; then bad "re-running the old join command succeeded"; else ok "old join command failed"; fi

step "delete node 2 while its agent runs (cordoned nodes may be deleted; the kick disconnects it)"
curl -fsS -X PATCH -H "Authorization: Bearer $PAAS_TOKEN" -H 'Content-Type: application/json' -d '{"status":"cordoned"}' "$PAAS_SERVER/api/v1/nodes/$NID" >/dev/null || bad "cordon failed"
SINCE=$(node date '+%Y-%m-%d %H:%M:%S')
curl -fsS -X DELETE -H "Authorization: Bearer $PAAS_TOKEN" "$PAAS_SERVER/api/v1/nodes/$NID" >/dev/null && ok "node deleted" || bad "delete failed"
# The agent keeps its old nkey and retries: every reconnect must be refused.
sleep 5
if node journalctl -u paasd --since "'$SINCE'" --no-pager | grep -qi "authorization violation"; then ok "node 2 journal has authorization violation"; else bad "no authorization violation in node 2 journal"; fi
echo
[ $fail -eq 0 ] && echo "PASS (plus MANUAL items above)" || echo "FAIL"
exit $fail
