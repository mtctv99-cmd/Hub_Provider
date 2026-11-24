#!/usr/bin/env python3
"""Retention/archive tool for ``observability_events``.

Default behaviour is a read-only dry run. Nothing is deleted until BOTH
``--apply`` and ``--yes`` are supplied, and even then the tool:

1. Refuses to run without a successful database backup younger than 24 hours
   (see scripts/ops/sqlite-backup.py).
2. Exports the affected rows to a gzip JSONL archive on local disk first and
   verifies the exported row count.
3. Deletes only rows with ``created_at`` older than the retention cutoff, in
   small key-ordered batches, never whole tables.

``detail`` can contain prompt/customer content, so it is omitted from the
archive by default. Passing ``--include-detail`` is an explicit owner decision
and should be paired with an encryption policy before any copy leaves the host.

The provider/account columns are currently empty in production (see audit M6);
this tool prints their population ratio on every run because a retention policy
alone does not make events attributable. Populating them is a Go-code change
tracked outside this script.

Retention window is intentionally bounded to 30..90 days.
"""

from __future__ import annotations

import argparse
import gzip
import hashlib
import json
import os
import shutil
import sqlite3
import sys
import time
from datetime import datetime, timedelta, timezone
from pathlib import Path

DEFAULT_DB = "/media/reup/Data_sv4/Sever_provider/data/orchestrator.db"
DEFAULT_BACKUP_DIR = "~/backups/sever-provider"
DEFAULT_ARCHIVE_DIR = "~/backups/sever-provider/observability-archive"
MIN_DAYS = 30
MAX_DAYS = 90
BACKUP_RE = "orchestrator-*.db"


def now_utc() -> datetime:
    return datetime.now(timezone.utc)


def log(message: str) -> None:
    print(f"{now_utc().strftime('%Y-%m-%dT%H:%M:%SZ')} {message}", flush=True)


def fail(message: str) -> int:
    print(f"ERROR: {message}", file=sys.stderr, flush=True)
    return 1


def open_ro(path: Path) -> sqlite3.Connection:
    conn = sqlite3.connect(path.resolve().as_uri() + "?mode=ro", uri=True, timeout=30)
    conn.execute("PRAGMA query_only=1")
    conn.execute("PRAGMA busy_timeout=30000")
    return conn


def populate_stats(conn: sqlite3.Connection) -> dict[str, int]:
    row = conn.execute(
        """
        SELECT COUNT(*) AS n,
               SUM(provider IS NOT NULL AND provider != '') AS provider_set,
               SUM(account_id IS NOT NULL AND account_id != '') AS account_set,
               SUM(model IS NOT NULL AND model != '') AS model_set
        FROM observability_events
        """
    ).fetchone()
    return {"total": row[0], "provider_set": row[1], "account_set": row[2], "model_set": row[3]}


def window_stats(conn: sqlite3.Connection, cutoff: str) -> dict:
    total_old = conn.execute(
        "SELECT COUNT(*) FROM observability_events WHERE created_at < ?", (cutoff,)
    ).fetchone()[0]
    bounds = conn.execute(
        "SELECT MIN(created_at), MAX(created_at) FROM observability_events WHERE created_at < ?",
        (cutoff,),
    ).fetchone()
    by_type = conn.execute(
        """
        SELECT event_type, COUNT(*) FROM observability_events
        WHERE created_at < ? GROUP BY event_type ORDER BY 2 DESC
        """,
        (cutoff,),
    ).fetchall()
    by_error = conn.execute(
        """
        SELECT COALESCE(NULLIF(error_kind,''),'(none)'), COUNT(*)
        FROM observability_events WHERE created_at < ?
        GROUP BY 1 ORDER BY 2 DESC LIMIT 15
        """,
        (cutoff,),
    ).fetchall()
    return {
        "old_rows": total_old,
        "old_min": bounds[0],
        "old_max": bounds[1],
        "by_type": by_type,
        "by_error": by_error,
    }


def freshest_backup(backup_dir: Path) -> Path | None:
    if not backup_dir.is_dir():
        return None
    candidates = [item for item in backup_dir.glob(BACKUP_RE) if item.is_file()]
    if not candidates:
        return None
    return max(candidates, key=lambda item: item.stat().st_mtime)


def verify_fresh_backup(backup_dir: Path, max_age_hours: int = 24) -> tuple[bool, str]:
    backup = freshest_backup(backup_dir)
    if backup is None:
        return False, f"no {BACKUP_RE} backup found in {backup_dir}"
    age_hours = (time.time() - backup.stat().st_mtime) / 3600
    if age_hours > max_age_hours:
        return False, f"newest backup {backup.name} is {age_hours:.1f}h old (> {max_age_hours}h)"
    try:
        conn = open_ro(backup)
        try:
            integrity = conn.execute("PRAGMA quick_check").fetchone()[0]
        finally:
            conn.close()
    except sqlite3.Error as exc:
        return False, f"cannot open backup {backup.name}: {exc}"
    if integrity != "ok":
        return False, f"backup {backup.name} quick_check={integrity}"
    return True, f"{backup.name} age={age_hours:.1f}h quick_check=ok"


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def export_archive(
    db: Path, archive: Path, cutoff: str, include_detail: bool
) -> tuple[int, int] | None:
    columns = ["id", "request_id", "event_type", "provider", "model", "account_id",
               "latency_ms", "error_kind", "created_at"]
    if include_detail:
        columns.insert(8, "detail")
    query = f"SELECT {', '.join(columns)} FROM observability_events WHERE created_at < ? ORDER BY id"

    exported = 0
    conn = open_ro(db)
    try:
        expected = conn.execute(
            "SELECT COUNT(*) FROM observability_events WHERE created_at < ?", (cutoff,)
        ).fetchone()[0]
        with gzip.open(archive, "wt", encoding="utf-8", compresslevel=6) as handle:
            cursor = conn.execute(query, (cutoff,))
            while True:
                rows = cursor.fetchmany(2000)
                if not rows:
                    break
                for row in rows:
                    handle.write(json.dumps(dict(zip(columns, row)), ensure_ascii=False) + "\n")
                    exported += 1
    finally:
        conn.close()
    return exported, expected


def delete_old_rows(db: Path, cutoff: str, batch_size: int) -> int:
    conn = sqlite3.connect(str(db), timeout=30)
    conn.execute("PRAGMA busy_timeout=30000")
    deleted = 0
    last_id = 0
    try:
        while True:
            rows = conn.execute(
                """
                SELECT id FROM observability_events
                WHERE id > ? AND created_at < ?
                ORDER BY id LIMIT ?
                """,
                (last_id, cutoff, batch_size),
            ).fetchall()
            if not rows:
                break
            boundary = rows[-1][0]
            conn.execute("BEGIN IMMEDIATE")
            try:
                cursor = conn.execute(
                    """
                    DELETE FROM observability_events
                    WHERE id > ? AND id <= ? AND created_at < ?
                    """,
                    (last_id, boundary, cutoff),
                )
                deleted += cursor.rowcount
                conn.commit()
            except Exception:
                conn.rollback()
                raise
            last_id = boundary
            log(f"deleted_batch boundary_id={boundary} total_deleted={deleted}")
        conn.execute("PRAGMA wal_checkpoint(PASSIVE)")
    finally:
        conn.close()
    return deleted


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="observability_events retention/archive")
    parser.add_argument("--db", default=DEFAULT_DB)
    parser.add_argument(
        "--days",
        type=int,
        default=60,
        help=f"retention days ({MIN_DAYS}..{MAX_DAYS}, default 60)",
    )
    parser.add_argument("--backup-dir", default=DEFAULT_BACKUP_DIR)
    parser.add_argument("--archive-dir", default=DEFAULT_ARCHIVE_DIR)
    parser.add_argument("--batch-size", type=int, default=5000)
    parser.add_argument("--include-detail", action="store_true",
                        help="archive detail column (may contain customer content)")
    parser.add_argument("--apply", action="store_true", help="actually archive and delete")
    parser.add_argument("--yes", action="store_true", help="confirm destructive apply")
    parser.add_argument("--vacuum", action="store_true",
                        help="VACUUM after deletion (requires fresh backup and free space)")
    return parser.parse_args(argv)


def main(argv: list[str]) -> int:
    os.umask(0o077)
    args = parse_args(argv)
    if not (MIN_DAYS <= args.days <= MAX_DAYS):
        return fail(f"--days must be between {MIN_DAYS} and {MAX_DAYS}")
    if args.batch_size < 100:
        return fail("--batch-size must be >= 100")

    db = Path(os.path.expanduser(args.db))
    if not db.is_file():
        return fail(f"database not found: {db}")

    cutoff_dt = now_utc() - timedelta(days=args.days)
    cutoff = cutoff_dt.strftime("%Y-%m-%d %H:%M:%S")

    conn = open_ro(db)
    try:
        stats = populate_stats(conn)
        window = window_stats(conn, cutoff)
    finally:
        conn.close()

    total = stats["total"] or 0
    log(f"db={db} total_events={total}")
    log(
        "provider_set={provider_set} account_set={account_set} model_set={model_set} "
        "(provider/account must be populated by the writer; retention cannot fix attribution)".format(
            **stats
        )
    )
    if total:
        log(
            f"provider_coverage={(stats['provider_set'] / total):.4f} "
            f"account_coverage={(stats['account_set'] / total):.4f}"
        )
    log(
        f"retention_days={args.days} cutoff_utc={cutoff} old_rows={window['old_rows']} "
        f"old_range=[{window['old_min']} .. {window['old_max']}]"
    )
    for event_type, count in window["by_type"]:
        log(f"  old event_type={event_type!r} n={count}")
    for error_kind, count in window["by_error"]:
        log(f"  old error_kind={error_kind!r} n={count}")

    if not args.apply:
        log("dry-run only; no rows exported or deleted (pass --apply --yes to execute)")
        return 0

    if not args.yes:
        return fail("--apply requires --yes; refusing to modify production data")

    ok, detail = verify_fresh_backup(Path(os.path.expanduser(args.backup_dir)))
    if not ok:
        return fail(f"backup precondition failed: {detail}")
    log(f"backup precondition OK: {detail}")

    if window["old_rows"] == 0:
        log("nothing older than cutoff; no action")
        return 0

    archive_dir = Path(os.path.expanduser(args.archive_dir))
    archive_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(archive_dir, 0o700)
    stamp = cutoff_dt.strftime("%Y%m%dT%H%M%SZ")
    archive = archive_dir / f"observability-events-before-{stamp}.jsonl.gz"
    if archive.exists():
        return fail(f"archive already exists: {archive} (refusing to overwrite)")

    started = time.time()
    result = export_archive(db, archive, cutoff, args.include_detail)
    if result is None:
        return fail("archive export returned no result")
    exported, expected = result
    if exported != expected:
        return fail(
            f"archive verification failed: exported={exported} expected={expected}; "
            f"archive kept at {archive}, no rows deleted"
        )
    os.chmod(archive, 0o600)
    digest = sha256_file(archive)
    log(
        f"archive OK path={archive} rows={exported} sha256={digest} "
        f"include_detail={args.include_detail} (no rows deleted yet)"
    )

    deleted = delete_old_rows(db, cutoff, args.batch_size)
    manifest = {
        "created_at_utc": now_utc().strftime("%Y-%m-%dT%H:%M:%SZ"),
        "cutoff_utc": cutoff,
        "retention_days": args.days,
        "archive": str(archive),
        "archive_sha256": digest,
        "archive_rows": exported,
        "deleted_rows": deleted,
        "include_detail": bool(args.include_detail),
        "before": stats,
        "duration_seconds": round(time.time() - started, 2),
    }
    (Path(str(archive) + ".manifest.json")).write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    log(f"deleted_rows={deleted} archive_manifest={archive}.manifest.json")

    if args.vacuum:
        free = shutil.disk_usage(db.parent).free
        size = db.stat().st_size
        if free < size * 2:
            return fail(f"not enough free space for VACUUM: free={free} db={size}")
        log("VACUUM start (rewrites the live database; backup already verified)")
        vac = sqlite3.connect(str(db), timeout=60)
        try:
            vac.execute("PRAGMA busy_timeout=60000")
            vac.execute("VACUUM")
        finally:
            vac.close()
        log("VACUUM done")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
