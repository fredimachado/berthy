#!/usr/bin/python3
"""A reverse proxy that lets this tree's CLI talk to an agent of a release before projects.

    rewriteproxy.py LISTEN TARGET

A request for /v1/projects/<project>/environments/<environment>/<rest> is sent to TARGET as
/v1/environments/<environment>/<rest>, which is the only form such an agent serves. Everything else, such as /healthz, is
sent as it is. The status, headers and body of the answer come back unchanged. It buffers each answer, so it serves the
short requests a scenario sends and no event stream.
"""
import http.client
import re
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PROJECT_PATH = re.compile(r"^/v1/projects/[^/]+/environments/")


def split(addr):
    host, _, port = addr.rpartition(":")
    return host, int(port)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    target = ("127.0.0.1", 0)

    def forward(self):
        path = PROJECT_PATH.sub("/v1/environments/", self.path, count=1)
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else None
        headers = {k: v for k, v in self.headers.items() if k.lower() not in ("host", "connection", "proxy-connection")}
        try:
            conn = http.client.HTTPConnection(*self.target, timeout=30)
            conn.request(self.command, path, body=body, headers=headers)
            resp = conn.getresponse()
            data = resp.read()
        except OSError:
            self.send_error(502)
            return
        self.send_response(resp.status)
        for key, value in resp.getheaders():
            if key.lower() not in ("transfer-encoding", "connection", "content-length"):
                self.send_header(key, value)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)
        conn.close()

    do_GET = do_POST = do_PUT = do_DELETE = do_PATCH = forward

    def log_message(self, *args):
        pass


def main():
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    Handler.target = split(sys.argv[2])
    ThreadingHTTPServer(split(sys.argv[1]), Handler).serve_forever()


if __name__ == "__main__":
    main()
