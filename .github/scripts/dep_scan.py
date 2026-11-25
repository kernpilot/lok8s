#!/usr/bin/env python3
"""The daily dependency scan: osv-scanner, offline, and a verdict that fails closed.

  dep_scan.py --scanner FILE --database DIR --since SECONDS --ignore FILE [--go VERSION]

Run it in the root of the repo. The workflow downloads one whole advisory
archive per ecosystem first (DIR/osv-scalibr/ECOSYSTEM/all.zip). osv-scanner
then matches every lock file that git tracks against these archives on this
machine. It sends no package name and no version.

Status 1 (red):
  - an advisory is HIGH or CRITICAL, has a fixed version, and no valid ignore
    entry names it. An advisory without a severity counts as HIGH.
  - an advisory says that the package is malware (a MAL id), fix or not, or
  - an ignore entry is past its expiry date (also when its finding is gone).
Every other advisory is listed and is not red.

Status 2 (the scan is not proven, so it is not a clean scan):
  - the repo has no lock file, or one without a rule here,
  - an archive is missing, empty, older than this run, cut off, has an entry
    that does not read, has too few advisories or no new one,
  - osv-scanner ends with another status than "vulnerabilities found",
  - the report is missing, empty, not JSON, or has no package with a version of
    a lock file,
  - a package has an ecosystem without an archive, or
  - a control package with a known advisory has no finding.

The checks of the archives and the controls are not a luxury. osv-scanner 2.7.1
skips an archive entry that it cannot parse, without a word. It also ends with
"vulnerabilities found" when the archive of another ecosystem did not load.

Status 0 (green): everything else.
"""

import argparse
import datetime
import json
import os
import re
import subprocess
import sys
import tempfile
import time
import zipfile
import zlib

# The lock files that the scan reads, and the ecosystem of each.
LOCK_FILES = {
    "bun.lock": "npm",
    "go.mod": "Go",
    "package-lock.json": "npm",
    "pnpm-lock.yaml": "npm",
    "yarn.lock": "npm",
}
# Lock files that need a rule above and an archive in the workflow first.
NO_RULE = {
    "Cargo.lock", "Gemfile.lock", "Pipfile.lock", "bun.lockb", "composer.lock", "gradle.lockfile", "mix.lock",
    "npm-shrinkwrap.json", "packages.lock.json", "pdm.lock", "poetry.lock", "pubspec.lock", "pylock.toml", "uv.lock",
}
# One package per ecosystem with an advisory that stays: file name, content,
# package, advisory.
CONTROLS = {
    "Go": (
        "go.mod",
        "module example.com/control\n\ngo 1.21\n\nrequire golang.org/x/text v0.3.0\n",
        "golang.org/x/text",
        "GO-2021-0113",
    ),
    "npm": ("yarn.lock", '# yarn lockfile v1\n\n\nlodash@4.17.4:\n  version "4.17.4"\n', "lodash", "GHSA-jf85-cpcp-j695"),
}
# Each archive has thousands of advisories and changes each day or two.
MIN_ADVISORIES = 1000
STALE_DAYS = 14
SCAN_SECONDS = 400
STATUS = {
    0: "no vulnerability found, but the control packages have some: nothing was matched",
    127: "an error of the scanner",
    128: "no package found",
    129: "a request to an API failed",
    130: "a configuration file is not valid",
}


class InputError(Exception):
    """Something that the verdict cannot rest on."""


def one_line(text):
    """Foreign text stays on one line, so that it cannot start a workflow
    command in the log."""
    return " ".join(str(text).split())


def load_ignore(path, today):
    """Return (valid, expired) entries of the ignore file."""
    try:
        with open(path, encoding="utf-8") as f:
            entries = json.load(f)["ignore"]
    except (OSError, ValueError, KeyError, TypeError) as err:
        raise InputError(f"{path}: {err!r}") from err
    if not isinstance(entries, list):
        raise InputError(f"{path}: 'ignore' is not a list")
    valid, expired, seen = [], [], set()
    for entry in entries:
        if not isinstance(entry, dict) or set(entry) != {"id", "package", "reason", "expires"}:
            raise InputError(f"{path}: an entry needs exactly id, package, reason and expires: {entry}")
        if not all(isinstance(v, str) and v.strip() for v in entry.values()):
            raise InputError(f"{path}: an empty or non-text value: {entry}")
        try:
            expires = datetime.date.fromisoformat(entry["expires"])
        except ValueError as err:
            raise InputError(f"{path}: {entry['id']}: {err}") from err
        key = (entry["id"], entry["package"])
        if key in seen:
            raise InputError(f"{path}: {entry['id']} for {entry['package']} is listed twice")
        seen.add(key)
        (expired if expires < today else valid).append(entry)
    return valid, expired


def find_ignore(valid, ids, package):
    """The entry that names one of the ids of a finding and its package."""
    return next((e for e in valid if e["package"] == package and e["id"] in ids), None)


def tracked_files():
    """Every file that git tracks here."""
    try:
        done = subprocess.run(["git", "ls-files", "-z"], check=True, capture_output=True)
    except (OSError, subprocess.CalledProcessError) as err:
        raise InputError(f"git ls-files: {err!r}") from err
    return [os.fsdecode(name) for name in done.stdout.split(b"\0") if name]


def lock_files(tracked):
    """Return {path: ecosystem} of every lock file in the list."""
    unknown = sorted(p for p in tracked if os.path.basename(p) in NO_RULE)
    if unknown:
        raise InputError(f"no rule for a lock file: {', '.join(unknown)}")
    found = {p: LOCK_FILES[os.path.basename(p)] for p in sorted(tracked) if os.path.basename(p) in LOCK_FILES}
    if not found:
        raise InputError("the repo has no lock file: nothing to scan")
    return found


def check_archive(path, ecosystem, since, today):
    """Return (number of advisories, day of the newest one) of one archive.
    Raise when it can be something else than the whole archive of this run."""
    try:
        info = os.stat(path)
    except OSError as err:
        raise InputError(f"{ecosystem}: no advisory archive: {err}") from err
    if info.st_size == 0:
        raise InputError(f"{ecosystem}: the advisory archive is empty")
    if info.st_mtime < since:
        raise InputError(f"{ecosystem}: the advisory archive is older than this run")
    count, newest, name = 0, "", "the list of entries"
    try:
        with zipfile.ZipFile(path) as archive:
            for name in archive.namelist():
                if not name.endswith(".json"):
                    continue
                advisory = json.loads(archive.read(name))
                if not advisory["id"] or not advisory["modified"]:
                    raise ValueError("no id or no date")
                affected = advisory.get("affected") or []
                if any((a.get("package") or {}).get("ecosystem") == ecosystem for a in affected):
                    count += 1
                    newest = max(newest, advisory["modified"])
        if count < MIN_ADVISORIES:
            raise InputError(f"{ecosystem}: the archive has {count} advisories, so it is not whole")
        day = datetime.date.fromisoformat(newest[:10])
    except (
        OSError, ValueError, KeyError, TypeError, AttributeError, EOFError, RuntimeError,
        NotImplementedError, zipfile.BadZipFile, zlib.error,
    ) as err:
        raise InputError(f"{ecosystem}: the archive does not read at {name}: {err!r}") from err
    if (today - day).days > STALE_DAYS:
        raise InputError(f"{ecosystem}: the newest advisory of the archive is of {day}: a stopped export")
    return count, day


def write_file(path, text):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as f:
        f.write(text)
    return path


def run_scanner(scanner, database, files, work, out):
    """Run osv-scanner on the files and return the path of its report."""
    report = os.path.join(work, "report.json")
    # One config for every lock file: an osv-scanner.toml of the repo, which can
    # ignore an advisory without a date, has no effect. The key keeps the standard
    # library in the package list, so every go.mod has a package to show.
    config = write_file(os.path.join(work, "osv-scanner.toml"), "ScanGoModVersion = true\n")
    command = [
        os.path.abspath(scanner), "scan", "source",
        # No request to an API, and no request of a plugin.
        "--offline",
        # This one is on by default for Go, also offline: it runs the Go toolchain,
        # and that loads modules from a proxy.
        "--no-call-analysis=all",
        "--no-ignore", "--all-packages", "--all-vulns", "--format=json",
        f"--config={config}", f"--output-file={report}",
    ] + [f"--lockfile={path}" for path in files]
    env = {
        "HOME": work,
        "OSV_SCANNER_LOCAL_DB_CACHE_DIRECTORY": os.path.abspath(database),
        # No service listens there: a request that should not exist fails.
        "HTTP_PROXY": "http://127.0.0.1:9", "HTTPS_PROXY": "http://127.0.0.1:9",
    }
    try:
        done = subprocess.run(
            command, env=env, cwd=work, timeout=SCAN_SECONDS, stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
        )
    except (OSError, subprocess.TimeoutExpired) as err:
        raise InputError(f"osv-scanner: {one_line(err)}") from err
    for line in done.stdout.decode("utf-8", "replace").splitlines():
        print(f"osv-scanner: {one_line(line)}", file=out)
    # The control packages have advisories, so 1 is the one good status.
    if done.returncode != 1:
        raise InputError(f"osv-scanner ended with status {done.returncode}: {STATUS.get(done.returncode, 'not known')}")
    return report


def read_report(path, files, ecosystems):
    """Return {file: packages} of the report. Every file must be in it."""
    sources = {}
    try:
        with open(path, encoding="utf-8") as f:
            report = json.load(f)
        for result in report["results"]:
            packages = sources.setdefault(os.path.normpath(result["source"]["path"]), [])
            packages.extend(result["packages"])
        for packages in sources.values():
            for item in packages:
                if item["package"]["ecosystem"] not in ecosystems:
                    raise InputError(f"no archive for the ecosystem of a package: {one_line(item['package'])}")
    except (OSError, ValueError, KeyError, TypeError, AttributeError) as err:
        raise InputError(f"the report of osv-scanner: {err!r}") from err
    # A package without a version cannot match, so it proves nothing.
    missing = [p for p in files if not any(i["package"].get("version") for i in sources.get(p, []))]
    if missing:
        raise InputError(
            f"the report has no package of {len(missing)} of {len(files)} lock files:"
            f" {', '.join(missing)}; it has {', '.join(sorted(sources)) or 'none'}"
        )
    return sources


def check_control(packages, ecosystem, package, advisory):
    """Raise when the known advisory of the control package is not found."""
    for item in packages:
        if item["package"]["name"] == package:
            for vuln in item.get("vulnerabilities") or []:
                if advisory in {vuln["id"], *(vuln.get("aliases") or [])}:
                    return
    raise InputError(f"{ecosystem}: {advisory} of the control package {package} was not found: the archive was not used")


RANK = {"LOW": 1, "MODERATE": 2, "MEDIUM": 2, "HIGH": 3, "CRITICAL": 4}
NAMES = {1: "LOW", 2: "MODERATE", 3: "HIGH", 4: "CRITICAL"}


def severity(vuln, vulns, groups):
    """The severity that the advisory states, else the highest one that an alias
    of it states, else the one of the CVSS score."""
    def stated(v):
        return RANK.get(str((v.get("database_specific") or {}).get("severity", "")).upper(), 0)

    if stated(vuln):
        return NAMES[stated(vuln)]
    for group in groups:
        if vuln["id"] in group.get("ids", []):
            highest = max([stated(v) for v in vulns if v["id"] in group["ids"]], default=0)
            if highest:
                return NAMES[highest]
            try:
                score = float(group.get("max_severity") or 0)
            except ValueError:
                return "UNKNOWN"
            for floor, rank in ((9.0, 4), (7.0, 3), (4.0, 2), (0.1, 1)):
                if score >= floor:
                    return NAMES[rank]
    return "UNKNOWN"


def fixed_versions(vuln, package):
    """Every fixed version that the advisory names for the package."""
    found = []
    for affected in vuln.get("affected") or []:
        named = affected.get("package") or {}
        if named.get("name") != package["name"] or named.get("ecosystem") != package["ecosystem"]:
            continue
        for span in affected.get("ranges") or []:
            found += [event["fixed"] for event in span.get("events") or [] if event.get("fixed")]
    return found


def findings(sources):
    """The findings of {name: packages}, one per advisory, package and version."""
    found = {}
    for name in sorted(sources):
        for item in sources[name]:
            package, vulns = item["package"], item.get("vulnerabilities") or []
            for vuln in vulns:
                key = (package["name"], package["version"], vuln["id"])
                entry = found.setdefault(key, {
                    "ids": {vuln["id"], *(vuln.get("aliases") or [])},
                    "id": vuln["id"],
                    "package": package["name"],
                    "version": package["version"],
                    "severity": severity(vuln, vulns, item.get("groups") or []),
                    "fixed": fixed_versions(vuln, package),
                    "summary": vuln.get("summary", ""),
                    "in": [],
                })
                entry["in"].append(name)
    return [found[key] for key in sorted(found)]


def counts(finding):
    """True for a finding that makes the run red."""
    if finding["id"].startswith("MAL-"):
        return True
    return finding["severity"] in ("HIGH", "CRITICAL", "UNKNOWN") and bool(finding["fixed"])


def verdict(valid, expired, found, out):
    """Write the verdict to `out` and return the exit status."""
    red, used = False, []
    for item in found:
        fix = f"fixed in {', '.join(item['fixed'])}" if item["fixed"] else "no fixed version"
        line = one_line(
            f"{item['severity']} {item['id']} {item['package']} {item['version']} ({fix};"
            f" {', '.join(item['in'])}): {item['summary']}"
        )
        entry = find_ignore(valid, item["ids"], item["package"])
        if not counts(item):
            print(f"listed: {line}", file=out)
        elif entry:
            used.append(entry)
            print(f"ignored: {line}; until {entry['expires']}: {one_line(entry['reason'])}", file=out)
        else:
            red = True
            print(f"::error::{line}", file=out)
    for entry in expired:
        red = True
        print(f"::error::the ignore entry {entry['id']} ({entry['package']}) expired on {entry['expires']}", file=out)
    for entry in valid:
        if entry not in used:
            print(f"::warning::the ignore entry {entry['id']} ({entry['package']}) matches no finding: remove it", file=out)
    print("verdict: red" if red else "verdict: green", file=out)
    return 1 if red else 0


def scan(args, work, out):
    """The whole scan. Return the exit status."""
    today = datetime.datetime.now(datetime.timezone.utc).date()
    valid, expired = load_ignore(args.ignore, today)
    locks = lock_files(tracked_files())
    ecosystems = sorted(set(locks.values()))
    for ecosystem in ecosystems:
        path = os.path.join(args.database, "osv-scalibr", ecosystem, "all.zip")
        count, day = check_archive(path, ecosystem, args.since, today)
        print(f"{ecosystem}: {count} advisories, the newest of {day}", file=out)
    root = os.path.realpath(os.getcwd())
    files = {os.path.normpath(os.path.join(root, path)): path for path in locks}
    if "Go" in ecosystems:
        # The standard library of the build. The go line of a go.mod is only the
        # lowest Go that the module accepts.
        if not re.fullmatch(r"1\.\d+\.\d+", args.go or ""):
            raise InputError(f"--go needs the whole Go version of the build, like 1.26.11, got {args.go!r}")
        path = os.path.join(work, "toolchain", "go.mod")
        files[write_file(path, f"module example.com/toolchain\n\ngo {args.go}\n")] = f"Go {args.go}"
    controls = {}
    for ecosystem in ecosystems:
        name, text, package, advisory = CONTROLS[ecosystem]
        path = write_file(os.path.join(work, f"control-{ecosystem}", name), text)
        controls[path] = (ecosystem, package, advisory)
    wanted = [*files, *controls]
    sources = read_report(run_scanner(args.scanner, args.database, wanted, work, out), wanted, ecosystems)
    try:
        for path, control in controls.items():
            check_control(sources[path], *control)
        found = findings(own_packages(files, locks, sources, out))
    except (KeyError, TypeError, AttributeError) as err:
        raise InputError(f"the report of osv-scanner: {err!r}") from err
    print(f"{len(locks)} of {len(locks)} lock files scanned", file=out)
    return verdict(valid, expired, found, out)


def own_packages(files, locks, sources, out):
    """Return {name: packages} without the controls. List what was read."""
    named = {}
    for path, name in files.items():
        packages = sources[path]
        blind = sorted(i["package"]["name"] for i in packages if not i["package"].get("version"))
        if blind:
            print(f"::warning::{one_line(name)}: no version, so no match: {one_line(', '.join(blind))}", file=out)
        print(f"{name}: {len(packages)} packages", file=out)
        named[name] = packages
        if locks.get(name) == "Go":
            named[name] = [i for i in packages if i["package"]["name"] != "stdlib"]
    return named


def main(argv, out=sys.stdout):
    parser = argparse.ArgumentParser(description="The daily dependency scan.")
    parser.add_argument("--scanner", required=True)
    parser.add_argument("--database", required=True)
    parser.add_argument("--since", required=True, type=int, help="the start of this run, in seconds")
    parser.add_argument("--ignore", required=True)
    parser.add_argument("--go", help="the Go version of the build")
    args = parser.parse_args(argv)
    try:
        if not 0 < args.since <= time.time():
            raise InputError(f"--since {args.since} is not the start of this run")
        with tempfile.TemporaryDirectory() as work:
            return scan(args, os.path.realpath(work), out)
    except InputError as err:
        print(f"::error::{one_line(err)}", file=out)
        return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
