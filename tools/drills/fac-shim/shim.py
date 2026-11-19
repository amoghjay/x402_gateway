# Drill-only proxy in front of a facilitator, so a drill can slow /settle down or make it
# hang without touching the real one. Setup and teardown are in shim.yaml.
#   GET /_mode?m=forward          pass everything through (default)
#   GET /_mode?m=delay&d=SECONDS  forward at once, then hold the reply for d seconds
#                                 (the transfer lands; the filter hears late)
#   GET /_mode?m=hang             accept and never answer, never forward (blackhole)
import json, os, sys, threading, time, urllib.error, urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

UPSTREAM = os.environ.get("UPSTREAM", "https://facilitator.testnet.radiustech.xyz")
state = {"m": "forward", "d": 0.0}
lock = threading.Lock()


def log(*a):
    print(time.strftime("%H:%M:%S"), f"{time.time() % 1:.3f}"[1:], *a, flush=True)


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def reply(self, code, body, ctype="application/json"):
        self.send_response(code)
        self.send_header("content-type", ctype)
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        u = urlparse(self.path)
        if u.path == "/_mode":
            q = parse_qs(u.query)
            with lock:
                state["m"] = q.get("m", ["forward"])[0]
                state["d"] = float(q.get("d", ["0"])[0])
                s = dict(state)
            log("mode", s)
            return self.reply(200, json.dumps(s).encode())
        self.proxy(None)

    def do_POST(self):
        n = int(self.headers.get("content-length", "0"))
        self.proxy(self.rfile.read(n))

    def proxy(self, body):
        with lock:
            m, d = state["m"], state["d"]
        log("in", self.command, self.path, "mode", m, d)
        if m == "hang":
            time.sleep(3600)
            return
        req = urllib.request.Request(UPSTREAM + self.path, data=body, method=self.command,
                                     # Cloudflare refuses Python's default User-Agent (1010).
                                     headers={"content-type": self.headers.get("content-type", "application/json"),
                                              "user-agent": self.headers.get("user-agent", "Go-http-client/1.1")})
        try:
            with urllib.request.urlopen(req, timeout=30) as r:
                code, out = r.status, r.read()
        except urllib.error.HTTPError as e:
            code, out = e.code, e.read()
        log("upstream", self.path, code, out[:160])
        if m == "delay":
            time.sleep(d)
        try:
            self.reply(code, out)
            log("replied", self.path, code)
        except Exception as e:  # the filter gave up and closed the connection
            log("reply failed", self.path, repr(e))


if __name__ == "__main__":
    ThreadingHTTPServer(("", 8080), H).serve_forever()
