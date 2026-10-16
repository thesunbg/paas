#!/bin/sh
# Tenant network isolation check: run as root on a worker node.
# Checks from inside container A that the host, metadata service, mesh and
# another project are blocked while DNS, Internet and own domain work.
#   ./scripts/tenant-isolation.sh <container-id-A> <container-id-B-of-another-project> <port-of-B>
# Uses the host's curl inside the container's network namespace (nsenter -n).
set -u
A=$1; B=$2; BPORT=$3
pid() { ctr -n paas task ls | awk -v id="$1" '$1==id {print $2}'; }
PA=$(pid "$A"); PB=$(pid "$B")
[ -n "$PA" ] && [ -n "$PB" ] || { echo "container not running"; exit 1; }
ipof() { nsenter -t "$1" -n ip -o -4 addr show eth0 | awk '{print $4}' | cut -d/ -f1; }
IPA=$(ipof "$PA"); IPB=$(ipof "$PB")
GW=$(nsenter -t "$PA" -n ip route | awk '/default/ {print $3}')
HOST=$(ip -o -4 route get 1.1.1.1 | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}')
MESH=$(ip -o -4 addr show paaswg0 2>/dev/null | awk '{print $4}' | cut -d/ -f1)
fail=0
in_a() { nsenter -t "$PA" -n "$@"; }
must_fail() { if in_a curl -s -o /dev/null -m3 "$2"; then echo "LEAK  $1 ($2)"; fail=1; else echo "ok    $1 blocked"; fi; }
# nsenter -n keeps the host's resolv.conf (127.0.0.53 does not exist in the
# container's netns): resolve through the container's DNS (the gateway) and pin it.
resolve() { in_a nslookup "$1" "$GW" | awk '/^Address: /{print $2}' | grep -v : | head -1; }
must_pass() { h=$(echo "$2" | sed -E 's|https?://([^/:]+).*|\1|'); ip=$(resolve "$h"); [ -n "$ip" ] || ip=$h
  if in_a curl -sk -o /dev/null -m5 --resolve "$h:443:$ip" --resolve "$h:80:$ip" "$2"; then echo "ok    $1 reachable"; else echo "BROKEN $1 ($2)"; fail=1; fi; }
echo "A=$IPA B=$IPB gw=$GW host=$HOST mesh=${MESH:-none}"
must_fail metadata        http://169.254.169.254/
must_fail host:8080       http://$HOST:8080/
must_fail gw:8080         http://$GW:8080/
must_fail host:4222       telnet://$HOST:4222
must_fail host:7070       http://$HOST:7070/
[ -n "$MESH" ] && must_fail mesh:8080 http://$MESH:8080/
must_fail projectB        http://$IPB:$BPORT/
must_pass internet        https://example.com/
DASH=$(sed -n 's/^PAAS_DASHBOARD_DOMAIN=//p' /etc/paas/paasd.env)
[ -n "$DASH" ] && must_pass own-domain:443 "https://$DASH/"
if in_a sh -c "getent hosts example.com >/dev/null || nslookup example.com $GW >/dev/null"; then echo "ok    dns"; else echo "BROKEN dns"; fail=1; fi
nft list chain inet paas input >/dev/null && echo "ok    input chain present" || { echo "BROKEN no input chain"; fail=1; }
journalctl -u buildkit --since "$(systemctl show buildkit -p ActiveEnterTimestamp --value)" --no-pager | grep -q "using host network" && { echo "LEAK  buildkit still on host network"; fail=1; } || echo "ok    buildkit not on host network"
exit $fail
