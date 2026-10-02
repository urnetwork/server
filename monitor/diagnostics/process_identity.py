"""Bounded local identity audit for the single owned Main monitor.

This module never starts, stops, signals, or restarts the watcher. A failed
audit is unavailable evidence, including when a preceding promotion succeeded.
Only the exact proc executable is hashed, through a noninteractive read-only
sudo command. There is no fallback to the installed file or another PID.
"""

import datetime
import os
import re
import selectors
import subprocess
import time

UNIT = "fp2-main-monitor-cap2-20260929.service"
AUDIT_SECONDS = 5.0
BOOT_PATH = "/proc/sys/kernel/random/boot_id"
CAUSES = frozenset((
    "invalid_authority", "owner_timeout", "tool_unavailable", "command_failed",
    "command_timeout", "command_output_cap", "command_reap_unavailable",
    "unit_invalid", "unit_mismatch", "proc_unavailable", "proc_invalid",
    "generation_mismatch", "sudo_or_hash_failed", "hash_timeout",
    "hash_invalid", "hash_mismatch",
))


class AuditError(Exception):
    def __init__(self, cause):
        if cause not in CAUSES:
            cause = "invalid_authority"
        self.cause = cause
        super().__init__(cause)


def validate_authority(authority):
    if not isinstance(authority, dict) or set(authority) != {
        "unit", "pid", "sha256", "start_ticks", "boot_id", "n_restarts"
    }:
        raise AuditError("invalid_authority")
    if (authority["unit"] != UNIT or type(authority["pid"]) is not int
            or not 1 <= authority["pid"] <= 4194304
            or type(authority["start_ticks"]) is not int
            or not 1 <= authority["start_ticks"] < 2**64
            or type(authority["n_restarts"]) is not int
            or authority["n_restarts"] != 0
            or not isinstance(authority["sha256"], str)
            or not re.fullmatch(r"[0-9a-f]{64}", authority["sha256"])
            or not isinstance(authority["boot_id"], str)
            or not re.fullmatch(r"[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}", authority["boot_id"])):
        raise AuditError("invalid_authority")


def _remaining(deadline):
    left = deadline - time.monotonic()
    if left <= 0:
        raise AuditError("owner_timeout")
    return left


def _environment():
    # No inherited loader overrides, Python paths, credentials or pager hooks.
    env = {key: os.environ[key] for key in (
        "HOME", "USER", "LOGNAME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"
    ) if key in os.environ}
    env.update(PATH="/usr/bin:/bin", LC_ALL="C", SYSTEMD_PAGER="cat")
    return env


def _run_bounded(argv, deadline, *, stdout_cap=2048, stderr_cap=1024):
    """Finite bytes and deadline; never expose subprocess text in exceptions."""
    # Reserve reaping time inside the same deadline, rather than first killing
    # at the final instant and spuriously losing a still-exiting child's owner.
    work_deadline = deadline - .1
    _remaining(work_deadline)
    try:
        process = subprocess.Popen(argv, stdin=subprocess.DEVNULL,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                   env=_environment(), close_fds=True)
    except OSError:
        raise AuditError("tool_unavailable") from None
    output = bytearray()
    stderr_bytes = 0
    selector = selectors.DefaultSelector()
    try:
        for stream in (process.stdout, process.stderr):
            os.set_blocking(stream.fileno(), False)
            selector.register(stream, selectors.EVENT_READ)
        while selector.get_map():
            if work_deadline <= time.monotonic():
                raise AuditError("command_timeout")
            for key, _ in selector.select(min(_remaining(work_deadline), .05)):
                stream = key.fileobj
                chunk = os.read(stream.fileno(), 4096)
                if not chunk:
                    selector.unregister(stream)
                elif stream is process.stdout:
                    if len(output) + len(chunk) > stdout_cap:
                        raise AuditError("command_output_cap")
                    output.extend(chunk)
                else:
                    stderr_bytes += len(chunk)
                    if stderr_bytes > stderr_cap:
                        raise AuditError("command_output_cap")
        try:
            status = process.wait(timeout=_remaining(work_deadline))
        except subprocess.TimeoutExpired:
            raise AuditError("command_timeout") from None
        return status, bytes(output), stderr_bytes
    except OSError:
        raise AuditError("command_failed") from None
    finally:
        selector.close()
        process.stdout.close()
        process.stderr.close()
        if process.poll() is None:
            try:
                process.kill()
            except (ProcessLookupError, PermissionError):
                pass
            # sudo's root hash child has its own fixed two-second timeout.
            # Never signal the watcher or broaden privilege to kill a process.
            try:
                process.wait(timeout=max(.001, deadline - time.monotonic()))
            except subprocess.TimeoutExpired:
                raise AuditError("command_reap_unavailable") from None


def _read(path, limit):
    try:
        with open(path, "rb") as stream:
            data = stream.read(limit + 1)
    except OSError:
        raise AuditError("proc_unavailable") from None
    if len(data) > limit:
        raise AuditError("proc_invalid")
    return data


def _generation(authority):
    raw = _read(f"/proc/{authority['pid']}/stat", 8192)
    # comm is parenthesized and may itself contain ')' or spaces. Field 22 is
    # suffix index 19 after the final closing parenthesis (field 3 is index 0).
    end = raw.rfind(b")")
    try:
        pid = int(raw[:raw.index(b" (")])
        fields = raw[end + 2:].split()
        if end < 0 or len(fields) < 20 or not re.fullmatch(rb"[0-9]+", fields[19]):
            raise ValueError()
        ticks = int(fields[19])
        boot = _read(BOOT_PATH, 64).decode("ascii").strip()
    except (ValueError, IndexError, UnicodeError):
        raise AuditError("proc_invalid") from None
    if (pid != authority["pid"] or ticks != authority["start_ticks"]
            or boot != authority["boot_id"]):
        raise AuditError("generation_mismatch")


def _unit(authority, deadline):
    code, out, stderr_bytes = _run_bounded([
        "/usr/bin/systemctl", "--user", "show", UNIT,
        "--property=ActiveState,SubState,MainPID,NRestarts",
    ], min(deadline, time.monotonic() + 1.0))
    if code != 0 or stderr_bytes:
        raise AuditError("command_failed")
    try:
        lines = out.decode("ascii").splitlines()
        pairs = [line.split("=", 1) for line in lines]
        if any(len(pair) != 2 for pair in pairs):
            raise ValueError()
        state = dict(pairs)
        if len(state) != len(pairs) or set(state) != {"ActiveState", "SubState", "MainPID", "NRestarts"}:
            raise ValueError()
    except (ValueError, UnicodeError):
        raise AuditError("unit_invalid") from None
    if state != {"ActiveState": "active", "SubState": "running",
                 "MainPID": str(authority["pid"]), "NRestarts": "0"}:
        raise AuditError("unit_mismatch")


def _hash(authority, deadline):
    # Root timeout supervises only sha256sum, including if the parent audit is
    # interrupted. No shell, arbitrary path, sudo prompt, or watcher signal.
    argv = ["/usr/bin/sudo", "-n", "--", "/usr/bin/timeout",
            "--signal=TERM", "--kill-after=0.2s", "2s",
            "/usr/bin/sha256sum", f"/proc/{authority['pid']}/exe"]
    # Do not launch the privileged child unless its complete bound fits.
    if _remaining(deadline) < 2.5:
        raise AuditError("owner_timeout")
    code, out, stderr_bytes = _run_bounded(argv, min(deadline, time.monotonic() + 2.5))
    if code in (124, 137):
        raise AuditError("hash_timeout")
    if code != 0 or stderr_bytes:
        raise AuditError("sudo_or_hash_failed")
    pattern = rb"([0-9a-f]{64})  /proc/" + str(authority["pid"]).encode() + rb"/exe\n"
    match = re.fullmatch(pattern, out)
    if match is None:
        raise AuditError("hash_invalid")
    if match[1].decode() != authority["sha256"]:
        raise AuditError("hash_mismatch")


def verify(authority):
    """Verify exactly one frozen process generation. No discovery or retries."""
    validate_authority(authority)
    began = time.monotonic()
    deadline = began + AUDIT_SECONDS
    _unit(authority, deadline)
    _generation(authority)
    _hash(authority, deadline)
    _generation(authority)
    _unit(authority, deadline)
    _remaining(deadline)
    return {
        "at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "watcher_pid": authority["pid"], "watcher_sha256": authority["sha256"],
        "watcher_start_ticks": authority["start_ticks"], "watcher_boot_id": authority["boot_id"],
        "monotonic_completed": time.monotonic(), "ssh_created": 0,
        "audit_policy": "unit_and_proc_generation_bounded_sudo_hash",
    }
