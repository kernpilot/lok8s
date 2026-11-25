#!/usr/bin/env python3
"""Tests of dep_scan.py: the proof of the scan, the verdict and the ignore file.

No network and no real scanner. A small script stands in for osv-scanner: it
writes the report that the test asks for and ends with the status of the test.
"""

import contextlib
import datetime
import io
import json
import os
import subprocess
import sys
import tempfile
import time
import unittest
import zipfile
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import dep_scan  # noqa: E402

FAKE = """#!{python}
import json, os, sys
here = os.path.dirname(os.path.abspath(__file__))
plan = json.load(open(os.path.join(here, "plan.json")))
json.dump({{"argv": sys.argv[1:], "env": dict(os.environ), "config": open(
    [a for a in sys.argv if a.startswith("--config=")][0].split("=", 1)[1]).read()}},
    open(os.path.join(here, "seen.json"), "w"))
print(plan["log"])
out = [a for a in sys.argv if a.startswith("--output-file=")][0].split("=", 1)[1]
locks = [a.split("=", 1)[1] for a in sys.argv if a.startswith("--lockfile=")]
results = []
for path in locks:
    key = [k for k in plan["packages"] if path.endswith("/" + k)]
    key = max(key, key=len) if key else None
    if key is not None and key not in plan["drop"]:
        results.append({{"source": {{"path": path, "type": "lockfile"}}, "packages": plan["packages"][key]}})
text = plan["raw"] if plan["raw"] is not None else json.dumps({{"results": results}})
if not plan["no_report"]:
    open(out, "w").write(text)
sys.exit(plan["status"])
"""
CONTROL = {
    "npm": ("control-npm/yarn.lock", "lodash", "4.17.4", "GHSA-jf85-cpcp-j695"),
    "Go": ("control-Go/go.mod", "golang.org/x/text", "0.3.0", "GO-2021-0113"),
}


def package(name, version, ecosystem, vulns=(), groups=()):
    return {
        "package": {"name": name, "version": version, "ecosystem": ecosystem},
        "vulnerabilities": list(vulns),
        "groups": list(groups),
    }


def advisory(ident, name, ecosystem, severity=None, fixed=None, **more):
    events = [{"introduced": "0"}] + ([{"fixed": fixed}] if fixed else [])
    vuln = {
        "id": ident,
        "summary": f"about {ident}",
        "affected": [{"package": {"name": name, "ecosystem": ecosystem}, "ranges": [{"events": events}]}],
    }
    if severity:
        vuln["database_specific"] = {"severity": severity}
    return {**vuln, **more}


class Case(unittest.TestCase):
    """A repo with the given lock files, whole archives, and a scanner that
    finds the control packages and nothing else."""

    locks = {"yarn.lock": "npm"}
    go = None

    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.root = os.path.realpath(self.dir.name)
        self.repo = os.path.join(self.root, "repo")
        os.makedirs(self.repo)
        old = os.getcwd()
        self.addCleanup(os.chdir, old)
        os.chdir(self.repo)
        self.today = datetime.datetime.now(datetime.timezone.utc).date()
        self.since = int(time.time()) - 60
        for patch in (
            mock.patch.object(dep_scan, "MIN_ADVISORIES", 3),
            mock.patch.object(dep_scan, "tracked_files", lambda: [*self.locks, "README.md"]),
        ):
            patch.start()
            self.addCleanup(patch.stop)
        self.plan = {
            "packages": {}, "drop": [], "raw": None, "no_report": False, "status": 1, "log": "a line of the scanner",
        }
        for path, ecosystem in self.locks.items():
            self.plan["packages"][path] = [package("clean", "1.0.0", ecosystem)]
        for ecosystem in set(self.locks.values()):
            self.archive(ecosystem)
            key, name, version, ident = CONTROL[ecosystem]
            self.plan["packages"][key] = [package(name, version, ecosystem, [advisory(ident, name, ecosystem)])]
        if self.go:
            self.plan["packages"]["toolchain/go.mod"] = [package("stdlib", self.go, "Go")]
        self.ignore([])
        self.scanner = os.path.join(self.root, "osv-scanner")
        with open(self.scanner, "w") as f:
            f.write(FAKE.format(python=sys.executable))
        os.chmod(self.scanner, 0o755)

    def archive_path(self, ecosystem):
        return os.path.join(self.root, "db", "osv-scalibr", ecosystem, "all.zip")

    def archive(self, ecosystem, entries=None, day=None):
        path = self.archive_path(ecosystem)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        if entries is None:
            stamp = f"{day or self.today}T01:02:03.456789Z"
            entries = {
                f"A-{n}.json": json.dumps({**advisory(f"A-{n}", f"p{n}", ecosystem), "modified": stamp}) for n in range(3)
            }
        with zipfile.ZipFile(path, "w", zipfile.ZIP_DEFLATED) as z:
            for name, text in entries.items():
                z.writestr(name, text)
        return path

    def ignore(self, entries):
        self.ignore_path = os.path.join(self.root, "ignore.json")
        with open(self.ignore_path, "w") as f:
            json.dump({"ignore": entries}, f)

    def entry(self, ident, name, days=30):
        return {
            "id": ident, "package": name, "reason": "not reached",
            "expires": (self.today + datetime.timedelta(days=days)).isoformat(),
        }

    def finds(self, vulns, name="left", version="1.2.3", lock="yarn.lock", ecosystem="npm", groups=()):
        self.plan["packages"][lock].append(package(name, version, ecosystem, vulns, groups))

    def run_scan(self, *extra):
        with open(os.path.join(self.root, "plan.json"), "w") as f:
            json.dump(self.plan, f)
        out = io.StringIO()
        argv = ["--scanner", self.scanner, "--database", os.path.join(self.root, "db"), "--since", str(self.since)]
        argv += ["--ignore", self.ignore_path] + (["--go", self.go] if self.go else []) + list(extra)
        status = dep_scan.main(argv, out)
        return status, out.getvalue()

    def seen(self):
        with open(os.path.join(self.root, "seen.json")) as f:
            return json.load(f)

    def refuses(self, text, *extra):
        """Status 2, one error with the text, and no verdict."""
        status, out = self.run_scan(*extra)
        self.assertEqual(status, 2, out)
        self.assertIn(text, out)
        self.assertNotIn("verdict:", out)

    def green(self):
        status, out = self.run_scan()
        self.assertEqual(status, 0, out)
        self.assertIn("verdict: green", out)
        return out

    def red(self):
        status, out = self.run_scan()
        self.assertEqual(status, 1, out)
        self.assertIn("verdict: red", out)
        return out


class Clean(Case):
    def test_a_clean_repo_is_green_and_says_what_was_scanned(self):
        out = self.green()
        self.assertIn(f"npm: 3 advisories, the newest of {self.today}", out)
        self.assertIn("yarn.lock: 1 packages", out)
        self.assertIn("1 of 1 lock files scanned", out)
        self.assertIn("osv-scanner: a line of the scanner", out)

    def test_the_scanner_runs_offline_without_call_analysis(self):
        self.green()
        argv = self.seen()["argv"]
        self.assertEqual(argv[:2], ["scan", "source"])
        for flag in ("--offline", "--no-call-analysis=all", "--no-ignore", "--all-packages", "--format=json"):
            self.assertIn(flag, argv)
        self.assertNotIn("--download-offline-databases", argv)

    def test_the_scanner_gets_the_archives_of_this_run(self):
        self.green()
        self.assertEqual(self.seen()["env"]["OSV_SCANNER_LOCAL_DB_CACHE_DIRECTORY"], os.path.join(self.root, "db"))

    def test_a_request_of_the_scanner_goes_nowhere(self):
        self.green()
        env = self.seen()["env"]
        self.assertEqual({env["HTTP_PROXY"], env["HTTPS_PROXY"]}, {"http://127.0.0.1:9"})

    def test_the_scanner_gets_no_other_variable_of_the_job(self):
        with mock.patch.dict(os.environ, {"GITHUB_TOKEN": "x", "PATH": os.environ["PATH"]}):
            self.green()
        env = self.seen()["env"]
        self.assertNotIn("GITHUB_TOKEN", env)
        self.assertNotIn("PATH", env)

    def test_one_config_replaces_the_config_files_of_the_repo(self):
        self.green()
        self.assertEqual(self.seen()["config"], "ScanGoModVersion = true\n")

    def test_a_line_of_the_scanner_cannot_start_a_workflow_command(self):
        self.plan["log"] = "first\n::error::not ours"
        self.assertNotIn("\n::error::", "\n" + self.green())


class Archive(Case):
    def test_no_archive(self):
        os.remove(self.archive_path("npm"))
        self.refuses("npm: no advisory archive")

    def test_an_empty_archive(self):
        with open(self.archive_path("npm"), "w"):
            pass
        self.refuses("npm: the advisory archive is empty")

    def test_an_archive_of_an_older_run(self):
        os.utime(self.archive_path("npm"), (self.since - 1, self.since - 1))
        self.refuses("npm: the advisory archive is older than this run")

    def test_an_archive_of_the_first_second_of_the_run_is_good(self):
        os.utime(self.archive_path("npm"), (self.since, self.since))
        self.green()

    def test_an_archive_that_is_cut_off(self):
        path = self.archive_path("npm")
        with open(path, "rb") as f:
            data = f.read()
        with open(path, "wb") as f:
            f.write(data[:-30])
        self.refuses("npm: the archive does not read")

    def test_an_error_page_in_place_of_the_archive(self):
        with open(self.archive_path("npm"), "w") as f:
            f.write("<Error><Code>NoSuchKey</Code></Error>")
        self.refuses("npm: the archive does not read")

    def test_an_entry_with_damaged_bytes(self):
        path = self.archive_path("npm")
        with zipfile.ZipFile(path) as z:
            info = z.infolist()[1]
            start = info.header_offset + 30 + len(info.filename)
        with open(path, "r+b") as f:
            f.seek(start + 5)
            f.write(b"\x00\xff\x00\xff\x00")
        self.refuses("npm: the archive does not read at A-1.json")

    def test_an_entry_that_is_not_json(self):
        good = json.dumps({**advisory("A-1", "p", "npm"), "modified": f"{self.today}T00:00:00Z"})
        self.archive("npm", {"a.json": good, "b.json": good, "c.json": good, "d.json": "{"})
        self.refuses("npm: the archive does not read at d.json")

    def test_an_entry_without_an_id(self):
        good = {**advisory("A-1", "p", "npm"), "modified": f"{self.today}T00:00:00Z"}
        entries = {f"{n}.json": json.dumps(good) for n in range(3)}
        entries["x.json"] = json.dumps({**good, "id": ""})
        self.archive("npm", entries)
        self.refuses("npm: the archive does not read at x.json")

    def test_an_archive_without_an_entry(self):
        self.archive("npm", {})
        self.refuses("npm: the archive has 0 advisories, so it is not whole")

    def test_an_archive_with_too_few_advisories(self):
        good = json.dumps({**advisory("A-1", "p", "npm"), "modified": f"{self.today}T00:00:00Z"})
        self.archive("npm", {"a.json": good, "b.json": good})
        self.refuses("npm: the archive has 2 advisories, so it is not whole")

    def test_the_archive_of_another_ecosystem(self):
        good = json.dumps({**advisory("A-1", "p", "PyPI"), "modified": f"{self.today}T00:00:00Z"})
        self.archive("npm", {f"{n}.json": good for n in range(9)})
        self.refuses("npm: the archive has 0 advisories, so it is not whole")

    def test_a_file_that_is_no_advisory_is_not_counted(self):
        good = json.dumps({**advisory("A-1", "p", "npm"), "modified": f"{self.today}T00:00:00Z"})
        self.archive("npm", {"a.json": good, "b.json": good, "readme.txt": good})
        self.refuses("npm: the archive has 2 advisories")

    def test_an_export_that_stopped(self):
        self.archive("npm", day=self.today - datetime.timedelta(days=15))
        self.refuses("a stopped export")

    def test_an_export_of_the_last_days_is_good(self):
        self.archive("npm", day=self.today - datetime.timedelta(days=14))
        self.green()

    def test_a_start_in_the_future_is_refused(self):
        self.since = int(time.time()) + 3600
        self.refuses("is not the start of this run")

    def test_a_start_of_zero_is_refused(self):
        self.since = 0
        self.refuses("is not the start of this run")


class TwoEcosystems(Case):
    locks = {"yarn.lock": "npm", "operator/go.mod": "Go"}
    go = "1.26.11"

    def test_both_are_scanned(self):
        out = self.green()
        self.assertIn("Go: 3 advisories", out)
        self.assertIn("npm: 3 advisories", out)
        self.assertIn("2 of 2 lock files scanned", out)

    def test_one_missing_archive_is_refused(self):
        os.remove(self.archive_path("Go"))
        self.refuses("Go: no advisory archive")

    def test_one_archive_that_did_not_load_is_refused_also_when_the_other_has_findings(self):
        # The scanner ends with 1 here, because of the findings of npm.
        self.finds([advisory("GHSA-1", "left", "npm", "HIGH", "2")])
        self.plan["packages"]["control-Go/go.mod"][0]["vulnerabilities"] = []
        self.refuses("Go: GO-2021-0113 of the control package golang.org/x/text was not found")

    def test_one_lock_file_that_was_not_scanned_is_refused(self):
        self.plan["drop"] = ["operator/go.mod"]
        self.refuses("no package of 1 of 5 lock files")

    def test_the_go_of_the_build_is_scanned(self):
        self.finds([advisory("GO-1", "stdlib", "Go", None, "1.26.12")], "stdlib", "1.26.11", "toolchain/go.mod", "Go")
        self.assertIn("::error::UNKNOWN GO-1 stdlib 1.26.11 (fixed in 1.26.12; Go 1.26.11)", self.red())

    def test_the_go_line_of_a_module_is_not_the_go_of_the_build(self):
        self.finds([advisory("GO-1", "stdlib", "Go", None, "1.26.12")], "stdlib", "1.26.0", "operator/go.mod", "Go")
        self.assertNotIn("GO-1", self.green())

    def test_a_module_that_the_code_needs_is_red(self):
        self.finds([advisory("GHSA-1", "example.com/m", "Go", "HIGH", "2")], "example.com/m", "1", "operator/go.mod", "Go")
        self.assertIn("operator/go.mod", self.red())

    def test_a_floating_go_is_refused(self):
        self.go = "1.26"
        self.refuses("--go needs the whole Go version of the build")

    def test_no_go_is_refused(self):
        self.go = None
        self.refuses("--go needs the whole Go version of the build")

    def test_a_go_with_more_text_is_refused(self):
        self.go = "1.26.11\nrequire x v1"
        self.refuses("--go needs the whole Go version of the build")

    def test_the_toolchain_that_was_not_scanned_is_refused(self):
        self.plan["drop"] = ["toolchain/go.mod"]
        self.refuses("the report has no package of 1 of 5 lock files")


class Scanner(Case):
    def test_status_0_is_not_a_clean_scan(self):
        self.plan["status"] = 0
        self.refuses("osv-scanner ended with status 0: no vulnerability found, but the control packages have some")

    def test_no_packages_found(self):
        self.plan["status"] = 128
        self.refuses("osv-scanner ended with status 128: no package found")

    def test_a_general_error(self):
        self.plan["status"] = 127
        self.refuses("osv-scanner ended with status 127: an error of the scanner")

    def test_an_api_error(self):
        self.plan["status"] = 129
        self.refuses("osv-scanner ended with status 129: a request to an API failed")

    def test_a_config_error(self):
        self.plan["status"] = 130
        self.refuses("osv-scanner ended with status 130")

    def test_a_status_that_is_not_known(self):
        self.plan["status"] = 2
        self.refuses("osv-scanner ended with status 2: not known")

    def test_no_scanner(self):
        self.refuses("osv-scanner: ", "--scanner", os.path.join(self.root, "absent"))

    def test_a_scanner_that_hangs(self):
        with open(self.scanner, "w") as f:
            f.write(f"#!{sys.executable}\nimport time\ntime.sleep(20)\n")
        with mock.patch.object(dep_scan, "SCAN_SECONDS", 0.3):
            self.refuses("timed out")


class Report(Case):
    def test_no_report(self):
        self.plan["no_report"] = True
        self.refuses("the report of osv-scanner")

    def test_an_empty_report(self):
        self.plan["raw"] = ""
        self.refuses("the report of osv-scanner")

    def test_a_report_that_is_not_json(self):
        self.plan["raw"] = '{"results": ['
        self.refuses("the report of osv-scanner")

    def test_a_report_without_results(self):
        self.plan["raw"] = "{}"
        self.refuses("the report of osv-scanner: KeyError('results')")

    def test_a_report_with_no_result(self):
        self.plan["raw"] = '{"results": []}'
        self.refuses("the report has no package of 2 of 2 lock files")

    def test_a_result_without_packages(self):
        self.plan["packages"]["yarn.lock"] = []
        self.refuses("the report has no package of 1 of 2 lock files")

    def test_a_package_of_an_ecosystem_without_an_archive(self):
        self.finds([], "remote", "", ecosystem="GIT")
        self.refuses("no archive for the ecosystem of a package")

    def test_the_control_that_was_not_scanned(self):
        self.plan["drop"] = ["control-npm/yarn.lock"]
        self.refuses("the report has no package of 1 of 2 lock files")

    def test_the_control_with_another_advisory(self):
        self.plan["packages"]["control-npm/yarn.lock"][0]["vulnerabilities"][0]["id"] = "GHSA-other"
        self.refuses("npm: GHSA-jf85-cpcp-j695 of the control package lodash was not found")

    def test_the_control_advisory_on_another_package(self):
        self.plan["packages"]["control-npm/yarn.lock"][0]["package"]["name"] = "other"
        self.refuses("of the control package lodash was not found")

    def test_the_control_by_an_alias(self):
        self.plan["packages"]["control-npm/yarn.lock"][0]["vulnerabilities"][0].update(
            id="CVE-2019-10744", aliases=["GHSA-jf85-cpcp-j695"])
        self.green()

    def test_the_findings_of_the_control_are_not_in_the_verdict(self):
        self.plan["packages"]["control-npm/yarn.lock"][0]["vulnerabilities"].append(
            advisory("GHSA-9", "lodash", "npm", "CRITICAL", "5"))
        self.assertNotIn("GHSA-9", self.green())

    def test_a_lock_file_with_no_version_at_all(self):
        self.plan["packages"]["yarn.lock"] = [package("local", "", "npm")]
        self.refuses("the report has no package of 1 of 2 lock files")

    def test_a_report_with_another_shape(self):
        for vulns in ([{"summary": "no id"}], ["GHSA-1"], 7):
            with self.subTest(vulns):
                item = package("left", "1", "npm")
                item["vulnerabilities"] = vulns
                self.plan["packages"]["yarn.lock"] = [item]
                self.refuses("the report of osv-scanner")

    def test_a_package_without_a_version_is_named(self):
        self.finds([], "local", "")
        self.assertIn("::warning::yarn.lock: no version, so no match: local", self.green())


class Locks(unittest.TestCase):
    def test_every_lock_file_is_found_in_every_directory(self):
        self.assertEqual(
            dep_scan.lock_files(["b/go.mod", "bun.lock", "a/b/yarn.lock", "pnpm-lock.yaml", "x/package-lock.json", "go.sum"]),
            {"a/b/yarn.lock": "npm", "b/go.mod": "Go", "bun.lock": "npm", "pnpm-lock.yaml": "npm", "x/package-lock.json": "npm"},
        )

    def test_no_lock_file_is_refused(self):
        with self.assertRaisesRegex(dep_scan.InputError, "no lock file"):
            dep_scan.lock_files(["README.md"])

    def test_a_lock_file_without_a_rule_is_refused(self):
        with self.assertRaisesRegex(dep_scan.InputError, "no rule for a lock file: py/uv.lock"):
            dep_scan.lock_files(["go.mod", "py/uv.lock"])

    def test_the_floor_is_a_thousand_advisories(self):
        self.assertEqual(dep_scan.MIN_ADVISORIES, 1000)

    def test_each_ecosystem_has_a_control(self):
        self.assertEqual(set(dep_scan.CONTROLS), set(dep_scan.LOCK_FILES.values()))

    def test_the_tracked_files_come_from_git(self):
        with tempfile.TemporaryDirectory() as root:
            subprocess.run(["git", "init", "-q", root], check=True)
            for name in ("go.mod", "not-tracked.lock"):
                with open(os.path.join(root, name), "w"):
                    pass
            subprocess.run(["git", "-C", root, "add", "go.mod"], check=True)
            with chdir(root):
                self.assertEqual(dep_scan.tracked_files(), ["go.mod"])

    def test_no_git_repo_is_refused(self):
        with tempfile.TemporaryDirectory() as root:
            with mock.patch.dict(os.environ, {"GIT_CEILING_DIRECTORIES": os.path.dirname(os.path.realpath(root))}):
                with chdir(root), self.assertRaisesRegex(dep_scan.InputError, "git ls-files"):
                    dep_scan.tracked_files()


@contextlib.contextmanager
def chdir(path):
    old = os.getcwd()
    os.chdir(path)
    try:
        yield
    finally:
        os.chdir(old)


class Verdict(Case):
    def test_high_and_critical_with_a_fix_are_red(self):
        for word in ("HIGH", "CRITICAL", "high"):
            with self.subTest(word):
                self.plan["packages"]["yarn.lock"] = []
                self.finds([advisory("GHSA-1", "left", "npm", word, "2.0.0")])
                self.assertIn(f"::error::{word.upper()} GHSA-1 left 1.2.3 (fixed in 2.0.0; yarn.lock)", self.red())

    def test_lower_severities_are_listed(self):
        for word in ("MODERATE", "MEDIUM", "LOW"):
            with self.subTest(word):
                self.plan["packages"]["yarn.lock"] = []
                self.finds([advisory("GHSA-1", "left", "npm", word, "2.0.0")])
                self.assertIn("listed: ", self.green())

    def test_without_a_fix_it_is_listed(self):
        self.finds([advisory("GHSA-1", "left", "npm", "CRITICAL")])
        self.assertIn("listed: CRITICAL GHSA-1 left 1.2.3 (no fixed version;", self.green())

    def test_the_fix_of_another_package_is_not_a_fix(self):
        self.finds([advisory("GHSA-1", "right", "npm", "HIGH", "2.0.0")])
        self.assertIn("no fixed version", self.green())

    def test_the_fix_in_another_ecosystem_is_not_a_fix(self):
        self.finds([advisory("GHSA-1", "left", "PyPI", "HIGH", "2.0.0")])
        self.assertIn("no fixed version", self.green())

    def test_no_severity_counts_as_high(self):
        self.finds([advisory("GHSA-1", "left", "npm", None, "2.0.0")])
        self.assertIn("::error::UNKNOWN GHSA-1", self.red())

    def test_an_alias_gives_the_severity(self):
        self.finds(
            [advisory("GO-1", "left", "npm", None, "2"), advisory("GHSA-1", "left", "npm", "MODERATE", "2")],
            groups=[{"ids": ["GO-1", "GHSA-1"], "max_severity": "9.8"}],
        )
        self.assertIn("listed: MODERATE GO-1", self.green())

    def test_the_highest_alias_gives_the_severity(self):
        self.finds(
            [advisory("GO-1", "left", "npm", None, "2"), advisory("A", "left", "npm", "LOW", "2"),
             advisory("B", "left", "npm", "HIGH")],
            groups=[{"ids": ["GO-1", "A", "B"]}],
        )
        self.assertIn("::error::HIGH GO-1", self.red())

    def test_an_advisory_of_another_group_gives_no_severity(self):
        self.finds(
            [advisory("GO-1", "left", "npm", None, "2"), advisory("GHSA-1", "left", "npm", "LOW", "2")],
            groups=[{"ids": ["GHSA-1"]}, {"ids": ["GO-1"]}],
        )
        self.assertIn("::error::UNKNOWN GO-1", self.red())

    def test_the_score_gives_the_severity(self):
        for score, word in (("9.0", "CRITICAL"), ("8.9", "HIGH"), ("7.0", "HIGH"), ("6.9", "MODERATE"), ("3.9", "LOW")):
            with self.subTest(score):
                self.plan["packages"]["yarn.lock"] = []
                self.finds([advisory("G", "left", "npm", None, "2")], groups=[{"ids": ["G"], "max_severity": score}])
                self.assertIn(f"{word} G left", self.run_scan()[1])

    def test_a_score_that_is_no_number_counts_as_high(self):
        self.finds([advisory("G", "left", "npm", None, "2")], groups=[{"ids": ["G"], "max_severity": "n/a"}])
        self.assertIn("::error::UNKNOWN G", self.red())

    def test_malware_is_red_without_a_fix(self):
        self.finds([advisory("MAL-1", "left", "npm", "LOW")])
        self.assertIn("::error::LOW MAL-1", self.red())

    def test_the_same_finding_in_two_lock_files_is_one_line(self):
        self.plan["packages"]["yarn.lock"] = []
        for _ in range(2):
            self.finds([advisory("GHSA-1", "left", "npm", "HIGH", "2")])
        self.assertEqual(self.red().count("GHSA-1 left"), 1)

    def test_foreign_text_stays_on_one_line(self):
        self.finds([advisory("GHSA-1", "left", "npm", "LOW", "2", summary="x\n::error::y")])
        self.assertNotIn("\n::error::", "\n" + self.green())


class Ignore(Case):
    def test_a_valid_entry_is_green(self):
        self.finds([advisory("GHSA-1", "left", "npm", "HIGH", "2")])
        self.ignore([self.entry("GHSA-1", "left")])
        self.assertIn("ignored: HIGH GHSA-1 left", self.green())

    def test_an_entry_by_an_alias(self):
        self.finds([advisory("GHSA-1", "left", "npm", "HIGH", "2", aliases=["CVE-1"])])
        self.ignore([self.entry("CVE-1", "left")])
        self.green()

    def test_an_entry_of_another_package_does_not_match(self):
        self.finds([advisory("GHSA-1", "left", "npm", "HIGH", "2")])
        self.ignore([self.entry("GHSA-1", "right")])
        self.assertIn("matches no finding", self.red())

    def test_an_entry_of_another_id_does_not_match(self):
        self.finds([advisory("GHSA-1", "left", "npm", "HIGH", "2")])
        self.ignore([self.entry("GHSA-2", "left")])
        self.red()

    def test_the_expiry_day_is_still_valid(self):
        self.finds([advisory("GHSA-1", "left", "npm", "HIGH", "2")])
        self.ignore([self.entry("GHSA-1", "left", 0)])
        self.green()

    def test_an_expired_entry_is_red_also_without_its_finding(self):
        self.ignore([self.entry("GHSA-1", "left", -1)])
        self.assertIn("::error::the ignore entry GHSA-1 (left) expired", self.red())

    def test_an_entry_without_a_finding_is_a_warning(self):
        self.ignore([self.entry("GHSA-1", "left")])
        self.assertIn("::warning::the ignore entry GHSA-1 (left) matches no finding", self.green())

    def test_a_used_entry_gives_no_warning(self):
        self.finds([advisory("GHSA-1", "left", "npm", "HIGH", "2")])
        self.ignore([self.entry("GHSA-1", "left")])
        self.assertNotIn("matches no finding", self.green())

    def test_bad_entries_are_refused(self):
        good = self.entry("GHSA-1", "left")
        for bad in (
            {k: v for k, v in good.items() if k != "reason"}, {**good, "more": "x"}, {**good, "reason": " "},
            {**good, "expires": "soon"}, {**good, "id": 1}, "GHSA-1",
        ):
            with self.subTest(bad):
                self.ignore([bad])
                self.refuses("ignore.json")
        self.ignore([good, good])
        self.refuses("is listed twice")

    def test_a_bad_file_is_refused(self):
        for text in ("", "[]", "{}", '{"ignore": {}}'):
            with self.subTest(text):
                with open(self.ignore_path, "w") as f:
                    f.write(text)
                self.refuses("ignore.json")


if __name__ == "__main__":
    unittest.main()
