#!/bin/bash
# Runs cmd/bench against one module on the stand of CiNim docs/store.md ("Speed"): two Caddy nodes on /dev/shm, one rqlite, the same 8 MiB / 64 KiB / 1 GiB load.
# usage: tools/bench/stand.sh nim|go-dav|go-local [GOGC]   (BENCHFLAGS=-nokeepalive for nim; BENCHFLAGS="-seal <key>" for a sealed go-local)
# needs: a rqlite at $RQ (default http://127.0.0.1:24790), build/caddy-store, and build/storage (nim) or build/blobgate (go); builds: go build -o build/blobgate ./cmd/blobgate; go build -o build/bench ./cmd/bench
set -u
KIND=$1; GC=${2:-}
CADDY=${CADDY:-build/caddy-store}            # the node of the Nim stand (CiNim: tools/store/build-caddy.sh)
NIMBIN=${NIMBIN:-build/storage}               # the Nim module (CiNim: nim c -d:ssl -p:src -o:build/storage src/storage/main.nim)
GOBIN=${GOBIN:-build/blobgate}
MP=$((24810 + RANDOM % 80)); P1=$((24900 + RANDOM % 40)); P2=$((24950 + RANDOM % 40))
RQ=${RQ:-http://127.0.0.1:24790}
SECRET=the-secret-of-shard-001-go-test; ADMIN=an-admin-token-for-the-go-test
B=/dev/shm/standbench; rm -rf $B; mkdir -p $B/n1 $B/n2 $B/xdg $B/xdgc
curl -s -XPOST localhost:24790/db/execute -H 'Content-Type: application/json' -d '["DROP TABLE IF EXISTS storage_stores","DROP TABLE IF EXISTS storage_objects","DROP TABLE IF EXISTS storage_repair","DROP TABLE IF EXISTS storage_parts","DROP TABLE IF EXISTS storage_uploads","DROP TABLE IF EXISTS storage_lease"]' >/dev/null
pids=()
cleanup() { for p in "${pids[@]}"; do kill -TERM $p 2>/dev/null; done; sleep 1; rm -rf $B; }
trap cleanup EXIT
caddy_node() { # port root withauth
  local f=$B/Caddyfile-$1
  if [ "$3" = auth ]; then
    printf '{\n\torder webdav before file_server\n\tauto_https off\n\tadmin off\n}\n:%s {\n\tforward_auth 127.0.0.1:%s {\n\t\turi /v1/internal/node-auth?node=127.0.0.1:%s\n\t}\n\twebdav {\n\t\troot %s\n\t\tatomic_put\n\t\tcreate_dirs\n\t}\n}\n' $1 $MP $1 $2 > $f
  else
    printf '{\n\torder webdav before file_server\n\tauto_https off\n\tadmin off\n}\n:%s {\n\twebdav {\n\t\troot %s\n\t\tatomic_put\n\t\tcreate_dirs\n\t}\n}\n' $1 $2 > $f
  fi
  XDG_DATA_HOME=$B/xdg XDG_CONFIG_HOME=$B/xdgc $CADDY run --config $f --adapter caddyfile > $B/caddy-$1.log 2>&1 &
  pids+=($!)
}
ENVV="BLOBGATE_RQLITE_URL=$RQ BLOBGATE_PORT=$MP BLOBGATE_SHIM_PORT=0 BLOBGATE_TENANTS=001=$SECRET BLOBGATE_ADMIN_TOKEN=$ADMIN BLOBGATE_SEAL_KEY=a-seal-key-for-the-bench"
case $KIND in
 nim)
   caddy_node $P1 $B/n1 auth; caddy_node $P2 $B/n2 auth
   bash -c "cd ${CINIM_DIR:-.}; exec env $ENVV $NIMBIN" > $B/mod.log 2>&1 & echo $! > $B/mod.pid ;;
 go-dav)
   caddy_node $P1 $B/n1 plain; caddy_node $P2 $B/n2 plain
   bash -c "exec env $ENVV ${GC:+GOGC=$GC} $GOBIN" > $B/mod.log 2>&1 & echo $! > $B/mod.pid ;;
 go-local)
   bash -c "exec env $ENVV ${GC:+GOGC=$GC} $GOBIN" > $B/mod.log 2>&1 & echo $! > $B/mod.pid ;;
esac
for i in $(seq 1 60); do curl -s -o /dev/null localhost:$MP/healthz && break; sleep 0.5; done; sleep 1; MPID=$(cat $B/mod.pid); pids+=($MPID); echo "module pid $MPID: $(cat /proc/$MPID/comm 2>&1) $(grep VmHWM /proc/$MPID/status 2>&1)"
if [ $KIND = nim ]; then
  curl -s -o /dev/null -w "register nim store: %{http_code}\n" -XPUT localhost:$MP/v1/admin/stores/bench -H "Authorization: Bearer $ADMIN" -d "{\"kind\":\"dav\",\"nodes\":[\"http://127.0.0.1:$P1\",\"http://127.0.0.1:$P2\"],\"token\":\"the-token-of-the-two-nodes\",\"default\":true}"
  NODES=""
elif [ $KIND = go-dav ]; then
  curl -s -o /dev/null -w "register go store: %{http_code}\n" -XPUT localhost:$MP/v1/admin/stores/bench -H "Authorization: Bearer $ADMIN" -d "{\"kind\":\"mirror\",\"default\":true,\"nodes\":[{\"remote\":\":webdav,url='http://127.0.0.1:$P1',vendor=other:\"},{\"remote\":\":webdav,url='http://127.0.0.1:$P2',vendor=other:\"}]}"
  NODES=""
else
  NODES="-nodes $B/n1,$B/n2"
fi
${BENCH:-build/bench} -url http://127.0.0.1:$MP -secret $SECRET -admin $ADMIN $NODES -pid $MPID -gib 1 ${BENCHFLAGS:-}
