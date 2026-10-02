#!/usr/bin/env python3
"""Focused failure/reuse checks; these do not run the service's test suite."""

import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
SCRIPT_DIR = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("report_evidence", SCRIPT_DIR / "test-report-evidence.py")
evidence = importlib.util.module_from_spec(spec)
spec.loader.exec_module(evidence)


class ExecutionEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.directory = Path(self.tmp.name)
        self.inputs = {"source_fingerprint": "source", "toolchain_fingerprint": "toolchain",
                       "packages": {"sample/api": True, "sample/cmd": False}, "command": evidence.COMMAND}
        events = [
            {"Package": "sample/api", "Action": "run", "Test": "TestIntegration"},
            {"Package": "sample/api", "Action": "pass", "Test": "TestIntegration"},
            {"Package": "sample/api", "Action": "pass"},
            {"Package": "sample/cmd", "Action": "skip"},
        ]
        self.events = events
        self.write_events(events)
        (self.directory / "coverage.out").write_text("mode: atomic\nsample/api/api.go:1.1,2.2 1 1\n")
        (self.directory / "gofmt.txt").write_text("")
        (self.directory / "build.txt").write_text("")
        self.snapshot = patch.object(evidence, "snapshot", return_value=self.inputs)
        self.snapshot.start()
        self.addCleanup(self.snapshot.stop)
        self.run = patch.object(evidence, "run", return_value="a" * 40)
        self.run.start()
        self.addCleanup(self.run.stop)

    def write_events(self, events):
        (self.directory / "test-events.json").write_text("".join(json.dumps(event) + "\n" for event in events))

    def seal(self):
        evidence.evidence("begin", self.directory)
        evidence.evidence("seal", self.directory)

    def test_completed_execution_can_render_without_database_environment(self):
        with patch.dict(os.environ, {"REPORT_FIXTURE_ID": "sha256:abc123"}, clear=True):
            self.seal()
        with patch.dict(os.environ, {}, clear=True):
            evidence.evidence("validate", self.directory)
        record = json.loads((self.directory / "execution-evidence.json").read_text())
        self.assertEqual(record["fixture_id"], "sha256:abc123")
        self.assertEqual(record["status"], "PASS")
        self.assertEqual(set(record["artifacts"]), set(evidence.ARTIFACTS))

    def test_failed_or_incomplete_test_events_cannot_be_sealed(self):
        cases = {
            "missing package": self.events[:-1],
            "failed test": [dict(event, Action="fail") if event.get("Test") and event["Action"] == "pass" else event for event in self.events],
            "unfinished test": [event for event in self.events if not (event.get("Test") and event["Action"] == "pass")],
            "unexpected package": self.events + [{"Package": "sample/other", "Action": "pass"}],
            "skipped test package": [dict(event, Action="skip") if not event.get("Test") and event["Package"] == "sample/api" else event for event in self.events],
            "duplicate package completion": self.events + [self.events[-1]],
        }
        for label, events in cases.items():
            with self.subTest(label=label):
                self.write_events(events)
                evidence.evidence("begin", self.directory)
                with self.assertRaises(ValueError):
                    evidence.evidence("seal", self.directory)
                self.assertFalse((self.directory / "execution-evidence.json").exists())

    def test_changed_source_or_toolchain_rejects_reuse(self):
        for field in ("source_fingerprint", "toolchain_fingerprint"):
            with self.subTest(field=field):
                self.seal()
                with patch.object(evidence, "snapshot", return_value=dict(self.inputs, **{field: "changed"})):
                    with self.assertRaisesRegex(ValueError, "does not match"):
                        evidence.evidence("validate", self.directory)

    def test_mutated_artifact_rejects_reuse(self):
        self.seal()
        (self.directory / "build.txt").write_text("changed build output")
        with self.assertRaisesRegex(ValueError, "changed after completion"):
            evidence.evidence("validate", self.directory)

    def test_missing_original_fixture_identity_rejects_reuse(self):
        self.seal()
        path = self.directory / "execution-evidence.json"
        record = json.loads(path.read_text())
        del record["fixture_id"]
        path.write_text(json.dumps(record))
        with self.assertRaisesRegex(ValueError, "fixture identity"):
            evidence.evidence("validate", self.directory)

    def test_optional_external_binary_execution_cannot_be_reused_by_path_alone(self):
        with patch.dict(os.environ, {"TEST_FACTORY_ADMISSION_CLIENT": "/tmp/fixture-client"}):
            self.seal()
            with self.assertRaisesRegex(ValueError, "external contract/hardware"):
                evidence.evidence("validate", self.directory)

    def test_invalid_coverage_and_formatting_never_seal(self):
        for name, content in (("coverage.out", "mode: set\nsample/api/api.go:1.1,2.2 1 1\n"),
                              ("coverage.out", "mode: atomic\n"),
                              ("gofmt.txt", "api.go\n")):
            with self.subTest(name=name, content=content):
                original = (self.directory / name).read_text()
                (self.directory / name).write_text(content)
                evidence.evidence("begin", self.directory)
                with self.assertRaises(ValueError):
                    evidence.evidence("seal", self.directory)
                (self.directory / name).write_text(original)

    def test_new_execution_invalidates_older_passing_evidence(self):
        self.seal()
        evidence.evidence("begin", self.directory)
        self.assertFalse((self.directory / "execution-evidence.json").exists())
        with self.assertRaises(FileNotFoundError):
            evidence.evidence("validate", self.directory)

    def test_credentials_are_not_accepted_as_fixture_identity(self):
        with patch.dict(os.environ, {"REPORT_FIXTURE_ID": "postgres://user:password@localhost/db"}):
            with self.assertRaisesRegex(ValueError, "non-secret"):
                evidence.evidence("begin", self.directory)


class SourceFingerprintTests(unittest.TestCase):
    def test_contract_link_contents_and_presence_are_fingerprinted(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "account"
            (root / "docs").mkdir(parents=True)
            contracts = Path(temporary) / "contracts"
            (root / "docs/rtk_cloud_contracts_doc").symlink_to(contracts)

            def git_result(*args):
                if "-C" not in args:
                    return "docs/rtk_cloud_contracts_doc\0"
                return str(contracts) if "rev-parse" in args else "SPEC.md\0"

            with patch.object(evidence, "run", side_effect=git_result):
                missing = evidence.source_fingerprint(root, root / "reports")
                contracts.mkdir()
                (contracts / "SPEC.md").write_text("first contract")
                present = evidence.source_fingerprint(root, root / "reports")
                self.assertNotEqual(missing, present)
                (contracts / "SPEC.md").write_text("changed contract at the same link")
                self.assertNotEqual(present, evidence.source_fingerprint(root, root / "reports"))
                before_mode = evidence.source_fingerprint(root, root / "reports")
                (contracts / "SPEC.md").chmod(0o755)
                self.assertNotEqual(before_mode, evidence.source_fingerprint(root, root / "reports"))

    def test_test_environment_changes_invalidate_but_output_and_primary_database_do_not(self):
        with patch.dict(os.environ, {"JWT_ACCESS_SECRET": "fixture", "TEST_DATABASE_URL": "first"}, clear=True):
            original = evidence.environment_fingerprint()
            with patch.dict(os.environ, {"TEST_DATABASE_URL": "second", "REPORT_FILE": "new-candidate", "PWD": "new-checkout"}):
                self.assertEqual(original, evidence.environment_fingerprint())
            with patch.dict(os.environ, {"JWT_ACCESS_SECRET": "changed-fixture"}):
                self.assertNotEqual(original, evidence.environment_fingerprint())
            with patch.dict(os.environ, {"NEW_TEST_FEATURE_FLAG": "true"}):
                self.assertNotEqual(original, evidence.environment_fingerprint())

    def test_generated_report_commit_does_not_invalidate_source_but_source_mode_and_env_do(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "docs").mkdir()
            (root / "source.go").write_text("package source\n")
            (root / "docs/test_report.md").write_text("old report")
            (root / ".env").write_text("TEST_SECRET=local-only")
            with patch.object(evidence, "run", return_value="source.go\0docs/test_report.md\0"):
                before = evidence.source_fingerprint(root, root / "reports")
                (root / "docs/test_report.md").write_text("new report")
                self.assertEqual(before, evidence.source_fingerprint(root, root / "reports"))
                (root / "source.go").chmod(0o755)
                self.assertNotEqual(before, evidence.source_fingerprint(root, root / "reports"))
                before_env = evidence.source_fingerprint(root, root / "reports")
                (root / ".env").write_text("TEST_SECRET=changed-local-only")
                self.assertNotEqual(before_env, evidence.source_fingerprint(root, root / "reports"))
                before_delete = evidence.source_fingerprint(root, root / "reports")
                (root / "source.go").unlink()
                self.assertNotEqual(before_delete, evidence.source_fingerprint(root, root / "reports"))


class ReportFailureTests(unittest.TestCase):
    def test_format_test_and_build_failures_preserve_report_and_do_not_leak_raw_logs(self):
        for failure in ("format", "test", "build"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                binaries = root / "bin"
                binaries.mkdir()
                (binaries / "gofmt").write_text("#!/bin/sh\n[ \"$FAKE_FAILURE\" != format ] || echo unformatted.go\nexit 0\n")
                (binaries / "go").write_text("""#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
with Path(os.environ['FAKE_CALLS']).open('a') as log:
    log.write(sys.argv[1] + '\\n')
if sys.argv[1] == 'env':
    print(json.dumps({'GOVERSION':'go-test', 'GOOS':'test', 'GOARCH':'test', 'GOWORK':'off'}))
elif sys.argv[1] == 'list':
    print('sample/api test' if '-f' in sys.argv else 'sample/api')
elif sys.argv[1] == 'test':
    print('private diagnostic: must-not-appear', file=sys.stderr)
    sys.exit(1 if os.environ['FAKE_FAILURE'] == 'test' else 0)
elif sys.argv[1] == 'build':
    print('private build diagnostic: must-not-appear')
    sys.exit(1)
""")
                for executable in binaries.iterdir():
                    executable.chmod(0o755)
                report = root / "test_report.md"
                report.write_text("maintained report")
                outputs = root / "reports"
                outputs.mkdir()
                (outputs / "execution-evidence.json").write_text("obsolete pass")
                calls = root / "calls"
                environment = dict(os.environ, PATH=str(binaries) + os.pathsep + os.environ["PATH"],
                                   REPORT_DIR=str(outputs), REPORT_FILE=str(report),
                                   TEST_DATABASE_URL="local-test-no-network", FAKE_FAILURE=failure, FAKE_CALLS=str(calls))
                environment.pop("REPORT_REUSE_DIR", None)
                result = subprocess.run(["bash", str(SCRIPT_DIR / "test-report.sh")], env=environment,
                                        text=True, capture_output=True, timeout=30)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(report.read_text(), "maintained report")
                self.assertFalse((outputs / "execution-evidence.json").exists())
                self.assertNotIn("must-not-appear", result.stdout + result.stderr)
                executed = calls.read_text().splitlines() if calls.exists() else []
                if failure == "format":
                    self.assertNotIn("test", executed)
                else:
                    self.assertIn("test", executed)
                self.assertFalse((outputs / ".test-report.lock").exists())


if __name__ == "__main__":
    unittest.main()
