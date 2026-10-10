#!/usr/bin/env python3
"""Safely supervise the pinned official CLI device login; never relay raw output."""

import argparse
import json
import os
import re
import selectors
import signal
import stat
import subprocess
import sys
import tempfile
import time
from contextlib import redirect_stdout
from io import StringIO
from pathlib import Path
from urllib.parse import urlsplit
from unittest import mock

CLI = Path("/tmp/opencode/official-codex-cli/node_modules/.bin/codex")
PINNED_BINARY = Path("/tmp/opencode/official-codex-cli/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex")
HOME = Path("/tmp/opencode/official-codex-auth-v3")
STATE = Path("/tmp/opencode/official-codex-session-v3")
STATUS = STATE / "status.json"
READY = STATE / "ready.json"
READY_DATA = b'{"schema_version":1,"session":"M5.1-050-v3","cli_version":"0.162.1"}\n'
MAX_OUTPUT = 65536
MAX_LINE = 4096
MAX_SECONDS = 840
PRIVATE_TEXT = re.compile(r"oauth|authorization\s+code|access[_ -]?token|refresh[_ -]?token|id[_ -]?token|bearer\s+|eyJ[A-Za-z0-9_-]{8,}\.", re.I)
STDERR_SENSITIVE = re.compile(r"oauth|authorization|access.?token|refresh.?token|id.?token|credential|secret|bearer|\b(?:error|failed|failure|denied|invalid)\b|\b[A-Z0-9]{4}-[A-Z0-9]{4}\b", re.I)
ANSI_CSI = re.compile(r"\x1b\[[0-?]*[ -/]*m")
PROMPT_LINE = "2. Enter this one-time code (expires in 15 minutes)"
MAX_CODE_BLANK_LINES = 3
DEVICE_CODE = re.compile(r"(?=.{6,20}\Z)(?=[A-Za-z0-9-]+\Z)(?!-)[A-Za-z0-9]+(?:-[A-Za-z0-9]+)?(?<!-)")


def safe_dir(path, create=False):
    if create:
        path.mkdir(mode=0o700, parents=True, exist_ok=True)
    st = path.lstat()
    if not path.is_dir() or path.is_symlink() or st.st_uid != os.getuid() or stat.S_IMODE(st.st_mode) != 0o700:
        raise ValueError("UNSAFE_PRIVATE_DIRECTORY")


def write_status(status, **values):
    data = {"status": status, **values}
    tmp = STATE / ".status.tmp"
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as f:
        json.dump(data, f, separators=(",", ":"))
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, STATUS)
    os.chmod(STATUS, 0o600)


def validate_ready():
    st = READY.lstat()
    if not READY.is_file() or READY.is_symlink() or st.st_uid != os.getuid() or st.st_mode & 0o077 or st.st_size > 256:
        raise ValueError("V3_SESSION_NOT_READY")
    if READY.read_bytes() != READY_DATA:
        raise ValueError("V3_SESSION_NOT_READY")


def prepare():
    safe_dir(HOME, create=True)
    safe_dir(STATE, create=True)
    if any(HOME.iterdir()):
        raise ValueError("AUTH_HOME_NOT_FRESH")
    entries = list(STATE.iterdir())
    if entries:
        validate_ready()
        if len(entries) != 1 or entries[0] != READY:
            raise ValueError("SESSION_STATE_NOT_FRESH")
    else:
        fd = os.open(READY, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "wb") as f:
            f.write(READY_DATA)
            f.flush()
            os.fsync(f.fileno())
    print("READY")


def load_status():
    st = STATUS.lstat()
    if not STATUS.is_file() or STATUS.is_symlink() or st.st_uid != os.getuid() or st.st_mode & 0o077 or st.st_size > 4096:
        raise ValueError("STATUS_UNSAFE")
    data = json.loads(STATUS.read_text(encoding="utf-8"))
    if type(data) is not dict or set(data) - {"status", "url", "device_code", "pid", "exit", "parse_shape"}:
        raise ValueError("STATUS_INVALID")
    if data.get("status") not in {"starting", "waiting", "code_ready", "complete", "cancelled", "failed"}:
        raise ValueError("STATUS_INVALID")
    if "exit" in data and data["exit"] not in {"CANCELLED", "FLOW_TIMEOUT", "OUTPUT_LIMIT", "OUTPUT_LINE_LIMIT", "INVALID_URL", "INVALID_CODE", "INVALID_CODE_LINE", "UNSAFE_OUTPUT", "UNSAFE_CONTROL", "UNSAFE_STDERR", "CODE_WITHOUT_VERIFICATION_URL", "PROMPT_CONTEXT_EXPIRED", "CLI_EXIT_NONZERO", "CLI_START_FAILED", "AUTH_HOME_NOT_FRESH", "WORKER_FAILED"}:
        raise ValueError("STATUS_INVALID")
    if "url" in data and data["url"] != "https://auth.openai.com/codex/device":
        raise ValueError("STATUS_INVALID")
    if "device_code" in data and not DEVICE_CODE.fullmatch(data["device_code"]):
        raise ValueError("STATUS_INVALID")
    if "parse_shape" in data:
        shape = data["parse_shape"]
        if (type(shape) is not dict or set(shape) != {"line_length", "blank_lines", "segment_count", "first_segment_length", "last_segment_length"}
                or any(type(v) is not int or v < 0 or v > MAX_LINE for v in shape.values())):
            raise ValueError("STATUS_INVALID")
    return data


class DevicePromptParser:
    """Parse the pinned CLI's multiline device prompt, never generic codes."""

    def __init__(self):
        self.pending = ""
        self.url = None
        self.url_lines = 0
        self.expect_code = False
        self.code_lines = 0
        self.code_blank_lines = 0
        self.failure_shape = None
        self.total = 0
        self.skip_lf = False

    def _line(self, raw):
        line = ANSI_CSI.sub("", raw)
        if "\x1b" in line or any(ord(c) < 0x20 and c not in "\t" for c in line):
            return None, None, "UNSAFE_CONTROL"
        if PRIVATE_TEXT.search(line):
            return None, None, "UNSAFE_OUTPUT"
        links = re.findall(r"(?:https?|ftp)://[^\s\"'<>]+", line)
        for link in links:
            candidate = link.rstrip(".,);]")
            try:
                parsed = urlsplit(candidate)
                valid = (parsed.scheme == "https" and parsed.hostname == "auth.openai.com"
                         and parsed.path == "/codex/device" and not parsed.query and not parsed.fragment
                         and not parsed.username and not parsed.password and parsed.port is None)
            except ValueError:
                valid = False
            if not valid or len(links) != 1:
                return None, None, "INVALID_URL"
            self.url = "https://auth.openai.com/codex/device"
            self.url_lines = 0
        if self.url:
            self.url_lines += 1
            if self.url_lines > 8:
                return None, None, "PROMPT_CONTEXT_EXPIRED"
        if line.strip() == PROMPT_LINE:
            if not self.url:
                return None, None, "CODE_WITHOUT_VERIFICATION_URL"
            self.expect_code = True
            self.code_lines = 0
            self.code_blank_lines = 0
            return None, None, None
        if not self.expect_code:
            return None, None, None
        self.code_lines += 1
        short = line.strip()
        if not short:
            self.code_blank_lines += 1
            if self.code_blank_lines <= MAX_CODE_BLANK_LINES:
                return None, None, None
        elif DEVICE_CODE.fullmatch(short):
            self.expect_code = False
            return self.url, short.upper(), None
        pieces = short.split("-")
        self.failure_shape = {
            "line_length": len(short),
            "blank_lines": self.code_blank_lines,
            "segment_count": len(pieces),
            "first_segment_length": len(pieces[0]),
            "last_segment_length": len(pieces[-1]),
        }
        return None, None, "INVALID_CODE_LINE"

    def feed(self, chunk):
        self.total += len(chunk)
        if self.total > MAX_OUTPUT:
            return None, None, "OUTPUT_LIMIT"
        incoming = chunk.decode("utf-8", "replace")
        if self.skip_lf:
            if incoming.startswith("\n"):
                incoming = incoming[1:]
            self.skip_lf = False
        self.pending += incoming
        if len(self.pending) > MAX_LINE and "\n" not in self.pending and "\r" not in self.pending:
            return None, None, "OUTPUT_LINE_LIMIT"
        found_code = None
        while True:
            ends = [i for i in (self.pending.find("\n"), self.pending.find("\r")) if i >= 0]
            if not ends:
                break
            i = min(ends)
            separator = self.pending[i]
            line, self.pending = self.pending[:i], self.pending[i + 1:]
            if separator == "\r":
                if self.pending.startswith("\n"):
                    self.pending = self.pending[1:]
                elif not self.pending:
                    self.skip_lf = True
            url, code, error = self._line(line)
            if error:
                return None, None, error
            if code:
                found_code = code
        return self.url, found_code, None

    def finish(self):
        if not self.pending:
            return self.url, None, None
        line, self.pending = self.pending, ""
        return self._line(line)


def capture_process(child, cancel_file, publish):
    sel = selectors.DefaultSelector()
    sel.register(child.stdout, selectors.EVENT_READ, "stdout")
    sel.register(child.stderr, selectors.EVENT_READ, "stderr")
    stderr_seen = 0
    stderr_tail = ""
    parser = DevicePromptParser()
    url = code = None
    published = False
    code_ready_at = None
    start = time.monotonic()
    outcome = None
    try:
        while sel.get_map() or child.poll() is None:
            if cancel_file.exists():
                outcome = "CANCELLED"
                terminate_child(child)
                break
            if time.monotonic() - start > MAX_SECONDS:
                outcome = "FLOW_TIMEOUT"
                terminate_child(child)
                break
            for key, _ in sel.select(0.2):
                chunk = os.read(key.fileobj.fileno(), 4096)
                if not chunk:
                    sel.unregister(key.fileobj)
                    continue
                if key.data == "stderr":
                    stderr_seen += len(chunk)
                    if stderr_seen > MAX_OUTPUT:
                        outcome = "OUTPUT_LIMIT"
                        terminate_child(child)
                        break
                    stderr_tail = (stderr_tail + chunk.decode("utf-8", "replace"))[-MAX_LINE:]
                    stderr_scan = ANSI_CSI.sub("", stderr_tail)
                    if "\x1b" in stderr_scan or STDERR_SENSITIVE.search(stderr_scan) or re.search(r"(?:https?|ftp)://", stderr_scan):
                        outcome = "UNSAFE_STDERR"
                        terminate_child(child)
                        break
                    continue
                new_url, new_code, error = parser.feed(chunk)
                if error:
                    outcome = error
                    terminate_child(child)
                    break
                if new_code:
                    url, code = new_url, new_code
                    code_ready_at = time.monotonic()
            if outcome:
                break
            if (url and code and not published and code_ready_at is not None
                    and (not sel.get_map() and child.poll() is not None or time.monotonic() - code_ready_at >= 0.2)):
                publish(url, code)
                published = True
        if not outcome and not code:
            url, code, outcome = parser.finish()
            if code and not published and not outcome:
                publish(url, code)
        if child.poll() is None:
            try:
                child.wait(timeout=5)
            except subprocess.TimeoutExpired:
                kill_child(child)
        return outcome, child.wait(), url, code, parser.failure_shape
    finally:
        sel.close()
        child.stdout.close()
        child.stderr.close()


def terminate_child(child):
    try:
        os.killpg(child.pid, signal.SIGTERM)
    except ProcessLookupError:
        pass


def kill_child(child):
    try:
        os.killpg(child.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass


def worker():
    safe_dir(HOME)
    safe_dir(STATE)
    validate_ready()
    if any(HOME.iterdir()):
        write_status("failed", exit="AUTH_HOME_NOT_FRESH")
        (STATE / "run.lock").unlink(missing_ok=True)
        return 2
    env = os.environ.copy()
    for name in ("OPENAI_API_KEY", "CODEX_ACCESS_TOKEN", "OPENAI_BASE_URL", "OPENAI_API_BASE"):
        env.pop(name, None)
    env["CODEX_HOME"] = str(HOME)
    try:
        child = subprocess.Popen([str(CLI), "login", "--device-auth"], stdin=subprocess.DEVNULL,
                                 stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env,
                                 start_new_session=True)
    except Exception:
        write_status("failed", exit="CLI_START_FAILED")
        return 2
    write_status("waiting", pid=os.getpid())
    cancel_file = STATE / "cancel"
    try:
        outcome, exit_code, url, code, parse_shape = capture_process(
            child, cancel_file,
            lambda public_url, short_code: write_status("code_ready", url=public_url, device_code=short_code, pid=os.getpid()),
        )
        if outcome:
            write_status("cancelled" if outcome == "CANCELLED" else "failed", exit=outcome,
                         **({"parse_shape": parse_shape} if parse_shape else {}))
        elif exit_code == 0:
            write_status("complete", **({"url": url, "device_code": code} if url and code else {}))
        else:
            write_status("failed", exit="CLI_EXIT_NONZERO")
        return 0 if exit_code == 0 and not outcome else 1
    finally:
        try:
            cancel_file.unlink()
        except FileNotFoundError:
            pass
        try:
            (STATE / "run.lock").unlink()
        except FileNotFoundError:
            pass


def start():
    if not CLI.is_file() or not os.access(CLI, os.X_OK):
        raise ValueError("PINNED_CLI_UNAVAILABLE")
    try:
        version = subprocess.run([str(CLI), "--version"], stdin=subprocess.DEVNULL,
                                 stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                 timeout=5, check=False)
    except Exception:
        raise ValueError("PINNED_CLI_UNAVAILABLE")
    if version.returncode != 0 or len(version.stdout) > 128 or version.stdout.decode("utf-8", "replace").strip() != "codex-cli 0.162.1":
        raise ValueError("PINNED_CLI_VERSION_MISMATCH")
    safe_dir(HOME, create=True)
    safe_dir(STATE, create=True)
    validate_ready()
    if any(HOME.iterdir()):
        raise ValueError("AUTH_HOME_NOT_FRESH")
    try:
        lock_fd = os.open(STATE / "run.lock", os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    except FileExistsError:
        raise ValueError("LOGIN_ALREADY_RUNNING_OR_REVIEW_REQUIRED")
    try:
        fd = os.open(STATE / "attempted", os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        os.close(fd)
    except FileExistsError:
        os.close(lock_fd)
        (STATE / "run.lock").unlink()
        raise ValueError("LOGIN_ATTEMPT_ALREADY_MADE_REVIEW_REQUIRED")
    try:
        (STATE / "cancel").unlink()
    except FileNotFoundError:
        pass
    write_status("starting")
    try:
        proc = launch_worker()
    except Exception:
        os.close(lock_fd)
        write_status("failed", exit="WORKER_FAILED")
        (STATE / "run.lock").unlink(missing_ok=True)
        raise
    os.close(lock_fd)
    if load_status().get("status") == "starting":
        write_status("starting", pid=proc.pid)
    print("STARTED")


def launch_worker():
    return subprocess.Popen([sys.executable, str(Path(__file__).resolve()), "_worker"],
                            stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                            stderr=subprocess.DEVNULL, start_new_session=True,
                            close_fds=True)


def cancel():
    safe_dir(STATE)
    if not (STATE / "run.lock").exists():
        print("NOT_RUNNING")
        return
    fd = os.open(STATE / "cancel", os.O_WRONLY | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    os.close(fd)
    os.chmod(STATE / "cancel", 0o600)
    print("CANCEL_REQUESTED")


def poll():
    safe_dir(STATE)
    data = load_status()
    print(data["status"])
    if "url" in data and "device_code" in data:
        print(f"Verification URL: {data['url']}")
        print(f"Device code: {data['device_code']}")
    if "exit" in data:
        print(f"Diagnostic: {data['exit']}")
    if "parse_shape" in data:
        shape = data["parse_shape"]
        fields = ("line_length", "blank_lines", "segment_count", "first_segment_length", "last_segment_length")
        print("Parse shape: " + " ".join(f"{key}={shape[key]}" for key in fields))


def self_test():
    verify_pinned_prompt_source()
    fixture = (b"\nWelcome to Codex [v\x1b[90m0.162.1\x1b[0m]\n"
               b"\x1b[90mOpenAI's command-line coding agent\x1b[0m\n\n"
               b"Follow these steps to sign in with ChatGPT using device code authorization:\n\n"
               b"1. Open this link in your browser and sign in to your account\n"
               b"   \x1b[94mhttps://auth.openai.com/codex/device\x1b[0m\n\n"
               b"2. Enter this one-time code \x1b[90m(expires in 15 minutes)\x1b[0m\n"
               b"   \x1b[94mABCD-EF23\x1b[0m\n\n"
               b"\x1b[90mContinue only if you started this login in Codex. If a website or another person gave you this code, cancel.\x1b[0m\n")
    for transcript in (fixture, fixture.replace(b"\n", b"\r\n")):
        parser = DevicePromptParser()
        parsed = (None, None)
        for byte in transcript:
            url, code, error = parser.feed(bytes((byte,)))
            if error:
                raise AssertionError("pinned CLI byte-split prompt fixture rejected")
            if code:
                parsed = (url, code)
        if parsed != ("https://auth.openai.com/codex/device", "ABCD-EF23"):
            raise AssertionError("pinned CLI prompt fixture did not yield URL and code")
    for code in ("ABC123XYZ", "ABCD-EF23", "AB12-CD3456", "ABCDEF"):
        transcript = ("https://auth.openai.com/codex/device\n"
                      "2. Enter this one-time code (expires in 15 minutes)\n\n\n"
                      f"{code}\n").encode()
        parser = DevicePromptParser()
        url, found, error = parser.feed(transcript)
        if error or (url, found) != ("https://auth.openai.com/codex/device", code):
            raise AssertionError("bounded official device-code shape/blank-line fixture rejected")
    malformed = DevicePromptParser()
    _, _, error = malformed.feed(b"https://auth.openai.com/codex/device\n"
                                 b"2. Enter this one-time code (expires in 15 minutes)\n\n"
                                 b"ABCD_$PRIVATE\n")
    if (error != "INVALID_CODE_LINE" or malformed.failure_shape != {
            "line_length": 13, "blank_lines": 1, "segment_count": 1,
            "first_segment_length": 13, "last_segment_length": 13}):
        raise AssertionError("failed parse diagnostic must contain counts only")
    bad_prompts = (
        b"https://evil.example/device\n2. Enter this one-time code (expires in 15 minutes)\nABCD-EF23\n",
        b"https://auth.openai.com/codex/device?next=evil\n2. Enter this one-time code (expires in 15 minutes)\nABCD-EF23\n",
        b"https://auth.openai.com/codex/device\nABCD-EF23\n",
        b"https://auth.openai.com/codex/device\n2. Enter this one-time code (expires in 15 minutes; token=synthetic-secret)\nABCD-EF23\n",
        b"https://auth.openai.com/codex/device\n2. Enter this one-time code (expires in 15 minutes)\nBearer synthetic-secret\n",
        b"https://auth.openai.com/codex/device\n2. Enter this one-time code (expires in 15 minutes)\nABCD-EF23\nhttps://evil.example/warn\n",
        b"https://auth.openai.com/codex/device\x1b]8;;https://evil.example\n2. Enter this one-time code (expires in 15 minutes)\nABCD-EF23\n",
        b"https://auth.openai.com/codex/device\x1b[2J\n2. Enter this one-time code (expires in 15 minutes)\nABCD-EF23\n",
        b"https://auth.openai.com/codex/device\n2. Enter this one-time code (expires in 14 minutes)\nABCD-EF23\n",
    )
    for bad in bad_prompts:
        parser = DevicePromptParser()
        _, code, error = parser.feed(bad)
        if not error and code is None:
            _, code, error = parser.finish()
        if not error and code is not None:
            raise AssertionError("poisoned or generic-code prompt accepted")
    too_large = DevicePromptParser()
    if too_large.feed(b"x" * (MAX_LINE + 1))[2] != "OUTPUT_LINE_LIMIT":
        raise AssertionError("line bound self-test failed")
    too_much = DevicePromptParser()
    if too_much.feed(b"\n" * (MAX_OUTPUT + 1))[2] != "OUTPUT_LIMIT":
        raise AssertionError("total output bound self-test failed")
    fake_script = "import os; os.write(1,bytes.fromhex('" + fixture.hex() + "')); os.write(2,b'diagnostic-only')"
    fake = subprocess.Popen([sys.executable, "-c", fake_script], stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
    public = []
    outcome, exit_code, _, _, _ = capture_process(fake, Path("/nonexistent-cancel"), lambda u, c: public.append((u, c)))
    if outcome or exit_code or public != [("https://auth.openai.com/codex/device", "ABCD-EF23")]:
        raise AssertionError("private subprocess capture self-test failed")
    poison_script = "import os; os.write(1,bytes.fromhex('" + fixture.hex() + "')); os.write(2,b'\\x1b[1mBea'); os.write(2,b'rer synthetic-private-token\\x1b[0m')"
    poison = subprocess.Popen([sys.executable, "-c", poison_script], stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
    withheld = []
    poisoned, _, _, _, _ = capture_process(poison, Path("/nonexistent-cancel"), lambda u, c: withheld.append((u, c)))
    if poisoned != "UNSAFE_STDERR" or withheld:
        raise AssertionError("sensitive stderr was not suppressed fail-closed")
    bad = subprocess.Popen([sys.executable, "-c", "print('https://evil.example/authorize')"], stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
    bad_outcome, _, _, _, _ = capture_process(bad, Path("/nonexistent-cancel"), lambda *_: None)
    if bad_outcome != "INVALID_URL":
        raise AssertionError("unsafe subprocess URL was not rejected")
    global MAX_SECONDS
    old_timeout = MAX_SECONDS
    MAX_SECONDS = 0
    timed = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(10)"], stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
    try:
        timed_out, _, _, _, _ = capture_process(timed, Path("/nonexistent-cancel"), lambda *_: None)
        if timed_out != "FLOW_TIMEOUT":
            raise AssertionError("bounded-flow timeout self-test failed")
    finally:
        MAX_SECONDS = old_timeout
    cancel_path = Path(__file__).with_name(".m050-self-test-cancel")
    cancel_path.touch(mode=0o600)
    slow = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(10)"], stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
    try:
        cancelled, _, _, _, _ = capture_process(slow, cancel_path, lambda *_: None)
        if cancelled != "CANCELLED":
            raise AssertionError("subprocess cancellation self-test failed")
    finally:
        cancel_path.unlink(missing_ok=True)
    self_test_v3_paths()
    print("PASS pinned 0.162.1 prompt parsing; v3 private preparation, nonempty-home/attempt/lock gates and arbitrary-path rejection; zero worker launch on rejected starts; poison, bounds, timeout and cancel checks passed")


def self_test_v3_paths():
    with tempfile.TemporaryDirectory(prefix="m050-paths-selftest-", dir="/tmp/opencode") as temp:
        base = Path(temp)
        home, state = base / "auth-v3", base / "state-v3"
        module = sys.modules[__name__]
        with mock.patch.multiple(module, HOME=home, STATE=state, STATUS=state / "status.json",
                                 READY=state / "ready.json"):
            with redirect_stdout(StringIO()):
                prepare()
            if home.stat().st_mode & 0o777 != 0o700 or state.stat().st_mode & 0o777 != 0o700 or any(home.iterdir()):
                raise AssertionError("v3 preparation did not create private empty auth home")
            if READY.read_bytes() != READY_DATA or READY.stat().st_mode & 0o777 != 0o600:
                raise AssertionError("v3 readiness metadata invalid")
            spawned = []
            with mock.patch.object(module, "launch_worker", side_effect=lambda: spawned.append(True)):
                (home / "nonempty-sentinel").touch(mode=0o600)
                try:
                    start()
                except ValueError as error:
                    if str(error) != "AUTH_HOME_NOT_FRESH":
                        raise
                else:
                    raise AssertionError("nonempty v3 auth home accepted")
                (home / "nonempty-sentinel").unlink()
                (state / "attempted").touch(mode=0o600)
                try:
                    start()
                except ValueError as error:
                    if str(error) != "LOGIN_ATTEMPT_ALREADY_MADE_REVIEW_REQUIRED":
                        raise
                else:
                    raise AssertionError("existing one-attempt marker accepted")
                if (state / "run.lock").exists():
                    raise AssertionError("attempt-marker collision left a login lock")
                (state / "attempted").unlink()
                (state / "run.lock").touch(mode=0o600)
                try:
                    start()
                except ValueError as error:
                    if str(error) != "LOGIN_ALREADY_RUNNING_OR_REVIEW_REQUIRED":
                        raise
                else:
                    raise AssertionError("existing login lock accepted")
                if spawned:
                    raise AssertionError("failed v3 preflight spawned auth worker")
        rejected_path = subprocess.run([sys.executable, str(Path(__file__).resolve()), "start",
                                        "--home", str(base / "arbitrary")],
                                       stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                       stderr=subprocess.PIPE, check=False, timeout=5)
        if rejected_path.returncode == 0 or b"unrecognized arguments" not in rejected_path.stderr:
            raise AssertionError("arbitrary auth-home selector was accepted")


def verify_pinned_prompt_source():
    version = subprocess.run([str(CLI), "--version"], stdin=subprocess.DEVNULL,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                              timeout=5, check=False)
    if version.returncode != 0 or version.stdout.decode("utf-8", "replace").strip() != "codex-cli 0.162.1" or not PINNED_BINARY.is_file():
        raise AssertionError("pinned 0.162.1 prompt source unavailable")
    literals = (b"ChatGPT using device code authorization:",
                b"1. Open this link in your browser and sign in to your account",
                b"2. Enter this one-time code ", b"(expires in 15 minutes)")
    offsets = []
    for literal in literals:
        result = subprocess.run(["grep", "-aobF", "-m1", literal.decode("ascii"), str(PINNED_BINARY)],
                                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                timeout=20, check=False)
        if result.returncode != 0:
            raise AssertionError("pinned binary prompt literal missing")
        offsets.append(int(result.stdout.split(b":", 1)[0]))
    if offsets != sorted(offsets) or offsets[3] - offsets[2] > 64:
        raise AssertionError("pinned prompt literals/order do not match expected prompt")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("command", nargs="?", choices=("prepare", "start", "poll", "cancel", "_worker"))
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    if args.self_test:
        self_test()
        return 0
    cmd = args.command
    if cmd is None:
        parser.error("command is required")
    try:
        if cmd == "start":
            start()
            return 0
        if cmd == "prepare":
            prepare()
            return 0
        if cmd == "poll":
            poll()
            return 0
        if cmd == "cancel":
            cancel()
            return 0
        return worker()
    except Exception:
        if cmd == "_worker":
            try:
                safe_dir(STATE)
                write_status("failed", exit="WORKER_FAILED")
                (STATE / "run.lock").unlink(missing_ok=True)
            except Exception:
                pass
        print("AUTH_WRAPPER_ERROR")
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
