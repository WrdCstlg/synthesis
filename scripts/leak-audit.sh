#!/usr/bin/env sh
# Automated repository hygiene, size, binary, commit-identity, network, and denylist audit (POSIX / CI).
#
# Generic checks run unconditionally. Confidential denylist patterns are read
# exclusively from the LEAK_AUDIT_DENYLIST environment variable (CI secret) or
# an untracked local file outside the repository (~/.claude/private/public-demo-denylist.txt).
# On a confidential-list match, only the 1-based rule index ("private-term #N")
# is printed, never the matched term. When unconfigured, the confidential leg
# reports SKIPPED and is never reported as PASS.
set -eu

REPO_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$REPO_ROOT"

python3 - <<'PY'
import os
import re
import subprocess
import sys

public_terms = [
    ("generic:host-user-dir", re.compile(r"[A-Za-z]:[\\/]Users[\\/]|(^|[\s\"'`])/Users/[A-Za-z0-9._-]+", re.IGNORECASE)),
    ("generic:register-id", re.compile(r"\b(D|OQ|BL|D-WS|D-PUB)-\d{2,4}\b", re.IGNORECASE)),
    ("generic:retired-product-name", re.compile(r"(?<!syn)(?<!hypo)the[s]is", re.IGNORECASE)),
]

private_patterns = []
private_source = ""


def add_private_line(raw: str) -> None:
    s = raw.strip()
    if not s or s.startswith("#") or len(s) < 3:
        return
    if s.startswith("regex:"):
        rx = s[6:].strip()
        if len(rx) >= 3:
            private_patterns.append(re.compile(rx, re.IGNORECASE))
    else:
        private_patterns.append(re.compile(re.escape(s), re.IGNORECASE))


env_list = os.environ.get("LEAK_AUDIT_DENYLIST", "")
if env_list.strip():
    private_source = "env:LEAK_AUDIT_DENYLIST"
    for ln in env_list.splitlines():
        add_private_line(ln)
else:
    cand = os.environ.get("LEAK_AUDIT_DENYLIST_FILE", "").strip()
    if not cand:
        cand = os.path.expanduser("~/.claude/private/public-demo-denylist.txt")
    if cand and os.path.isfile(cand):
        private_source = "local-file"
        with open(cand, "r", encoding="utf-8") as df:
            for ln in df:
                add_private_line(ln)

gh_owner_re = re.compile(r"github\.com/([A-Za-z0-9_.-]+)", re.IGNORECASE)
allowed_owners = {"piercepartners", "wrdcstlg"}
max_bytes = 5 * 1024 * 1024
forbidden_exts = {".exe", ".dll", ".so", ".dylib", ".bin", ".bundle", ".tar", ".gz", ".zip", ".7z", ".wasm"}
allowed_img_exts = {".png", ".jpg", ".jpeg", ".gif", ".webp"}
expected_name = "Senan Sumrein"
expected_email = "186500574+WrdCstlg@users.noreply.github.com"

files_out = subprocess.check_output(["git", "ls-files", "--cached", "--others", "--exclude-standard"], text=True)
rel_files = [f.strip() for f in files_out.splitlines() if f.strip()]

failures = []

for rel in rel_files:
    norm = rel.replace("\\", "/")
    if not os.path.isfile(rel):
        continue

    for lbl, rx in public_terms:
        if rx.search(norm):
            failures.append(f"filename hit [{lbl}]: {norm}")

    for idx, rx in enumerate(private_patterns, start=1):
        if rx.search(norm):
            failures.append(f"filename hit [private-term #{idx}]: {norm}")

    sz = os.path.getsize(rel)
    if sz > max_bytes:
        failures.append(f"size violation (>5MB): {norm} ({sz} bytes)")

    _, ext = os.path.splitext(norm)
    ext = ext.lower()
    if ext in forbidden_exts:
        failures.append(f"binary extension forbidden: {norm}")

    with open(rel, "rb") as fh:
        raw = fh.read()

    if b"\x00" in raw[:8192]:
        if not (norm.startswith("docs/") and ext in allowed_img_exts):
            failures.append(f"binary file outside docs/ images: {norm}")
        continue

    text = raw.decode("utf-8", errors="replace")
    for lbl, rx in public_terms:
        if rx.search(text):
            failures.append(f"content hit [{lbl}] in {norm}")

    for m in gh_owner_re.finditer(text):
        owner = m.group(1).lower()
        if owner not in allowed_owners:
            failures.append(f"disallowed GitHub owner in {norm}: github.com/{m.group(1)}")

    for idx, rx in enumerate(private_patterns, start=1):
        if rx.search(text):
            failures.append(f"content hit [private-term #{idx}] in {norm}")

    if norm.endswith(".go") and not norm.endswith("_test.go"):
        for um in re.finditer(r"https?://[^\s\"'`]+", text, re.IGNORECASE):
            u = um.group(0)
            if not re.match(r"^http://(127\.0\.0\.1|localhost|\{host\}|n[123]-peer|kv-n[123]|%s|www\.w3\.org/2000/svg)", u, re.IGNORECASE):
                failures.append(f"external URL in runtime Go source {norm}: {u}")

has_head = subprocess.call(["git", "rev-parse", "--verify", "HEAD"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL) == 0
if has_head:
    commits = subprocess.check_output(["git", "log", "--format=%H|%an|%ae|%cn|%ce"], text=True).splitlines()
    for line in commits:
        if not line.strip():
            continue
        sha, an, ae, cn, ce = line.split("|", 4)
        if an != expected_name or ae != expected_email or cn != expected_name or ce != expected_email:
            failures.append(f"commit {sha} does not match required author/committer identity ({expected_name} <{expected_email}>)")

    msgs = subprocess.check_output(["git", "log", "--format=%H%n%B%n---END---"], text=True)
    has_legacy_root = subprocess.call(
        ["git", "cat-file", "-e", "d03c24733659bb6bd0e4c8fa4ad507dab4e4825d"],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    ) == 0
    if has_legacy_root:
        post_root_msgs = subprocess.check_output(
            ["git", "log", "d03c24733659bb6bd0e4c8fa4ad507dab4e4825d..HEAD", "--format=%H%n%B%n---END---"],
            text=True,
        )
    else:
        post_root_msgs = msgs
    if re.search(r"[Cc]o-[Aa]uthored-[Bb]y:|[Ss]igned-[Oo]ff-[Bb]y:", msgs):
        failures.append("commit history contains attribution or sign-off trailer")
    for lbl, rx in public_terms:
        target_msgs = post_root_msgs if lbl == "generic:retired-product-name" else msgs
        if target_msgs.strip() and rx.search(target_msgs):
            failures.append(f"commit message hit [{lbl}]")
    for idx, rx in enumerate(private_patterns, start=1):
        if rx.search(msgs):
            failures.append(f"commit message hit [private-term #{idx}]")

if failures:
    print(f"LEAK AUDIT FAILED ({len(failures)} finding(s)):", file=sys.stderr)
    for item in failures:
        print(f"  - {item}", file=sys.stderr)
    sys.exit(1)

if private_patterns:
    print(f"leak-audit: public checks PASS ({len(rel_files)} file(s)); private denylist PASS ({len(private_patterns)} rule(s) from {private_source} -> 0 hits)")
else:
    if os.environ.get("LEAK_AUDIT_REQUIRE_PRIVATE", "") == "1":
        print("LEAK AUDIT FAILED: LEAK_AUDIT_REQUIRE_PRIVATE=1 and no private denylist is configured", file=sys.stderr)
        sys.exit(1)
    print(f"leak-audit: public checks PASS ({len(rel_files)} file(s))")
    print("leak-audit: private denylist SKIPPED (neither LEAK_AUDIT_DENYLIST secret nor ~/.claude/private/public-demo-denylist.txt is configured)")
PY
