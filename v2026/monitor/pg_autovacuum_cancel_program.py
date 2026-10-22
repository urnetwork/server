# Bounded PG18 JSON cancellation projection for SIGNALS.md 2.4e.
# Trusted cluster/path administration and server code remain explicit premises.
import datetime
import json
import os
import re
import socket
import stat
import time

MAX_BYTES = 40000000
MAX_ROWS = 100000
MAX_INDEX_BYTES = 8192
MAX_RECORD_BYTES = 1048576
MAX_FIELDS = 64
DATA_PATH = "/var/lib/postgresql/18/main"
BOOT_PATH = "/proc/sys/kernel/random/boot_id"
TIMESTAMP = re.compile(r"^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3} UTC$")
SESSION_START = re.compile(r"^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2} UTC$")
STATE_CODE = re.compile(r"^[0-9A-Z]{5}$")
RECORD_LEVELS = frozenset(("DEBUG", "LOG", "INFO", "NOTICE", "WARNING", "ERROR", "FATAL", "PANIC"))


def file_fields(value):
    """Project the stable descriptor metadata without path or content."""
    return [str(value.st_dev), str(value.st_ino), str(value.st_size),
            str(value.st_mtime_ns), str(value.st_ctime_ns)]


def owner_fields(value):
    """Bind an administrator-controlled object to its uid and permission mode."""
    return (value.st_dev, value.st_ino, value.st_uid, value.st_mode)


def owned_regular(value, owner):
    """A log/index leaf must share the trusted cluster owner and refuse other writers."""
    return stat.S_ISREG(value.st_mode) and value.st_uid == owner and not value.st_mode & 0o022


def read_index(data_path, owner):
    """Read one bounded stable current_logfiles index; preserve spaces in paths."""
    index_path = os.path.join(data_path, "current_logfiles")
    descriptor = os.open(index_path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        before = os.fstat(descriptor)
        if not owned_regular(before, owner) or before.st_size <= 0 or before.st_size > MAX_INDEX_BYTES:
            raise ValueError("index")
        body = b""
        while len(body) < before.st_size:
            part = os.read(descriptor, before.st_size - len(body))
            if not part:
                break
            body += part
        after = os.fstat(descriptor)
    finally:
        os.close(descriptor)
    current = os.stat(index_path, follow_symlinks=False)
    if len(body) != before.st_size or not body.endswith(b"\n") or b"\x00" in body or b"\r" in body:
        raise ValueError("index")
    if file_fields(before) != file_fields(after) or owner_fields(before) != owner_fields(after) or \
            file_fields(before) != file_fields(current) or owner_fields(before) != owner_fields(current):
        raise ValueError("index")
    entries = {}
    for line in body[:-1].split(b"\n"):
        destination, separator, path = line.partition(b" ")
        if not separator or not path or destination not in (b"stderr", b"csvlog", b"jsonlog") or destination in entries:
            raise ValueError("index")
        entries[destination] = path
    if b"jsonlog" not in entries:
        raise ValueError("index")
    selected = os.fsdecode(entries[b"jsonlog"])
    if not os.path.isabs(selected):
        selected = os.path.join(data_path, selected)
    return (file_fields(before), owner_fields(before), body, selected)


def unique_object(pairs):
    """Reject duplicate keys before dictionary collapse at every parsed object."""
    if len(pairs) > MAX_FIELDS:
        raise ValueError("fields")
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate")
        result[key] = value
    return result


def invalid_number(value):
    """The pinned writer emits integer JSON numbers, never floats or NaN/Infinity."""
    raise ValueError("number")


def source_timestamp(value, session=False):
    """Only the pinned UTC millisecond/whole-second writer forms are supported."""
    pattern = SESSION_START if session else TIMESTAMP
    if not isinstance(value, str) or pattern.fullmatch(value) is None:
        raise ValueError("timestamp")
    layout = "%Y-%m-%d %H:%M:%S UTC" if session else "%Y-%m-%d %H:%M:%S.%f UTC"
    stamp = datetime.datetime.strptime(value, layout)
    delta = stamp - datetime.datetime(1970, 1, 1)
    result = (delta.days * 86400 + delta.seconds) * 1000000 + delta.microseconds
    if result <= 0 or result > 9223372036854775807:
        raise ValueError("timestamp")
    return result


def record_fields(line):
    """Decode one physical writer object once; string contents never become records."""
    record = json.loads(line.decode("utf-8"), object_pairs_hook=unique_object,
                        parse_constant=invalid_number, parse_float=invalid_number)
    if not isinstance(record, dict):
        raise ValueError("record")
    # The pinned writer uses scalar strings/integers and omits NULL fields.
    # Unknown future scalar fields are ignored, never copied to the projection.
    for value in record.values():
        if type(value) not in (str, int) or (type(value) is int and not -9223372036854775808 <= value <= 18446744073709551615):
            raise ValueError("record")
        if isinstance(value, str) and any(0xD800 <= ord(c) <= 0xDFFF for c in value):
            raise ValueError("record")
    ended = source_timestamp(record.get("timestamp"))
    pid, line_num = record.get("pid"), record.get("line_num")
    if type(pid) is not int or not 0 < pid <= 2147483647 or type(line_num) is not int or not 0 < line_num <= 9223372036854775807:
        raise ValueError("record")
    started = source_timestamp(record.get("session_start"), session=True)
    session_id = record.get("session_id")
    if started > ended or session_id != format(started // 1000000, "x") + "." + format(pid, "x"):
        raise ValueError("record")
    severity, message, backend = record.get("error_severity"), record.get("message"), record.get("backend_type")
    if severity not in RECORD_LEVELS or not isinstance(message, str) or not isinstance(backend, str) or not backend:
        raise ValueError("record")
    state = record.get("state_code")
    if state is not None and (not isinstance(state, str) or STATE_CODE.fullmatch(state) is None or state == "00000"):
        raise ValueError("record")
    if severity in ("ERROR", "FATAL", "PANIC") and state is None:
        raise ValueError("record")
    # These compiler-owned primary fields cannot be replaced by user/db/SQL
    # strings. They establish an observed server record, not pass/intent facts.
    cancellation = backend == "autovacuum worker" and severity == "ERROR" and \
        state == "57014" and message == "canceling autovacuum task"
    if backend == "autovacuum worker" and severity == "ERROR" and state == "57014" and not cancellation:
        # An unsupported translation/message cannot certify this worker quiet.
        raise ValueError("message")
    return ended, session_id, line_num, cancellation


def collect(data_path, host, boot_path=BOOT_PATH, clock=time.time_ns, monotonic=time.monotonic_ns):
    """Observe one indexed JSON file; missing JSON has no plaintext fallback."""
    with open(boot_path, "rb") as boot_file:
        boot_before = boot_file.read(128).decode("ascii").strip()
    started = clock() // 1000
    mono_started = monotonic() // 1000
    data_before = os.stat(data_path, follow_symlinks=False)
    if not stat.S_ISDIR(data_before.st_mode) or data_before.st_mode & 0o022:
        raise ValueError("cluster")
    owner = data_before.st_uid
    index_before = read_index(data_path, owner)
    path = index_before[3]
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        before = os.fstat(descriptor)
        if not owned_regular(before, owner):
            raise ValueError("file")
        offset = max(0, before.st_size - MAX_BYTES)
        os.lseek(descriptor, offset, os.SEEK_SET)
        remaining = before.st_size - offset
        chunks = []
        while remaining:
            part = os.read(descriptor, min(1048576, remaining))
            if not part:
                break
            chunks.append(part)
            remaining -= len(part)
        body = b"".join(chunks)
        chunks.clear()
        after = os.fstat(descriptor)
        snapshot = clock() // 1000
        mono_snapshot = monotonic() // 1000
    finally:
        os.close(descriptor)
    if not owned_regular(after, owner) or owner_fields(before) != owner_fields(after):
        raise ValueError("file")
    flags = set()
    if offset:
        flags.add("tail-capped")
    if len(body) != before.st_size - offset:
        flags.add("short-read")
    if file_fields(before) != file_fields(after):
        flags.add("snapshot-changed")
    cursor = offset
    if offset:
        first_newline = body.find(b"\n")
        if first_newline < 0:
            body = b""
        else:
            cursor += first_newline + 1
            body = body[first_newline + 1:]
        flags.add("partial-first")
    if body and not body.endswith(b"\n"):
        flags.add("partial-last")
        body = body[:body.rfind(b"\n") + 1]
    rows, sessions = [], {}
    first_record = last_record = previous_record = None
    position = 0
    while position < len(body):
        line_end = body.find(b"\n", position) + 1
        line = body[position:line_end - 1]
        row_offset = cursor
        cursor += line_end - position
        position = line_end
        if len(line) > MAX_RECORD_BYTES:
            flags.add("record-capped")
            continue
        try:
            ended, session_id, line_num, cancellation = record_fields(line)
        except (ValueError, UnicodeError, OverflowError, RecursionError):
            flags.add("record-schema")
            continue
        if session_id not in sessions and len(sessions) == MAX_ROWS:
            flags.add("row-cap")
            break
        if line_num <= sessions.get(session_id, 0):
            flags.add("record-session")
            continue
        sessions[session_id] = line_num
        if previous_record is not None and ended < previous_record:
            flags.add("record-order")
        previous_record = ended
        first_record = ended if first_record is None else min(first_record, ended)
        last_record = ended if last_record is None else max(last_record, ended)
        if ended > snapshot:
            flags.add("future-record")
        if not cancellation or ended > snapshot:
            continue
        if len(rows) == MAX_ROWS:
            flags.add("row-cap")
            break
        rows.append([str(row_offset), str(ended)])
    try:
        index_after = read_index(data_path, owner)
        if index_before != index_after:
            flags.add("source-index-changed")
    except (OSError, ValueError, UnicodeError):
        flags.add("source-index-changed")
    data_after = os.stat(data_path, follow_symlinks=False)
    if owner_fields(data_before) != owner_fields(data_after):
        raise ValueError("cluster")
    try:
        current = os.stat(path, follow_symlinks=False)
        if not owned_regular(current, owner) or file_fields(current) != file_fields(before) or owner_fields(current) != owner_fields(before):
            flags.add("path-changed")
    except OSError:
        flags.add("path-changed")
    with open(boot_path, "rb") as boot_file:
        boot_after = boot_file.read(128).decode("ascii").strip()
    finished = clock() // 1000
    mono_finished = monotonic() // 1000
    return {"version": "2", "status": "snapshot", "host": host,
            "clock": [str(started), str(snapshot), str(finished),
                      str(mono_snapshot - mono_started), str(mono_finished - mono_started)],
            "boot": [boot_before, boot_after],
            "file_before": file_fields(before), "file_after": file_fields(after),
            "read": [str(offset), str(before.st_size - offset - remaining)],
            "span": [str(first_record or 0), str(last_record or 0)],
            "flags": sorted(flags), "rows": rows}


def observe(data_path, host, boot_path=BOOT_PATH, clock=time.time_ns, monotonic=time.monotonic_ns):
    """The production exception/output boundary is also exercised by private fixtures."""
    try:
        result = collect(data_path, host, boot_path, clock, monotonic)
        output = json.dumps(result, separators=(",", ":"))
        if len(output) > 8388608:
            raise ValueError("output")
    except Exception:
        output = '{"version":"2","status":"unavailable"}'
    return output


def main():
    """Use only the fixed reviewed cluster and structured-source discovery."""
    print(observe(DATA_PATH, socket.gethostname().split(".", 1)[0]))


if __name__ == "__main__":
    main()
