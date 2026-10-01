#!/usr/bin/env python3
"""Bounded, streaming M1 Responses observer. Never records raw payloads or headers."""

import argparse
import http.client
import json
import os
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MAX_POSTS = 8
MAX_BODY = 65536
MAX_EVENT = 65536
ALLOWED_PATHS = {"alpha.txt", "beta.txt"}
LOCK = threading.Lock()
DATA = []
POSTS = 0
STOP = False
TRACE_PATH = "/tmp/m1-gateway-observer.json"


def safe_item(value):
    if not isinstance(value, dict):
        return {"type": "unknown"}
    return {key: value[key] for key in ("type", "id", "call_id", "name") if isinstance(value.get(key), str)}


def safe_request(body):
    request = json.loads(body)
    tools = request.get("tools", [])
    inputs = request.get("input", [])
    capabilities = []
    if request.get("stream") is True:
        capabilities.append("llm.streaming")
    if isinstance(tools, list) and tools:
        capabilities.append("llm.tools")
    if request.get("parallel_tool_calls") is True and isinstance(tools, list) and tools:
        capabilities.append("llm.tools.parallel")
    if isinstance(request.get("reasoning"), dict):
        capabilities.append("llm.reasoning")
    text = request.get("text")
    formatting = text.get("format") if isinstance(text, dict) else None
    if isinstance(formatting, dict) and formatting.get("type") in ("json_schema", "json_object"):
        capabilities.append("llm.structured_output")
    safe_tools = []
    for tool in tools if isinstance(tools, list) else []:
        if isinstance(tool, dict):
            function = tool.get("function") if isinstance(tool.get("function"), dict) else {}
            safe_tools.append({"type": tool.get("type"), "name": tool.get("name") or function.get("name")})
    return {
        "top_level_fields": sorted(request.keys()),
        "model": request.get("model"),
        "stream": request.get("stream"),
        "required_capabilities": capabilities,
        "parallel_tool_calls": request.get("parallel_tool_calls"),
        "reasoning_fields": sorted(request["reasoning"].keys()) if isinstance(request.get("reasoning"), dict) else None,
        "tools": safe_tools,
        "input_items": [safe_item(value) for value in inputs if isinstance(value, dict)] if isinstance(inputs, list) else [],
    }


class SSERecorder:
    """Retain event structure and allowlisted tool args only; event scratch is bounded."""

    def __init__(self):
        self.buffer = bytearray()
        self.events = []  # Initialize before feeding any event (regression for follow-up2 KeyError).
        self.calls = {}
        self.arguments = {}
        self.first_event_ms = None

    def feed(self, chunk, elapsed_ms):
        self.buffer.extend(chunk)
        if len(self.buffer) > MAX_EVENT:
            raise ValueError("sse_event_limit")
        while True:
            separators = [(self.buffer.find(b"\n\n"), 2), (self.buffer.find(b"\r\n\r\n"), 4)]
            separators = [(index, width) for index, width in separators if index >= 0]
            if not separators:
                break
            index, width = min(separators)
            frame = self.buffer[:index]
            remaining = self.buffer[index + width :]
            self.buffer = bytearray(remaining)
            self._record(frame, elapsed_ms)
        if len(self.buffer) > MAX_EVENT:
            raise ValueError("sse_event_limit")

    def finish(self, elapsed_ms):
        if self.buffer.strip():
            frame = bytes(self.buffer)
            self.buffer.clear()
            self._record(frame, elapsed_ms)

    def _record(self, frame, elapsed_ms):
        data = b"\n".join(line[5:].strip() for line in frame.splitlines() if line.startswith(b"data:"))
        if not data or data == b"[DONE]":
            return
        try:
            event = json.loads(data)
        except (json.JSONDecodeError, UnicodeDecodeError):
            return
        if not isinstance(event, dict):
            return
        kind = event.get("type")
        item_event = {"t_ms": round(elapsed_ms, 1), "type": kind}
        if self.first_event_ms is None:
            self.first_event_ms = round(elapsed_ms, 1)
            item_event["first_event"] = True
        for key in ("output_index", "item_id", "response_id"):
            if key in event:
                item_event[key] = event[key]
        item = event.get("item")
        if isinstance(item, dict):
            item_event["item"] = safe_item(item)
            if item.get("type") == "function_call" and isinstance(item.get("id"), str):
                item_id = item["id"]
                self.calls[item_id] = {key: item.get(key) for key in ("id", "call_id", "name")}
                self.arguments.setdefault(item_id, "")
        item_id = event.get("item_id")
        if kind == "response.function_call_arguments.delta" and isinstance(item_id, str):
            delta = event.get("delta")
            if isinstance(delta, str):
                self.arguments[item_id] = self.arguments.get(item_id, "") + delta
        if kind == "response.function_call_arguments.done" and isinstance(item_id, str):
            arguments = event.get("arguments")
            if isinstance(arguments, str):
                self.arguments[item_id] = arguments
        if kind in ("response.completed", "response.incomplete", "response.failed"):
            response = event.get("response")
            if isinstance(response, dict):
                item_event["status"] = response.get("status", kind)
                usage = response.get("usage")
                if isinstance(usage, dict):
                    item_event["usage"] = usage
                output = response.get("output")
                if isinstance(output, list):
                    item_event["output_items"] = [safe_item(value) for value in output if isinstance(value, dict)]
                for output_item in output if isinstance(output, list) else []:
                    if isinstance(output_item, dict) and output_item.get("type") == "function_call":
                        output_id = output_item.get("id")
                        args = output_item.get("arguments")
                        if isinstance(output_id, str) and isinstance(args, str):
                            self.calls.setdefault(output_id, {key: output_item.get(key) for key in ("id", "call_id", "name")})
                            self.arguments[output_id] = args
        self.events.append(item_event)

    def safe_tool_calls(self):
        result = []
        for item_id, metadata in self.calls.items():
            arguments = self.arguments.get(item_id, "")
            try:
                path = json.loads(arguments).get("path")
            except (json.JSONDecodeError, AttributeError):
                path = None
            safe_arguments = arguments if metadata.get("name") == "read" and path in ALLOWED_PATHS else "[omitted]"
            result.append({"item_id": item_id, "call_id": metadata.get("call_id"), "name": metadata.get("name"), "arguments": safe_arguments})
        return result


def self_test():
    # Include missing optional keys and split delimiters to reproduce the previous recorder crash offline.
    fixture = b"".join(
        b"data: " + json.dumps(event, separators=(",", ":")).encode() + b"\r\n\r\n"
        for event in (
            {"type": "response.created"},
            {"type": "response.output_text.delta", "delta": "PRIVATE_FIXTURE_TEXT"},
            {"type": "response.future_event"},
            {"type": "response.output_item.added", "item": {"id": "fc-1", "type": "function_call", "call_id": "call-1", "name": "read"}},
            {"type": "response.function_call_arguments.delta", "item_id": "fc-1", "delta": '{"path":"alpha.txt"}'},
            {"type": "response.function_call_arguments.done", "item_id": "fc-1", "arguments": '{"path":"alpha.txt"}'},
            {"type": "response.completed", "response": {"status": "completed", "usage": {"total_tokens": 7}, "output": [{"type": "function_call", "id": "fc-1", "call_id": "call-1", "name": "read"}]}},
        )
    )
    recorder = SSERecorder()
    for start in range(0, len(fixture), 13):
        recorder.feed(fixture[start : start + 13], float(start))
    recorder.finish(float(len(fixture)))
    assert len(recorder.events) == 7
    assert recorder.events[0]["type"] == "response.created" and recorder.events[0]["first_event"] is True
    assert recorder.events[1]["type"] == "response.output_text.delta"
    assert "item_id" not in recorder.events[1] and "output_index" not in recorder.events[1]
    assert recorder.events[2]["type"] == "response.future_event"
    assert recorder.events[-1]["usage"]["total_tokens"] == 7
    assert recorder.safe_tool_calls() == [{"item_id": "fc-1", "call_id": "call-1", "name": "read", "arguments": '{"path":"alpha.txt"}'}]
    assert "PRIVATE_FIXTURE_TEXT" not in json.dumps(recorder.events)
    print("observer self-test passed: split SSE, missing optional keys, safe arguments, usage")


def persist(entry):
    with LOCK:
        DATA.append(entry)
        fd = os.open(TRACE_PATH, os.O_CREAT | os.O_WRONLY | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w") as output:
            json.dump(DATA, output, indent=2)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Length", "2")
        self.end_headers()
        self.wfile.write(b"ok")

    def do_POST(self):
        global POSTS, STOP
        length = int(self.headers.get("Content-Length", "0"))
        entry = {"method": "POST", "path": self.path, "request_body_bytes": length, "events": [], "tool_calls": []}
        with LOCK:
            locked_or_full = STOP or POSTS >= MAX_POSTS
            oversized = length > MAX_BODY
            if not locked_or_full and not oversized:
                POSTS += 1
                entry["post"] = POSTS
            if oversized:
                STOP = True
        if locked_or_full or oversized:
            entry["observer_error"] = "request_body_limit" if oversized else "observer_locked_or_post_limit"
            persist(entry)
            self.send_error(413 if oversized else 429)
            return
        try:
            request_body = self.rfile.read(length)
            if self.headers.get("Authorization") != "Bearer " + os.environ.get("PESTIROUTE_UPSTREAM_BEARER", ""):
                STOP = True
                entry["observer_error"] = "gateway_credential_mismatch"
                persist(entry)
                self.send_error(401)
                return
            entry.update(safe_request(request_body))
            if self.path != "/v1/responses" or entry.get("model") != "gpt-5.4-mini" or entry.get("stream") is not True:
                STOP = True
                entry["observer_error"] = "unexpected_model_path_or_stream"
                persist(entry)
                self.send_error(400)
                return
            connection = http.client.HTTPSConnection("api.openai.com", 443, timeout=120)
            request_start = time.monotonic()
            # The opaque request body is passed byte-for-byte; only gateway auth is replaced with the provider key.
            connection.request("POST", "/v1/responses", body=request_body, headers={
                "Authorization": "Bearer " + os.environ["OPENAI_API_KEY"],
                "Content-Type": "application/json",
                "Accept": "text/event-stream",
                "Accept-Encoding": "identity",
            })
            response = connection.getresponse()
            entry["status"] = response.status
            entry["response_content_type"] = response.getheader("Content-Type")
            is_sse = "text/event-stream" in (entry["response_content_type"] or "").lower()
            recorder = SSERecorder()
            if response.status >= 400:
                STOP = True
                entry["stop_after_upstream_error"] = True
            self.send_response(response.status)
            for header in ("Content-Type", "Cache-Control", "Retry-After"):
                value = response.getheader(header)
                if value:
                    self.send_header(header, value)
            self.send_header("Connection", "close")
            self.end_headers()
            first_byte = True
            while True:
                chunk = response.read1(2048)
                if not chunk:
                    break
                elapsed_ms = (time.monotonic() - request_start) * 1000
                if first_byte:
                    entry["first_byte_ms"] = round(elapsed_ms, 1)
                    first_byte = False
                self.wfile.write(chunk)
                self.wfile.flush()
                if is_sse:
                    event_count = len(recorder.events)
                    recorder.feed(chunk, elapsed_ms)
                    if any(event.get("type") in ("response.failed", "response.incomplete") for event in recorder.events[event_count:]):
                        STOP = True
                        entry["stop_after_terminal_error"] = True
            if is_sse:
                recorder.finish((time.monotonic() - request_start) * 1000)
                entry["events"] = recorder.events
                entry["first_event_ms"] = recorder.first_event_ms
                entry["tool_calls"] = recorder.safe_tool_calls()
                entry["usage_reported"] = any("usage" in event for event in recorder.events)
            connection.close()
            persist(entry)
        except Exception as error:
            STOP = True
            entry["observer_error"] = str(error) if str(error) == "sse_event_limit" else type(error).__name__
            persist(entry)
            try:
                self.connection.shutdown(1)
            except OSError:
                pass


def main():
    global TRACE_PATH
    parser = argparse.ArgumentParser()
    parser.add_argument("--self-test", action="store_true")
    parser.add_argument("--trace", default=TRACE_PATH)
    args = parser.parse_args()
    if args.self_test:
        self_test()
        return
    if not os.environ.get("OPENAI_API_KEY") or not os.environ.get("PESTIROUTE_UPSTREAM_BEARER"):
        parser.error("OPENAI_API_KEY and PESTIROUTE_UPSTREAM_BEARER must be set in the environment")
    TRACE_PATH = args.trace
    ThreadingHTTPServer(("127.0.0.1", 18082), Handler).serve_forever()


if __name__ == "__main__":
    main()
