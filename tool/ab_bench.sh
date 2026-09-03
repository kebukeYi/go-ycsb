#!/usr/bin/env bash
# A/B benchmark between two embedded go-ycsb DB adapters (default: trainkv vs badger).
#
# Both engines run with identical, explicit parameters:
#   - same workload files, recordcount / operationcount / threads
#   - own isolated data dir under /tmp/ab-<db>, wiped via dropdata=true before load
#   - engine config left at defaults (both have SyncWrites=false by default)
#
# Usage:
#   tool/ab_bench.sh [dbA] [dbB]
# Env overrides: RECORDCOUNT (default 100000) OPERATIONCOUNT (default 50000)
#                THREADS (default 16) WORKLOADS (default "a c e")
#                PROP_DIR: workload 属性文件目录 (默认 workloads)
#                EXTRA_A / EXTRA_B: extra -p props applied to dbA/dbB (word split)
set -uo pipefail
cd "$(dirname "$0")/.."

BIN=${BIN:-./bin/go-ycsb}
DB_A=${1:-trainkv}
DB_B=${2:-badger}
RECORDCOUNT=${RECORDCOUNT:-100000}
OPERATIONCOUNT=${OPERATIONCOUNT:-50000}
THREADS=${THREADS:-16}
WORKLOADS=${WORKLOADS:-"a c e"}
PROP_DIR=${PROP_DIR:-workloads}

RESULTS=$(mktemp -d)
trap 'rm -rf "$RESULTS"' EXIT

declare -A DIR_PROP=([trainkv]=trainkv.dir [badger]=badger.dir [boltdb]=bolt.path [pebble]=pebble.dir)

db_dir() { echo "/tmp/ab-$1"; }

# 路径语义: boltdb 是单文件(bolt.path), 其余为数据目录
db_path() {
    local db=$1
    if [ "$db" = boltdb ]; then
        echo "$(db_dir "$db")/data.db"
    else
        echo "$(db_dir "$db")"
    fi
}

run_phase() {
    local db=$1 phase=$2 dir extra=() props=()
    dir=$(db_dir "$db")
    local out="$RESULTS/$db.$phase.log"
    local prop_name=${DIR_PROP[$db]:-}
    if [ -n "$prop_name" ]; then
        props+=(-p "$prop_name=$(db_path "$db")")
    fi

    if [ "$db" = "$DB_A" ] && [ -n "${EXTRA_A:-}" ]; then read -r -a extra <<<"$EXTRA_A"; fi
    if [ "$db" = "$DB_B" ] && [ -n "${EXTRA_B:-}" ]; then read -r -a extra <<<"$EXTRA_B"; fi

    echo "== [$phase] $db (path=$(db_path "$db") ${extra[*]:-}) =="
    if [ "$phase" = load ]; then
        # 引擎侧 dropdata 只删库文件/目录, 父目录仍需存在 (boltdb/pebble 不自建)
        rm -rf "$dir"
        mkdir -p "$dir"
        "$BIN" load "$db" -P "$PROP_DIR/workloada" \
            -p dropdata=true -p recordcount="$RECORDCOUNT" -p threads="$THREADS" \
            "${props[@]}" "${extra[@]}" >"$out" 2>&1
    else
        "$BIN" run "$db" -P "$PROP_DIR/workload$phase" \
            -p operationcount="$OPERATIONCOUNT" -p threads="$THREADS" \
            "${props[@]}" "${extra[@]}" >"$out" 2>&1
    fi

    if ! grep -q 'Run finished' "$out"; then
        echo "!! [$phase] $db FAILED:" >&2
        tail -20 "$out" >&2
        exit 1
    fi
}

# phase interleaved across engines to reduce temporal bias
for phase in load $WORKLOADS; do
    for db in "$DB_A" "$DB_B"; do
        run_phase "$db" "$phase"
    done
done

# parse go-ycsb summary lines: "<OP> - ... Count: N, ... OPS: X ..."
parse_log() {
    awk '
        / - Takes\(s\):/ {
            op=$1; gsub(/[ \t]+/, "", op)
            c=""; o=""
            if (match($0, /Count: [0-9]+/)) c=substr($0, RSTART+7, RLENGTH-7)
            if (match($0, /OPS: [0-9.]+/))  o=substr($0, RSTART+5, RLENGTH-5)
            print op, c, o
        }' "$1"
}

declare -A CNT OPS
for phase in load $WORKLOADS; do
    for db in "$DB_A" "$DB_B"; do
        while read -r op c o; do
            [ -n "$op" ] || continue
            CNT["$phase|$db|$op"]=$c
            OPS["$phase|$db|$op"]=$o
        done < <(parse_log "$RESULTS/$db.$phase.log")
    done
done

echo
printf '%-8s %-8s %12s %12s %10s %s\n' Phase Op "$DB_A(ops)" "$DB_B(ops)" A/B '' 
printf '%s\n' '-------------------------------------------------------------------------'
for phase in load $WORKLOADS; do
    first=1
    while read -r op c o; do
        [ -n "$op" ] || continue
        a=${OPS["$phase|$DB_A|$op"]:-"-"}
        b=${OPS["$phase|$DB_B|$op"]:-"-"}
        ca=${CNT["$phase|$DB_A|$op"]:-"-"}
        cb=${CNT["$phase|$DB_B|$op"]:-"-"}
        ratio="-"
        if [ "$a" != "-" ] && [ "$b" != "-" ] && awk -v x="$b" 'BEGIN{exit(x==0)}'; then
            ratio=$(awk -v x="$a" -v y="$b" 'BEGIN{printf "%.2fx", x/y}')
        fi
        warn=""
        [ "$ca" != "-" ] && [ "$cb" != "-" ] && [ "$ca" != "$cb" ] && warn=" (counts differ: $ca vs $cb)"
        if [ "$first" = 1 ]; then
            printf '%-8s %-8s %12s %12s %10s %s\n' "$phase" "$op" "$a" "$b" "$ratio" "$warn"
            first=0
        else
            printf '%-8s %-8s %12s %12s %10s %s\n' '' "$op" "$a" "$b" "$ratio" "$warn"
        fi
    done < <(parse_log "$RESULTS/$DB_A.$phase.log")
done
