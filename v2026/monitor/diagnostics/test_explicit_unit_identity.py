"""Synthetic exact-unit identity controls; no service, sudo, watcher or Main contact."""
import dataclasses
import json
import pathlib
import tempfile
import unittest
from unittest import mock

import process_identity as identity
import promotion_identity as promotion
import promote_monitor as promoter

CURRENT_UNIT = "synthetic-current-monitor.service"
FOREIGN_UNIT = "synthetic-foreign-monitor.service"
BOOT = "12345678-1234-1234-1234-123456789012"


def authority(unit=CURRENT_UNIT, pid=12345, ticks=987654):
    """Construct visibly synthetic process authority, never a retained live identity."""
    return dict(unit=unit, pid=pid, sha256="a"*64, start_ticks=ticks, boot_id=BOOT, n_restarts=0)


class ExplicitUnitTests(unittest.TestCase):
    """Exercise real verifier/capture/marker composition behind synthetic OS boundaries."""

    def setUp(self):
        """Each control has one independently bound exact service and clean command trace."""
        self.scope = identity.UnitAuthority(CURRENT_UNIT)
        self.commands = []
        self.reads = []

    def read(self, path, limit):
        """Only expected synthetic proc fields are available."""
        self.reads.append(path)
        if path == identity.BOOT_PATH:
            return BOOT.encode()
        self.assertEqual(path, "/proc/12345/stat")
        return b"12345 (synthetic monitor) S " + b"0 "*18 + b"987654 0\n"

    def command(self, argv, deadline):
        """Record exact local argv without creating a child or reading live state."""
        self.commands.append(argv)
        if argv[0] == "/usr/bin/systemctl":
            self.assertEqual(argv, ["/usr/bin/systemctl", "--user", "show", self.expected_unit,
                                   "--property=ActiveState,SubState,MainPID,NRestarts"])
            return 0, b"ActiveState=active\nSubState=running\nMainPID=12345\nNRestarts=0\n", 0
        self.assertEqual(argv, ["/usr/bin/sudo", "-n", "--", "/usr/bin/timeout",
                               "--signal=TERM", "--kill-after=0.2s", "2s",
                               "/usr/bin/sha256sum", "/proc/12345/exe"])
        return 0, b"a"*64 + b"  /proc/12345/exe\n", 0

    def assert_cause(self, cause, call):
        """Typed finite refusal must not carry source diagnostic text."""
        with self.assertRaises(identity.AuditError) as raised:
            call()
        self.assertEqual(raised.exception.cause, cause)
        self.assertEqual(str(raised.exception), cause)

    def test_explicit_current_unit_healthy_exact_generation(self):
        """A current exact service uses both unit checks and exactly one fixed proc hash."""
        self.expected_unit = CURRENT_UNIT
        with mock.patch.object(identity, "_read", side_effect=self.read), mock.patch.object(identity, "_run_bounded", side_effect=self.command):
            result = identity.verify(authority(), unit_authority=self.scope)
        self.assertEqual(result["watcher_pid"], 12345)
        self.assertEqual(result["watcher_sha256"], "a"*64)
        self.assertEqual(len(self.commands), 3)
        self.assertEqual(sum(row[0] == "/usr/bin/sudo" for row in self.commands), 1)

    def test_legacy_default_exact_commands_and_receipt(self):
        """Omitting the new keyword preserves cap2-only authority and original receipt fields."""
        self.expected_unit = identity.UNIT
        with mock.patch.object(identity, "_read", side_effect=self.read), mock.patch.object(identity, "_run_bounded", side_effect=self.command):
            result = identity.verify(authority(identity.UNIT))
        self.assertEqual(set(result), {"at", "watcher_pid", "watcher_sha256", "watcher_start_ticks", "watcher_boot_id", "monotonic_completed", "ssh_created", "audit_policy"})
        self.assertEqual(len(self.commands), 3)

    def test_missing_or_foreign_scope_refuses_before_contact(self):
        """An observed service string cannot grant its own expected-unit authority."""
        with mock.patch.object(identity, "_read") as read, mock.patch.object(identity, "_run_bounded") as run:
            self.assert_cause("invalid_authority", lambda: identity.verify(authority()))
            self.assert_cause("invalid_authority", lambda: identity.verify(authority(FOREIGN_UNIT), unit_authority=self.scope))
            self.assert_cause("invalid_authority", lambda: identity.verify(authority(), unit_authority=None))
            read.assert_not_called()
            run.assert_not_called()

    def test_invalid_unit_literals_and_untyped_scope_never_contact(self):
        """Paths, options, globs, templates, control text and unknown scope remain unavailable."""
        invalid = ["", "--all.service", "/tmp/synthetic.service", "a*.service", "a@b.service", "a\n.service", "x"*241 + ".service", "synthetic.socket", None, True]
        with mock.patch.object(identity, "_read") as read, mock.patch.object(identity, "_run_bounded") as run:
            for value in invalid:
                self.assert_cause("invalid_authority", lambda: identity.verify(authority(), unit_authority=identity.UnitAuthority(value)))
            self.assert_cause("invalid_authority", lambda: promotion.current("a"*64, unit_authority={"unit": CURRENT_UNIT}))
            read.assert_not_called()
            run.assert_not_called()

    def test_unit_authority_is_immutable(self):
        """The expected unit cannot change between validation and the second unit check."""
        with self.assertRaises(dataclasses.FrozenInstanceError):
            self.scope.unit = FOREIGN_UNIT

    def test_wrong_process_unit_state_refuses_before_hash(self):
        """Another MainPID does not justify privileged hashing of the expected PID."""
        with mock.patch.object(identity, "_read") as read, mock.patch.object(identity, "_run_bounded", return_value=(0, b"ActiveState=active\nSubState=running\nMainPID=99999\nNRestarts=0\n", 0)) as run:
            self.assert_cause("unit_mismatch", lambda: identity.verify(authority(), unit_authority=self.scope))
            read.assert_not_called()
            self.assertEqual(run.call_count, 1)

    def test_wrong_generation_refuses_before_privileged_hash(self):
        """PID, start-tick and boot mismatch stop before any sudo command."""
        self.expected_unit = CURRENT_UNIT
        for changed in (dict(pid=12346), dict(start_ticks=987655), dict(boot_id="87654321-1234-1234-1234-123456789012")):
            self.commands = []
            def read(path, limit):
                """Return a different tuple using the requested path only as synthetic input."""
                return BOOT.encode() if path == identity.BOOT_PATH else b"12345 (synthetic) S " + b"0 "*18 + b"987654 0\n"
            with mock.patch.object(identity, "_read", side_effect=read), mock.patch.object(identity, "_run_bounded", side_effect=self.command):
                # A changed requested PID is caught at unit state, the others at proc generation.
                self.assert_cause("unit_mismatch" if "pid" in changed else "generation_mismatch", lambda: identity.verify(dict(authority(), **changed), unit_authority=self.scope))
            self.assertEqual(len(self.commands), 1)

    def test_wrong_digest_remains_unknown_after_one_hash(self):
        """A well-formed wrong digest needs the one permitted hash; no fallback or retry exists."""
        self.expected_unit = CURRENT_UNIT
        with mock.patch.object(identity, "_read", side_effect=self.read), mock.patch.object(identity, "_run_bounded", side_effect=self.command):
            self.assert_cause("hash_mismatch", lambda: identity.verify(dict(authority(), sha256="b"*64), unit_authority=self.scope))
        self.assertEqual(len(self.commands), 2)

    def test_scoped_current_composes_actual_verifier(self):
        """Generation capture discovers only the explicitly expected unit and verifies it again."""
        self.expected_unit = CURRENT_UNIT
        with mock.patch.object(identity, "_read", side_effect=self.read), mock.patch.object(identity, "_run_bounded", side_effect=self.command):
            captured, audit = promotion.current("a"*64, unit_authority=self.scope)
        self.assertEqual(captured, authority())
        self.assertEqual(audit["watcher_start_ticks"], 987654)
        self.assertEqual(len(self.commands), 4)

    def test_same_unit_replacement_composes_current_and_old_absence(self):
        """The new scope supports the existing same-unit check, never cross-unit cutover."""
        self.expected_unit = CURRENT_UNIT
        with mock.patch.object(identity, "_read", side_effect=self.read), mock.patch.object(identity, "_run_bounded", side_effect=self.command), mock.patch.object(pathlib.Path, "stat", side_effect=FileNotFoundError()):
            result = promotion.replacement(authority(pid=12344, ticks=987653), "a"*64, unit_authority=self.scope)
        self.assertTrue(result["old_pid_retired"])
        self.assertEqual(result["authority"], authority())

    def test_replacement_and_marker_foreign_scope_never_contact(self):
        """A cross-unit predecessor cannot query state or spend the durable attempt marker."""
        with mock.patch.object(identity, "_read") as read, mock.patch.object(identity, "_run_bounded") as run, mock.patch.object(promotion.os, "open") as opened:
            self.assert_cause("invalid_authority", lambda: promotion.replacement(authority(FOREIGN_UNIT), "a"*64, unit_authority=self.scope))
            self.assert_cause("invalid_authority", lambda: promotion.begin_once("/synthetic-not-opened", authority(FOREIGN_UNIT), "a"*64, unit_authority=self.scope))
            read.assert_not_called()
            run.assert_not_called()
            opened.assert_not_called()

    def test_scoped_marker_conserves_one_attempt_and_original_schema(self):
        """Private synthetic marker custody remains exclusive, durable and nonretryable."""
        with tempfile.TemporaryDirectory() as root:
            result = promotion.begin_once(root, authority(), "b"*64, unit_authority=self.scope)
            marker = pathlib.Path(root) / "promotion-attempt.json"
            before = marker.read_bytes()
            self.assertEqual(json.loads(before), result)
            self.assertEqual(result["old_authority"]["unit"], CURRENT_UNIT)
            self.assertFalse(result["automatic_retry"])
            with self.assertRaises(FileExistsError):
                promotion.begin_once(root, authority(), "b"*64, unit_authority=self.scope)
            self.assertEqual(marker.read_bytes(), before)
            self.assertEqual(marker.stat().st_mode & 511, 0o600)

    def test_mutating_legacy_promoter_refuses_current_unit_before_io(self):
        """Typed read-only support does not widen fixed binary paths or restart authority."""
        with mock.patch.object(promoter, "sha") as hashed, mock.patch.object(promoter.subprocess, "check_output") as command:
            with self.assertRaises(ValueError):
                promoter.preflight({"unit": CURRENT_UNIT, "authorized": True}, pathlib.Path("/synthetic-not-opened"))
            hashed.assert_not_called()
            command.assert_not_called()


if __name__ == "__main__":
    unittest.main()
