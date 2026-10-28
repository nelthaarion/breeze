# perf/ - benchmark harness

Reproduces the measurements in `PERFORMANCE.md`. It is a separate Go module (`go.mod` here), so `go build ./...` at the repository root does not touch it.

Needs Linux (WSL is fine), Go 1.27.3, Python 3 and [wrk](https://github.com/wg/wrk) on PATH.

    ./run_ab.sh /path/to/original/checkout /path/to/this/checkout plain json rest api rest

`run_ab.sh` builds the benchmark server once against each tree and then alternates original / new / new + `PollYieldAuto` runs (5 x 5 s per scenario), reporting medians, the min..max spread, and the server's own CPU microseconds per request read from `/proc`.
CPU per request is the more reliable figure when `wrk` and the server share cores.

| file | what it is |
|---|---|
| `server/` | the benchmark app: scenarios `plain json api rest`, and a `-mw` list (`recover cors secure ratelimit compress jwt etag log`, plus `logbuf jwtcache compressmin` on the optimized tree) |
| `ab.py` | interleaved N-way driver. `--cfg "label\|binary\|server args\|K=V,K=V env\|mw list"` (repeatable, first is the baseline). Also does a status-200 sanity check on every path before measuring, so a run can never silently measure 404s or 429s. `--profdir DIR` writes CPU/alloc pprof profiles |
| `wirecheck.py` | differential wire test: same raw requests to two binaries, compares status, headers (order-insensitive: the server randomises header order per response) and bodies |
| `bsum.py` | median-of-runs summary for `go test -bench` output; with two files prints A vs B |
| `results/` | the raw JSON / benchmark output from the runs reported in `PERFORMANCE.md` |
| `breeze-performance.patch` | the change as a unified diff against the zip you uploaded (line endings normalised; for reading, the zip has the final files) |

Examples

    # the opt-in poll-yield switch on its own, on your hardware
    python3 ab.py --cfg "yield kept|.build/bin_new" --cfg "yield skipped|.build/bin_new|-pollyield=auto" --scenario json --reps 8 --dur 5

    # router scaling, no network involved
    go test -run '^$' -bench RouterLookupScaling -benchmem -count 5 .       # at the repository root
