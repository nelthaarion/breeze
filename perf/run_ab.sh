#!/usr/bin/env bash
# Interleaved A/B load test: original checkout vs this one.
#   ./run_ab.sh ORIGINAL_CHECKOUT OPTIMIZED_CHECKOUT [scenario ...]
# Scenarios: plain json jsonstruct api apiparam rest reststatic (default: plain json rest)
# Needs Linux (WSL is fine), Go 1.27.3, Python 3 and wrk (https://github.com/wg/wrk) on PATH.
set -euo pipefail
ORIG=$(cd "${1:?path to the original checkout}" && pwd)
NEW=$(cd "${2:?path to the optimized checkout}" && pwd)
shift 2
HERE=$(cd "$(dirname "$0")" && pwd)
SCEN=${*:-plain json rest}

build() { # name checkout build-tags
  local d="$HERE/.build/$1"
  rm -rf "$d"; mkdir -p "$d"; cp "$HERE"/server/*.go "$d/"
  ( cd "$d" \
    && go mod init rpsbench >/dev/null 2>&1 \
    && go mod edit -require=github.com/nelthaarion/breeze/v2@v2.0.0 -replace=github.com/nelthaarion/breeze/v2="$2" \
    && go mod tidy >/dev/null \
    && go build ${3:+-tags "$3"} -o "$HERE/.build/bin_$1" . )
}
build orig "$ORIG" ""
build new "$NEW" newapi

for sc in $SCEN; do
  echo "##### $sc"
  python3 "$HERE/ab.py" \
    --cfg "orig|$HERE/.build/bin_orig" \
    --cfg "new (default)|$HERE/.build/bin_new" \
    --cfg "new + PollYieldAuto|$HERE/.build/bin_new|-pollyield=auto" \
    --scenario "$sc" --reps 5 --dur 5
done
echo
echo "Wire-level equivalence check:  python3 $HERE/wirecheck.py $HERE/.build/bin_orig $HERE/.build/bin_new"
