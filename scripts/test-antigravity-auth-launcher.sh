#!/bin/sh
set -eu
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/pestiroute-auth-launcher.XXXXXX")
trap 'rm -rf -- "$WORK"' 0 HUP INT TERM
mkdir "$WORK/root"
: >"$WORK/root/gateway.db"
: >"$WORK/root/master.key"
cat >"$WORK/stub" <<'STUB'
#!/bin/sh
{
  printf 'secret=%s\n' "${PESTIROUTE_ANTIGRAVITY_CLIENT_SECRET-}"
  printf 'client_id=%s\n' "${PESTIROUTE_ANTIGRAVITY_CLIENT_ID-}"
  printf 'transport=%s\n' "${PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT-}"
  printf 'arg=<%s>\n' "$@"
} >"$CAPTURE"
env >"$ENV_CAPTURE"
cat >"$INPUT_CAPTURE"
printf 'called\n' >>"$CALLS"
exit "${STUB_EXIT:-0}"
STUB
chmod +x "$WORK/stub"

export TMPDIR=$WORK
export PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT=$WORK/root
export GATEWAY_BIN=$WORK/stub
export CAPTURE=$WORK/capture
export ENV_CAPTURE=$WORK/environment
export CALLS=$WORK/calls
export INPUT_CAPTURE=$WORK/input
export PESTIROUTE_ANTIGRAVITY_CLIENT_SECRET=synthetic-secret
export PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT=1
export REAL_STTY=$(command -v stty)
export STTY_PID_FILE=$WORK/stty-pid
unset PESTIROUTE_ANTIGRAVITY_CLIENT_ID

assert_debug_order() {
  python3 - "$CAPTURE" "$1" <<'PY'
import sys
args = [line.rstrip("\n")[5:-1] for line in open(sys.argv[1]) if line.startswith("arg=<")]
index = args.index("auth")
if args[index:index + 3] != ["auth", "--debug", sys.argv[2]]:
    raise AssertionError("--debug is not immediately after auth: " + repr(args))
PY
}

"$SCRIPT_DIR/antigravity-auth.sh" --debug start
assert_debug_order start
grep -Fx 'secret=synthetic-secret' "$CAPTURE" >/dev/null
grep -Fx 'transport=1' "$CAPTURE" >/dev/null
grep -Fx 'arg=<auth>' "$CAPTURE" >/dev/null
grep -Fx 'arg=<--debug>' "$CAPTURE" >/dev/null
grep -Fx 'arg=<start>' "$CAPTURE" >/dev/null
grep -Fx "arg=<$WORK/root/gateway.db>" "$CAPTURE" >/dev/null
grep -Fx "arg=<$WORK/root/master.key>" "$CAPTURE" >/dev/null
! grep -F 'synthetic-secret' "$CAPTURE" | grep -F 'arg=' >/dev/null

"$SCRIPT_DIR/antigravity-auth.sh" --debug continue safe-session </dev/null
assert_debug_order continue
grep -Fx 'arg=<--debug>' "$CAPTURE" >/dev/null
grep -Fx 'arg=<continue>' "$CAPTURE" >/dev/null
grep -Fx 'arg=<safe-session>' "$CAPTURE" >/dev/null
grep -Fx 'arg=<--callback-stdin>' "$CAPTURE" >/dev/null
! grep -Fx 'arg=<--allow-onboard>' "$CAPTURE" >/dev/null
grep -Fx "arg=<$WORK/root/gateway.db>" "$CAPTURE" >/dev/null
grep -Fx "arg=<$WORK/root/master.key>" "$CAPTURE" >/dev/null
[ ! -s "$INPUT_CAPTURE" ]

PESTIROUTE_ANTIGRAVITY_ALLOW_ONBOARD=1 "$SCRIPT_DIR/antigravity-auth.sh" continue onboard-session </dev/null
grep -Fx 'arg=<--allow-onboard>' "$CAPTURE" >/dev/null
unset PESTIROUTE_ANTIGRAVITY_ALLOW_ONBOARD

env -u PESTIROUTE_ANTIGRAVITY_CLIENT_SECRET PESTIROUTE_ANTIGRAVITY_CLIENT_ID=synthetic-public-client \
  "$SCRIPT_DIR/antigravity-auth.sh" continue public-session </dev/null
grep -Fx 'secret=' "$CAPTURE" >/dev/null
grep -Fx 'client_id=synthetic-public-client' "$CAPTURE" >/dev/null
grep -Fx 'arg=<public-session>' "$CAPTURE" >/dev/null

PESTIROUTE_AUTH_DEBUG=1 "$SCRIPT_DIR/antigravity-auth.sh" continue safe-session </dev/null
assert_debug_order continue
grep -Fx 'arg=<--debug>' "$CAPTURE" >/dev/null
printf 'synthetic-pipe-canary\n' | "$SCRIPT_DIR/antigravity-auth.sh" continue pipe-session
grep -Fx 'synthetic-pipe-canary' "$INPUT_CAPTURE" >/dev/null
if STUB_EXIT=37 "$SCRIPT_DIR/antigravity-auth.sh" continue exit-session </dev/null >/dev/null; then
  echo 'launcher swallowed gateway exit status' >&2
  exit 1
else
  [ "$?" -eq 37 ] || { echo 'launcher changed gateway exit status' >&2; exit 1; }
fi

if env -u PESTIROUTE_ANTIGRAVITY_CLIENT_SECRET "$SCRIPT_DIR/antigravity-auth.sh" continue no-secret </dev/null >/dev/null 2>&1; then
  echo 'launcher unexpectedly accepted missing client secret' >&2; exit 1
fi
if PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT= "$SCRIPT_DIR/antigravity-auth.sh" continue no-transport </dev/null >/dev/null 2>&1; then
  echo 'launcher unexpectedly accepted disabled real transport' >&2; exit 1
fi
before=$(wc -l <"$CALLS")
PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT=$WORK/missing "$SCRIPT_DIR/antigravity-auth.sh" start >/dev/null 2>&1 && {
  echo 'launcher unexpectedly accepted missing operator root' >&2; exit 1
}
after=$(wc -l <"$CALLS")
[ "$before" -eq "$after" ] || { echo 'refusal invoked the auth command' >&2; exit 1; }

mkdir "$WORK/stty-shim"
cat >"$WORK/stty-shim/stty" <<'STTY'
#!/bin/sh
if [ "${1:-}" = -echo ] && [ -n "${STTY_SIGNAL:-}" ]; then
  "$REAL_STTY" "$@" || exit
  printf '%s\n' "$$" >"$STTY_PID_FILE"
  kill -s "$STTY_SIGNAL" "$PPID"
  sleep 0.25
  exit
fi
exec "$REAL_STTY" "$@"
STTY
chmod +x "$WORK/stty-shim/stty"

python3 - "$SCRIPT_DIR/antigravity-auth.sh" <<'PY'
import atexit, os, pty, select, signal, subprocess, sys, termios, time

launcher = sys.argv[1]
env = os.environ.copy()
active = []

def cleanup_children():
    for proc in active:
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    for proc in active:
        if proc.poll() is None:
            proc.wait()

atexit.register(cleanup_children)

def spawn(overrides=None):
    master, slave = pty.openpty()
    original = termios.tcgetattr(slave)
    child_env = env.copy()
    child_env.update(overrides or {})
    proc = subprocess.Popen([launcher, "continue", "pty-session"], stdin=slave,
                            stdout=slave, stderr=slave, env=child_env, close_fds=True,
                            start_new_session=True)
    active.append(proc)
    return proc, master, slave, original

def wait_bounded(proc, timeout=5):
    try:
        result = proc.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        os.killpg(proc.pid, signal.SIGKILL)
        proc.wait()
        active.remove(proc)
        raise AssertionError("launcher/stty shim cleanup exceeded timeout")
    active.remove(proc)
    return result

def prompt(proc, master):
    data = b""
    deadline = time.monotonic() + 5
    while b"Paste the OAuth callback URL or authorization code: " not in data:
        if time.monotonic() > deadline or proc.poll() is not None:
            raise AssertionError("interactive prompt missing")
        ready, _, _ = select.select([master], [], [], .1)
        if ready:
            data += os.read(master, 4096)
    return data

def wait_echo_off(slave):
    deadline = time.monotonic() + 5
    while termios.tcgetattr(slave)[3] & termios.ECHO:
        if time.monotonic() > deadline:
            raise AssertionError("terminal echo remained enabled during callback read")
        time.sleep(.01)

calls_before = open(env["CALLS"]).read().count("called")
proc, master, slave, original = spawn()
output = prompt(proc, master)
wait_echo_off(slave)
canary = b"private-pty-callback-canary\n"
os.write(master, canary)
deadline = time.monotonic() + 5
while proc.poll() is None and time.monotonic() < deadline:
    ready, _, _ = select.select([master], [], [], .1)
    if ready:
        output += os.read(master, 4096)
if wait_bounded(proc, 1) != 0:
    raise AssertionError("interactive launcher failed")
while select.select([master], [], [], 0)[0]:
    output += os.read(master, 4096)
if canary.strip() in output or termios.tcgetattr(slave) != original:
    raise AssertionError("callback echoed or original terminal settings were not restored")
if open(env["INPUT_CAPTURE"], "rb").read() != canary:
    raise AssertionError("callback was not forwarded on stdin")
for line in open(env["CAPTURE"], "rb").read().splitlines():
    if canary.strip() in line:
        raise AssertionError("callback leaked to gateway arguments or environment")
if canary.strip() in open(env["ENV_CAPTURE"], "rb").read():
    raise AssertionError("callback leaked into gateway environment")
os.close(master)
os.close(slave)

calls_before = open(env["CALLS"]).read().count("called")
proc, master, slave, original = spawn({"PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT": ""})
output = b""
deadline = time.monotonic() + 5
while proc.poll() is None and time.monotonic() < deadline:
    ready, _, _ = select.select([master], [], [], .1)
    if ready:
        output += os.read(master, 4096)
if wait_bounded(proc, 1) != 2 or b"Paste the OAuth" in output:
    raise AssertionError("disabled transport prompted or was accepted")
if open(env["CALLS"]).read().count("called") != calls_before:
    raise AssertionError("disabled transport invoked gateway")
os.close(master)
os.close(slave)

calls_before = open(env["CALLS"]).read().count("called")
for sig, status in ((signal.SIGHUP, 129), (signal.SIGINT, 130), (signal.SIGTERM, 143)):
    child_env = {"PATH": os.path.join(os.path.dirname(os.environ["GATEWAY_BIN"]), "stty-shim") + os.pathsep + env["PATH"]}
    child_env["STTY_SIGNAL"] = signal.Signals(sig).name[3:]
    try:
        os.unlink(env["STTY_PID_FILE"])
    except FileNotFoundError:
        pass
    proc, master, slave, original = spawn(child_env)
    prompt(proc, master)
    wait_echo_off(slave)
    if wait_bounded(proc) != status or termios.tcgetattr(slave) != original:
        raise AssertionError("signal during stty mutation did not restore original settings")
    shim_pid = int(open(env["STTY_PID_FILE"]).read())
    try:
        os.kill(shim_pid, 0)
    except ProcessLookupError:
        pass
    else:
        raise AssertionError("stty shim process was orphaned")
    if open(env["CALLS"]).read().count("called") != calls_before:
        raise AssertionError("signal cancellation invoked gateway")
    os.close(master)
    os.close(slave)

calls_before = open(env["CALLS"]).read().count("called")
proc, master, slave, original = spawn()
prompt(proc, master)
os.write(master, b"\n")
if wait_bounded(proc) != 2 or termios.tcgetattr(slave) != original:
    raise AssertionError("empty callback was accepted or terminal settings leaked")
if open(env["CALLS"]).read().count("called") != calls_before:
    raise AssertionError("empty callback invoked gateway")
os.close(master)
os.close(slave)

calls_before = open(env["CALLS"]).read().count("called")
proc, master, slave, original = spawn()
prompt(proc, master)
wait_echo_off(slave)
os.write(master, b" \t \n")
if wait_bounded(proc) != 2 or termios.tcgetattr(slave) != original:
    raise AssertionError("whitespace callback was accepted or terminal settings leaked")
if open(env["CALLS"]).read().count("called") != calls_before:
    raise AssertionError("whitespace callback invoked gateway")
os.close(master)
os.close(slave)

calls_before = open(env["CALLS"]).read().count("called")
proc, master, slave, original = spawn()
prompt(proc, master)
wait_echo_off(slave)
os.write(master, b"\x04")
if wait_bounded(proc) != 2 or termios.tcgetattr(slave) != original:
    raise AssertionError("EOF callback was accepted or terminal settings leaked")
if open(env["CALLS"]).read().count("called") != calls_before:
    raise AssertionError("EOF callback invoked gateway")
os.close(master)
os.close(slave)
PY
echo 'synthetic launcher checks passed'
