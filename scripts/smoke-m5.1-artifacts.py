#!/usr/bin/env python3
"""Validate sanitized M5.1 Codex smoke artifacts or exercise loopback fakes."""

import argparse
import http.client
import http.server
import json
import os
import re
import threading
from datetime import datetime, timezone
from pathlib import Path

SCENARIOS = {"plain_text", "single_tool", "two_calls", "two_rounds", "encrypted_reasoning", "unsupported_rejection"}
PROFILES = {"offline_fixture", "subscription_codex"}
BAD_KEY = re.compile(r"authorization|cookie|api.?key|secret|credential|prompt|payload|raw.?body|arguments|access.?token|refresh.?token|id.?token|oauth.?code|device.?code|user.?code", re.I)
BAD_VALUE = re.compile(
    r"(?i)(sk-(?:ant-|proj-|live-|test-)?[a-z0-9_-]{8,}|rt_[a-z0-9_-]{8,}|bearer\s+\S+|"
    r"eyJ[a-zA-Z0-9_-]{8,}\.[a-zA-Z0-9_-]{8,}\.[a-zA-Z0-9_-]{8,}|"
    r"(?:^|[^a-z])(?:code|oauth_code)=?[\s:=]+[a-z0-9._~-]{8,}|(?:^|[;\s])(?:set-cookie|cookie)\s*[:=]|"
    r"synthetic-(?:codex|client)-key)"
)
MODEL = "gpt-5.4-mini"
CLIENTS = {"Codex CLI", "OpenAI SDK", "Python http.client", "Go http.Client", "curl"}
ROOT_KEYS = {"schema_version", "leg", "endpoint_profile", "captured_at_utc", "client", "model", "scenario", "fixture_id", "limits", "requests", "outcome", "observations"}
LIMIT_KEYS = {"timeout_seconds", "max_output_tokens", "client_runs"}
REQUEST_KEYS = {"run", "attempt", "timeout_seconds", "max_output_tokens", "status", "stream", "api_path"}
OBS_KEYS = {"early_delivery", "event_order", "tool_links", "usage", "target_dispatches", "retries", "client_status", "reasoning_observation"}
AUTH_KEYS = {"schema_version", "endpoint_profile", "captured_at_utc", "artifacts"}
AUTH_RECORD_KEYS = {"leg", "flow", "status", "expiry_preserved", "token_present"}


def require(ok, message):
    if not ok:
        raise ValueError(message)


def redact(value, path="$", key=""):
    if isinstance(value, dict):
        for k, v in value.items():
            require(isinstance(k, str) and not BAD_KEY.search(k), f"forbidden sensitive field at {path}.{k}")
            redact(v, f"{path}.{k}", k)
    elif isinstance(value, list):
        for i, v in enumerate(value):
            redact(v, f"{path}[{i}]", key)
    elif isinstance(value, str):
        require(not BAD_VALUE.search(value), f"possible secret or private value at {path}")


def json_file(path):
    def pairs(pairs):
        obj = {}
        for key, value in pairs:
            require(key not in obj, f"duplicate JSON key: {key}")
            obj[key] = value
        return obj
    return json.loads(Path(path).read_text(encoding="utf-8"), object_pairs_hook=pairs)


def valid_stamp(value):
    require(isinstance(value, str) and re.fullmatch(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ", value), "captured_at_utc must be RFC3339 UTC")
    datetime.strptime(value, "%Y-%m-%dT%H:%M:%SZ")


def load_inference(path, leg):
    data = json_file(path)
    require(type(data) is dict and set(data) == ROOT_KEYS, "invalid inference artifact fields")
    require(type(data["schema_version"]) is int and data["schema_version"] == 1, "schema_version must be 1")
    require(data["leg"] == leg and type(data["endpoint_profile"]) is str and data["endpoint_profile"] in PROFILES, "invalid leg or endpoint profile")
    valid_stamp(data["captured_at_utc"])
    require(data["model"] == MODEL and type(data["scenario"]) is str and data["scenario"] in SCENARIOS, "model or scenario mismatch")
    expected_fixture = "m51-" + data["scenario"].replace("_", "-") + "-v1"
    require(data["fixture_id"] == expected_fixture, "fixture_id must be the scenario's sanitized fixture label")
    client = data["client"]
    require(type(client) is dict and set(client) == {"name", "version"} and type(client["name"]) is str and client["name"] in CLIENTS and type(client["version"]) is str and len(client["version"]) <= 32 and re.fullmatch(r"v?\d+(?:\.\d+){0,3}", client["version"]), "invalid client provenance")
    limits = data["limits"]
    require(type(limits) is dict and set(limits) == LIMIT_KEYS, "invalid limit fields")
    require(type(limits["timeout_seconds"]) is int and 0 < limits["timeout_seconds"] <= 30, "timeout exceeds 30 seconds")
    require(type(limits["max_output_tokens"]) is int and 0 < limits["max_output_tokens"] <= 4096, "output cap exceeds 4096")
    require(type(limits["client_runs"]) is int and 0 < limits["client_runs"] <= 2, "client run cap exceeds 2")
    require(type(data["requests"]) is list and len(data["requests"]) <= limits["client_runs"], "request/run cap exceeded")
    runs = set()
    for req in data["requests"]:
        require(type(req) is dict and set(req) == REQUEST_KEYS, "invalid request record fields")
        require(type(req["run"]) is int and 1 <= req["run"] <= limits["client_runs"] and type(req["attempt"]) is int and req["attempt"] == 1, "invalid run or hidden retry")
        require(type(req["timeout_seconds"]) is int and 0 < req["timeout_seconds"] <= min(30, limits["timeout_seconds"]), "request timeout exceeds cap")
        require(type(req["max_output_tokens"]) is int and 0 < req["max_output_tokens"] <= limits["max_output_tokens"] <= 4096, "request token cap exceeded")
        require(type(req["status"]) is int and type(req["stream"]) is bool, "invalid request status/stream")
        require(req["api_path"] == "/v1/responses", "invalid API path")
        require(req["run"] not in runs, "duplicate client run")
        runs.add(req["run"])
    obs = data["observations"]
    require(type(obs) is dict and set(obs) == OBS_KEYS, "invalid observation fields")
    event_types = {"response.created", "response.output_text.delta", "response.function_call_arguments.delta", "response.output_item.done", "response.completed"}
    require(type(obs["early_delivery"]) is bool and type(obs["event_order"]) is list and all(type(x) is str and x in event_types for x in obs["event_order"]), "malformed event observations")
    require(type(obs["tool_links"]) is list and all(type(x) is str and re.fullmatch(r"(?:call|result_for_call)_\d+", x) for x in obs["tool_links"]), "invalid sanitized tool links")
    require(type(obs["usage"]) is str and obs["usage"] in {"reported", "unknown"}, "invalid usage observation")
    require(type(obs["target_dispatches"]) is int and 0 <= obs["target_dispatches"] <= 2 and type(obs["retries"]) is int and obs["retries"] == 0, "dispatch cap or retry count invalid")
    require(obs["target_dispatches"] == len(data["requests"]), "dispatch/request count mismatch")
    require(type(obs["client_status"]) is int and (200 <= obs["client_status"] < 300 or 400 <= obs["client_status"] < 500), "client status must be success or rejection, not redirect/server error")
    require(type(obs["reasoning_observation"]) is str and obs["reasoning_observation"] in {"not_applicable", "encrypted_content_present_redacted"}, "invalid reasoning observation")
    if data["scenario"] == "unsupported_rejection":
        require(type(data["outcome"]) is str and data["outcome"] == "rejected" and 400 <= obs["client_status"] < 500 and obs["target_dispatches"] == 0 and not data["requests"] and not obs["early_delivery"] and not obs["event_order"] and not obs["tool_links"], "unsupported feature must reject before dispatch")
        require(obs["reasoning_observation"] == "not_applicable", "unsupported rejection cannot claim reasoning evidence")
    else:
        require(type(data["outcome"]) is str and data["outcome"] == "completed" and obs["client_status"] == 200 and obs["target_dispatches"] > 0 and obs["early_delivery"], "successful scenario requires HTTP 200, dispatch and early delivery")
        require(all(req["status"] == 200 and req["stream"] is True for req in data["requests"]), "successful provider requests require HTTP 200 streaming")
        cycles = {
            "plain_text": [["response.created", "response.output_text.delta", "response.completed"]],
            "single_tool": [["response.created", "response.function_call_arguments.delta", "response.completed"]],
            "two_calls": [["response.created", "response.function_call_arguments.delta", "response.function_call_arguments.delta", "response.completed"]],
            "two_rounds": [["response.created", "response.function_call_arguments.delta", "response.completed"], ["response.created", "response.function_call_arguments.delta", "response.completed"]],
            "encrypted_reasoning": [["response.created", "response.output_item.done", "response.completed"]],
        }
        expected_events = [event for cycle in cycles[data["scenario"]] for event in cycle]
        require(obs["event_order"] == expected_events, "event categories/order do not prove the declared scenario")
        require(len(data["requests"]) == len(cycles[data["scenario"]]) and [req["run"] for req in data["requests"]] == list(range(1, len(cycles[data["scenario"]]) + 1)), "scenario run count/order mismatch")
        expected_links = {
            "plain_text": [],
            "single_tool": ["call_1", "result_for_call_1"],
            "two_calls": ["call_1", "result_for_call_1", "call_2", "result_for_call_2"],
            "two_rounds": ["call_1", "result_for_call_1", "call_2", "result_for_call_2"],
            "encrypted_reasoning": [],
        }[data["scenario"]]
        require(obs["tool_links"] == expected_links, "scenario requires complete ordered call/result links")
        expected_reasoning = "encrypted_content_present_redacted" if data["scenario"] == "encrypted_reasoning" else "not_applicable"
        require(obs["reasoning_observation"] == expected_reasoning, "encrypted reasoning must be explicitly marked present but redacted")
    redact(data)
    return data


def validate_pair(direct_path, gateway_path):
    direct = load_inference(direct_path, "direct_codex")
    gateway = load_inference(gateway_path, "gateway_responses_to_codex")
    # Only leg identity and capture time legitimately differ between paired artifacts.
    comparable_direct = {k: v for k, v in direct.items() if k not in {"leg", "captured_at_utc"}}
    comparable_gateway = {k: v for k, v in gateway.items() if k not in {"leg", "captured_at_utc"}}
    require(comparable_direct == comparable_gateway, "paired artifacts differ in client, outcome, event order, tool links, usage, counters or limits")
    require(direct["observations"]["target_dispatches"] + gateway["observations"]["target_dispatches"] <= 4, "paired provider dispatch cap exceeded")
    print(f"PASS paired {direct['scenario']} fixture={direct['fixture_id']} direct/gateway schemas, limits, counters and redaction valid")


def validate_auth(path):
    data = json_file(path)
    require(type(data) is dict and set(data) == AUTH_KEYS and type(data["schema_version"]) is int and data["schema_version"] == 1, "invalid auth artifact fields/version")
    require(type(data["endpoint_profile"]) is str and data["endpoint_profile"] in PROFILES, "invalid auth endpoint profile")
    valid_stamp(data["captured_at_utc"])
    require(type(data["artifacts"]) is list and len(data["artifacts"]) == 2, "auth artifact must contain direct and gateway records")
    seen = set()
    for record in data["artifacts"]:
        require(type(record) is dict and set(record) == AUTH_RECORD_KEYS, "invalid auth record fields")
        require(type(record["leg"]) is str and record["leg"] in {"direct_auth", "gateway_auth"} and record["leg"] not in seen, "invalid/duplicate auth leg")
        seen.add(record["leg"])
        require(type(record["flow"]) is str and record["flow"] in {"device_start", "device_continue", "refresh"}, "invalid auth flow")
        require(type(record["status"]) is int and 100 <= record["status"] <= 599, "invalid auth status")
        require(type(record["expiry_preserved"]) is bool and type(record["token_present"]) is bool, "auth values must be booleans")
    require(seen == {"direct_auth", "gateway_auth"}, "auth artifact must pair both legs")
    for record in data["artifacts"]:
        success = 200 <= record["status"] < 300
        require(success or 400 <= record["status"] <= 599, "auth redirects are forbidden")
        if success:
            allowed_statuses = {"device_start": {200}, "device_continue": {200, 202}, "refresh": {200}}[record["flow"]]
            require(record["status"] in allowed_statuses, "unexpected successful auth status for flow")
        expected_token = record["status"] == 200 and record["flow"] in {"device_continue", "refresh"}
        require(record["token_present"] is expected_token and record["expiry_preserved"] is expected_token, "auth status/token/expiry semantics disagree")
    direct, gateway = sorted(data["artifacts"], key=lambda r: r["leg"])
    require({k: v for k, v in direct.items() if k != "leg"} == {k: v for k, v in gateway.items() if k != "leg"}, "paired auth flow, status, expiry or token-presence differs")
    redact(data)
    print("PASS paired direct/gateway auth schema and redaction valid")


class Fake(http.server.BaseHTTPRequestHandler):
    hits = []
    gate = None
    sent = None

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        request = json.loads(body)
        leg = self.headers.get("X-Smoke-Leg")
        self.hits.append((self.path, request, leg))
        if self.path == "/auth/device/continue" and request == {"flow": "device_continue"}:
            if leg not in {"direct_auth", "gateway_auth"}:
                self.send_error(400)
                return
            payload = json.dumps({"expiry_preserved": True, "token_present": True}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            return
        if self.path != "/v1/responses" or request != {"model": MODEL, "stream": True, "max_output_tokens": 64} or leg not in {"direct_codex", "gateway_responses_to_codex"}:
            self.send_error(400)
            return
        events = [b'event: response.created\ndata: {"type":"response.created"}\n\n', b'event: response.output_text.delta\ndata: {"type":"response.output_text.delta","delta":"ok"}\n\n', b'event: response.completed\ndata: {"type":"response.completed"}\n\n']
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_chunked()
        self.wfile.write(f"{len(events[0]):X}\r\n".encode() + events[0] + b"\r\n")
        self.wfile.flush()
        self.sent.set()
        if not self.gate.wait(5):
            return
        for event in events[1:]:
            self.wfile.write(f"{len(event):X}\r\n".encode() + event + b"\r\n")
            self.wfile.flush()
        self.wfile.write(b"0\r\n\r\n")

    def send_chunked(self):
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()

    def log_message(self, *_):
        pass


def request_fixture(port, leg):
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=5)
    conn.request("POST", "/v1/responses", body=json.dumps({"model": MODEL, "stream": True, "max_output_tokens": 64}), headers={"Content-Type": "application/json", "X-Smoke-Leg": leg})
    response = conn.getresponse()
    require(response.status == 200 and response.getheader("Content-Type") == "text/event-stream", "loopback SSE request failed")
    first = response.readline() + response.readline() + response.readline()
    require(b"response.created" in first, "first SSE event missing")
    return conn, response, first


def auth_fixture(port, leg):
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=5)
    conn.request("POST", "/auth/device/continue", body=json.dumps({"flow": "device_continue"}), headers={"Content-Type": "application/json", "X-Smoke-Leg": leg})
    response = conn.getresponse()
    result = json.loads(response.read())
    conn.close()
    require(response.status == 200 and set(result) == {"expiry_preserved", "token_present"}, "loopback auth fixture failed")
    return response.status, result


def must_reject(action, label):
    try:
        action()
    except (ValueError, json.JSONDecodeError):
        return
    raise AssertionError(f"validator accepted deliberate {label} fixture")


def require_offline_environment():
    unsafe = ("OPENAI_BASE_URL", "OPENAI_API_BASE", "CODEX_BASE_URL", "PESTIROUTE_CODEX_BASE_URL", "PESTIROUTE_CODEX_ENDPOINT", "PESTIROUTE_SMOKE_M5_1_BASE_URL")
    require(not any(os.environ.get(key) for key in unsafe), "public endpoint override environment is unsafe for self-test")


def self_test(outdir):
    require_offline_environment()
    outdir.mkdir(parents=True, exist_ok=True)
    Fake.hits = []
    Fake.gate, Fake.sent = threading.Event(), threading.Event()
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Fake)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        port = server.server_address[1]
        legs = []
        for leg in ("direct_codex", "gateway_responses_to_codex"):
            conn, response, first = request_fixture(port, leg)
            require(Fake.sent.wait(5), "fake did not signal first-event delivery")
            early = not Fake.gate.is_set() and not response.isclosed()
            Fake.gate.set()
            rest = response.read()
            conn.close()
            events = re.findall(rb"event: ([^\r\n]+)", first + rest)
            require(early and events == [b"response.created", b"response.output_text.delta", b"response.completed"], "incremental SSE/order invariant failed")
            legs.append({"schema_version": 1, "leg": leg, "endpoint_profile": "offline_fixture", "captured_at_utc": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"), "client": {"name": "Python http.client", "version": "3"}, "model": MODEL, "scenario": "plain_text", "fixture_id": "m51-plain-text-v1", "limits": {"timeout_seconds": 5, "max_output_tokens": 64, "client_runs": 1}, "requests": [{"run": 1, "attempt": 1, "timeout_seconds": 5, "max_output_tokens": 64, "status": 200, "stream": True, "api_path": "/v1/responses"}], "outcome": "completed", "observations": {"early_delivery": early, "event_order": [x.decode() for x in events], "tool_links": [], "usage": "reported", "target_dispatches": 1, "retries": 0, "client_status": 200, "reasoning_observation": "not_applicable"}})
            Fake.gate.clear()
            Fake.sent.clear()
        require(len(Fake.hits) == 2 and [(hit[0], hit[2]) for hit in Fake.hits] == [("/v1/responses", leg) for leg in ("direct_codex", "gateway_responses_to_codex")], "direct/gateway SSE captures were not independently counted")
        for artifact, name in zip(legs, ("direct.json", "gateway.json")):
            path = outdir / name
            path.write_text(json.dumps(artifact, indent=2) + "\n", encoding="utf-8")
            path.chmod(0o600)
        auth_records = []
        for leg in ("direct_auth", "gateway_auth"):
            status, result = auth_fixture(port, leg)
            auth_records.append({"leg": leg, "flow": "device_continue", "status": status, **result})
        require(len(Fake.hits) == 4 and [(hit[0], hit[2]) for hit in Fake.hits[2:]] == [("/auth/device/continue", leg) for leg in ("direct_auth", "gateway_auth")], "direct/gateway auth captures were not independently counted")
        auth = {"schema_version": 1, "endpoint_profile": "offline_fixture", "captured_at_utc": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"), "artifacts": auth_records}
        auth_path = outdir / "auth.json"
        auth_path.write_text(json.dumps(auth, indent=2) + "\n", encoding="utf-8")
        auth_path.chmod(0o600)
        validate_pair(outdir / "direct.json", outdir / "gateway.json")
        validate_auth(auth_path)
        direct = json.loads((outdir / "direct.json").read_text())
        gateway = json.loads((outdir / "gateway.json").read_text())
        scenario_values = {
            "single_tool": ([("response.created", "response.function_call_arguments.delta", "response.completed")], ["call_1", "result_for_call_1"], "not_applicable", 1),
            "two_calls": ([("response.created", "response.function_call_arguments.delta", "response.function_call_arguments.delta", "response.completed")], ["call_1", "result_for_call_1", "call_2", "result_for_call_2"], "not_applicable", 1),
            "two_rounds": ([("response.created", "response.function_call_arguments.delta", "response.completed")] * 2, ["call_1", "result_for_call_1", "call_2", "result_for_call_2"], "not_applicable", 2),
            "encrypted_reasoning": ([("response.created", "response.output_item.done", "response.completed")], [], "encrypted_content_present_redacted", 1),
        }
        for scenario, (cycles, links, reasoning, request_count) in scenario_values.items():
            for artifact in (direct, gateway):
                artifact["scenario"] = scenario
                artifact["fixture_id"] = "m51-" + scenario.replace("_", "-") + "-v1"
                artifact["observations"].update({"event_order": [event for cycle in cycles for event in cycle], "tool_links": links, "target_dispatches": request_count, "reasoning_observation": reasoning})
                artifact["limits"]["client_runs"] = request_count
                artifact["requests"] = [{"run": run, "attempt": 1, "timeout_seconds": 5, "max_output_tokens": 64, "status": 200, "stream": True, "api_path": "/v1/responses"} for run in range(1, request_count + 1)]
            direct_path, gateway_path = outdir / f"{scenario}-direct.json", outdir / f"{scenario}-gateway.json"
            direct_path.write_text(json.dumps(direct), encoding="utf-8")
            gateway_path.write_text(json.dumps(gateway), encoding="utf-8")
            validate_pair(direct_path, gateway_path)
        for artifact in (direct, gateway):
            artifact.update({"scenario": "unsupported_rejection", "fixture_id": "m51-unsupported-rejection-v1", "outcome": "rejected", "requests": []})
            artifact["observations"].update({"client_status": 422, "target_dispatches": 0, "early_delivery": False, "event_order": [], "tool_links": [], "usage": "unknown", "reasoning_observation": "not_applicable"})
            artifact["limits"]["client_runs"] = 1
        direct_path, gateway_path = outdir / "reject-direct.json", outdir / "reject-gateway.json"
        direct_path.write_text(json.dumps(direct), encoding="utf-8")
        gateway_path.write_text(json.dumps(gateway), encoding="utf-8")
        validate_pair(direct_path, gateway_path)

        base = json.loads((outdir / "direct.json").read_text())
        invalids = (
            ("credential leak", lambda x: x.update({"access_token": "rt_aaaaaaaaaaaaaaaa"})),
            ("raw prompt", lambda x: x.update({"raw_prompt": "private fixture text"})),
            ("over-cap", lambda x: x["limits"].update({"max_output_tokens": 4097})),
            ("hidden retry", lambda x: x["requests"][0].update({"attempt": 2})),
            ("malformed events", lambda x: x["observations"].update({"event_order": ["bad.event"]})),
            ("missing tool links", lambda x: (x.update({"scenario": "single_tool", "fixture_id": "m51-single-tool-v1"}), x["observations"].update({"event_order": ["response.created", "response.function_call_arguments.delta", "response.completed"], "tool_links": []}))),
            ("missing encrypted-reasoning redaction", lambda x: (x.update({"scenario": "encrypted_reasoning", "fixture_id": "m51-encrypted-reasoning-v1"}), x["observations"].update({"event_order": ["response.created", "response.output_item.done", "response.completed"], "reasoning_observation": "not_applicable"}))),
        )
        for index, (label, mutate) in enumerate(invalids):
            bad = json.loads(json.dumps(base))
            mutate(bad)
            path = outdir / f"invalid-{index}.json"
            path.write_text(json.dumps(bad), encoding="utf-8")
            must_reject(lambda: load_inference(path, "direct_codex"), label)

        def rejected_inference_mutation(key, value):
            bad = json.loads(json.dumps(base))
            bad["client"]["version"] = value
            must_reject(lambda: redact(bad), key)
        for secret in ("rt_aaaaaaaaaaaaaaaa", "Bearer synthetic-value", "Cookie: session=private"):
            rejected_inference_mutation("secret value", secret)
        for version in ("1.0-hello-bob-secret-plan", "1.0+Set-Cookie", "1" * 33):
            bad_version = json.loads(json.dumps(base))
            bad_version["client"]["version"] = version
            version_path = outdir / "invalid-client-version.json"
            version_path.write_text(json.dumps(bad_version), encoding="utf-8")
            must_reject(lambda: load_inference(version_path, "direct_codex"), "unbounded client version")
        old_override = os.environ.get("OPENAI_BASE_URL")
        os.environ["OPENAI_BASE_URL"] = "https://invalid.example"
        must_reject(require_offline_environment, "public endpoint override")
        if old_override is None:
            del os.environ["OPENAI_BASE_URL"]
        else:
            os.environ["OPENAI_BASE_URL"] = old_override

        mismatched = json.loads(json.dumps(base))
        mismatched["leg"] = "gateway_responses_to_codex"
        mismatched["observations"]["usage"] = "unknown"
        mismatch_path = outdir / "mismatch-gateway.json"
        mismatch_path.write_text(json.dumps(mismatched), encoding="utf-8")
        must_reject(lambda: validate_pair(outdir / "direct.json", mismatch_path), "paired observation mismatch")
        mismatched_client = json.loads(json.dumps(base))
        mismatched_client["leg"] = "gateway_responses_to_codex"
        mismatched_client["client"] = {"name": "curl", "version": "3"}
        mismatch_path.write_text(json.dumps(mismatched_client), encoding="utf-8")
        must_reject(lambda: validate_pair(outdir / "direct.json", mismatch_path), "paired client mismatch")

        auth = json.loads(auth_path.read_text())
        auth_leak = json.loads(json.dumps(auth))
        auth_leak["artifacts"][0]["refresh_token"] = "rt_aaaaaaaaaaaaaaaa"
        auth_bad_path = outdir / "invalid-auth.json"
        auth_bad_path.write_text(json.dumps(auth_leak), encoding="utf-8")
        must_reject(lambda: validate_auth(auth_bad_path), "auth secret leak")
        auth_bad = json.loads(json.dumps(auth))
        auth_bad["artifacts"][0]["token_present"] = False
        auth_bad_path.write_text(json.dumps(auth_bad), encoding="utf-8")
        must_reject(lambda: validate_auth(auth_bad_path), "auth success without token/expiry")
        auth_mismatch = json.loads(json.dumps(auth))
        auth_mismatch["artifacts"][1]["flow"] = "refresh"
        auth_bad_path.write_text(json.dumps(auth_mismatch), encoding="utf-8")
        must_reject(lambda: validate_auth(auth_bad_path), "paired auth mismatch")
        auth_status_mismatch = json.loads(json.dumps(auth))
        auth_status_mismatch["artifacts"][1].update({"status": 202, "token_present": False, "expiry_preserved": False})
        auth_bad_path.write_text(json.dumps(auth_status_mismatch), encoding="utf-8")
        must_reject(lambda: validate_auth(auth_bad_path), "paired auth status mismatch")
        print("PASS independently counted direct/gateway SSE; all scenario schemas; pair/auth mismatches; missing links/reasoning; secret, prompt, cap, retry and malformed negatives; no provider calls")
    finally:
        Fake.gate.set()
        server.shutdown()
        server.server_close()
        thread.join()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("direct", nargs="?")
    parser.add_argument("gateway", nargs="?")
    parser.add_argument("--auth", type=Path)
    parser.add_argument("--self-test", action="store_true")
    parser.add_argument("--output-dir", type=Path)
    args = parser.parse_args()
    if args.self_test:
        require(args.output_dir is not None and not args.direct and not args.gateway and args.auth is None, "--self-test requires only --output-dir")
        self_test(args.output_dir)
    elif args.auth:
        require(not args.direct and not args.gateway, "--auth cannot be combined with inference artifacts")
        validate_auth(args.auth)
    else:
        require(args.direct and args.gateway, "supply paired direct and gateway artifacts")
        validate_pair(args.direct, args.gateway)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        raise SystemExit(f"FAIL {exc}")
