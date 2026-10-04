#!/usr/bin/env python3
"""Validate sanitized M4 smoke JSON, or exercise it with loopback-only fakes."""

import argparse
import hashlib
import http.client
import http.server
import json
import platform
import re
import os
import subprocess
import threading
from datetime import datetime, timezone
from pathlib import Path

SCENARIOS = {"plain_text", "single_tool", "two_calls", "two_rounds", "reasoning_rejection"}
ROOT_KEYS = {"schema_version", "leg", "endpoint_profile", "captured_at_utc", "source_revision", "client",
             "gateway", "provider", "scenario", "fixture_id", "backend_model", "limits",
             "requests", "outcome", "observations"}
COMPAT_ROOT_KEYS = ROOT_KEYS | {"test_only_model_override"}
BAD_KEY = re.compile(r"authorization|cookie|x-api-key|api.?key|secret|credential|prompt|arguments|body", re.I)
BAD_VALUE = re.compile(r"(?i)(sk-(?:ant-|proj-|live-|test-)?[a-z0-9_-]{8,}|bearer\s+\S+|synthetic-(?:anthropic|client)-key|synthetic-loopback-only)")
MODEL_ID = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:/-]{0,95}")


def require(condition, message):
    if not condition:
        raise ValueError(message)


def load(path, expected_leg):
    data = json.loads(Path(path).read_text())
    require(type(data) is dict and set(data) in (ROOT_KEYS, COMPAT_ROOT_KEYS), "artifact has missing or extra top-level fields")
    require(data["schema_version"] == 1, "schema_version must be 1")
    require(data["leg"] == expected_leg, f"leg must be {expected_leg}")
    require(data["endpoint_profile"] in {"offline_fixture", "official_anthropic", "compatible_endpoint"}, "unknown endpoint profile")
    if data["endpoint_profile"] == "compatible_endpoint":
        require(set(data) == COMPAT_ROOT_KEYS and data["test_only_model_override"] is True, "compatible capture must declare test-only model override")
    else:
        require(set(data) == ROOT_KEYS, "test-only model override is not valid for this profile")
    require(isinstance(data["captured_at_utc"], str) and re.fullmatch(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ", data["captured_at_utc"]), "captured_at_utc must be RFC3339 UTC")
    datetime.strptime(data["captured_at_utc"], "%Y-%m-%dT%H:%M:%SZ")
    require(isinstance(data["source_revision"], str) and re.fullmatch(r"(?:[a-fA-F0-9]{7,64}|offline-fixture-revision)", data["source_revision"]), "source_revision must be a git hash")
    require(data["scenario"] in SCENARIOS, "unknown scenario")
    require(isinstance(data["fixture_id"], str) and re.fullmatch(r"[A-Za-z0-9._:-]{1,96}", data["fixture_id"]), "fixture_id must be a safe matched-fixture label")
    require(isinstance(data["backend_model"], str) and MODEL_ID.fullmatch(data["backend_model"]) and ".." not in data["backend_model"], "invalid backend model label")
    if data["endpoint_profile"] != "compatible_endpoint":
        require(data["backend_model"] == "claude-opus-5-5", "official/offline backend model mismatch")
    limits = data["limits"]
    require(type(limits) is dict and set(limits) == {"timeout_seconds", "max_output_tokens", "client_runs"}, "invalid limits fields")
    require(type(limits["timeout_seconds"]) is int and 0 < limits["timeout_seconds"] <= 30, "timeout exceeds 30 seconds")
    require(type(limits["max_output_tokens"]) is int and 0 < limits["max_output_tokens"] <= 4096, "output cap exceeds 4096")
    require(type(limits["client_runs"]) is int and 0 < limits["client_runs"] <= 2, "client run cap exceeds 2")
    require(type(data["requests"]) is list, "requests must be an array")
    runs = set()
    attempts = {}
    for req in data["requests"]:
        require(type(req) is dict and set(req) == {"run", "attempt", "timeout_seconds", "max_output_tokens", "status", "stream", "api_path"}, "invalid request record fields")
        require(type(req["run"]) is int and 1 <= req["run"] <= limits["client_runs"], "run count exceeds cap")
        require(type(req["attempt"]) is int and req["attempt"] == req["run"] and 1 <= req["attempt"] <= 2, "attempt/run count exceeds cap")
        require(type(req["timeout_seconds"]) is int and 0 < req["timeout_seconds"] <= 30, "request timeout exceeds 30 seconds")
        require(type(req["max_output_tokens"]) is int and 0 < req["max_output_tokens"] <= 4096, "request token cap exceeds 4096")
        require(type(req["status"]) is int and 100 <= req["status"] <= 599 and type(req["stream"]) is bool, "invalid request outcome")
        require(req["api_path"] == ("/v1/messages" if expected_leg == "direct_messages" else "/v1/responses"), "API path does not match trace leg")
        runs.add(req["run"])
        attempts.setdefault(req["run"], set()).add(req["attempt"])
    require(len(runs) <= limits["client_runs"] and all(max(a) <= 2 for a in attempts.values()), "request attempts exceed cap")
    require(type(data["provider"]) is dict and data["provider"] == {"api": "anthropic-messages", "api_version": "2023-06-01"}, "provider/API version mismatch")
    require(type(data["client"]) is dict and set(data["client"]) == {"name", "version"} and all(isinstance(v, str) and re.fullmatch(r"[A-Za-z0-9.+/: _-]{1,80}", v) for v in data["client"].values()), "client name/version required")
    if expected_leg == "direct_messages":
        compatible_go_probe = data["endpoint_profile"] == "compatible_endpoint" and data["client"]["name"] == "Go http.Client" and re.fullmatch(r"go[0-9.]+(?:[-+:][A-Za-z0-9._:-]+)?", data["client"]["version"])
        require(data["gateway"] is None and (data["client"]["name"] in {"curl", "Python http.client"} or compatible_go_probe), "direct leg must identify curl, the offline harness, or the compatible Go probe")
    else:
        require(type(data["gateway"]) is dict and set(data["gateway"]) == {"client_protocol", "route_model", "source_revision"}, "gateway provenance required")
        require(data["gateway"]["client_protocol"] == "openai.responses.v1" and data["gateway"]["source_revision"], "invalid gateway provenance")
        route_model = data["gateway"]["route_model"]
        if data["endpoint_profile"] == "compatible_endpoint":
            require(isinstance(route_model, str) and MODEL_ID.fullmatch(route_model) and ".." not in route_model, "gateway route model must be a safe compatible label")
        else:
            require(re.fullmatch(r"[A-Za-z0-9._:-]{1,96}", route_model), "gateway route model must be a safe label")
        require((data["client"]["name"] == "OpenCode" and data["client"]["version"] == "2.0.6") or data["client"]["name"] in {"curl", "Python http.client"} or (data["client"]["name"] == "Go http.Client" and re.fullmatch(r"go[0-9.]+(?:[-+:][A-Za-z0-9._:-]+)?", data["client"]["version"])), "gateway client/version must identify OpenCode 2.0.6, curl, or the local Go test client")
    require(type(data["observations"]) is dict and set(data["observations"]) == {"terminal", "tool_links", "usage", "target_dispatches"}, "invalid comparison observations")
    require(type(data["observations"]["tool_links"]) is list and type(data["observations"]["target_dispatches"]) is int, "invalid comparison output")
    require(data["observations"]["usage"] in {"reported", "unknown", "reported_or_unknown"}, "invalid usage comparison")
    require(all(isinstance(link, str) and re.fullmatch(r"(?:call|result_for_call)_\d+", link) for link in data["observations"]["tool_links"]), "tool links must use sanitized ordinal labels")
    require(data["outcome"] in {"completed", "rejected", "not_run"}, "invalid scenario outcome")
    if data["scenario"] == "reasoning_rejection":
        if expected_leg == "direct_messages":
            require(data["outcome"] == "not_run" and not data["requests"] and data["observations"]["target_dispatches"] == 0, "reasoning must not be sent to direct provider")
        else:
            require(data["outcome"] == "rejected" and data["observations"]["target_dispatches"] == 0, "reasoning rejection must have zero upstream dispatch")
    else:
        require(data["outcome"] == "completed" and data["requests"] and data["observations"]["target_dispatches"] >= 1, "positive scenario must complete with target dispatch")
    walk_redaction(data)
    return data


def walk_redaction(value, path="$", parent_key=""):
    if isinstance(value, dict):
        for key, child in value.items():
            require(not BAD_KEY.search(key), f"forbidden sensitive field at {path}.{key}")
            walk_redaction(child, f"{path}.{key}", key)
    elif isinstance(value, list):
        for i, child in enumerate(value):
            walk_redaction(child, f"{path}[{i}]", parent_key)
    elif isinstance(value, str):
        require(not BAD_VALUE.search(value), f"possible secret/header value at {path}")
        require(value not in {"<ANTHROPIC_KEY_REDACTED>", "<CLIENT_KEY_REDACTED>"}, f"store only credential_present, not a credential placeholder, at {path}")


def validate_pair(direct_path, gateway_path):
    direct = load(direct_path, "direct_messages")
    gateway = load(gateway_path, "gateway_responses_to_messages")
    require(direct["scenario"] == gateway["scenario"], "direct/gateway scenarios differ")
    require(direct["endpoint_profile"] == gateway["endpoint_profile"], "direct/gateway endpoint profiles differ")
    require(direct["fixture_id"] == gateway["fixture_id"], "direct/gateway matched fixture differs")
    require(direct["backend_model"] == gateway["backend_model"], "backend models differ")
    require(direct["limits"] == gateway["limits"], "direct/gateway budgets differ")
    if direct["scenario"] != "reasoning_rejection":
        require(len(direct["requests"]) == len(gateway["requests"]), "direct/gateway request-round counts differ")
    print(f"PASS {direct['scenario']} fixture={direct['fixture_id']} direct={len(direct['requests'])} request(s) gateway={len(gateway['requests'])} request(s); limits/redaction/schema valid")


class Fake(http.server.BaseHTTPRequestHandler):
    seen = []

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        request = json.loads(body)
        record = {"path": self.path, "headers": self.headers, "request": request, "status": None, "response": b""}
        self.seen.append(record)
        if self.path == "/v1/messages":
            if request.get("model") != "claude-opus-5-5" or request.get("max_tokens") != 256 or not request.get("stream") or "messages" not in request or len(request.get("tools", [])) != 2:
                self.send_error(400)
                return
            turn = len(self.seen) - 1
            events = [{"type": "message_start", "message": {"usage": {"input_tokens": 1}}}]
            if turn == 0:
                calls = [("fixture-weather-1", "weather", {"city": "Paris"})]
            elif turn == 1:
                calls = [("fixture-weather-2", "weather", {"city": "Paris"}), ("fixture-clock-2", "clock", {"zone": "UTC"})]
            else:
                calls = []
                events.extend([
                    {"type": "content_block_start", "index": 0, "content_block": {"type": "text"}},
                    {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "Weather and time recorded."}},
                    {"type": "content_block_stop", "index": 0},
                ])
            for index, (call_id, name, arguments) in enumerate(calls):
                events.extend([
                    {"type": "content_block_start", "index": index, "content_block": {"type": "tool_use", "id": call_id, "name": name}},
                    {"type": "content_block_delta", "index": index, "delta": {"type": "input_json_delta", "partial_json": json.dumps(arguments, separators=(",", ":"))}},
                    {"type": "content_block_stop", "index": index},
                ])
            events.extend([
                {"type": "message_delta", "delta": {"stop_reason": "tool_use" if calls else "end_turn"}},
                {"type": "message_stop"},
            ])
            payload = "".join(f"event: {event['type']}\ndata: {json.dumps(event, separators=(',', ':'))}\n\n" for event in events).encode()
            status = 200
        else:
            status, payload = 404, b'{}'
        record["status"], record["response"] = status, payload
        self.send_response(status)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *_):
        pass


def self_test(outdir):
    Fake.seen = []
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Fake)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        port = server.server_address[1]
        client_timeout = 30
        base_messages = [{"role": "user", "content": "Help me check the weather and time."}]
        weather_call = {"type": "tool_use", "id": "fixture-weather-1", "name": "weather", "input": {"city": "Paris"}}
        base_messages += [{"role": "assistant", "content": [weather_call]}, {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "fixture-weather-1", "content": "client result for weather"}]}]
        round_two_calls = [
            {"type": "tool_use", "id": "fixture-weather-2", "name": "weather", "input": {"city": "Paris"}},
            {"type": "tool_use", "id": "fixture-clock-2", "name": "clock", "input": {"zone": "UTC"}},
        ]
        base_messages += [{"role": "assistant", "content": round_two_calls}, {"role": "user", "content": [
            {"type": "tool_result", "tool_use_id": "fixture-weather-2", "content": "client result for weather"},
            {"type": "tool_result", "tool_use_id": "fixture-clock-2", "content": "client result for clock"},
        ]}]
        tools = [
            {"name": "weather", "description": "Get fixture weather", "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"], "additionalProperties": False}},
            {"name": "clock", "description": "Get fixture UTC time", "input_schema": {"type": "object", "properties": {"zone": {"type": "string"}}, "required": ["zone"], "additionalProperties": False}},
        ]
        calls = []
        for turn, history in enumerate((base_messages[:1], base_messages[:3], base_messages), 1):
            request_body = {"model": "claude-opus-5-5", "max_tokens": 256, "stream": True, "messages": history, "tools": tools}
            conn = http.client.HTTPConnection("127.0.0.1", port, timeout=client_timeout)
            conn.request("POST", "/v1/messages", body=json.dumps(request_body), headers={"x-api-key": "synthetic-loopback-only", "anthropic-version": "2023-06-01", "Content-Type": "application/json"})
            response = conn.getresponse()
            response_body = response.read()
            calls.append(response.status)
            conn.close()
            if turn == 3:
                require(b"message_stop" in response_body, "direct Messages fake response did not complete")
        require(calls == [200, 200, 200] and len(Fake.seen) == 3 and all(item["path"] == "/v1/messages" for item in Fake.seen), "direct Messages loopback fake did not capture all rounds")
        require(all(item["headers"].get("x-api-key") == "synthetic-loopback-only" and item["headers"].get("anthropic-version") == "2023-06-01" for item in Fake.seen), "direct Messages fake did not observe expected headers")
        outdir.mkdir(parents=True, exist_ok=True)
        stamp = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        digest = hashlib.sha256(Path("cmd/gateway/translation_tool_rounds_test.go").read_bytes()).hexdigest()
        seen = Fake.seen[0]
        requests = [{"run": 1, "attempt": 1, "timeout_seconds": client_timeout, "max_output_tokens": item["request"]["max_tokens"], "status": item["status"], "stream": item["request"]["stream"], "api_path": item["path"]} for item in Fake.seen]
        direct = {"schema_version": 1, "leg": "direct_messages", "endpoint_profile": "offline_fixture", "captured_at_utc": stamp, "source_revision": digest, "client": {"name": "Python http.client", "version": platform.python_version()}, "gateway": None, "provider": {"api": "anthropic-messages", "api_version": seen["headers"].get("anthropic-version")}, "scenario": "two_rounds", "fixture_id": "tool-rounds-weather-clock-v1", "backend_model": seen["request"]["model"], "limits": {"timeout_seconds": client_timeout, "max_output_tokens": max(req["max_output_tokens"] for req in requests), "client_runs": 1}, "requests": requests, "outcome": "completed", "observations": {"terminal": "message_stop", "tool_links": ["call_1", "call_2", "call_3"], "usage": "unknown", "target_dispatches": len(Fake.seen)}}
        direct_path = outdir / "direct.json"
        direct_path.write_text(json.dumps(direct, indent=2) + "\n")
        direct_path.chmod(0o600)
        env = os.environ.copy()
        env.update({"GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off", "PESTIROUTE_SMOKE_M4_ARTIFACT_DIR": str(outdir)})
        subprocess.run(["go", "test", "-race", "-v", "-count=1", "./cmd/gateway", "-run", "^TestTranslationClientOwnedToolRounds$"], check=True, env=env, timeout=120)
        validate_pair(outdir / "direct.json", outdir / "gateway.json")
        compatible = json.loads((outdir / "direct.json").read_text())
        compatible["endpoint_profile"] = "compatible_endpoint"
        compatible["backend_model"] = "cc/claude-sonnet-5-5"
        compatible["test_only_model_override"] = True
        compatible_path = outdir / "compatible-model.json"
        compatible_path.write_text(json.dumps(compatible))
        require(load(compatible_path, "direct_messages")["backend_model"] == "cc/claude-sonnet-5-5", "compatible profile rejected explicit model override")
        compatible.pop("test_only_model_override")
        compatible_path.write_text(json.dumps(compatible))
        try:
            load(compatible_path, "direct_messages")
        except ValueError:
            pass
        else:
            raise AssertionError("compatible profile accepted an artifact without explicit test-only override marker")
        compatible["test_only_model_override"] = True
        compatible_path.write_text(json.dumps(compatible))
        compatible_gateway = json.loads((outdir / "gateway.json").read_text())
        compatible_gateway["endpoint_profile"] = "compatible_endpoint"
        compatible_gateway["backend_model"] = "cc/claude-sonnet-5-5"
        compatible_gateway["test_only_model_override"] = True
        compatible_gateway["client"] = {"name": "Go http.Client", "version": "go1.0"}
        compatible_gateway["gateway"]["route_model"] = "cc/claude-sonnet-5-5"
        compatible_gateway_path = outdir / "compatible-gateway.json"
        compatible_gateway_path.write_text(json.dumps(compatible_gateway))
        validate_pair(compatible_path, compatible_gateway_path)
        compatible["endpoint_profile"] = "official_anthropic"
        compatible.pop("test_only_model_override")
        compatible["backend_model"] = "claude-opus-5-5"
        compatible_path.write_text(json.dumps(compatible))
        require(load(compatible_path, "direct_messages")["backend_model"] == "claude-opus-5-5", "official profile did not retain pinned model")
        compatible["endpoint_profile"] = "compatible_endpoint"
        compatible["test_only_model_override"] = True
        compatible["backend_model"] = "../invalid"
        compatible_path.write_text(json.dumps(compatible))
        try:
            load(compatible_path, "direct_messages")
        except ValueError:
            pass
        else:
            raise AssertionError("artifact validator accepted an unsafe compatible model label")
        bad = json.loads((outdir / "gateway.json").read_text())
        bad["endpoint_profile"] = "compatible_endpoint"
        bad_path = outdir / "invalid-profile.json"
        bad_path.write_text(json.dumps(bad))
        try:
            validate_pair(outdir / "direct.json", bad_path)
        except ValueError:
            pass
        else:
            raise AssertionError("validator paired compatible-endpoint evidence with an offline fixture")
        bad["endpoint_profile"] = "offline_fixture"
        bad["limits"]["max_output_tokens"] = 4097
        bad_path = outdir / "invalid-cap.json"
        bad_path.write_text(json.dumps(bad))
        try:
            load(bad_path, "gateway_responses_to_messages")
        except ValueError:
            pass
        else:
            raise AssertionError("validator accepted deliberate over-cap fixture")
        bad["limits"]["max_output_tokens"] = 32
        bad["client"]["version"] = "Bearer leaked-value"
        bad_path.write_text(json.dumps(bad))
        try:
            load(bad_path, "gateway_responses_to_messages")
        except ValueError:
            pass
        else:
            raise AssertionError("validator accepted deliberate credential leak")
        print("PASS loopback direct Messages + protected PestiRoute gateway/fake Messages legs; deliberate over-cap and credential-leak fixtures rejected")
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
        require(args.output_dir is not None, "--self-test requires --output-dir")
        self_test(args.output_dir)
    else:
        require(args.direct and args.gateway, "supply direct and gateway artifact paths")
        validate_pair(args.direct, args.gateway)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        raise SystemExit(f"FAIL {exc}")
