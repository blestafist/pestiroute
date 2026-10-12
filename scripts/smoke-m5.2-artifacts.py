#!/usr/bin/env python3
"""Validate bounded M5.2 direct/gateway artifacts or run offline loopback fakes."""

import argparse
import hashlib
import ipaddress
import http.client
import http.server
import json
import os
import re
import stat
import threading
import time
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import urlsplit

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
SAFE_EVENTS = {"response.created", "response.in_progress", "response.output_item.added", "response.output_item.done",
               "response.content_part.added", "response.content_part.done", "response.output_text.delta", "response.output_text.done",
               "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.reasoning_summary_text.delta",
               "response.reasoning_summary_text.done", "response.reasoning_summary_part.added", "response.completed", "response.failed", "response.incomplete"}


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


def loopback_url(value):
    parsed = urlsplit(value)
    require(parsed.scheme == "http" and parsed.hostname and not parsed.username and not parsed.password and not parsed.query and not parsed.fragment, "endpoint must be a plain local HTTP URL without userinfo/query/fragment")
    try:
        local = ipaddress.ip_address(parsed.hostname).is_loopback
    except ValueError:
        local = False
    require(local, "endpoint host must be a numeric loopback IP")
    return parsed


class RequestRejected(ValueError):
    def __init__(self, status, code):
        self.status, self.code = status, code
        super().__init__(f"endpoint returned HTTP {status} code {code}")


def error_code(response, conn, deadline):
    count, chunks = 0, bytearray()
    while True:
        require(time.monotonic() < deadline, "request deadline exceeded")
        if conn.sock is not None:
            conn.sock.settimeout(max(0.001, deadline - time.monotonic()))
        chunk = response.read(min(8192, LIMITS["max_response_bytes"] - count + 1))
        count += len(chunk)
        require(count <= LIMITS["max_response_bytes"], "error response byte limit exceeded")
        if not chunk:
            break
        chunks.extend(chunk)
    try:
        value = json.loads(chunks).get("error", {}).get("code", "other")
    except (ValueError, AttributeError):
        value = "other"
    return value if value in {"unsupported_semantics", "unsupported_capability", "unsupported_target", "unsupported_feature", "invalid_request"} else "other"


def one_request(url, body, auth, stream):
    parsed = loopback_url(url)
    deadline = time.monotonic() + LIMITS["deadline_seconds"]
    conn = http.client.HTTPConnection(parsed.hostname, parsed.port or 80, timeout=LIMITS["deadline_seconds"])
    start = time.monotonic()
    headers = {"Content-Type": "application/json", "Accept-Encoding": "identity"}
    if auth:
        headers["Authorization"] = "Bearer " + auth
    try:
        conn.connect()
        require(time.monotonic() < deadline, "request deadline exceeded")
        conn.sock.settimeout(max(0.001, deadline - time.monotonic()))
        conn.request("POST", parsed.path or "/", body=body, headers=headers)
        response = conn.getresponse()
        if response.status != 200:
            raise RequestRejected(response.status, error_code(response, conn, deadline))
        content_type = response.getheader("Content-Type", "").split(";", 1)[0].strip().lower()
        require(content_type == ("text/event-stream" if stream else "application/json"), "unexpected response content type")
        count = 0
        first_text_delta = None
        terminal = None
        output_tokens = "unknown"
        event_order = []
        function_calls = []
        if stream:
            event_name = None
            data_lines = []
            while True:
                require(time.monotonic() < deadline, "request deadline exceeded")
                if conn.sock is not None:
                    conn.sock.settimeout(max(0.001, deadline - time.monotonic()))
                line = response.readline(LIMITS["max_response_bytes"] - count + 1)
                count += len(line)
                require(count <= LIMITS["max_response_bytes"], "response byte limit exceeded")
                if line.startswith(b"event:"):
                    event_name = line[6:].strip().decode("ascii", "replace")
                    event_order.append(event_name if event_name in SAFE_EVENTS else "other")
                    if event_name == "response.output_text.delta" and first_text_delta is None:
                        first_text_delta = int((time.monotonic() - start) * 1000)
                elif line.startswith(b"data:"):
                    data_lines.append(line[5:].strip())
                if line in (b"\n", b"\r\n"):
                    data = b"\n".join(data_lines)
                    try:
                        event_data = json.loads(data)
                    except (ValueError, TypeError):
                        event_data = {}
                    if event_name == "response.completed":
                        terminal = "completed"
                        response_data = event_data.get("response", {})
                        for item in (response_data.get("output", []) if type(response_data) is dict else []):
                            if type(item) is dict and item.get("type") == "function_call":
                                function_calls.append({key: item.get(key) for key in ("id", "call_id", "name", "arguments")})
                        output_tokens = response_data.get("usage", {}).get("output_tokens", "unknown") if type(response_data) is dict else "unknown"
                    elif event_name in {"response.failed", "response.incomplete"}:
                        terminal = event_name.removeprefix("response.")
                    event_name, data_lines = None, []
                if not line:
                    break
        else:
            chunks = bytearray()
            while True:
                require(time.monotonic() < deadline, "request deadline exceeded")
                if conn.sock is not None:
                    conn.sock.settimeout(max(0.001, deadline - time.monotonic()))
                chunk = response.read(min(8192, LIMITS["max_response_bytes"] - count + 1))
                count += len(chunk)
                require(count <= LIMITS["max_response_bytes"], "response byte limit exceeded")
                if not chunk:
                    break
                chunks.extend(chunk)
            try:
                decoded = json.loads(chunks)
                terminal = {"completed": "completed", "failed": "failed", "incomplete": "incomplete"}.get(decoded.get("status"), "unknown")
                output_tokens = decoded.get("usage", {}).get("output_tokens", "unknown")
            except (ValueError, AttributeError):
                terminal = "unknown"
        elapsed = int((time.monotonic() - start) * 1000)
        require(elapsed <= LIMITS["max_latency_ms"], "request deadline exceeded")
        require(terminal in {"completed", "failed", "incomplete"}, "missing recognized terminal response")
        require(output_tokens == "unknown" or (type(output_tokens) is int and 0 <= output_tokens <= LIMITS["max_output_tokens"]), "reported output token count exceeds bound")
        return {"status": response.status, "latency_ms": elapsed, "first_text_delta_ms": first_text_delta,
                "terminal": terminal, "output_tokens": output_tokens, "response_bytes": count, "event_order": event_order,
                "function_calls": function_calls}
    finally:
        conn.close()


def run_pair(args):
    require(tuple(__import__("sys").version_info[:3]) == (3, 14, 7), "CPython 3.14.7 is required")
    require(args.profile in PROFILES, "profile must be ollama, vllm, or llama.cpp")
    require(re.fullmatch(r"sha256:[0-9a-f]{64}", args.template) and re.fullmatch(r"sha256:[0-9a-f]{64}", args.parser), "template and parser must be immutable SHA-256 identities")
    require(re.fullmatch(r"(?:sha256:[0-9a-f]{64}|git:[0-9a-f]{40})", args.model_revision), "model revision must be immutable")
    body = Path(args.body).read_bytes()
    require(0 < len(body) <= 1048576, "request body must be at most 1 MiB")
    decoded = json.loads(body)
    require(type(decoded) is dict and type(decoded.get("model")) is str and decoded.get("stream") is args.stream, "body model/stream does not match runner arguments")
    require(type(decoded.get("max_output_tokens")) is int and 0 < decoded["max_output_tokens"] <= LIMITS["max_output_tokens"], "body must bound max_output_tokens to 4096")
    direct_url, gateway_url = loopback_url(args.direct_url), loopback_url(args.gateway_url)
    direct_auth = os.environ.get(args.direct_bearer_env, "") if args.direct_auth == "bearer" else ""
    gateway_auth = os.environ.get(args.gateway_key_env, "")
    require(args.direct_auth != "bearer" or bool(direct_auth), "direct bearer credential env is unset")
    require(bool(gateway_auth), "gateway virtual-key env is unset")
    require(not direct_auth or direct_auth != gateway_auth, "direct bearer credential and gateway virtual key must be distinct")
    # Never allow either credential to be routed to the other leg.
    require(direct_url.geturl() != gateway_url.geturl(), "direct and gateway endpoints must differ")
    observations = [one_request(args.direct_url, body, direct_auth, args.stream),
                    one_request(args.gateway_url, body, gateway_auth, args.stream)]
    require(args.gateway_dispatches == 1, "trusted gateway upstream observation must confirm exactly one dispatch")
    for leg, obs in zip(("direct", "gateway"), observations):
        require(obs["terminal"] == "completed", f"{leg} did not complete; no artifact written")
    if args.stream:
        require(all(obs["first_text_delta_ms"] is not None for obs in observations), "both legs must deliver response.output_text.delta before terminal")
    if not args.stream:
        print("PASS matched direct/gateway JSON; one request per leg; bodies and outputs not retained")
        for leg, obs in zip(("direct", "gateway"), observations):
            print(json.dumps({"leg": leg, "status": obs["status"], "terminal": obs["terminal"], "latency_ms": obs["latency_ms"],
                              "output_tokens": obs["output_tokens"], "response_bytes": obs["response_bytes"], "dispatches": 1, "retries": 0}))
        return
    args.output_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    require(stat.S_IMODE(args.output_dir.stat().st_mode) & 0o077 == 0, "artifact directory must be private (mode 0700)")
    info = PROFILES[args.profile]
    for leg, obs in zip(("direct", "gateway"), observations):
        artifact = {"schema_version": 1, "leg": leg, "profile": args.profile, "runtime": info["runtime"],
            "model": {"name": decoded["model"], "revision": args.model_revision}, "template": args.template, "parser": args.parser,
            "client": {"name": "Python http.client", "version": "3.14.7"},
            "captured_at_utc": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"), "limits": LIMITS,
            "requests": [{"attempt": 1, "status": 200, "stream": True}], "outcome": obs["terminal"],
            "observations": {"dispatches": args.gateway_dispatches if leg == "gateway" else 1, "retries": 0, "latency_ms": obs["latency_ms"], "first_delta_ms": obs["first_text_delta_ms"],
                             "terminal": obs["terminal"], "output_tokens": obs["output_tokens"]}}
        path = args.output_dir / f"{args.profile}-{leg}.json"
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as output:
            output.write(json.dumps(artifact, indent=2) + "\n")
    validate_pair(args.output_dir / f"{args.profile}-direct.json", args.output_dir / f"{args.profile}-gateway.json")
    for leg, obs in zip(("direct", "gateway"), observations):
        print(json.dumps({"leg": leg, "status": obs["status"], "terminal": obs["terminal"], "latency_ms": obs["latency_ms"],
                          "first_text_delta_ms": obs["first_text_delta_ms"], "event_order": obs["event_order"],
                          "output_tokens": obs["output_tokens"], "response_bytes": obs["response_bytes"], "dispatches": 1, "retries": 0}))
    print("PASS paired SSE runner; response content and credentials not retained")


def run_tool_roundtrip(args):
    require(tuple(__import__("sys").version_info[:3]) == (3, 14, 7), "CPython 3.14.7 is required")
    require(args.profile in PROFILES, "profile must be ollama, vllm, or llama.cpp")
    require(re.fullmatch(r"sha256:[0-9a-f]{64}", args.template) and re.fullmatch(r"sha256:[0-9a-f]{64}", args.parser), "template and parser must be immutable SHA-256 identities")
    require(re.fullmatch(r"(?:sha256:[0-9a-f]{64}|git:[0-9a-f]{40})", args.model_revision), "model revision must be immutable")
    body = Path(args.body).read_bytes()
    require(0 < len(body) <= 1048576, "request body must be at most 1 MiB")
    decoded = json.loads(body)
    tools = decoded.get("tools") if type(decoded) is dict else None
    require(type(decoded) is dict and decoded.get("stream") is True and type(decoded.get("model")) is str and
            type(decoded.get("input")) is str and type(decoded.get("max_output_tokens")) is int and
            0 < decoded["max_output_tokens"] <= 4096, "tool probe requires bounded streaming text request")
    require(type(tools) is list and len(tools) == 1 and type(tools[0]) is dict and
            tools[0].get("type") == "function" and tools[0].get("name") == "smoke_lookup",
            "tool probe requires exactly the smoke_lookup function")
    direct_url, gateway_url = loopback_url(args.direct_url), loopback_url(args.gateway_url)
    direct_auth = os.environ.get(args.direct_bearer_env, "") if args.direct_auth == "bearer" else ""
    gateway_auth = os.environ.get(args.gateway_key_env, "")
    require(args.direct_auth != "bearer" or bool(direct_auth), "direct bearer credential env is unset")
    require(bool(gateway_auth) and (not direct_auth or direct_auth != gateway_auth), "gateway virtual key missing or direct/gateway credentials are identical")
    require(direct_url.geturl() != gateway_url.geturl(), "direct and gateway endpoints must differ")

    try:
        initial = [one_request(args.direct_url, body, direct_auth, True), one_request(args.gateway_url, body, gateway_auth, True)]
    except RequestRejected as exc:
        if exc.code == "unsupported_capability":
            print("BLOCKED: protected gateway capability is unsupported/unknown; no tool continuation or bypass attempted")
        raise
    calls = []
    for observation in initial:
        found = observation["function_calls"]
        require(len(found) == 1, "tool behavior remains unknown: each leg must emit exactly one completed function call")
        call = found[0]
        require(all(type(call.get(key)) is str and 0 < len(call[key]) <= 4096 for key in ("id", "call_id", "name", "arguments")) and
                call["name"] == "smoke_lookup", "returned tool call does not match the bounded smoke function")
        try:
            arguments = json.loads(call["arguments"])
        except ValueError as exc:
            raise ValueError("returned tool arguments are not valid JSON") from exc
        require(type(arguments) is dict and set(arguments) == {"key"} and type(arguments["key"]) is str and arguments["key"] in {"alpha", "beta"},
                "returned tool arguments are outside the harmless fixed map")
        calls.append((call, arguments["key"]))

    followups = []
    for call, key in calls:
        history = decoded["input"] if type(decoded["input"]) is list else [
            {"type": "message", "role": "user", "content": [{"type": "input_text", "text": decoded["input"]}]}
        ]
        followup = dict(decoded)
        followup["input"] = history + [
            {"type": "function_call", "id": call["id"], "call_id": call["call_id"], "name": call["name"], "arguments": call["arguments"]},
            {"type": "function_call_output", "call_id": call["call_id"], "output": json.dumps({"marker": "synthetic-" + key}, separators=(",", ":"))},
        ]
        encoded = json.dumps(followup, separators=(",", ":")).encode()
        require(0 < len(encoded) <= 1048576, "tool continuation body exceeds 1 MiB request bound")
        followups.append(encoded)
    results = [one_request(args.direct_url, followups[0], direct_auth, True),
               one_request(args.gateway_url, followups[1], gateway_auth, True)]
    require(args.gateway_dispatches == 2, "trusted gateway ingress counter must confirm exactly two tool-roundtrip dispatches")
    require(all(result["terminal"] == "completed" for result in results), "tool result continuation did not complete")

    args.output_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    require(stat.S_IMODE(args.output_dir.stat().st_mode) & 0o077 == 0, "tool summary directory must be private (mode 0700)")
    summary = {"schema_version": 1, "profile": args.profile, "runtime": PROFILES[args.profile]["runtime"],
               "requests_per_leg": 2, "dispatches": {"direct": 2, "gateway": args.gateway_dispatches}, "retries": 0,
               "tool_calls": {"direct": 1, "gateway": 1}, "linked_results": {"direct": 1, "gateway": 1},
               "terminal": {"direct": results[0]["terminal"], "gateway": results[1]["terminal"]}}
    path = args.output_dir / "tool-roundtrip-summary.json"
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as output:
        output.write(json.dumps(summary, indent=2) + "\n")
    print("PASS one client-owned tool call/result link per leg; IDs, arguments and request/response bodies retained only in memory")
    print(json.dumps(summary))


def check_rejection(args):
    require(tuple(__import__("sys").version_info[:3]) == (3, 14, 7), "CPython 3.14.7 is required")
    parsed = loopback_url(args.gateway_url)
    require(args.gateway_dispatches == 0, "trusted gateway upstream observation must confirm zero dispatches")
    body = Path(args.body).read_bytes()
    require(0 < len(body) <= 1048576, "request body must be at most 1 MiB")
    decoded = json.loads(body)
    require(type(decoded) is dict and type(decoded.get("model")) is str and
            type(decoded.get("max_output_tokens")) is int and 0 < decoded["max_output_tokens"] <= 4096 and
            type(decoded.get("stream")) is bool, "rejection body must have a model, bounded max_output_tokens, and stream boolean")
    key = os.environ.get(args.gateway_key_env, "")
    require(bool(key), "gateway virtual-key env is unset")
    deadline = time.monotonic() + LIMITS["deadline_seconds"]
    conn = http.client.HTTPConnection(parsed.hostname, parsed.port or 80, timeout=LIMITS["deadline_seconds"])
    try:
        conn.connect()
        require(time.monotonic() < deadline, "rejection probe deadline exceeded")
        conn.sock.settimeout(max(0.001, deadline - time.monotonic()))
        conn.request("POST", parsed.path or "/", body=body, headers={"Content-Type": "application/json", "Accept-Encoding": "identity", "Authorization": "Bearer " + key})
        response = conn.getresponse()
        count = 0
        chunks = bytearray()
        while True:
            require(time.monotonic() < deadline, "rejection probe deadline exceeded")
            if conn.sock is not None:
                conn.sock.settimeout(max(0.001, deadline - time.monotonic()))
            chunk = response.read(min(8192, LIMITS["max_response_bytes"] - count + 1))
            count += len(chunk)
            require(count <= LIMITS["max_response_bytes"], "rejection response byte limit exceeded")
            if not chunk: break
            chunks.extend(chunk)
        try:
            error = json.loads(chunks).get("error", {})
        except (ValueError, AttributeError):
            error = {}
        require(response.status == 400 and error.get("code") == args.expected_code,
                f"expected exact protected HTTP 400 {args.expected_code} rejection")
        print(f"PASS gateway rejection HTTP 400 {args.expected_code}; request/error body not retained; trusted upstream dispatch count=0")
    finally:
        conn.close()


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
    fixture_server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Fake)
    fixture_thread = threading.Thread(target=fixture_server.serve_forever, daemon=True)
    fixture_thread.start()
    try:
        for profile, info in PROFILES.items():
            legs = []
            for leg in ("direct", "gateway"):
                start = time.monotonic()
                conn = http.client.HTTPConnection("127.0.0.1", fixture_server.server_port, timeout=5)
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
        class PairFake(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                self.server.hits += 1
                body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
                self.server.body_bytes.append(body)
                self.server.body_sha.append(hashlib.sha256(body).hexdigest())
                self.server.auth.append(self.headers.get("Authorization", ""))
                request = json.loads(body)
                if "tools" in request:
                    call_id, item_id = self.server.tag + "-call", self.server.tag + "-item"
                    if type(request["input"]) is str:
                        call = {"type": "function_call", "id": item_id, "call_id": call_id, "name": "smoke_lookup", "arguments": '{"key":"alpha"}'}
                        events = (b'event: response.output_item.added\ndata: {"type":"response.output_item.added"}\n\n',
                                  b'event: response.function_call_arguments.delta\ndata: {"type":"response.function_call_arguments.delta","delta":"{\\"key\\":\\"alpha\\"}"}\n\n',
                                  b'event: response.function_call_arguments.done\ndata: {"type":"response.function_call_arguments.done"}\n\n',
                                  (b'event: response.completed\ndata: ' + json.dumps({"type":"response.completed","response":{"status":"completed","output":[call],"usage":{"output_tokens":1}}}).encode() + b'\n\n'))
                    else:
                        history=request["input"]
                        calls=[item for item in history if item.get("type")=="function_call"]
                        outputs=[item for item in history if item.get("type")=="function_call_output"]
                        self.server.tool_links.append(len(calls)==1 and len(outputs)==1 and calls[0].get("id")==item_id and
                            calls[0].get("call_id")==call_id and outputs[0].get("call_id")==call_id and outputs[0].get("output")=='{"marker":"synthetic-alpha"}')
                        events=(b'event: response.completed\ndata: {"type":"response.completed","response":{"status":"completed","output":[],"usage":{"output_tokens":1}}}\n\n',)
                    self.send_response(200); self.send_header("Content-Type", "text/event-stream"); self.end_headers()
                    for event in events: self.wfile.write(event); self.wfile.flush()
                    return
                if not request["stream"]:
                    data=b'{"status":"completed","usage":{"output_tokens":1}}'
                    self.send_response(200); self.send_header("Content-Type", "application/json"); self.send_header("Content-Length",str(len(data))); self.end_headers(); self.wfile.write(data)
                    return
                events = (b'event: response.created\ndata: {"type":"response.created"}\n\n',
                          b'event: response.output_text.delta\ndata: {"type":"response.output_text.delta","delta":"synthetic"}\n\n',
                          b'event: response.completed\ndata: {"type":"response.completed","response":{"usage":{"output_tokens":1}}}\n\n')
                self.send_response(200); self.send_header("Content-Type", "text/event-stream"); self.end_headers()
                for event in events: self.wfile.write(event); self.wfile.flush()
            def log_message(self, *_): pass
        pair_servers=[]
        for _ in range(2):
            server=http.server.ThreadingHTTPServer(("127.0.0.1",0),PairFake)
            server.hits=0; server.body_bytes=[]; server.body_sha=[]; server.auth=[]; server.tag="direct" if not pair_servers else "gateway"; server.tool_links=[]
            threading.Thread(target=server.serve_forever,daemon=True).start(); pair_servers.append(server)
        old_direct=os.environ.get("M52_TEST_DIRECT"); old_gateway=os.environ.get("M52_TEST_GATEWAY")
        os.environ["M52_TEST_DIRECT"]="synthetic-upstream-secret"; os.environ["M52_TEST_GATEWAY"]="synthetic-virtual-key"
        body_path=outdir/"synthetic-request.json"; body_path.write_bytes(b'{"model":"synthetic-model","input":"synthetic","stream":true,"max_output_tokens":8}')
        pair_args=argparse.Namespace(profile="ollama",template="sha256:"+"1"*64,parser="sha256:"+"2"*64,
            model_revision="sha256:"+"0"*64,body=str(body_path),stream=True,
            direct_url=f"http://127.0.0.1:{pair_servers[0].server_port}/v1/responses",
            gateway_url=f"http://127.0.0.1:{pair_servers[1].server_port}/v1/responses",direct_auth="bearer",
            direct_bearer_env="M52_TEST_DIRECT",gateway_key_env="M52_TEST_GATEWAY",output_dir=outdir/"runner",
            gateway_dispatches=1,tool_capability_reviewed=True)
        run_pair(pair_args)
        require(pair_servers[0].hits==pair_servers[1].hits==1 and pair_servers[0].body_bytes==pair_servers[1].body_bytes,
                "pair runner must send identical bytes once to each leg")
        require(pair_servers[0].auth==["Bearer synthetic-upstream-secret"] and pair_servers[1].auth==["Bearer synthetic-virtual-key"],
                "pair runner credential isolation failed")
        for artifact_path in (outdir/"runner"/"ollama-direct.json", outdir/"runner"/"ollama-gateway.json"):
            saved=artifact_path.read_text()
            require("synthetic-upstream-secret" not in saved and "synthetic-virtual-key" not in saved and '"input"' not in saved,
                    "runner artifact retained a credential or request body field")
        body_path.write_bytes(b'{"model":"synthetic-model","input":"synthetic","stream":false,"max_output_tokens":8}')
        pair_args.stream=False
        run_pair(pair_args)
        require(pair_servers[0].hits==pair_servers[1].hits==2 and pair_servers[0].body_bytes==pair_servers[1].body_bytes,
                "JSON pair runner must send identical bytes once to each leg")
        require(pair_servers[0].auth==["Bearer synthetic-upstream-secret"]*2 and pair_servers[1].auth==["Bearer synthetic-virtual-key"]*2,
                "JSON pair credential isolation failed")
        body_path.write_bytes(b'{"model":"synthetic-model","input":"Call smoke_lookup once with key alpha.","stream":true,"max_output_tokens":8,"tools":[{"type":"function","name":"smoke_lookup","parameters":{"type":"object","properties":{"key":{"type":"string","enum":["alpha","beta"]}},"required":["key"],"additionalProperties":false}}]}')
        pair_args.output_dir=outdir/"tool-roundtrip"; pair_args.gateway_dispatches=2; pair_args.stream=True
        run_tool_roundtrip(pair_args)
        require(pair_servers[0].tool_links==[True] and pair_servers[1].tool_links==[True],
                "tool continuation must use each leg's own returned IDs and linked fixed result")
        summary_text=(outdir/"tool-roundtrip"/"tool-roundtrip-summary.json").read_text()
        summary=json.loads(summary_text)
        require("direct-call" not in summary_text and "gateway-call" not in summary_text and summary["linked_results"]=={"direct":1,"gateway":1},
                "tool summary must omit call IDs and report only sanitized link counts")
        class NoTextFake(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                self.rfile.read(int(self.headers.get("Content-Length", "0")))
                event=self.server.delta_event
                data=json.dumps({"type":event,"delta":"synthetic"}) if event=="response.output_text.delta" else json.dumps({"type":event,"delta":"synthetic"})
                trace=f"event: {event}\ndata: {data}\n\nevent: response.completed\ndata: {{\"type\":\"response.completed\",\"response\":{{\"status\":\"completed\",\"output\":[]}}}}\n\n".encode()
                self.send_response(200); self.send_header("Content-Type","text/event-stream"); self.end_headers(); self.wfile.write(trace); self.wfile.flush()
            def log_message(self, *_): pass
        no_text_servers=[]
        pair_args.gateway_dispatches=1
        for event in ("response.reasoning_summary_text.delta", "response.function_call_arguments.delta"):
            pair=[]
            for _ in range(2):
                server=http.server.ThreadingHTTPServer(("127.0.0.1",0),NoTextFake); server.delta_event=event
                threading.Thread(target=server.serve_forever,daemon=True).start(); pair.append(server); no_text_servers.append(server)
            pair_args.body=str(outdir/"synthetic-text-request.json"); Path(pair_args.body).write_bytes(b'{"model":"synthetic-model","input":"synthetic","stream":true,"max_output_tokens":8}')
            pair_args.direct_url=f"http://127.0.0.1:{pair[0].server_port}/v1/responses"; pair_args.gateway_url=f"http://127.0.0.1:{pair[1].server_port}/v1/responses"
            pair_args.output_dir=outdir/(event.replace(".","-"))
            try:
                run_pair(pair_args)
            except ValueError as exc:
                require("response.output_text.delta" in str(exc), "non-text delta failed for an unexpected reason")
            else:
                raise AssertionError(f"text runner accepted {event} as a text delta")
        class RejectFake(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                self.server.hits += 1
                self.server.auth.append(self.headers.get("Authorization", ""))
                self.rfile.read(int(self.headers.get("Content-Length", "0")))
                data=json.dumps({"error":{"type":"unsupported_feature","code":self.server.error_code,"message":"safe"}}).encode()
                self.send_response(400); self.send_header("Content-Length",str(len(data))); self.end_headers(); self.wfile.write(data)
            def log_message(self, *_): pass
        reject=http.server.ThreadingHTTPServer(("127.0.0.1",0),RejectFake)
        reject.hits=0; reject.auth=[]; reject.upstream_hits=0; reject.error_code="unsupported_semantics"
        threading.Thread(target=reject.serve_forever,daemon=True).start()
        body_path.write_bytes(b'{"model":"synthetic-model","input":"Call smoke_lookup once with key alpha.","stream":true,"max_output_tokens":8,"tools":[{"type":"function","name":"smoke_lookup","parameters":{"type":"object","properties":{"key":{"type":"string","enum":["alpha","beta"]}},"required":["key"],"additionalProperties":false}}]}')
        pair_args.body=str(body_path)
        pair_args.direct_url=f"http://127.0.0.1:{pair_servers[0].server_port}/v1/responses"
        pair_args.gateway_url=f"http://127.0.0.1:{reject.server_port}/v1/responses"
        pair_args.output_dir=outdir/"capability-blocked"; pair_args.gateway_dispatches=1
        reject.error_code="unsupported_capability"
        direct_before=pair_servers[0].hits
        try:
            run_tool_roundtrip(pair_args)
        except RequestRejected as exc:
            require(exc.status==400 and exc.code=="unsupported_capability", "tool gate must preserve exact unsupported_capability rejection")
        else:
            raise AssertionError("tool runner bypassed unknown protected capability")
        require(pair_servers[0].hits==direct_before+1 and reject.hits==1 and not (outdir/"capability-blocked").exists(),
                "unknown tool capability must stop before any continuation or evidence claim")
        for server in no_text_servers: server.shutdown(); server.server_close()
        for server in pair_servers: server.shutdown(); server.server_close()
        reject.error_code="unsupported_semantics"
        rejection_args=argparse.Namespace(gateway_url=f"http://127.0.0.1:{reject.server_port}/v1/responses",
            body=str(body_path),gateway_dispatches=0,gateway_key_env="M52_TEST_GATEWAY",expected_code="unsupported_semantics")
        check_rejection(rejection_args)
        reject.error_code="unsupported_capability"; rejection_args.expected_code="unsupported_capability"
        check_rejection(rejection_args)
        reject.error_code="unsupported_target"; rejection_args.expected_code="unsupported_target"
        check_rejection(rejection_args)
        reject.error_code="unsupported_capability"; rejection_args.expected_code="unsupported_semantics"
        try:
            check_rejection(rejection_args)
        except ValueError:
            pass
        else:
            raise AssertionError("rejection probe accepted the wrong protected error code")
        require(reject.hits==5 and reject.upstream_hits==0 and reject.auth==["Bearer synthetic-virtual-key"]*5,
                "rejection probe must use only gateway virtual key and observe zero fake upstream dispatches")
        reject.shutdown(); reject.server_close()
        if old_direct is None: os.environ.pop("M52_TEST_DIRECT",None)
        else: os.environ["M52_TEST_DIRECT"]=old_direct
        if old_gateway is None: os.environ.pop("M52_TEST_GATEWAY",None)
        else: os.environ["M52_TEST_GATEWAY"]=old_gateway
        print("PASS offline self-test: validator profiles; exact SSE/JSON byte and auth pairs; text-delta gate; leg-specific function-call result links; exact rejection codes; zero-send fake; loopback only")
    finally:
        fixture_server.shutdown()
        fixture_server.server_close()
        fixture_thread.join()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("direct", nargs="?")
    parser.add_argument("gateway", nargs="?")
    parser.add_argument("--self-test", action="store_true")
    parser.add_argument("--output-dir", type=Path)
    parser.add_argument("--run-pair", action="store_true", help="send one bounded matched request to two loopback endpoints")
    parser.add_argument("--tool-roundtrip", action="store_true", help="run a bounded client-owned function call and leg-specific result continuation")
    parser.add_argument("--tool-capability-reviewed", action="store_true", help="acknowledge prior exact-scope tool support review; never overrides gateway admission")
    parser.add_argument("--check-rejection", action="store_true", help="send one bounded unsupported request to the protected loopback gateway")
    parser.add_argument("--profile", choices=sorted(PROFILES))
    parser.add_argument("--direct-url")
    parser.add_argument("--gateway-url")
    parser.add_argument("--body")
    parser.add_argument("--model-revision")
    parser.add_argument("--template")
    parser.add_argument("--parser")
    parser.add_argument("--stream", choices=("true", "false"), default="true")
    parser.add_argument("--direct-auth", choices=("none", "bearer"), default="none")
    parser.add_argument("--direct-bearer-env", default="PESTIROUTE_M52_DIRECT_BEARER")
    parser.add_argument("--gateway-key-env", default="PESTIROUTE_M52_GATEWAY_VKEY")
    parser.add_argument("--gateway-dispatches", type=int)
    parser.add_argument("--expected-code", choices=("unsupported_semantics", "unsupported_capability", "unsupported_target"))
    args = parser.parse_args()
    if args.self_test:
        require(not args.direct and not args.gateway and args.output_dir is not None and not args.run_pair and not args.tool_roundtrip and not args.check_rejection, "--self-test requires only --output-dir")
        self_test(args.output_dir)
    elif args.run_pair:
        require(not args.direct and not args.gateway and not args.tool_roundtrip and not args.check_rejection and args.profile and args.direct_url and args.gateway_url and args.body and args.model_revision and args.template and args.parser and args.output_dir and args.gateway_dispatches is not None,
                "--run-pair requires profile, endpoints, body, immutable identities, output directory, and trusted gateway dispatch count")
        args.stream = args.stream == "true"
        run_pair(args)
    elif args.tool_roundtrip:
        require(not args.direct and not args.gateway and not args.run_pair and not args.check_rejection and args.profile and args.direct_url and args.gateway_url and args.body and args.model_revision and args.template and args.parser and args.output_dir and args.gateway_dispatches is not None and args.stream == "true" and args.tool_capability_reviewed,
                "--tool-roundtrip requires exact-scope tool-capability review, profile, endpoints, streaming body, immutable identities, output directory, and trusted gateway dispatch count")
        run_tool_roundtrip(args)
    elif args.check_rejection:
        require(not args.direct and not args.gateway and not args.run_pair and not args.tool_roundtrip and args.gateway_url and args.body and args.gateway_dispatches is not None and args.expected_code,
                "--check-rejection requires gateway URL, body, exact expected code, and trusted gateway dispatch count")
        check_rejection(args)
    else:
        require(args.direct and args.gateway and args.output_dir is None, "supply direct and gateway artifacts")
        validate_pair(args.direct, args.gateway)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        raise SystemExit(f"FAIL {exc}")
