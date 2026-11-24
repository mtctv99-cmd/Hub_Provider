#!/usr/bin/env python3
"""Safe online backup for the Sever Provider SQLite database.

Safety rules enforced here:

* The live ``.db`` file is never copied with ``cp``/``shutil.copy``. The copy is
  produced by SQLite itself, either through the ``sqlite3`` CLI ``.backup``
  command or through ``VACUUM INTO``. Both take a consistent read snapshot and
  include committed WAL content.
* The backup is written to a hidden temporary file and atomically renamed only
  after ``PRAGMA integrity_check`` returns ``ok``.
* Row counts for ``api_keys``, ``credentials``, ``credential_secrets``,
  ``identities`` and ``jobs`` are captured before and after the snapshot and
  must bracket the backup's counts (concurrent writes are tolerated, silent
  data loss is not).
* Retention is bounded to 7..14 daily copies and only files matching this
  script's own naming pattern are ever deleted.
* Nothing is copied off-host. Off-host shipping is an owner decision; see
  docs/operations-runbook.md.

Exit codes: 0 success/skip, 1 verification or runtime failure, 2 usage error.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import sqlite3
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

try:  # Linux only; the script still works without a lock on other platforms.
    import fcntl
except ImportError:  # pragma: no cover - Windows development hosts
    fcntl = None  # type: ignore[assignment]

DEFAULT_DB = "/media/reup/Data_sv4/Sever_provider/data/orchestrator.db"
DEFAULT_BACKUP_DIR = "~/backups/sever-provider"
REQUIRED_TABLES = ("api_keys", "credentials", "credential_secrets", "identities", "jobs")
BACKUP_NAME_RE = re.compile(r"^orchestrator-\d{8}T\d{6}Z\.db$")
NETWORK_FS = {"nfs", "nfs4", "cifs", "smb3", "smbfs", "fuse.sshfs", "9p"}
MIN_RETENTION = 7
MAX_RETENTION = 14


def now_utc() -> datetime:
    return datetime.now(timezone.utc)


def log(message: str) -> None:
    print(f"{now_utc().strftime('%Y-%m-%dT%H:%M:%SZ')} {message}", flush=True)


def utc_stamp() -> str:
    return now_utc().strftime("%Y%m%dT%H%M%SZ")


def fail(message: str) -> int:
    print(f"ERROR: {message}", file=sys.stderr, flush=True)
    return 1


def open_ro(path: Path) -> sqlite3.Connection:
    uri = path.resolve().as_uri() + "?mode=ro"
    conn = sqlite3.connect(uri, uri=True, timeout=30)
    conn.execute("PRAGMA query_only=1")
    conn.execute("PRAGMA busy_timeout=30000")
    return conn


def count_rows(conn: sqlite3.Connection, table: str) -> int:
    # Table names are from the fixed REQUIRED_TABLES tuple, not user input.
    return int(conn.execute(f"SELECT COUNT(*) FROM {table}").fetchone()[0])


def counts(conn: sqlite3.Connection) -> dict[str, int]:
    return {table: count_rows(conn, table) for table in REQUIRED_TABLES}


def fstype_for(path: Path) -> str:
    """"Best-effort mount fstype lookup on Linux; empty string when unknown."""
    try:
        mounts = Path("/proc/mounts").read_text(encoding="utf-8", errors="replace")
    except OSError:
        return ""
    target = str(path.resolve())
    best_mount = ""
    best_type = ""
    for line in mounts.splitlines():
        parts = line.split()
        if len(parts) < 3:
            continue
        mount = parts[1].replace("\\040", " ").replace("\\011", "\t").replace("\\134", "\\")
        if target == mount or target.startswith(mount.rstrip("/") + "/"):
            if len(mount) >= len(best_mount):
                best_mount = mount
                best_type = parts[2]
    return best_type


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def acquire_lock(directory: Path):
    lock_path = directory / ".backup.lock"
    handle = lock_path.open("w", encoding="utf-8")
    try:
        os.chmod(lock_path, 0o600)
    except OSError:
        pass
    if fcntl is not None:
        try:
            fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            handle.close()
            return None
    return handle


def backup_with_cli(sqlite3_bin: str, source: Path, destination: Path) -> None:
    if "'" in str(destination):
        raise ValueError("backup path must not contain a single quote")
    command = [sqlite3_bin, str(source), f".backup '{destination}'"]
    result = subprocess.run(command, capture_output=True, text=True, timeout=1800)
    if result.returncode != 0:
        raise RuntimeError(
            f"sqlite3 .backup failed rc={result.returncode}: {result.stderr.strip()[:400]}"
        )


def backup_with_vacuum_into(source: Path, destination: Path) -> None:
    # SQLite >= 3.27 requires a read-write handle for VACUUM INTO even though
    # the command only writes the destination file (a read-only/query_only
    # handle fails with "attempt to write a readonly database"). No statement
    # here modifies the source database; the snapshot is transactional.
    conn = sqlite3.connect(str(source), timeout=30)
    try:
        conn.execute("PRAGMA busy_timeout=30000")
        conn.execute("VACUUM INTO ?", (str(destination),))
    finally:
        conn.close()


def create_backup(source: Path, destination: Path, engine: str) -> str:
    sqlite3_bin = shutil.which("sqlite3")
    if engine == "cli" or (engine == "auto" and sqlite3_bin):
        if not sqlite3_bin:
            raise RuntimeError("--engine cli requested but sqlite3 CLI is not installed")
        backup_with_cli(sqlite3_bin, source, destination)
        return f"sqlite3 .backup ({sqlite3_bin})"
    backup_with_vacuum_into(source, destination)
    return "VACUUM INTO via python sqlite3"


def verify_backup(path: Path) -> tuple[bool, str, dict[str, int]]:
    conn = open_ro(path)
    try:
        rows = conn.execute("PRAGMA integrity_check").fetchall()
        integrity = ";".join(str(row[0]) for row in rows)
        table_counts = counts(conn)
    finally:
        conn.close()
    return integrity == "ok", integrity, table_counts


def prune(directory: Path, retention: int, keep: Path) -> list[Path]:
    candidates = sorted(
        (
            item
            for item in directory.iterdir()
            if item.is_file() and BACKUP_NAME_RE.match(item.name) and item != keep
        ),
        key=lambda item: item.name,
    )
    excess = max(0, len(candidates) + 1 - retention)
    removed: list[Path] = []
    for item in candidates[:excess]:
        removed.append(item)
        for suffix in ("", ".sha256", ".manifest.json"):
            companion = Path(str(item) + suffix)
            if companion.exists():
                companion.unlink()
    return removed


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--db", default=DEFAULT_DB, help="source SQLite database")
    parser.add_argument(
        "--backup-dir",
        default=DEFAULT_BACKUP_DIR,
        help="destination directory (default: ~/backups/sever-provider)",
    )
    parser.add_argument(
        "--retention",
        type=int,
        default=MAX_RETENTION,
        help=f"daily copies to keep ({MIN_RETENTION}..{MAX_RETENTION}, default 14)",
    )
    parser.add_argument(
        "--engine",
        choices=("auto", "cli", "vacuum"),
        default="auto",
        help="auto uses sqlite3 .backup when installed, else VACUUM INTO",
    )
    parser.add_argument("--dry-run", action="store_true", help="print the plan and exit")
    parser.add_argument("--allow-network-fs", action="store_true", help="allow NFS/CIFS backup dir")
    parser.add_argument("--verify-only", metavar="BACKUP", help="verify an existing backup file")
    return parser.parse_args(argv)


def cmd_verify_only(path: Path) -> int:
    if not path.is_file():
        return fail(f"backup not found: {path}")
    ok, integrity, table_counts = verify_backup(path)
    log(f"verify path={path} integrity={integrity} counts={json.dumps(table_counts, sort_keys=True)}")
    manifest_path = Path(str(path) + ".manifest.json")
    if manifest_path.is_file():
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        expected = manifest.get("counts", {})
        mismatches = {
            table: (expected.get(table), table_counts.get(table))
            for table in REQUIRED_TABLES
            if expected.get(table) != table_counts.get(table)
        }
        if mismatches:
            return fail(f"manifest row-count mismatch: {mismatches}")
        log("manifest row counts match")
    if not ok:
        return fail(f"integrity_check failed: {integrity}")
    log("backup verification OK")
    return 0


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    source = Path(os.path.expanduser(args.db))
    backup_dir = Path(os.path.expanduser(args.backup_dir))

    if args.verify_only:
        return cmd_verify_only(Path(os.path.expanduser(args.verify_only)))

    if not (MIN_RETENTION <= args.retention <= MAX_RETENTION):
        return fail(f"--retention must be between {MIN_RETENTION} and {MAX_RETENTION}")

    if not source.is_file():
        return fail(f"source database not found: {source}")

    fstype = fstype_for(backup_dir if backup_dir.exists() else backup_dir.parent)
    if fstype in NETWORK_FS and not args.allow_network_fs:
        return fail(
            f"backup directory is on {fstype}; SQLite WAL/backup on network FS is unsafe. "
            "Use local disk or pass --allow-network-fs explicitly."
        )

    source_size = source.stat().st_size
    stamp = utc_stamp()
    destination = backup_dir / f"orchestrator-{stamp}.db"

    if args.dry_run:
        log(f"dry-run source={source} size={source_size} destination={destination} "
            f"retention={args.retention} engine={args.engine} fstype={fstype or 'unknown'}")
        return 0

    backup_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(backup_dir, 0o700)

    if destination.exists():
        return fail(f"destination already exists: {destination} (refusing to overwrite)")

    lock = acquire_lock(backup_dir)
    if lock is None:
        log("another backup is already running; skipping this invocation")
        return 0

    free = shutil.disk_usage(backup_dir).free
    if free < source_size + 128 * 1024 * 1024:
        lock.close()
        return fail(
            f"insufficient free space: need >{source_size + 128 * 1024 * 1024} bytes, have {free}"
        )

    # Remove temp files left by a killed run older than one day.
    for stale in backup_dir.glob(".tmp-orchestrator-*.db"):
        try:
            if time.time() - stale.stat().st_mtime > 86400:
                stale.unlink()
        except OSError:
            pass

    started = time.time()
    temp_path = backup_dir / f".tmp-orchestrator-{stamp}-{os.getpid()}.db"
    try:
        source_conn = open_ro(source)
        try:
            before = counts(source_conn)
        finally:
            source_conn.close()

        log(f"snapshot start source={source} destination={destination}")
        engine_used = create_backup(source, temp_path, args.engine)
        os.chmod(temp_path, 0o600)
        ok, integrity, backup_counts = verify_backup(temp_path)

        source_conn = open_ro(source)
        try:
            after = counts(source_conn)
        finally:
            source_conn.close()

        bracketed: dict[str, int] = {}
        for table in REQUIRED_TABLES:
            low, high = sorted((before[table], after[table]))
            if not (low <= backup_counts[table] <= high):
                raise RuntimeError(
                    f"row-count check failed for {table}: backup={backup_counts[table]} "
                    f"source_before={before[table]} source_after={after[table]}"
                )
            bracketed[table] = backup_counts[table]

        if not ok:
            raise RuntimeError(f"integrity_check failed: {integrity}")

        os.replace(temp_path, destination)
    except Exception as exc:  # noqa: BLE001 - report and clean temp file
        try:
            temp_path.unlink()
        except OSError:
            pass
        lock.close()
        return fail(str(exc))

    digest = sha256_file(destination)
    duration = round(time.time() - started, 2)
    (Path(str(destination) + ".sha256")).write_text(
        f"{digest}  {destination.name}\n", encoding="utf-8"
    )
    os.chmod(Path(str(destination) + ".sha256"), 0o600)

    manifest = {
        "created_at_utc": now_utc().strftime("%Y-%m-%dT%H:%M:%SZ"),
        "source": str(source),
        "source_size": source_size,
        "source_mtime_utc": datetime.fromtimestamp(
            source.stat().st_mtime, tz=timezone.utc
        ).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "backup": str(destination),
        "backup_size": destination.stat().st_size,
        "sha256": digest,
        "engine": engine_used,
        "duration_seconds": duration,
        "integrity_check": integrity,
        "counts": bracketed,
        "source_counts_before": before,
        "source_counts_after": after,
        "retention": args.retention,
    }
    manifest_path = Path(str(destination) + ".manifest.json")
    manifest_path.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    os.chmod(manifest_path, 0o600)

    removed = prune(backup_dir, args.retention, destination)
    lock.close()

    log(
        f"backup OK path={destination} bytes={manifest['backup_size']} sha256={digest} "
        f"integrity={integrity} counts={json.dumps(bracketed, sort_keys=True)} "
        f"duration={duration}s engine={engine_used}"
    )
    for old in removed:
        log(f"retention removed {old.name}")
    log(f"retention kept newest {args.retention} copies in {backup_dir}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
