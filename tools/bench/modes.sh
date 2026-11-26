#!/bin/bash
# Compares a plain, a sealed and a deduplicated store over two local nodes, with every node or one needed for a write: the same loads and bytes (docs/benchmark.md).
# usage: tools/bench/modes.sh [runs]      (MODES="plain sealed dedup", ACKS="0 1", MODULE=build/blobgate choose a part, or another build)      needs: a rqlite at $RQ (default http://127.0.0.1:4001; its tables are dropped), build/blobgate and build/bench
set -u
RUNS=${1:-3}
RQ=${RQ:-http://127.0.0.1:4001}
PORT=${PORT:-18290}
[ "$PORT" -lt 32768 ] || { echo "PORT must be below 32768" >&2; exit 2; }
if curl -s -o /dev/null "localhost:$PORT"; then echo "port $PORT is in use" >&2; exit 2; fi
SECRET=the-secret-of-shard-001-go-test; ADMIN=an-admin-token-for-the-go-test
B=/dev/shm/modesbench
MPID=
stop() { if [ -n "$MPID" ]; then kill -TERM "$MPID" 2>/dev/null; wait "$MPID" 2>/dev/null; MPID=; fi; chmod -R u+w "$B" 2>/dev/null; rm -rf "$B"; }
trap stop EXIT
for mode in ${MODES:-plain sealed dedup}; do
  for minacks in ${ACKS:-0 1}; do
    for run in $(seq 1 "$RUNS"); do
      stop; mkdir -p "$B/n1" "$B/n2"
      for t in stores objects repair parts uploads lease quotas sets guard; do
        curl -s -o /dev/null -XPOST "$RQ/db/execute" -H 'Content-Type: application/json' -d "[\"DROP TABLE IF EXISTS storage_$t\"]"
      done
      BLOBGATE_RQLITE_URL=$RQ BLOBGATE_PORT=$PORT BLOBGATE_SHIM_PORT=0 BLOBGATE_TENANTS="001=$SECRET" BLOBGATE_ADMIN_TOKEN=$ADMIN BLOBGATE_SEAL_KEY=a-seal-key-for-the-bench \
        BLOBGATE_REPAIR_SECONDS=1 BLOBGATE_REPAIR_RETRY_SECONDS=1 ${MODULE:-build/blobgate} > "$B/module.log" 2>&1 &
      MPID=$!
      for i in $(seq 1 60); do curl -s -o /dev/null "localhost:$PORT/healthz" && break; sleep 0.5; done
      flags=(-minacks "$minacks" -distinct)
      [ $mode = sealed ] && flags+=(-seal a-key-for-the-blocks-of-the-bench)
      [ $mode = dedup ] && flags+=(-dedup tenant)
      [ "$minacks" = 1 ] && flags+=(-repair 32 -rqlite "$RQ")
      echo "=== mode=$mode min_acks=$minacks run=$run"
      build/bench -url "http://127.0.0.1:$PORT" -secret $SECRET -admin $ADMIN -nodes "$B/n1,$B/n2" -pid $MPID -gib 1 "${flags[@]}" || { tail -5 "$B/module.log"; exit 1; }
    done
  done
done
