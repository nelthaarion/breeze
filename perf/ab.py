#!/usr/bin/env python3
"""Interleaved A/B load test of two Breeze bench-server binaries, driven by wrk.

Same methodology as gnet's bench/scripts/ab_wrk.py: A and B runs alternate (A B A B ...)
and are compared by their medians, and the server's CPU time per request is read from
/proc (far less noisy than RPS when client and server share the only core).

Every run first sanity-checks each path (status 200) so a benchmark can never silently
measure 404s / 429s / 500s, and wrk's non-2xx and socket-error counters are enforced.
"""
import argparse, http.client, json, os, re, signal, socket, statistics, subprocess, sys, tempfile, time

WRK = os.environ.get("WRK", "wrk")
CLK = os.sysconf("SC_CLK_TCK")

SCENARIOS = {
    # name: (server -scenario, default paths)
    "plain": ("plain", ["/plaintext"]),
    "json": ("json", ["/json"]),
    "jsonstruct": ("json", ["/jsonstruct"]),
    "api": ("api", ["/users", "/users/42"]),
    "apiparam": ("api", ["/users/42"]),
    # paths spread over the whole 200-route table, incl. the last-registered static routes
    "rest": ("rest", ["/api/v1/users/17", "/api/v1/orgs/9/sub", "/api/v1/teams/3/sub/4", "/api/v1/gists/5",
                      "/api/v1/commits/8/sub/2", "/api/v1/billing/1", "/api/v1/audit/6/sub/1", "/health",
                      "/api/v1/ping", "/api/v1/stars/12/sub"]),
    "reststatic": ("rest", ["/health", "/api/v1/ping", "/api/v1/users", "/api/v1/audit"]),
}


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def cpu_seconds(pid):
    with open(f"/proc/{pid}/stat") as f:
        rest = f.read().rsplit(")", 1)[1].split()
    return (int(rest[11]) + int(rest[12])) / CLK  # utime + stime


def lua_script(paths, headers, depth):
    p = ",".join('"%s"' % x for x in paths)
    h = ",".join('["%s"]="%s"' % (k, v) for k, v in headers.items())
    return f"""
local paths = {{{p}}}
local headers = {{{h}}}
local depth = {depth}
local idx = 0
local reqs = {{}}
init = function(args)
  for i, p in ipairs(paths) do reqs[i] = wrk.format("GET", p, headers) end
end
request = function()
  local t = {{}}
  for d = 1, depth do
    idx = idx % #reqs + 1
    t[d] = reqs[idx]
  end
  return table.concat(t)
end
"""


def start_server(binary, a, extra_args, extra_env, port, profdir=None, mw=None):
    env = dict(os.environ, GOMAXPROCS=str(a.gomaxprocs))
    env.update(extra_env)
    sc = SCENARIOS[a.scenario][0]
    tokfile = tempfile.mktemp(prefix="tok")
    cmd = [binary, "-port", str(port), "-scenario", sc, "-mw", a.mw if mw is None else mw, "-tokenout", tokfile]
    if profdir:
        cmd += ["-profdir", profdir]
    cmd += a.server_args.split() + extra_args.split()
    out = subprocess.DEVNULL
    if a.stdout == "file":
        out = open(f"/tmp/server_stdout_{port}.log", "wb")
    srv = subprocess.Popen(cmd, stdout=out, stderr=subprocess.DEVNULL, env=env)
    for _ in range(200):
        try:
            socket.create_connection(("127.0.0.1", port), 0.2).close()
            break
        except OSError:
            time.sleep(0.05)
    else:
        srv.kill()
        raise RuntimeError(f"{binary} did not start")
    time.sleep(0.2)
    tok = None
    if os.path.exists(tokfile):
        tok = open(tokfile).read().strip()
        os.unlink(tokfile)
    return srv, tok


def sanity(port, paths, headers, label):
    for p in paths:
        c = http.client.HTTPConnection("127.0.0.1", port, timeout=5)
        c.request("GET", p, headers=headers)
        r = c.getresponse()
        body = r.read()
        if r.status != 200:
            raise RuntimeError(f"[{label}] sanity: GET {p} -> {r.status} {body[:80]!r}")
        c.close()


def run_wrk(port, a, paths, headers, dur):
    cmd = [WRK, f"-t{a.threads}", f"-c{a.conns}", f"-d{dur}s", "--latency"]
    if a.depth > 1 or len(paths) > 1 or headers:
        with tempfile.NamedTemporaryFile("w", suffix=".lua", delete=False) as f:
            f.write(lua_script(paths, headers, a.depth))
            script = f.name
        cmd += ["-s", script]
    else:
        script = None
    cmd.append(f"http://127.0.0.1:{port}{paths[0]}")
    out = subprocess.run(cmd, capture_output=True, text=True, timeout=dur + 40).stdout
    if script:
        os.unlink(script)
    return out


def parse(out):
    rps = float(re.search(r"Requests/sec:\s+([\d.]+)", out).group(1))
    total = int(re.search(r"(\d+) requests in", out).group(1))
    non2xx = re.search(r"Non-2xx or 3xx responses:\s+(\d+)", out)
    sockerr = re.search(r"Socket errors:(.*)", out)
    p50 = re.search(r"50%\s+([\d.]+)(us|ms|s)", out)
    p99 = re.search(r"99%\s+([\d.]+)(us|ms|s)", out)
    unit = {"us": 1.0, "ms": 1e3, "s": 1e6}
    f = lambda m: float(m.group(1)) * unit[m.group(2)] if m else float("nan")
    return dict(rps=rps, total=total, non2xx=int(non2xx.group(1)) if non2xx else 0,
                sockerr=sockerr.group(1).strip() if sockerr else "", p50=f(p50), p99=f(p99))


def run_once(label, binary, extra_args, extra_env, a, mw_override=None):
    port = free_port()
    srv, tok = start_server(binary, a, extra_args, extra_env, port, mw=(mw_override if mw_override is not None else (a.b_mw if label == "B" and a.b_mw is not None else None)))
    try:
        headers = dict(h.split(": ", 1) for h in a.header)
        if tok:
            headers["Authorization"] = "Bearer " + tok
        paths = a.paths.split(",") if a.paths else SCENARIOS[a.scenario][1]
        sanity(port, paths, headers, label)
        run_wrk(port, a, paths, headers, a.warm)  # warm-up (pools, JIT-less but caches, GC pacing)
        c0 = cpu_seconds(srv.pid)
        out = run_wrk(port, a, paths, headers, a.dur)
        c1 = cpu_seconds(srv.pid)
        r = parse(out)
        r["cpu_us"] = (c1 - c0) * 1e6 / r["total"]
        if r["non2xx"] or r["sockerr"]:
            print(f"  !! {label}: non2xx={r['non2xx']} sockerr={r['sockerr']}", file=sys.stderr)
        return r
    finally:
        srv.send_signal(signal.SIGTERM)
        try:
            srv.wait(5)
        except subprocess.TimeoutExpired:
            srv.kill()


def profile(a):
    """One server (binary A), wrk load, CPU+alloc profile written to --profdir."""
    port = free_port()
    os.makedirs(a.profdir, exist_ok=True)
    srv, tok = start_server(a.a, a, a.a_args, {}, port, profdir=a.profdir)
    try:
        headers = dict(h.split(": ", 1) for h in a.header)
        if tok:
            headers["Authorization"] = "Bearer " + tok
        paths = a.paths.split(",") if a.paths else SCENARIOS[a.scenario][1]
        sanity(port, paths, headers, "prof")
        run_wrk(port, a, paths, headers, a.warm)
        srv.send_signal(signal.SIGUSR1)
        time.sleep(0.2)
        out = run_wrk(port, a, paths, headers, a.dur)
        srv.send_signal(signal.SIGUSR2)
        time.sleep(1.0)
        print(out)
    finally:
        srv.send_signal(signal.SIGTERM)
        try:
            srv.wait(5)
        except subprocess.TimeoutExpired:
            srv.kill()


def run_multi(a):
    cfgs = []
    for c in a.cfg:
        label, binary, args, env, mw = (c.split("|") + [""] * 5)[:5]
        cfgs.append((label, binary, args, dict(x.split("=", 1) for x in env.split(",") if x), mw if mw else None))
    res = {c[0]: [] for c in cfgs}
    for i in range(a.reps):
        for label, binary, args, env, mw in cfgs:
            r = run_once(label, binary, args, env, a, mw_override=mw if mw is not None else a.mw)
            res[label].append(r)
            print(f"  run {i + 1} {label:14s}: {r['rps']:>9.0f} req/s {r['cpu_us']:6.3f} us/req p50 {r['p50']:6.0f} p99 {r['p99']:7.0f}", file=sys.stderr)
    med = lambda xs, k: statistics.median(x[k] for x in xs)
    base = cfgs[0][0]
    print(f"{'config':16s} {'req/s (median)':>15s} {'min..max':>17s} {'cpu us/req':>11s} {'p50 us':>7s} {'p99 us':>7s}  {'RPS vs base':>11s} {'CPU vs base':>11s}")
    for label, *_ in cfgs:
        rps, cpu = med(res[label], "rps"), med(res[label], "cpu_us")
        lo, hi = min(x["rps"] for x in res[label]), max(x["rps"] for x in res[label])
        print(f"{label:16s} {rps:>15,.0f} {lo:>8,.0f}..{hi:<8,.0f} {cpu:>11.3f} {med(res[label], 'p50'):>7.0f} {med(res[label], 'p99'):>7.0f}  "
              f"{100 * (rps / med(res[base], 'rps') - 1):>+10.1f}% {100 * (cpu / med(res[base], 'cpu_us') - 1):>+10.1f}%")
    if a.tag:
        os.makedirs("results", exist_ok=True)
        json.dump({k: v for k, v in res.items()} | {"_args": {k: v for k, v in vars(a).items()}}, open(f"results/{a.tag}.json", "w"), indent=1)


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--a", help="baseline server binary")
    p.add_argument("--cfg", action="append", default=[], help="N-way mode: label|binary|server args|K=V,K=V env|mw list (repeatable); first is the baseline")
    p.add_argument("--b", help="server binary under test")
    p.add_argument("--a-args", default="", help='extra server flags for A only (use "=")')
    p.add_argument("--b-args", default="", help='extra server flags for B only (use "=")')
    p.add_argument("--a-env", default="", help="K=V,K=V env for A")
    p.add_argument("--b-env", default="", help="K=V,K=V env for B")
    p.add_argument("--scenario", default="plain", choices=list(SCENARIOS))
    p.add_argument("--paths", default="", help="comma-separated override of the scenario paths")
    p.add_argument("--mw", default="", help="server -mw list")
    p.add_argument("--b-mw", default=None, help="-mw list for B only (new-API middleware names)")
    p.add_argument("--server-args", default="")
    p.add_argument("--header", action="append", default=[], help='"Name: value" (repeatable)')
    p.add_argument("--stdout", default="devnull", choices=["devnull", "file"])
    p.add_argument("--conns", type=int, default=64)
    p.add_argument("--threads", type=int, default=1)
    p.add_argument("--depth", type=int, default=1, help="pipelined requests per write")
    p.add_argument("--reps", type=int, default=5)
    p.add_argument("--dur", type=int, default=5)
    p.add_argument("--warm", type=int, default=1)
    p.add_argument("--gomaxprocs", type=int, default=2)
    p.add_argument("--profdir", default="", help="profile mode: write cpu/allocs pprof for A here")
    p.add_argument("--tag", default="", help="label for the JSON result file")
    a = p.parse_args()

    if a.profdir:
        return profile(a)
    if a.cfg:
        return run_multi(a)

    kv = lambda s: dict(x.split("=", 1) for x in s.split(",") if x)
    res = {"A": [], "B": []}
    runs = (("A", a.a, a.a_args, kv(a.a_env)), ("B", a.b, a.b_args, kv(a.b_env)))
    for i in range(a.reps):
        for name, binary, args, env in runs:
            r = run_once(name, binary, args, env, a)
            res[name].append(r)
            print(f"  run {i + 1} {name}: {r['rps']:>10.0f} req/s  {r['cpu_us']:7.3f} us srv-CPU/req  "
                  f"p50 {r['p50']:8.0f}us p99 {r['p99']:8.0f}us", file=sys.stderr)
    med = lambda xs, k: statistics.median(x[k] for x in xs)
    ra, rb, ca, cb = med(res["A"], "rps"), med(res["B"], "rps"), med(res["A"], "cpu_us"), med(res["B"], "cpu_us")
    pa, pb = med(res["A"], "p99"), med(res["B"], "p99")
    line = (f"A: {ra:,.0f} req/s {ca:.3f} us/req p99 {pa:.0f}us | B: {rb:,.0f} req/s {cb:.3f} us/req p99 {pb:.0f}us "
            f"=> RPS {100 * (rb / ra - 1):+.1f}%  CPU/req {100 * (cb / ca - 1):+.1f}%  p99 {100 * (pb / pa - 1):+.1f}%")
    print(line)
    if a.tag:
        os.makedirs("results", exist_ok=True)
        with open(f"results/{a.tag}.json", "w") as f:
            json.dump(dict(args=vars(a), A=res["A"], B=res["B"], summary=dict(
                A_rps=ra, B_rps=rb, A_cpu=ca, B_cpu=cb, A_p99=pa, B_p99=pb)), f, indent=1)


if __name__ == "__main__":
    main()
