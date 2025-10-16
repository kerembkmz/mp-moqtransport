#!/usr/bin/env bash
set -euo pipefail

PUB=pub; SUB=sub
WIFI_LAT=40ms; WIFI_LOSS=0.5%; WIFI_BW=""
LTE_LAT=250ms;  LTE_LOSS=3%;   LTE_BW=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --wifi-lat)  WIFI_LAT="$2"; shift 2;;
    --wifi-loss) WIFI_LOSS="$2"; shift 2;;
    --wifi-bw)   WIFI_BW="$2";  shift 2;;
    --lte-lat)   LTE_LAT="$2";  shift 2;;
    --lte-loss)  LTE_LOSS="$2"; shift 2;;
    --lte-bw)    LTE_BW="$2";   shift 2;;
    --clear)
      docker compose exec -T "$PUB" sh -c 'tc qdisc del dev eth0 root 2>/dev/null || true'
      docker compose exec -T "$SUB" sh -c 'tc qdisc del dev eth0 root 2>/dev/null || true'
      echo "cleared"; exit 0;;
    *) echo "unknown arg: $1"; exit 2;;
  esac
done

apply_pub() {
  docker compose exec -T "$PUB" sh -s <<SH
set -e
tc qdisc replace dev eth0 root handle 1: prio
tc qdisc replace dev eth0 parent 1:1 handle 10: netem delay "$WIFI_LAT" loss "$WIFI_LOSS"
tc qdisc replace dev eth0 parent 1:2 handle 20: netem delay "$LTE_LAT"  loss "$LTE_LOSS"
[ -n "$WIFI_BW" ] && tc qdisc replace dev eth0 parent 10: handle 100: tbf rate "$WIFI_BW" burst 32kbit latency 400ms || true
[ -n "$LTE_BW"  ] && tc qdisc replace dev eth0 parent 20: handle 200: tbf rate "$LTE_BW"  burst 16kbit latency 600ms || true
tc filter replace dev eth0 protocol ip parent 1:0 u32 match ip dport 8080 0xffff flowid 1:1
tc filter replace dev eth0 protocol ip parent 1:0 u32 match ip dport 8081 0xffff flowid 1:2
SH
}

apply_sub() {
  docker compose exec -T "$SUB" sh -s <<SH
set -e
tc qdisc replace dev eth0 root handle 1: prio
tc qdisc replace dev eth0 parent 1:1 handle 10: netem delay "$WIFI_LAT" loss "$WIFI_LOSS"
tc qdisc replace dev eth0 parent 1:2 handle 20: netem delay "$LTE_LAT"  loss "$LTE_LOSS"
[ -n "$WIFI_BW" ] && tc qdisc replace dev eth0 parent 10: handle 100: tbf rate "$WIFI_BW" burst 32kbit latency 400ms || true
[ -n "$LTE_BW"  ] && tc qdisc replace dev eth0 parent 20: handle 200: tbf rate "$LTE_BW"  burst 16kbit latency 600ms || true
tc filter replace dev eth0 protocol ip parent 1:0 u32 match ip sport 8080 0xffff flowid 1:1
tc filter replace dev eth0 protocol ip parent 1:0 u32 match ip sport 8081 0xffff flowid 1:2
SH
}

apply_pub
apply_sub
echo "shaping applied"
