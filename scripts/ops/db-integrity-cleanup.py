#!/usr/bin/env python3
"""Non-destructive planner/applier for known data-integrity defects (M17/L3).

Defects handled:

* ``account_cooldowns.cooldown_until`` stores four encodings (DATETIME text,
  integer epoch, real epoch, NULL). The Go reader normalises them, but stored
  ``state`` can disagree with the derived status. Rows are normalised to
  ``YYYY-MM-DD HH:MM:SS`` UTC; expired COOLDOWN rows are marked READY; READY
  rows with epoch 0 are set to NULL. ``updated_at`` is never rewritten, so
  last-attempt ordering is preserved.
* 11 legacy cooldown rows key ``identity_id`` by email and violate the FK.
  Because the matching identity already has a canonical row, and every legacy
  cooldown is long expired, the safe repair is deletion (derived status is
  unchanged). Future-dated orphans are never deleted; they require manual
  review.
* Orphan ``api_key_rate_windows`` rows (deleted parent key) are deleted.
* Orphan ``credential_secrets`` rows (deleted parent credential) are deleted.

Default is dry-run. ``--apply`` additionally requires ``--yes`` and a verified
database backup younger than 24 hours. No table-wide DELETE or DROP is ever
emitted, and tables other than the four named above are untouched.
"""

from __future__ import annotations

import argparse
import json
import os
import sqlite3
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

DEFAULT_DB = "/media/reup/Data_sv4/Sever_provider/data/orchestrator.db"
DEFAULT_BACKUP_DIR = "~/backups/sever-provider"
BACKUP_RE = "orchestrator-*.db"
NORMALIZED = """(CASE
    WHEN typeof(cooldown_until) IN ('integer','real')
      THEN datetime(CASE WHEN cooldown_until >= 1e12 THEN cooldown_until/1000.0 ELSE cooldown_until END,'unixepoch')
    WHEN typeof(cooldown_until) = 'text' AND cooldown_until NOT LIKE '%-%'
      THEN datetime(CAST(cooldown_until AS REAL)/(CASE WHEN CAST(cooldown_until AS REAL) >= 1e12 THEN 1000.0 ELSE 1 END),'unixepoch')
    ELSE cooldown_until END)"""


def now_utc() -> datetime:
    return datetime.now(timezone.utc)


def log(message: str) -> None:
    print(f"{now_utc().strftime('%Y-%m-%dT%H:%M:%SZ')} {message}", flush=True)


def fail(message: str) -> int:
    print(f"ERROR: {message}", file=sys.stderr, flush=True)
    return 1


def mask(value: str | None) -> str:
    if not value:
        return ""
    if "@" in value:
        local, _, domain = value.partition("@")
        return (local[:2] + "***@" + domain) if local else "***@" + domain
    return value


def open_ro(path: Path) -> sqlite3.Connection:
    conn = sqlite3.connect(path.resolve().as_uri() + "?mode=ro", uri=True, timeout=30)
    conn.execute("PRAGMA query_only=1")
    conn.execute("PRAGMA busy_timeout=30000")
    return conn


def verify_fresh_backup(backup_dir: Path, max_age_hours: int = 24) -> tuple[bool, str]:
    if not backup_dir.is_dir():
        return False, f"backup dir missing: {backup_dir}"
    candidates = [item for item in backup_dir.glob(BACKUP_RE) if item.is_file()]
    if not candidates:
        return False, f"no {BACKUP_RE} in {backup_dir}"
    backup = max(candidates, key=lambda item: item.stat().st_mtime)
    age_hours = (time.time() - backup.stat().st_mtime) / 3600
    if age_hours > max_age_hours:
        return False, f"newest backup {backup.name} is {age_hours:.1f}h old"
    try:
        conn = open_ro(backup)
        try:
            check = conn.execute("PRAGMA quick_check").fetchone()[0]
        finally:
            conn.close()
    except sqlite3.Error as exc:
        return False, f"cannot open {backup.name}: {exc}"
    if check != "ok":
        return False, f"{backup.name} quick_check={check}"
    return True, f"{backup.name} age={age_hours:.1f}h quick_check=ok"


def fk_counts(conn: sqlite3.Connection) -> dict[str, int]:
    result: dict[str, int] = {}
    for row in conn.execute("PRAGMA foreign_key_check"):
        result[row[0]] = result.get(row[0], 0) + 1
    return result


def plan(conn: sqlite3.Connection) -> tuple[list[dict], dict]:
    actions: list[dict] = []
    summary: dict = {}

    cooldown_rows = conn.execute(
        f"""
        SELECT identity_id, state, cooldown_until, typeof(cooldown_until),
               {NORMALIZED} AS norm,
               EXISTS(SELECT 1 FROM identities i WHERE i.id = account_cooldowns.identity_id) AS id_exists,
               (SELECT i.id FROM identities i
                 WHERE i.email = account_cooldowns.identity_id
                    OR i.normalized_email = lower(account_cooldowns.identity_id)
                 LIMIT 1) AS email_target,
               EXISTS(SELECT 1 FROM account_cooldowns x
                       WHERE x.identity_id = (SELECT i.id FROM identities i
                         WHERE i.email = account_cooldowns.identity_id
                            OR i.normalized_email = lower(account_cooldowns.identity_id) LIMIT 1)) AS target_collision
        FROM account_cooldowns
        ORDER BY identity_id
        """
    ).fetchall()

    summary["cooldowns_total"] = len(cooldown_rows)
    summary["cooldowns_typeof"] = dict(
        conn.execute(
            "SELECT typeof(cooldown_until), COUNT(*) FROM account_cooldowns GROUP BY 1"
        ).fetchall()
    )

    for identity_id, state, raw, raw_type, norm, id_exists, email_target, collision in cooldown_rows:
        row_label = mask(identity_id)
        if not id_exists and email_target and norm and norm <= now_utc().strftime("%Y-%m-%d %H:%M:%S"):
            actions.append({
                "kind": "delete_orphan_cooldown",
                "label": f"{row_label} state={state} expired={norm} target={email_target} collision={collision}",
                "sql": "DELETE FROM account_cooldowns WHERE identity_id=?",
                "params": (identity_id,),
            })
            continue
        if not id_exists:
            actions.append({
                "kind": "manual_review_cooldown",
                "label": f"{row_label} state={state} raw={raw!r} type={raw_type} target={email_target}",
            })
            continue
        if state == "READY":
            if raw is not None:
                actions.append({
                    "kind": "normalize_ready_null",
                    "label": f"{row_label} READY {raw_type}={raw!r} -> NULL",
                    "sql": "UPDATE account_cooldowns SET cooldown_until=NULL WHERE identity_id=?",
                    "params": (identity_id,),
                })
            continue
        if state == "COOLDOWN":
            if norm is None:
                actions.append({
                    "kind": "manual_review_cooldown",
                    "label": f"{row_label} COOLDOWN unparseable raw={raw!r} type={raw_type}",
                })
            elif norm <= now_utc().strftime("%Y-%m-%d %H:%M:%S"):
                actions.append({
                    "kind": "expire_cooldown",
                    "label": f"{row_label} COOLDOWN expired {norm} -> READY/NULL",
                    "sql": "UPDATE account_cooldowns SET state='READY', cooldown_until=NULL WHERE identity_id=?",
                    "params": (identity_id,),
                })
            elif raw_type != "text" or raw != norm:
                actions.append({
                    "kind": "normalize_future_cooldown",
                    "label": f"{row_label} COOLDOWN {raw_type}={raw!r} -> {norm}",
                    "sql": "UPDATE account_cooldowns SET cooldown_until=? WHERE identity_id=?",
                    "params": (norm, identity_id),
                })
            continue
        if state == "ACTIVE" and raw is not None:
            actions.append({
                "kind": "manual_review_cooldown",
                "label": f"{row_label} ACTIVE with cooldown_until={raw!r} type={raw_type}",
            })

    orphan_windows = conn.execute(
        """
        SELECT key_id, window_start, request_count
        FROM api_key_rate_windows akw
        WHERE NOT EXISTS(SELECT 1 FROM api_keys k WHERE k.id=akw.key_id)
        ORDER BY window_start
        """
    ).fetchall()
    summary["orphan_rate_windows"] = len(orphan_windows)
    for key_id, window_start, request_count in orphan_windows:
        actions.append({
            "kind": "delete_orphan_rate_window",
            "label": f"key={key_id} window_start={window_start} count={request_count}",
            "sql": "DELETE FROM api_key_rate_windows WHERE key_id=? AND window_start=?",
            "params": (key_id, window_start),
        })

    orphan_secrets = conn.execute(
        """
        SELECT credential_id FROM credential_secrets cs
        WHERE NOT EXISTS(SELECT 1 FROM credentials c WHERE c.id=cs.credential_id)
        """
    ).fetchall()
    summary["orphan_credential_secrets"] = len(orphan_secrets)
    for (credential_id,) in orphan_secrets:
        actions.append({
            "kind": "delete_orphan_secret",
            "label": f"credential_id={mask(credential_id)}",
            "sql": "DELETE FROM credential_secrets WHERE credential_id=?",
            "params": (credential_id,),
        })

    summary["fk_violations_before"] = fk_counts(conn)
    summary["expired_active_api_keys"] = conn.execute(
        "SELECT COUNT(*) FROM api_keys WHERE active=1 AND expires_at IS NOT NULL AND expires_at < datetime('now')"
    ).fetchone()[0]
    return actions, summary


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="M17/L3 dry-run-first integrity repair")
    parser.add_argument("--db", default=DEFAULT_DB)
    parser.add_argument("--backup-dir", default=DEFAULT_BACKUP_DIR)
    parser.add_argument("--apply", action="store_true")
    parser.add_argument("--yes", action="store_true")
    parser.add_argument("--json", action="store_true", help="emit plan as JSON")
    return parser.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    db = Path(os.path.expanduser(args.db))
    if not db.is_file():
        return fail(f"database not found: {db}")

    conn = open_ro(db)
    try:
        actions, summary = plan(conn)
    finally:
        conn.close()

    if args.json:
        print(json.dumps({"summary": summary, "actions": actions}, indent=2, sort_keys=True))
    else:
        log(f"cooldowns_total={summary['cooldowns_total']} typeof={summary['cooldowns_typeof']}")
        log(f"orphan_rate_windows={summary['orphan_rate_windows']} "
            f"orphan_credential_secrets={summary['orphan_credential_secrets']}")
        log(f"fk_violations_before={summary['fk_violations_before']}")
        log(f"expired_active_api_keys={summary['expired_active_api_keys']} (reported only)")
        counts: dict[str, int] = {}
        for action in actions:
            counts[action["kind"]] = counts.get(action["kind"], 0) + 1
        for action in actions:
            log(f"  plan {action['kind']}: {action['label']}")
        log(f"plan_counts={counts}")

    manual = [a for a in actions if a["kind"] == "manual_review_cooldown"]
    executable = [a for a in actions if "sql" in a]
    if manual:
        log(f"manual_review_required={len(manual)}; these rows are not modified by this tool")

    if not args.apply:
        log("dry-run only; no rows changed (pass --apply --yes after a fresh backup)")
        return 0
    if not args.yes:
        return fail("--apply requires --yes")
    if manual:
        return fail("manual-review rows present; resolve them or extend the tool before applying")

    ok, detail = verify_fresh_backup(Path(os.path.expanduser(args.backup_dir)))
    if not ok:
        return fail(f"backup precondition failed: {detail}")
    log(f"backup precondition OK: {detail}")

    conn = sqlite3.connect(str(db), timeout=30)
    conn.execute("PRAGMA busy_timeout=30000")
    conn.execute("PRAGMA foreign_keys=ON")
    try:
        conn.execute("BEGIN IMMEDIATE")
        try:
            changed = 0
            for action in executable:
                cursor = conn.execute(action["sql"], action["params"])
                changed += cursor.rowcount
            remaining = fk_counts(conn)
            if remaining:
                conn.rollback()
                return fail(f"foreign_key_check still reports violations after repair: {remaining}")
            conn.commit()
        except Exception:
            conn.rollback()
            raise
    finally:
        conn.close()

    log(f"applied actions={len(executable)} rows_changed={changed} "
        f"fk_violations_after=0")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
