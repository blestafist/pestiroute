#!/usr/bin/env python3
"""Validate bounded M5.2 direct/gateway artifacts or run offline loopback fakes."""

import argparse
import http.client
import http.server
import json
import os
import re
import threading
import time
from datetime import datetime, timezone
from pathlib import Path

PROFILES = {
    "ollama": {"runtime": "Ollama v0.40.3", "profile": "ollama-responses-http-sse-v1"},
    "vllm": {"runtime": "vLLM v0.31.0", "profile": "vllm-responses-http-sse-v1"},
    "llama.cpp": {"runtime": "llama.cpp v0.6.0", "profile": "llamacpp-responses-http-sse-v1"},
}
ROOT = {"schema_version", "leg", "profile", "runtime", "model", "template", "parser", "client", "captured_at_utc", "limits", "requests", "outcome", "observations"}
BAD_KEY = re.compile(r"authorization|cookie|api.?key|secret|credential|prompt|payload|raw.?body|arguments|token", re.I)
BAD_VALUE = re.compile(
    r"(?i)(sk-(?:ant-|proj-|live-|test-)?[a-z0-9_-]{8,}|rt_[a-z0-9_-]{8,}|bearer\s+\S+|"
    r"eyJ[a-zA-Z0-9_-]{8,}\.[a-zA-Z0-9_-]{8,}\.[a-zA-Z0-9_-]{8,}|"
    r"(?:^|[^a-z])(?:code|oauth_code)=?[\s:=]+[a-z0-9._~-]{8,}|(?:^|[;\s])(?:set-cookie|cookie)\s*[:=]|"
    r"synthetic-(?:codex|client)-key)"
)
CLIENTS = {"Codex CLI", "OpenAI SDK", "Python http.client", "Go http.Client", "curl"}
IDENTIFIER = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}")
GIT_REVISION = re.compile(r"git:[0-9a-f]{40}")
SHA256 = re.compile(r"sha256:[0-9a-f]{64}")
LIMITS = {"max_requests": 1, "max_latency_ms": 30000, "max_output_tokens": 4096, "deadline_seconds": 30, "max_response_bytes": 1048576}


def require(ok, message):
    if not ok:
        raise ValueError(message)


def redact(value, path="$"):
    if type(value) is dict:
        for key, child in value.items():
            safe_counter = key in {"max_output_tokens", "output_tokens"}
            require(type(key) is str and (safe_counter or not BAD_KEY.search(key)), f"forbidden sensitive field at {path}.{key}")
            redact(child, f"{path}.{key}")
    elif type(value) is list:
        for index, child in enumerate(value):
            redact(child, f"{path}[{index}]")
    elif type(value) is str:
        require(not BAD_VALUE.search(value), f"possible secret at {path}")


def load(path, leg):
    def pairs(items):
        obj = {}
        for key, value in items:
            require(key not in obj, f"duplicate JSON key: {key}")
            obj[key] = value
        return obj

    data = json.loads(Path(path).read_text(encoding="utf-8"), object_pairs_hook=pairs)
    require(type(data) is dict and set(data) == ROOT, "invalid artifact fields")
    require(type(data["schema_version"]) is int and data["schema_version"] == 1, "unsupported schema version")
    require(data["leg"] == leg and data["profile"] in PROFILES, "invalid leg/profile")
    require(data["runtime"] == PROFILES[data["profile"]]["runtime"], "runtime pin mismatch")
    model = data["model"]
    require(type(model) is dict and set(model) == {"name", "revision"} and type(model["name"]) is str and IDENTIFIER.fullmatch(model["name"]), "invalid model identifier")
    require(type(model["revision"]) is str and (GIT_REVISION.fullmatch(model["revision"]) or SHA256.fullmatch(model["revision"])), "model revision must be an immutable git or sha256 identity")
    for field in ("template", "parser"):
        require(type(data[field]) is str and SHA256.fullmatch(data[field]), f"{field} must be an immutable sha256 identity")
    client = data["client"]
    require(type(client) is dict and set(client) == {"name", "version"} and type(client["name"]) is str and client["name"] in CLIENTS and type(client["version"]) is str and re.fullmatch(r"v?\d+(?:\.\d+){0,3}", client["version"]), "invalid client provenance")
    stamp = data["captured_at_utc"]
    require(type(stamp) is str and re.fullmatch(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ", stamp), "invalid UTC timestamp")
    datetime.strptime(stamp, "%Y-%m-%dT%H:%M:%SZ")
    limits = data["limits"]
    require(type(limits) is dict and set(limits) == set(LIMITS), "invalid limit fields")
    require(all(type(limits[k]) is int and 0 < limits[k] <= cap for k, cap in LIMITS.items()), "configured bound exceeded")
    requests = data["requests"]
    require(type(requests) is list and len(requests) == limits["max_requests"] == 1, "single-request smoke requires exactly one request")
    for request in requests:
        require(type(request) is dict and set(request) == {"attempt", "status", "stream"} and request == {"attempt": 1, "status": 200, "stream": True}, "invalid request or automatic retry")
    obs = data["observations"]
    require(type(obs) is dict and set(obs) == {"dispatches", "retries", "latency_ms", "first_delta_ms", "terminal", "output_tokens"}, "invalid observation fields")
    require(type(obs["dispatches"]) is int and obs["dispatches"] == 1 and type(obs["retries"]) is int and obs["retries"] == 0, "single dispatch required; retries are forbidden")
    for field in ("latency_ms", "first_delta_ms"):
        require(type(obs[field]) is int and 0 <= obs[field] <= min(limits["max_latency_ms"], limits["deadline_seconds"] * 1000), f"{field} exceeds bound")
    require(obs["first_delta_ms"] <= obs["latency_ms"], "first delta occurs after completion")
    require(obs["terminal"] in {"completed", "failed", "incomplete", "unknown"}, "invalid terminal observation")
    tokens = obs["output_tokens"]
    require(tokens == "unknown" or (type(tokens) is int and 0 <= tokens <= limits["max_output_tokens"]), "output token bound exceeded")
    require(data["outcome"] in {"completed", "failed", "incomplete", "unknown"}, "invalid outcome")
    require(data["outcome"] == obs["terminal"], "outcome and terminal observation disagree")
    redact(data)
    return data


def validate_pair(direct_path, gateway_path):
    direct, gateway = load(direct_path, "direct"), load(gateway_path, "gateway")
    comparable = lambda value: {key: child for key, child in value.items() if key not in {"leg", "captured_at_utc", "observations"}}
    require(comparable(direct) == comparable(gateway), "paired profile, provenance, bounds or outcome differ")
    stable_obs = lambda value: {key: child for key, child in value["observations"].items() if key not in {"latency_ms", "first_delta_ms", "output_tokens"}}
    require(stable_obs(direct) == stable_obs(gateway), "paired dispatch, retry or terminal observations differ")
    require(direct["observations"]["dispatches"] + gateway["observations"]["dispatches"] <= 4, "paired dispatch cap exceeded")
    print(f"PASS paired {direct['profile']} direct/gateway schema, bounds and redaction")


class Fake(http.server.BaseHTTPRequestHandler):
    hits = 0

    def do_POST(self):
        type(self).hits += 1
        if self.path != "/v1/responses":
            self.send_error(404)
            return
        self.rfile.read(int(self.headers.get("Content-Length", "0")))
        events = (b'event: response.created\ndata: {"type":"response.created"}\n\n',
                  b'event: response.output_text.delta\ndata: {"type":"response.output_text.delta","delta":"synthetic"}\n\n',
                  b'event: response.completed\ndata: {"type":"response.completed"}\n\n')
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        for event in events:
            self.wfile.write(f"{len(event):X}\r\n".encode() + event + b"\r\n")
            self.wfile.flush()

    def log_message(self, *_):
        pass


def self_test(outdir):
    unsafe = ("OPENAI_BASE_URL", "OPENAI_API_BASE", "PESTIROUTE_CODEX_BASE_URL", "PESTIROUTE_SMOKE_M5_2_BASE_URL")
    require(not any(os.environ.get(key) for key in unsafe), "public endpoint override unsafe for self-test")
    outdir.mkdir(parents=True, exist_ok=True)
    Fake.hits = 0
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Fake)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        for profile, info in PROFILES.items():
            legs = []
            for leg in ("direct", "gateway"):
                start = time.monotonic()
                conn = http.client.HTTPConnection("127.0.0.1", server.server_port, timeout=5)
                conn.request("POST", "/v1/responses", body=b'{"model":"synthetic-model","stream":true}', headers={"Content-Type": "application/json"})
                response = conn.getresponse()
                require(response.status == 200 and response.getheader("Content-Type") == "text/event-stream", "loopback fake response invalid")
                body = bytearray()
                first_delta = None
                while True:
                    line = response.readline()
                    require(len(body) + len(line) <= LIMITS["max_response_bytes"], "response byte limit exceeded")
                    body.extend(line)
                    if b"response.output_text.delta" in body and first_delta is None:
                        first_delta = int((time.monotonic() - start) * 1000)
                    if not line:
                        break
                conn.close()
                elapsed = int((time.monotonic() - start) * 1000)
                require(first_delta is not None and b"response.completed" in body, "loopback SSE missing delta/terminal")
                artifact = {"schema_version": 1, "leg": leg, "profile": profile, "runtime": info["runtime"],
                    "model": {"name": "synthetic-model", "revision": "sha256:" + "0" * 64},
                    "template": "sha256:" + "1" * 64, "parser": "sha256:" + "2" * 64, "client": {"name": "Python http.client", "version": "3.14.7"},
                    "captured_at_utc": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"), "limits": LIMITS,
                    "requests": [{"attempt": 1, "status": 200, "stream": True}], "outcome": "completed",
                    "observations": {"dispatches": 1, "retries": 0, "latency_ms": elapsed, "first_delta_ms": first_delta, "terminal": "completed", "output_tokens": "unknown"}}
                path = outdir / f"{profile.replace('.', '-')}-{leg}.json"
                path.write_text(json.dumps(artifact, indent=2) + "\n", encoding="utf-8")
                path.chmod(0o600)
                legs.append(path)
            validate_pair(*legs)
        require(Fake.hits == 6, "expected exactly six loopback calls")
        base = json.loads(legs[0].read_text())
        bad = outdir / "invalid.json"

        def must_reject(mutate, label):
            invalid = json.loads(json.dumps(base))
            mutate(invalid)
            bad.write_text(json.dumps(invalid), encoding="utf-8")
            try:
                load(bad, "direct")
            except ValueError:
                return
            raise AssertionError(f"validator accepted {label} fixture")

        must_reject(lambda x: x.update({"access_token": "rt_aaaaaaaaaaaaaaaa"}), "credential leak")
        must_reject(lambda x: x["client"].update({"version": "Cookie: session=private"}), "cookie in client version")
        must_reject(lambda x: (x["requests"].append(dict(x["requests"][0])), x["observations"].update({"dispatches": 2})), "duplicate dispatch records")
        for field, value in (("model", {"name": "synthetic-model", "revision": "unknown"}),
                             ("model", {"name": "synthetic-model", "revision": "latest"}),
                             ("template", "unknown"), ("template", "latest"), ("template", "default"),
                             ("parser", "unknown"), ("parser", "latest"), ("parser", "default")):
            must_reject(lambda x, f=field, v=value: x.update({f: v}), f"floating {field} identity")
        must_reject(lambda x: x["observations"].update({"retries": 1}), "retry")
        print("PASS six loopback HTTP/SSE calls across three profiles; leak, cookie, duplicate-dispatch, floating-identity and retry negatives; no external calls")
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("direct", nargs="?")
    parser.add_argument("gateway", nargs="?")
    parser.add_argument("--self-test", action="store_true")
    parser.add_argument("--output-dir", type=Path)
    args = parser.parse_args()
    if args.self_test:
        require(not args.direct and not args.gateway and args.output_dir is not None, "--self-test requires only --output-dir")
        self_test(args.output_dir)
    else:
        require(args.direct and args.gateway and args.output_dir is None, "supply direct and gateway artifacts")
        validate_pair(args.direct, args.gateway)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        raise SystemExit(f"FAIL {exc}")
