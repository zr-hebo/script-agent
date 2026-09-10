package runner

const pythonEntry = `import json
import runpy
import sys
import traceback

source_path, params_path, result_path, error_path = sys.argv[1:]
try:
    with open(params_path, encoding="utf-8") as f:
        params = json.load(f)
    namespace = runpy.run_path(source_path, run_name="usercode")
    handle = namespace.get("handle")
    if not callable(handle):
        raise TypeError("expected def handle(params) returning a dict or None")
    result = handle(params)
    if result is not None and not isinstance(result, dict):
        raise TypeError("handle must return a dict or None")
    encoded = json.dumps(result, ensure_ascii=False, allow_nan=False)
    if len(encoded.encode("utf-8")) > 1048576:
        raise ValueError("result exceeds 1048576 bytes")
    with open(result_path, "w", encoding="utf-8") as f:
        f.write(encoded)
except BaseException as exc:
    detail = {
        "type": "python_error",
        "message": type(exc).__name__ + ": " + str(exc),
        "stack": traceback.format_exc(),
    }
    try:
        with open(error_path, "w", encoding="utf-8") as f:
            json.dump(detail, f)
    finally:
        traceback.print_exc()
    sys.exit(1)
`
