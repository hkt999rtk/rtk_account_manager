#!/usr/bin/env python3
"""Seal and verify one completed test-report execution; never execute tests."""

import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys


ARTIFACTS = ("test-events.json", "coverage.out", "gofmt.txt", "build.txt")
COMMAND = ["go", "test", "-p=1", "-json", "-count=1", "-timeout=20m", "./...",
           "-coverpkg=./internal/...", "-coverprofile=<evidence>/coverage.out", "-covermode=atomic"]
FIXTURE_PROFILE = "postgresql-full-integration-v1"
EXTERNAL_INPUTS = ("TEST_FACTORY_ADMISSION_CLIENT", "TEST_FACTORY_APPLICATION_BINARY",
                   "TEST_FACTORY_APPLICATION_DSN", "TEST_BILLING_BOOTSTRAP_BINARY",
                   "TEST_BILLING_BOOTSTRAP_DIR", "TEST_BILLING_BOOTSTRAP_DSN",
                   "ACCOUNT_MANAGER_TEST_PKCS11_MODULE_PATH", "VIDEO_CLOUD_SOURCE_ROOT",
                   "VIDEO_CLOUD_TEST_DSN", "TEST_BILLING_HANDOFF_URL",
                   "TRANSFER_FENCE_E2E_BASE_URL", "ACTIVATION_REPLAY_E2E_BASE_URL")


def digest(data):
    return hashlib.sha256(data).hexdigest()


def run(*args):
    return subprocess.check_output(args, stderr=subprocess.PIPE).decode().strip()


def environment_fingerprint():
    # Keep test/application settings, including unknown future settings. Ignore
    # only output controls and host/runner metadata that cannot choose test paths.
    ignored = {"DATABASE_URL", "TEST_DATABASE_URL", "PWD", "OLDPWD", "SHLVL", "_",
               "PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM", "COLORTERM",
               "TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL", "LC_CTYPE", "CI",
               "PAGER", "GIT_PAGER", "GH_PAGER", "CLICOLOR", "NO_COLOR", "LSCOLORS",
               "LS_COLORS", "COMMAND_MODE", "DISPLAY", "SSH_AUTH_SOCK"}
    prefixes = ("REPORT_", "GITHUB_", "RUNNER_", "CODEX_", "XPC_", "__CF_",
                "BROWSER_USE_", "NODE_REPL_", "ZSH_", "TERM_PROGRAM")
    settings = {key: value for key, value in os.environ.items()
                if key not in ignored and not key.startswith(prefixes)}
    return digest(json.dumps(settings, sort_keys=True).encode())


def valid_fixture_id(value):
    return isinstance(value, str) and re.fullmatch(r"[A-Za-z0-9_.:@+/-]{1,256}", value) and "://" not in value


def source_fingerprint(root, evidence_dir, unverified_symlinks=None):
    """Hash inputs rather than HEAD so committing a generated report is harmless.

    Deliberately conservative: all tracked and nonignored files count except the
    generated maintained report and this execution's output directory. A docs
    change elsewhere can be checked separately; it is not silently classified.
    """
    paths = set(run("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z").split("\0"))
    if (root / ".env").is_file():
        paths.add(".env")
    hashes = []
    for name in sorted(paths - {"", "docs/test_report.md"}):
        path = root / name
        if path == evidence_dir or evidence_dir in path.parents:
            continue
        if not path.exists() and not path.is_symlink():
            hashes.append([name, "deleted"])
            continue
        if path.is_symlink():
            content = b"symlink:" + os.readlink(path).encode()
            if name == "docs/rtk_cloud_contracts_doc" and path.exists():
                dependency = path.resolve()
                # The maintained contracts link is a real test dependency.
                # Hash its bounded tracked/nonignored repository file set, not
                # merely the symlink text or HEAD (which misses dirty changes).
                try:
                    if Path(run("git", "-C", str(dependency), "rev-parse", "--show-toplevel")).resolve() != dependency:
                        raise ValueError("contracts link is not a repository root")
                    names = set(run("git", "-C", str(dependency), "ls-files", "--cached", "--others", "--exclude-standard", "-z").split("\0"))
                    for linked_name in sorted(names - {""}):
                        linked = dependency / linked_name
                        label = name + "/" + linked_name
                        if linked.is_symlink():
                            if unverified_symlinks is not None:
                                unverified_symlinks.append(label)
                            hashes.append([label, "unverified-symlink", os.readlink(linked)])
                        elif linked.exists():
                            hashes.append([label, linked.stat().st_mode & 0o111, digest(linked.read_bytes())])
                        else:
                            hashes.append([label, "deleted"])
                except (ValueError, OSError, subprocess.CalledProcessError):
                    if unverified_symlinks is not None:
                        unverified_symlinks.append(name)
                    hashes.append([name, "unverified-contracts-directory"])
            elif name == "docs/rtk_cloud_contracts_doc":
                hashes.append([name, "missing-contracts-directory"])
            elif unverified_symlinks is not None:
                unverified_symlinks.append(name)
        else:
            content = path.read_bytes()
        hashes.append([name, path.lstat().st_mode & 0o111, digest(content)])
    return digest(json.dumps(hashes, separators=(",", ":")).encode())


def snapshot(evidence_dir):
    root = Path(run("git", "rev-parse", "--show-toplevel")).resolve()
    if Path.cwd().resolve() != root:
        raise ValueError("run the report from the repository root")
    if evidence_dir == root:
        raise ValueError("evidence directory must not be the repository root")
    environment = json.loads(run("go", "env", "-json", "GOVERSION", "GOOS", "GOARCH",
                                 "CGO_ENABLED", "GOEXPERIMENT", "GOFLAGS", "GOTOOLCHAIN", "GOWORK",
                                 "CC", "CXX", "CGO_CFLAGS", "CGO_CPPFLAGS", "CGO_CXXFLAGS", "CGO_LDFLAGS",
                                 "GOAMD64", "GOARM", "GOARM64"))
    if environment["GOWORK"] != "off":
        raise ValueError("test-report requires GOWORK=off")
    packages = {}
    for line in run("go", "list", "-f", "{{.ImportPath}} {{if or .TestGoFiles .XTestGoFiles}}test{{else}}no-test{{end}}", "./...").splitlines():
        name, kind = line.split()
        packages[name] = kind == "test"
    if not packages:
        raise ValueError("no Go packages found")
    unverified_symlinks = []
    fingerprint = source_fingerprint(root, evidence_dir, unverified_symlinks)
    return {
        "source_fingerprint": fingerprint,
        "unverified_symlinks": unverified_symlinks,
        "toolchain_fingerprint": digest(json.dumps(environment, sort_keys=True).encode()),
        "environment_fingerprint": environment_fingerprint(),
        "go_version": environment["GOVERSION"],
        "platform": environment["GOOS"] + "/" + environment["GOARCH"],
        "packages": packages,
        "command": COMMAND,
    }


def validate_events(path, packages):
    completed = {}
    running = set()
    passed = 0
    with path.open() as events:
        for line in events:
            event = json.loads(line)
            package, action, test = event.get("Package"), event.get("Action"), event.get("Test")
            if package not in packages:
                raise ValueError("test events contain an unexpected package")
            if action == "fail":
                raise ValueError("test events contain a failure")
            if test:
                key = (package, test)
                if action == "run":
                    running.add(key)
                elif action in ("pass", "skip"):
                    if key not in running:
                        raise ValueError("test completion lacks a start event")
                    running.remove(key)
                    passed += action == "pass"
            elif action in ("pass", "skip"):
                if package in completed:
                    raise ValueError("package has duplicate completion events")
                completed[package] = action
    if running or set(completed) != set(packages) or not passed:
        raise ValueError("test execution is incomplete")
    if any(packages[name] and action != "pass" for name, action in completed.items()):
        raise ValueError("a package with tests did not pass")


def validate_artifacts(directory, packages):
    for name in ARTIFACTS:
        path = directory / name
        if not path.is_file() or path.is_symlink():
            raise ValueError("missing or unsafe execution artifact: " + name)
    if (directory / "gofmt.txt").read_bytes():
        raise ValueError("formatting did not pass")
    profile = (directory / "coverage.out").read_text().splitlines()
    if len(profile) < 2 or profile[0] != "mode: atomic":
        raise ValueError("expected a complete atomic coverage profile")
    for line in profile[1:]:
        if not re.fullmatch(r"\S+:\d+\.\d+,\d+\.\d+ \d+ \d+", line):
            raise ValueError("malformed coverage profile")
    validate_events(directory / "test-events.json", packages)
    return {name: digest((directory / name).read_bytes()) for name in ARTIFACTS}


def write_json(path, value):
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")
    temporary.replace(path)


def evidence(mode, directory):
    directory = directory.resolve()
    current = snapshot(directory)
    pending = directory / "execution-start.json"
    manifest = directory / "execution-evidence.json"
    if mode == "begin":
        # A failed or interrupted rerun must never leave reusable older evidence.
        manifest.unlink(missing_ok=True)
        fixture = os.environ.get("REPORT_FIXTURE_ID", "operator-postgres")
        if not valid_fixture_id(fixture):
            raise ValueError("REPORT_FIXTURE_ID must be a non-secret fixture/image identifier")
        write_json(pending, {"schema_version": 1, "status": "RUNNING", "inputs": current,
                             "source_commit": run("git", "rev-parse", "HEAD"),
                             "fixture_id": fixture, "fixture_profile": FIXTURE_PROFILE,
                             "external_inputs": [key for key in EXTERNAL_INPUTS if os.environ.get(key)]})
        return
    record = json.loads((pending if mode == "seal" else manifest).read_text())
    expected = "RUNNING" if mode == "seal" else "PASS"
    if record.get("schema_version") != 1 or record.get("status") != expected or record.get("inputs") != current:
        raise ValueError("evidence does not match current source, test configuration, environment or toolchain")
    if not valid_fixture_id(record.get("fixture_id")) or record.get("fixture_profile") != FIXTURE_PROFILE:
        raise ValueError("missing or invalid original full-integration fixture identity")
    if not isinstance(record.get("external_inputs"), list):
        raise ValueError("missing external-input scope")
    if mode == "validate" and record["external_inputs"]:
        raise ValueError("external contract/hardware inputs require a fresh execution; reuse is not supported")
    if mode == "validate" and current.get("unverified_symlinks"):
        raise ValueError("unverified linked inputs require a fresh execution; reuse is not supported")
    hashes = validate_artifacts(directory, current["packages"])
    if mode == "seal":
        record.update(status="PASS", artifacts=hashes)
        write_json(manifest, record)
        pending.unlink()
    elif record.get("artifacts") != hashes:
        raise ValueError("execution artifacts changed after completion")


if __name__ == "__main__":
    try:
        if len(sys.argv) != 3 or sys.argv[1] not in ("begin", "seal", "validate"):
            raise ValueError("usage: test-report-evidence.py begin|seal|validate EVIDENCE_DIR")
        evidence(sys.argv[1], Path(sys.argv[2]))
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        # Never forward command output, environment values or malformed raw logs.
        print("test-report evidence rejected: " + (str(error) if isinstance(error, ValueError) else type(error).__name__), file=sys.stderr)
        sys.exit(1)
