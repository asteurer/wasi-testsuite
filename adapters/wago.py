import os
import shlex
import subprocess
from typing import Dict, List, Tuple, Optional

# shlex.split() splits according to shell quoting rules
WAGO = shlex.split(os.getenv("WAGO", "wago"), posix=os.name != "nt")

# The wago CLI (`wago run`) exposes no per-invocation flags for environment
# variables or preopened directories, so tests are executed through a small Go
# helper that embeds the wago runtime and the wago-org/wasi host functions
# (see tools/wago-runner). Override its path/command with WAGO_RUNNER.
WAGO_RUNNER = shlex.split(os.getenv("WAGO_RUNNER", "wago-runner"),
                          posix=os.name != "nt")

# Map a wasi-testsuite version to the wago-runner host bundle. The wasm32-wasip3
# suite is run through the Preview 2 component host (wago-org/wasi/p2) for now.
_WASI_MODE = {
    "wasm32-wasip1": "p1",
    "wasm32-wasip3": "p2",
}


def get_name() -> str:
    return "wago"


def get_version() -> str:
    # ensure no args when version is queried
    result = subprocess.run(WAGO[0:1] + ["--version"],
                            encoding="UTF-8", capture_output=True,
                            check=True)
    # `wago --version` prints key/value lines, e.g.:
    #   release      v0.1.0-beta.9
    #   manager      v0.1.0-beta.9  /path/to/wago
    for line in result.stdout.splitlines():
        parts = line.split()
        if len(parts) >= 2 and parts[0] in ("release", "manager"):
            return parts[1].lstrip("v")
    # fallback: last whitespace-separated token
    return result.stdout.split()[-1].lstrip("v")


def get_wasi_versions() -> List[str]:
    return ["wasm32-wasip1", "wasm32-wasip3"]


def get_wasi_worlds() -> List[str]:
    return ["wasi:cli/command", "wasi:http/service"]


def compute_argv(test_path: str,
                 args_env_root: Tuple[List[str], Dict[str, str], Optional[str]],
                 proposals: List[str],
                 wasi_world: str,
                 wasi_version: str) -> List[str]:

    args, env, root = args_env_root

    argv = list(WAGO_RUNNER)
    argv += ["--wasi", _WASI_MODE.get(wasi_version, "p1")]

    for k, v in env.items():
        argv += ["--env", f"{k}={v}"]

    if root:
        argv += ["--dir", str(root)]

    argv += [test_path]
    argv += args
    return argv
