"""Time `balanceOf` over Radius RPC for a long window, to see the tail and the error rate.

measure_checks.py takes 20-call bursts, which show the median but not p99, max, or how
often the RPC rate-limits us. This sends one call per interval per network for the whole
duration. Read-only. Standard library only, so it runs on a stock Python image in a pod:

    python3 measure_rpc_window.py --duration 600 --interval 1

Two kinds of call:
- warm: a kept-alive HTTPS connection, as the filter's HTTP client has under steady traffic;
- cold: a new connection (TCP + TLS) every --cold-every ticks, as a quiet route pays after
  its idle connection closes.
"""
import argparse, http.client, json, threading, time
from urllib.parse import urlsplit

NETWORKS = {"testnet": "https://rpc.testnet.radiustech.xyz", "mainnet": "https://rpc.radiustech.xyz"}
SBC = "0x33ad9e4BD16B69B5BFdED37D8B5D9fF9aba014Fb"  # same address on both networks
HOLDER = "0x402085c248EeA27D92E8b30b2C58ed07f9E20001"
BODY = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "eth_call",
                   "params": [{"to": SBC, "data": "0x70a08231" + HOLDER[2:].lower().rjust(64, "0")}, "latest"]})
THRESHOLDS_MS = (100, 140, 200)  # 140 ≈ what is left of the filter's 150 ms deadline


class Probe:
    def __init__(self, url):
        u = urlsplit(url)
        self.host, self.path = u.hostname, u.path or "/"
        self.conn = None
        self.samples = {"warm": [], "cold": []}  # (ms, outcome)

    def call(self, kind):
        if kind == "cold" and self.conn:
            self.conn.close()
            self.conn = None
        t = time.perf_counter()
        try:
            if self.conn is None:
                self.conn = http.client.HTTPSConnection(self.host, timeout=5)
            self.conn.request("POST", self.path, BODY, {"Content-Type": "application/json"})
            r = self.conn.getresponse()
            data = r.read()
            ms = (time.perf_counter() - t) * 1000
            if r.status == 429:
                outcome = "http_429"
            elif r.status != 200:
                outcome = f"http_{r.status}"
            else:
                outcome = "ok" if "result" in json.loads(data) else "rpc_error"
        except TimeoutError:
            ms, outcome = (time.perf_counter() - t) * 1000, "timeout"
        except (OSError, http.client.HTTPException, ValueError) as e:
            ms, outcome = (time.perf_counter() - t) * 1000, type(e).__name__
        if outcome != "ok" and self.conn:
            self.conn.close()  # a failed connection is not reused
            self.conn = None
        self.samples[kind].append((ms, outcome))


def pct(sorted_ms, p):
    return sorted_ms[min(len(sorted_ms) - 1, int(len(sorted_ms) * p))]  # nearest rank


def summary(samples):
    ok = sorted(ms for ms, o in samples if o == "ok")
    errors = {}
    for _, o in samples:
        if o != "ok":
            errors[o] = errors.get(o, 0) + 1
    s = {"n": len(samples), "errors": errors}
    if ok:
        s.update({"p50": pct(ok, .50), "p90": pct(ok, .90), "p99": pct(ok, .99), "max": ok[-1]})
        for th in THRESHOLDS_MS:  # errors count as over: the filter would not have a balance
            s[f"over_{th}ms"] = (sum(ms > th for ms in ok) + len(samples) - len(ok)) / len(samples)
    return s


def fmt(name, kind, s):
    if "p50" not in s:
        return f"{name} {kind}: n={s['n']} errors={s['errors']}"
    over = " ".join(f">{th}ms={s[f'over_{th}ms']:.2%}" for th in THRESHOLDS_MS)
    return (f"{name} {kind}: n={s['n']} p50={s['p50']:.0f} p90={s['p90']:.0f} p99={s['p99']:.0f} "
            f"max={s['max']:.0f} ms  {over}  errors={s['errors'] or 0}")


def run(name, probe, args, stop):
    probe.call("cold")  # opens the warm connection; counted as a cold sample
    tick = 1
    while not stop.is_set():
        probe.call("cold" if tick % args.cold_every == 0 else "warm")
        tick += 1
        stop.wait(args.interval)


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--duration", type=float, default=600, help="seconds (default 600)")
    ap.add_argument("--interval", type=float, default=1.0, help="seconds between calls per network")
    ap.add_argument("--cold-every", type=int, default=30, help="every Nth call uses a new connection")
    ap.add_argument("--networks", default="testnet,mainnet")
    args = ap.parse_args()

    probes = {n: Probe(NETWORKS[n]) for n in args.networks.split(",")}
    stop = threading.Event()
    threads = [threading.Thread(target=run, args=(n, p, args, stop), daemon=True) for n, p in probes.items()]
    for t in threads:
        t.start()
    start = time.monotonic()
    while time.monotonic() - start < args.duration:
        time.sleep(min(60, args.duration - (time.monotonic() - start)))
        print(f"-- {time.monotonic() - start:.0f}s", flush=True)
        for n, p in probes.items():
            print("  " + fmt(n, "warm", summary(list(p.samples["warm"]))), flush=True)
    stop.set()
    for t in threads:
        t.join(timeout=10)

    print("== final")
    result = {}
    for n, p in probes.items():
        for kind in ("warm", "cold"):
            s = summary(p.samples[kind])
            result[f"{n}_{kind}"] = s
            print("  " + fmt(n, kind, s))
    print("JSON " + json.dumps(result), flush=True)


if __name__ == "__main__":
    main()
