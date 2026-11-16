import os
import pathlib
import sys
import time
import unittest
from unittest.mock import patch

import process_identity as identity

AUTHORITY = dict(unit=identity.UNIT, pid=12345, sha256="a" * 64,
                 start_ticks=987654, boot_id="12345678-1234-1234-1234-123456789012",
                 n_restarts=0)
PRIVATE = "private-stderr-must-not-escape"


def stat(ticks=987654, pid=12345):
    return f"{pid} (monitor ) arbitrary) S ".encode() + b"0 " * 18 + str(ticks).encode() + b" 0\n"


class ProcessIdentityTests(unittest.TestCase):
    def read(self, path, limit):
        if path == identity.BOOT_PATH:
            return (AUTHORITY["boot_id"] + "\n").encode()
        self.assertEqual(path, "/proc/12345/stat")
        return stat()

    def command(self, argv, deadline):
        self.commands.append(argv)
        self.assertLessEqual(deadline - time.monotonic(), 2.501)
        if argv[0] == "/usr/bin/systemctl":
            self.assertEqual(argv, ["/usr/bin/systemctl", "--user", "show", identity.UNIT,
                                   "--property=ActiveState,SubState,MainPID,NRestarts"])
            return 0, b"ActiveState=active\nSubState=running\nMainPID=12345\nNRestarts=0\n", 0
        self.assertEqual(argv, ["/usr/bin/sudo", "-n", "--", "/usr/bin/timeout",
                               "--signal=TERM", "--kill-after=0.2s", "2s",
                               "/usr/bin/sha256sum", "/proc/12345/exe"])
        return 0, (AUTHORITY["sha256"] + "  /proc/12345/exe\n").encode(), 0

    def setUp(self):
        self.commands = []

    def verify(self, read=None, command=None, authority=None):
        with patch.object(identity, "_read", side_effect=read or self.read), \
                patch.object(identity, "_run_bounded", side_effect=command or self.command):
            return identity.verify(authority or AUTHORITY)

    def assertCause(self, cause, fn):
        with self.assertRaises(identity.AuditError) as raised:
            fn()
        self.assertEqual(raised.exception.cause, cause)
        self.assertEqual(str(raised.exception), cause)
        self.assertNotIn(PRIVATE, str(raised.exception))

    def test_exact_healthy_generation_and_read_only_commands(self):
        result = self.verify()
        self.assertEqual(result["watcher_pid"], 12345)
        self.assertEqual(result["watcher_start_ticks"], 987654)
        self.assertEqual(result["watcher_boot_id"], AUTHORITY["boot_id"])
        self.assertEqual(result["watcher_sha256"], AUTHORITY["sha256"])
        self.assertEqual(result["ssh_created"], 0)
        self.assertEqual(len(self.commands), 3)
        self.assertFalse(any("restart" in command or "kill" in command for command in self.commands))

    def test_unprivileged_exe_permission_not_used_or_retried(self):
        # The old verifier opened proc/exe directly after a successful restart.
        with patch.object(pathlib.Path, "read_bytes", side_effect=PermissionError(PRIVATE)):
            self.verify()
        self.assertEqual(sum(command[0] == "/usr/bin/sudo" for command in self.commands), 1)

    def test_invalid_authority_never_launches_or_reads(self):
        changes = [dict(pid=True), dict(pid=0), dict(pid="12345"), dict(pid=4194305),
                   dict(unit="other.service"), dict(sha256="b" * 63), dict(start_ticks=True),
                   dict(start_ticks=0), dict(boot_id="unknown"), dict(n_restarts=True), dict(n_restarts=1)]
        for change in changes:
            with self.subTest(change=change):
                authority = dict(AUTHORITY, **change)
                with patch.object(identity, "_read") as read, patch.object(identity, "_run_bounded") as command:
                    self.assertCause("invalid_authority", lambda: identity.verify(authority))
                    read.assert_not_called(); command.assert_not_called()
        self.assertCause("invalid_authority", lambda: identity.verify(dict(AUTHORITY, extra=1)))

    def test_sudo_refusal_no_fallback_or_retry(self):
        def command(argv, deadline):
            if argv[0] == "/usr/bin/sudo":
                self.commands.append(argv)
                return 1, b"", len(PRIVATE)
            return self.command(argv, deadline)
        self.assertCause("sudo_or_hash_failed", lambda: self.verify(command=command))
        self.assertEqual(len(self.commands), 2)

    def test_hash_output_is_exact_path_one_line_and_hash(self):
        for output, cause in [(b"", "hash_invalid"), (b"b" * 64 + b"  /proc/12345/exe\n", "hash_mismatch"),
                              (b"a" * 64 + b"  /proc/12346/exe\n", "hash_invalid"),
                              (b"a" * 64 + b"  /tmp/installed\n", "hash_invalid"),
                              (b"a" * 64 + b"  /proc/12345/exe\nextra\n", "hash_invalid")]:
            def command(argv, deadline):
                return (0, output, 0) if argv[0] == "/usr/bin/sudo" else self.command(argv, deadline)
            with self.subTest(cause=cause):
                self.assertCause(cause, lambda: self.verify(command=command))

    def test_privileged_hash_timeout_is_finite(self):
        def command(argv, deadline):
            return (124, b"", 0) if argv[0] == "/usr/bin/sudo" else self.command(argv, deadline)
        self.assertCause("hash_timeout", lambda: self.verify(command=command))

    def test_proc_generation_mismatch_before_hash(self):
        def read(path, limit):
            return stat(ticks=987655) if path.endswith("/stat") else self.read(path, limit)
        self.assertCause("generation_mismatch", lambda: self.verify(read=read))
        self.assertEqual(len(self.commands), 1)

    def test_same_pid_reused_during_hash_is_rejected(self):
        calls = 0
        def read(path, limit):
            nonlocal calls
            if path.endswith("/stat"):
                calls += 1
                return stat(ticks=987654 if calls == 1 else 987655)
            return self.read(path, limit)
        self.assertCause("generation_mismatch", lambda: self.verify(read=read))
        self.assertEqual(len(self.commands), 2)

    def test_boot_change_and_wrong_pid_rejected(self):
        for bad_path in (identity.BOOT_PATH, "/proc/12345/stat"):
            def read(path, limit):
                if path == bad_path:
                    return b"changed-boot\n" if path == identity.BOOT_PATH else stat(pid=12346)
                return self.read(path, limit)
            self.assertCause("generation_mismatch", lambda: self.verify(read=read))

    def test_unit_changes_after_hash_are_rejected(self):
        calls = 0
        def command(argv, deadline):
            nonlocal calls
            answer = self.command(argv, deadline)
            if argv[0] == "/usr/bin/systemctl":
                calls += 1
                if calls == 2:
                    return 0, answer[1].replace(b"MainPID=12345", b"MainPID=99999"), 0
            return answer
        self.assertCause("unit_mismatch", lambda: self.verify(command=command))

    def test_inactive_restarted_malformed_duplicate_unit(self):
        good = self.command(["/usr/bin/systemctl", "--user", "show", identity.UNIT,
                             "--property=ActiveState,SubState,MainPID,NRestarts"], time.monotonic() + 1)[1]
        for out, cause in [(good.replace(b"active", b"inactive"), "unit_mismatch"),
                           (good.replace(b"NRestarts=0", b"NRestarts=1"), "unit_mismatch"),
                           (good + b"MainPID=12345\n", "unit_invalid"),
                           (b"not-kv", "unit_invalid")]:
            self.assertCause(cause, lambda: self.verify(command=lambda *args: (0, out, 0)))

    def test_proc_unavailable_and_malformed_remain_unknown(self):
        with patch("builtins.open", side_effect=PermissionError(PRIVATE)):
            self.assertCause("proc_unavailable", lambda: identity._read("/proc/12345/stat", 8192))
        self.assertCause("proc_invalid", lambda: self.verify(read=lambda *args: b"malformed"))

    def test_owner_deadline_not_reset_before_privileged_command(self):
        with patch.object(identity, "_run_bounded") as command:
            self.assertCause("owner_timeout", lambda: identity._hash(AUTHORITY, time.monotonic() + 2.0))
            command.assert_not_called()

    def test_real_bounded_child_success_and_private_stderr_count_only(self):
        result = identity._run_bounded([sys.executable, "-I", "-c",
            "import sys;print('finite');sys.stderr.write('" + PRIVATE + "')"], time.monotonic() + 2)
        self.assertEqual(result, (0, b"finite\n", len(PRIVATE)))

    def test_real_child_output_caps(self):
        for stream in ("stdout", "stderr"):
            with self.subTest(stream=stream):
                self.assertCause("command_output_cap", lambda: identity._run_bounded(
                    [sys.executable, "-I", "-c", f"import sys;sys.{stream}.write('x'*8192)"],
                    time.monotonic() + 2))

    def test_real_stalled_child_is_reaped_within_bound(self):
        began = time.monotonic()
        with self.assertRaises(identity.AuditError) as raised:
            identity._run_bounded([sys.executable, "-I", "-c", "import time;time.sleep(5)"], began + .2)
        self.assertIn(raised.exception.cause, ("command_timeout", "owner_timeout"))
        self.assertLess(time.monotonic() - began, .5)

    def test_missing_tool_is_private_and_finite(self):
        self.assertCause("tool_unavailable", lambda: identity._run_bounded(
            ["/nonexistent/" + PRIVATE], time.monotonic() + 1))

    def test_environment_removes_loader_and_secret_overrides(self):
        with patch.dict(os.environ, {"LD_PRELOAD": PRIVATE, "PGPASSWORD": PRIVATE,
                                     "PYTHONPATH": PRIVATE, "SYSTEMD_PAGER": PRIVATE}):
            env = identity._environment()
        self.assertNotIn(PRIVATE, str(env))
        self.assertEqual(env["PATH"], "/usr/bin:/bin")


if __name__ == "__main__":
    unittest.main()
