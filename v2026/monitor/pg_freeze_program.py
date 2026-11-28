# Bounded PG18 JSON primary-record projection. Never emit log text or errors.
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
MAX_RECORD_FIELDS = 64
DATA_DIR = "/var/lib/postgresql/18/main"
BOOT_PATH = "/proc/sys/kernel/random/boot_id"
STAMP = re.compile(r"^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3} UTC$")
START = re.compile(r"^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2} UTC$")
SESSION = re.compile(r"^([1-9a-f][0-9a-f]{0,15})\.([1-9a-f][0-9a-f]{0,7})$")
FILE = re.compile(r"^[A-Za-z0-9_.-]{1,256}$")
FUNCTION = re.compile(r"^[A-Za-z_][A-Za-z_0-9]{0,255}$")
DURATION = re.compile(r"^duration: ([0-9]+)(?:\.([0-9]{1,6}))? ms(?:[ \t]|$)")
DURATION_FUNCTIONS = {"exec_simple_query", "exec_parse_message", "exec_bind_message", "exec_execute_message"}
SEVERITIES = {"DEBUG", "LOG", "INFO", "NOTICE", "WARNING", "ERROR", "FATAL", "PANIC"}


def unavailable():
    return {"version": "2", "status": "json-source-unavailable"}


def file_fields(value):
    return [str(value.st_dev), str(value.st_ino), str(value.st_size),
            str(value.st_mtime_ns), str(value.st_ctime_ns)]


def trusted_file(value, owner):
    return stat.S_ISREG(value.st_mode) and value.st_uid == owner and not value.st_mode & 0o022


def source_index(data_dir):
    # The fixed cluster directory and its administrator-controlled ancestry
    # are the trust root. Bind metadata/log ownership to that cluster owner.
    directory = os.stat(data_dir, follow_symlinks=False)
    if not stat.S_ISDIR(directory.st_mode) or directory.st_mode & 0o022:
        raise ValueError("source directory")
    path = os.path.join(data_dir, "current_logfiles")
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        before = os.fstat(descriptor)
        if not trusted_file(before, directory.st_uid) or not 0 < before.st_size <= MAX_INDEX_BYTES:
            raise ValueError("source index")
        body = os.read(descriptor, before.st_size)
        after = os.fstat(descriptor)
    finally:
        os.close(descriptor)
    current = os.stat(path, follow_symlinks=False)
    if len(body) != before.st_size or not body.endswith(b"\n") or file_fields(before) != file_fields(after) or file_fields(before) != file_fields(current):
        raise ValueError("source index changed")
    destinations = {}
    for line in body[:-1].split(b"\n"):
        destination, separator, filename = line.partition(b" ")
        if not separator or destination not in (b"stderr", b"csvlog", b"jsonlog") or destination in destinations or not filename:
            raise ValueError("source index shape")
        if any(byte < 32 or byte == 127 for byte in filename):
            raise ValueError("source index path")
        destinations[destination] = os.fsdecode(filename)
    if b"jsonlog" not in destinations:
        raise ValueError("JSON source absent")
    selected = destinations[b"jsonlog"]
    if not os.path.isabs(selected):
        selected = os.path.join(data_dir, selected)
    selected = os.path.normpath(selected)
    return selected, directory.st_uid, (file_fields(directory), file_fields(before), body)


def unique_object(pairs):
    if len(pairs) > MAX_RECORD_FIELDS:
        raise ValueError("record field cap")
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON key")
        result[key] = value
    return result


def invalid_number(value):
    raise ValueError("noninteger JSON number")


def timestamp(value, session_start=False):
    pattern = START if session_start else STAMP
    if type(value) is not str or pattern.fullmatch(value) is None:
        raise ValueError("source timestamp")
    fmt = "%Y-%m-%d %H:%M:%S UTC" if session_start else "%Y-%m-%d %H:%M:%S.%f UTC"
    stamp = datetime.datetime.strptime(value, fmt)
    delta = stamp - datetime.datetime(1970, 1, 1)
    return (delta.days * 86400 + delta.seconds) * 1000000 + delta.microseconds


def integer(value, ceiling=9223372036854775807):
    return type(value) is int and 0 < value <= ceiling


def record(line, previous_lines):
    # Decode exactly one primary object. Escaped identity/SQL/message strings
    # are never reparsed as records; all pinned writer values are scalars.
    if not line.startswith(b"{") or not line.endswith(b"}\n"):
        raise ValueError("record framing")
    value = json.loads(line.decode("utf-8"), object_pairs_hook=unique_object,
                       parse_float=invalid_number, parse_constant=invalid_number)
    if type(value) is not dict or any(type(item) not in (str, int) for item in value.values()):
        raise ValueError("record shape")
    for item in value.values():
        if type(item) is str:
            item.encode("utf-8")
    ended = timestamp(value.get("timestamp"))
    started = timestamp(value.get("session_start"), True)
    pid, number = value.get("pid"), value.get("line_num")
    session = value.get("session_id")
    match = SESSION.fullmatch(session) if type(session) is str else None
    if not integer(pid, 2147483647) or not integer(number) or match is None or int(match[2], 16) != pid or int(match[1], 16) * 1000000 != started or not 0 < started <= ended:
        raise ValueError("writer identity")
    if number <= previous_lines.get(session, 0):
        raise ValueError("duplicate or reordered writer identity")
    previous_lines[session] = number
    if value.get("error_severity") not in SEVERITIES or type(value.get("message")) is not str or type(value.get("backend_type")) is not str or not value["backend_type"]:
        raise ValueError("primary fields")
    filename, function, source_line = value.get("file_name"), value.get("func_name"), value.get("file_line_num")
    if type(filename) is not str or FILE.fullmatch(filename) is None or type(function) is not str or FUNCTION.fullmatch(function) is None or not integer(source_line, 2147483647):
        raise ValueError("verbose source location unavailable")
    return value, ended


def collect(data_dir, host, boot_path=BOOT_PATH, clock=time.time_ns, monotonic=time.monotonic_ns):
    try:
        return collect_snapshot(data_dir, host, boot_path, clock, monotonic)
    except Exception:
        return unavailable()


def collect_snapshot(data_dir, host, boot_path, clock, monotonic):
    with open(boot_path, "rb") as boot_file:
        boot_before = boot_file.read(128).decode("ascii").strip()
    started = clock() // 1000
    mono_started = monotonic() // 1000
    path, owner, index_before = source_index(data_dir)
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        before = os.fstat(descriptor)
        if not trusted_file(before, owner):
            return unavailable()
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
    flags = set()
    if offset:
        flags.add("tail-capped")
    if len(body) != before.st_size - offset:
        flags.add("short-read")
    if file_fields(before) != file_fields(after) or not trusted_file(after, owner):
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
    rows, previous_lines = [], {}
    first_record = last_record = previous_record = None
    position = 0
    while position < len(body):
        line_end = body.find(b"\n", position) + 1
        length = line_end - position
        row_offset = cursor
        cursor += length
        if length > MAX_RECORD_BYTES:
            flags.add("record-cap")
            position = line_end
            continue
        line = body[position:line_end]
        position = line_end
        try:
            value, ended = record(line, previous_lines)
        except (ValueError, UnicodeError, OverflowError, TypeError, RecursionError):
            flags.add("record-schema")
            continue
        if previous_record is not None and ended < previous_record:
            flags.add("record-order")
        previous_record = ended
        first_record = ended if first_record is None else min(first_record, ended)
        last_record = ended if last_record is None else max(last_record, ended)
        if ended > snapshot:
            flags.add("future-record")
        message = value["message"]
        if value["error_severity"] != "LOG" or not message.startswith("duration:"):
            continue
        # An application RAISE LOG is a real server message, not a native
        # duration event. Other unknown duration emitters leave membership open.
        if value["file_name"] == "pl_exec.c" and value["func_name"] == "exec_stmt_raise":
            continue
        if value["file_name"] != "postgres.c" or value["func_name"] not in DURATION_FUNCTIONS or value["backend_type"] != "client backend" or "state_code" in value:
            flags.add("duration-origin")
            continue
        duration_match = DURATION.match(message)
        if duration_match is None:
            flags.add("duration-schema")
            continue
        duration = int(duration_match[1]) * 1000000
        duration += int((duration_match[2] or "").ljust(6, "0"))
        if duration > 9223372036854775807:
            flags.add("duration-range")
            continue
        if duration < 1000000000:
            continue
        if len(rows) == MAX_ROWS:
            flags.add("row-cap")
            break
        rows.append([str(row_offset), str(ended), str(duration)])
    try:
        current = os.stat(path, follow_symlinks=False)
        if not trusted_file(current, owner) or (current.st_dev, current.st_ino) != (before.st_dev, before.st_ino):
            flags.add("path-changed")
    except OSError:
        flags.add("path-changed")
    try:
        selected_after, owner_after, index_after = source_index(data_dir)
        if selected_after != path or owner_after != owner or index_before != index_after:
            flags.add("source-index-changed")
    except (OSError, ValueError, UnicodeError):
        flags.add("source-index-changed")
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


def main():
    try:
        result = collect(DATA_DIR, socket.gethostname().split(".", 1)[0])
        output = json.dumps(result, separators=(",", ":"))
        if len(output) > 8388608:
            output = json.dumps(unavailable(), separators=(",", ":"))
    except Exception:
        output = json.dumps(unavailable(), separators=(",", ":"))
    print(output)


if __name__ == "__main__":
    main()
