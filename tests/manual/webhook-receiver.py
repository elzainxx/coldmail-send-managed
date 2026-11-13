#!/usr/bin/env python3
"""webhook-receiver.py — a logging sink for alert_webhook_url, on the compose network.

WHY THIS IS TRACKED. Stage 10 step 7 of Docs/install-and-deploy.md#part-7--developing-and-testing-locally requires proving the
alert webhook actually FIRES, not merely that active_conditions[] lists a condition. That needs an
HTTP endpoint the Control container can resolve — `localhost` from the host is not reachable from
inside the Control, so the receiver has to live on `coldmail-send-managed_default`. Doing that with an ad-hoc
inline heredoc failed twice on Windows (Git Bash rewrote the container-side mount path, then Docker
Desktop mounted a /tmp file as an empty directory), so it lives in the repo where a bind mount from
the project directory works.

It is a TEST DRIVER, not product code, and deliberately dumb: it accepts any POST, prints the raw
body with a UTC timestamp, and answers 200. Printing the body VERBATIM is the point — the payload is
subject to the two-vocabulary rule (CLAUDE.md Rule 5 names the alert webhook explicitly as untrusted
outbound egress), so the test has to be able to read exactly what crossed that boundary and confirm
it carries only condition codes, subject ids, and timestamps.

Run it (from the repo root):

    docker run -d --name s10hook --network coldmail-send-managed_default \
      -v "$(pwd)/tests/manual:/rcv:ro" python:3.15.2-alpine@sha256:a0f2e268f2ce8f3709611a90054ec6a8ae31f6251f5896b8648b9bb99835e30a python /rcv/webhook-receiver.py

then set alert_webhook_url to http://s10hook:9099/hook and read `docker logs s10hook`.
Set PORT to override 9099.
"""
import datetime
import http.server
import os

PORT = int(os.environ.get("PORT", "9099"))


class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length).decode("utf-8", "replace")
        # Verbatim, one line per delivery, so `docker logs` is greppable per condition code.
        now = datetime.datetime.now(datetime.timezone.utc).isoformat()
        print("%s %s %s" % (now, self.path, body), flush=True)
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok")

    # Silence the default per-request stderr line so the log holds only payloads.
    def log_message(self, *args):
        pass


if __name__ == "__main__":
    print("webhook-receiver listening on :%d" % PORT, flush=True)
    http.server.HTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
