package runner

const pythonEntry = `import json
import os
import runpy
import signal
import sys
import traceback

source_path, params_path, result_path, post_path = sys.argv[1:]
events = os.fdopen(3, "w", buffering=1)
log_prefix = os.environ.get("SCRIPT_LOG_MARKER")

class Cancelled(BaseException):
    pass

def interrupted(signum, frame):
    raise Cancelled("execution cancelled")

signal.signal(signal.SIGTERM, interrupted)
signal.signal(signal.SIGINT, interrupted)

def notify(phase):
    sys.stdout.flush()
    sys.stderr.flush()
    if log_prefix:
        marker = (log_prefix + phase + "\x1f").encode()
        os.write(1, marker)
        os.write(2, marker)
    events.write(json.dumps({"phase": phase}) + "\n")
    events.flush()

def failure(exc):
    status = "failed"
    if isinstance(exc, Cancelled):
        status = "cancelled"
        try:
            with open(os.environ["SCRIPT_INTERRUPT_FILE"]) as f:
                if f.read() == "timed_out":
                    status = "timed_out"
        except OSError:
            pass
    return {"status": status, "data": None,
            "error": type(exc).__name__ + ": " + str(exc),
            "stack": traceback.format_exc()}

def encode(out):
    if not isinstance(out, dict):
        raise TypeError("run must return an Outcome dict")
    status, error = out.get("status"), out.get("error")
    if status not in ("succeeded", "failed", "timed_out", "cancelled"):
        raise ValueError("invalid or missing outcome status")
    if error is not None and not isinstance(error, str):
        raise TypeError("outcome error must be a string or None")
    if status == "succeeded" and error is not None:
        raise ValueError("succeeded outcome cannot contain an error")
    if status != "succeeded" and not error:
        raise ValueError("unsuccessful outcome must contain an error")
    if out.get("data") is not None and not isinstance(out["data"], dict):
        raise TypeError("outcome data must be a dict or None")
    # Process metadata belongs to the supervisor, not to user return values.
    normalized = {"status": status, "data": out.get("data"), "error": error}
    if "stack" in out:
        normalized["stack"] = out["stack"]
    encoded = json.dumps(normalized, ensure_ascii=False, allow_nan=False)
    if len(encoded.encode("utf-8")) > 1048576:
        raise ValueError("outcome exceeds 1048576 bytes")
    return encoded

def save(path, encoded):
    with open(path, "w", encoding="utf-8") as f:
        f.write(encoded)

try:
    with open(params_path, encoding="utf-8") as f:
        params = json.load(f)
    namespace = runpy.run_path(source_path, run_name="usercode")
    legacy = "run" not in namespace
    run = namespace.get("handle" if legacy else "run")
    if not callable(run):
        raise TypeError("expected def run(params) (or legacy def handle(params))")
except BaseException as exc:
    save(result_path, encode(failure(exc)))
    traceback.print_exc()
    sys.exit(1)

try:
    prepare = namespace.get("prepare")
    if prepare is not None:
        if not callable(prepare):
            raise TypeError("prepare must be callable")
        prepare(params)
    notify("run")
    outcome = run(params)
    if legacy:
        if outcome is not None and not isinstance(outcome, dict):
            raise TypeError("handle must return a dict or None")
        outcome = {"status": "succeeded", "data": outcome, "error": None}
    encoded = encode(outcome)
except BaseException as exc:
    outcome = failure(exc)
    encoded = encode(outcome)
    traceback.print_exc()

save(result_path, encoded)
# Cleanup sees exactly the serialized primary result and cannot overwrite it.
outcome = json.loads(encoded)
notify("post-run")
post_run = namespace.get("post_run")
if post_run is None:
    save(post_path, json.dumps({"status": "skipped", "data": None, "error": None}))
else:
    try:
        if not callable(post_run):
            raise TypeError("post_run must be callable")
        post_run(params, outcome)
        save(post_path, encode({"status": "succeeded", "data": None, "error": None}))
    except BaseException as exc:
        save(post_path, encode(failure(exc)))
        traceback.print_exc()
`
