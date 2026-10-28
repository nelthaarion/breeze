#!/usr/bin/env python3
"""Wire-level differential test: same raw requests to the original and the new server, compare status, headers and body."""
import gzip, json, os, socket, subprocess, sys, tempfile, time, zlib

def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0)); return s.getsockname()[1]

def start(binary, extra, scenario, mw):
    port = free_port(); tok = tempfile.mktemp()
    cmd = [binary, "-port", str(port), "-scenario", scenario, "-mw", mw, "-tokenout", tok] + extra.split()
    p = subprocess.Popen(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=dict(os.environ, GOMAXPROCS="2"))
    for _ in range(200):
        try: socket.create_connection(("127.0.0.1", port), 0.2).close(); break
        except OSError: time.sleep(0.05)
    time.sleep(0.2)
    t = open(tok).read().strip() if os.path.exists(tok) else None
    return p, port, t

def request(port, method, path, headers=(), body=b""):
    h = [f"{method} {path} HTTP/1.1", "Host: x"] + list(headers)
    if body: h.append(f"Content-Length: {len(body)}")
    raw = ("\r\n".join(h) + "\r\n\r\n").encode() + body
    s = socket.create_connection(("127.0.0.1", port), 5); s.settimeout(3); s.sendall(raw)
    buf = b""
    while b"\r\n\r\n" not in buf:
        d = s.recv(65536)
        if not d: break
        buf += d
    head, _, rest = buf.partition(b"\r\n\r\n")
    lines = head.decode("latin1").split("\r\n"); status = lines[0]
    hdrs = [tuple(x.split(":", 1)) for x in lines[1:]]
    hd = {k.strip().lower(): v.strip() for k, v in hdrs}
    code = int(status.split()[1])
    n = 0 if (method == "HEAD" or code in (204, 304)) else int(hd.get("content-length", 0))
    while len(rest) < n:
        d = s.recv(65536)
        if not d: break
        rest += d
    s.close()
    return status, [(k.strip(), v.strip()) for k, v in hdrs], rest[:n]

def normalize(status, hdrs, body):
    ce = dict((k.lower(), v) for k, v in hdrs).get("content-encoding", "")
    if ce == "gzip": body = gzip.decompress(body)
    elif ce == "deflate": body = zlib.decompress(body)
    ct = dict((k.lower(), v) for k, v in hdrs).get("content-type", "")
    if "json" in ct:
        try: body = json.loads(body)
        except Exception: pass
    return status, sorted(hdrs), body

REST = [("GET", p, ()) for p in ["/api/v1/users/17", "/api/v1/orgs/9/sub", "/api/v1/teams/3/sub/4", "/api/v1/gists/5", "/api/v1/commits/8/sub/2",
        "/api/v1/billing/1", "/api/v1/audit/6/sub/1", "/health", "/version", "/metrics", "/api/v1/ping", "/api/v1/stars/12/sub", "/api/v1/users",
        "/api/v1/users/", "/api/v1/users//x", "//api/v1/users", "/api/v1/nope", "/nope", "/", "/api/v1/users/1/2/3", "/API/v1/users", "/api/v1/users/17?x=1&y=2",
        "/api/v1/users/%41", "/api/v1/users/a%2Fb", "/health?", "/api/v1"]] + [
    ("POST", "/api/v1/users", ()), ("PUT", "/api/v1/users/5", ()), ("DELETE", "/api/v1/users/5", ()), ("PATCH", "/api/v1/users/5", ()),
    ("HEAD", "/health", ()), ("HEAD", "/api/v1/users/3", ()), ("OPTIONS", "/api/v1/users", ("Origin: https://a.example", "Access-Control-Request-Method: POST", "Access-Control-Request-Headers: Content-Type")),
    ("GET", "/api/v1/users/17", ("Origin: https://a.example",)), ("GET", "/health", ("Origin: https://evil.example", "Cookie: a=b"))]

AE = ["", "gzip", "deflate", "br", "gzip, deflate, br", "gzip;q=0, br;q=0", "br;q=0.5, gzip", "*;q=0", "*", "identity", "GZIP", "gzip;q=abc", "br;q=0, gzip;q=0.2, deflate;q=0.9", "gzip , deflate"]
API = [("GET", "/users", (f"Accept-Encoding: {a}",) if a else ()) for a in AE] + [("GET", "/users/42?x=1", ()), ("GET", "/users/7", ("Accept-Encoding: gzip",)), ("HEAD", "/users", ("Accept-Encoding: gzip",))]

def run(binary, extra, scenario, mw, reqs, flow=None):
    p, port, tok = start(binary, extra, scenario, mw)
    try:
        out = []
        for m, path, hs in reqs:
            hs = list(hs) + ([f"Authorization: Bearer {tok}"] if tok and "jwt" in mw and not any(h.startswith("Authorization") for h in hs) else [])
            hs = [h.replace("@TOK@", tok or "") for h in hs]
            try: out.append(normalize(*request(port, m, path, hs)))
            except Exception as e: out.append(("ERR", [], repr(e)))
        if flow:
            st, hd, _ = request(port, "GET", "/users")
            etag = dict((k.lower(), v) for k, v in hd).get("etag", "")
            for hs in [(f"If-None-Match: {etag}",), ("If-None-Match: \"nope\"",), (f"If-None-Match: {etag}", "Accept-Encoding: gzip")]:
                out.append(normalize(*request(port, "GET", "/users", list(hs))))
        return out
    finally:
        p.terminate(); p.wait(5)

JWT = [("GET", "/users/42", ()), ("GET", "/users/42", ("Authorization: Bearer garbage.token.here",)), ("GET", "/users/42", ("Authorization: Bearer ",)),
       ("GET", "/users/42", ("Authorization: Basic abc",)), ("GET", "/users/42", ("Authorization: Bearer @TOK@x",)), ("GET", "/users/42", ("Authorization: bearer @TOK@",)),
       ("GET", "/users/42", ("Authorization: Bearer @TOK@",)), ("GET", "/users/42", ("Authorization: Bearer @TOK@",)), ("GET", "/users", ())]

if __name__ == "__main__":
    orig, new = sys.argv[1], sys.argv[2]
    groups = [("rest routing + cors + secure", "rest", "recover,cors,secure,ratelimit", REST, False),
              ("api + cors + secure + etag + compress (Accept-Encoding matrix, 304 flow)", "api", "recover,cors,secure,etag,compress", API, True),
              ("api + jwt", "api", "jwt", JWT, False)]
    variants = [("orig again (control)", orig, ""), ("new (default)", new, ""), ("new + PollYieldAuto", new, "-pollyield=auto")]
    bad = 0; total = 0
    for gname, sc, mw, reqs, flow in groups:
        base = run(orig, "", sc, mw, reqs, flow)
        for vname, vbin, extra in variants:
            got = run(vbin, extra, sc, mw, reqs, flow)
            diffs = 0
            for i, (a, b) in enumerate(zip(base, got)):
                total += 1
                if a != b:
                    diffs += 1; bad += 1
                    print(f"  DIFF [{gname}] [{vname}] request #{i}: {reqs[i] if i < len(reqs) else 'flow'}\n     orig: {a[0]} {str(a[2])[:90]}\n     new : {b[0]} {str(b[2])[:90]}")
                    if a[1] != b[1]: print("     headers differ:", [h for h in a[1] if h not in b[1]], "vs", [h for h in b[1] if h not in a[1]])
                    elif a[2] != b[2]: print("     BODY differs")
            print(f"{gname:78s} {vname:20s} {len(base) - diffs}/{len(base)} identical")
    print(f"TOTAL comparisons: {total}, differences: {bad}")
    sys.exit(1 if bad else 0)
