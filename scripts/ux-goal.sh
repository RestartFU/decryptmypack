#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPORT_DIR="${IMPROVEMENT_GOAL_REPORT_DIR:-${UX_GOAL_REPORT_DIR:-$ROOT_DIR/reports/improvement-goal}}"
STATE_FILE="${IMPROVEMENT_GOAL_STATE_FILE:-${UX_GOAL_STATE_FILE:-$REPORT_DIR/state.json}}"
OBJECTIVE="${IMPROVEMENT_GOAL_OBJECTIVE:-${UX_GOAL_OBJECTIVE:-find anything meaningful to improve in this codebase}}"
MAX_UPDATES="${IMPROVEMENT_GOAL_MAX_UPDATES:-${UX_GOAL_MAX_UPDATES:-15}}"
WEBHOOK_URL="${IMPROVEMENT_GOAL_WEBHOOK_URL:-${UX_GOAL_WEBHOOK_URL:-}}"
POST=0
DRY_RUN=0
OPEN_REPORT=0
MODE="auto"
REPORT_INPUT=""

usage() {
  cat <<'USAGE'
Usage:
  scripts/ux-goal.sh [auto] [--dry-run]
  scripts/ux-goal.sh scan [--post] [--dry-run] [--open]
  scripts/ux-goal.sh implement <report.md> [--post] [--dry-run]
  scripts/ux-goal.sh status

What it does:
  auto       Default. Runs Codex to find, implement, verify, commit, deploy, push,
             and post Discord updates for one low-risk improvement. Auto mode uses Codex YOLO (danger-full-access/no approvals).
  scan       Audits the repo for broadly useful improvements and writes a markdown report.
  implement  Turns a chosen report into an implementation prompt you can paste/run.
  status     Shows how many improvement-flow updates have been posted.

Improvement scope:
  Looks across UX, product, correctness, reliability, performance, security/privacy,
  accessibility, observability, operations, tests, maintainability, and developer experience.

Stop limit:
  By default, the flow stops after 15 successful posted updates. Override with
  IMPROVEMENT_GOAL_MAX_UPDATES (or legacy UX_GOAL_MAX_UPDATES).

Discord posting:
  Auto mode posts automatically when IMPROVEMENT_GOAL_WEBHOOK_URL is set.
  You can set it in your shell or .env.improvement-goal. Legacy .env.ux-goal /
  UX_GOAL_WEBHOOK_URL is still supported.

Examples:
  scripts/ux-goal.sh
  scripts/ux-goal.sh auto
  scripts/ux-goal.sh scan --open
  scripts/ux-goal.sh scan --post
  scripts/ux-goal.sh implement reports/improvement-goal/2026-05-26-000000.md --post
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    auto)
      MODE="auto"; shift ;;
    scan)
      MODE="scan"; shift ;;
    implement)
      MODE="implement"; REPORT_INPUT="${2:-}"; shift 2 ;;
    status)
      MODE="status"; shift ;;
    --post)
      POST=1; shift ;;
    --dry-run)
      DRY_RUN=1; shift ;;
    --open)
      OPEN_REPORT=1; shift ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2 ;;
  esac
done

cd "$ROOT_DIR"

if [[ -f "$ROOT_DIR/.env.ux-goal" ]]; then
  # shellcheck disable=SC1091
  set -a; source "$ROOT_DIR/.env.ux-goal"; set +a
fi
if [[ -f "$ROOT_DIR/.env.improvement-goal" ]]; then
  # shellcheck disable=SC1091
  set -a; source "$ROOT_DIR/.env.improvement-goal"; set +a
fi

# Re-resolve after loading env files so .env.improvement-goal can override defaults.
REPORT_DIR="${IMPROVEMENT_GOAL_REPORT_DIR:-${UX_GOAL_REPORT_DIR:-$REPORT_DIR}}"
STATE_FILE="${IMPROVEMENT_GOAL_STATE_FILE:-${UX_GOAL_STATE_FILE:-$STATE_FILE}}"
OBJECTIVE="${IMPROVEMENT_GOAL_OBJECTIVE:-${UX_GOAL_OBJECTIVE:-$OBJECTIVE}}"
MAX_UPDATES="${IMPROVEMENT_GOAL_MAX_UPDATES:-${UX_GOAL_MAX_UPDATES:-$MAX_UPDATES}}"
WEBHOOK_URL="${IMPROVEMENT_GOAL_WEBHOOK_URL:-${UX_GOAL_WEBHOOK_URL:-$WEBHOOK_URL}}"
mkdir -p "$REPORT_DIR"

read_update_count() {
  python3 - "$STATE_FILE" <<'PY_READ_COUNT'
import json, pathlib, sys
path = pathlib.Path(sys.argv[1])
try:
    data = json.loads(path.read_text())
except Exception:
    data = {}
print(int(data.get("posted_updates", 0) or 0))
PY_READ_COUNT
}

write_update_count() {
  local count="$1"
  python3 - "$STATE_FILE" "$count" "$MAX_UPDATES" <<'PY_WRITE_COUNT'
import datetime, json, pathlib, sys
path = pathlib.Path(sys.argv[1])
count = int(sys.argv[2])
max_updates = int(sys.argv[3])
path.parent.mkdir(parents=True, exist_ok=True)
try:
    data = json.loads(path.read_text())
except Exception:
    data = {}
data["posted_updates"] = count
data["max_updates"] = max_updates
data["updated_at"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
data["scope"] = "global-improvements"
path.write_text(json.dumps(data, indent=2) + "\n")
PY_WRITE_COUNT
}

ensure_under_update_limit() {
  local count
  count="$(read_update_count)"
  if (( count >= MAX_UPDATES )); then
    echo "Improvement flow already reached ${count}/${MAX_UPDATES} posted updates; stopping." >&2
    echo "Set IMPROVEMENT_GOAL_MAX_UPDATES higher if you intentionally want to continue." >&2
    exit 0
  fi
}

mark_posted_update() {
  local count
  count="$(read_update_count)"
  count=$((count + 1))
  write_update_count "$count"
  if (( count >= MAX_UPDATES )); then
    echo "Improvement flow reached ${count}/${MAX_UPDATES} posted updates; future runs will stop." >&2
  else
    echo "Improvement flow posted updates: ${count}/${MAX_UPDATES}" >&2
  fi
}

run_status() {
  local count
  count="$(read_update_count)"
  echo "Improvement flow posted updates: ${count}/${MAX_UPDATES}"
  echo "Objective: $OBJECTIVE"
  echo "Report dir: $REPORT_DIR"
  echo "State file: $STATE_FILE"
}

post_discord() {
  local title="$1"
  local body="$2"
  local report="$3"

  if [[ "$POST" != "1" ]]; then
    return 0
  fi
  ensure_under_update_limit
  if [[ -z "${WEBHOOK_URL:-}" ]]; then
    echo "--post requested but IMPROVEMENT_GOAL_WEBHOOK_URL / UX_GOAL_WEBHOOK_URL is not set" >&2
    return 1
  fi

  python3 - "$WEBHOOK_URL" "$title" "$body" "$report" "$DRY_RUN" <<'PY_POST'
import json, pathlib, re, sys, urllib.request
webhook, title, body, report, dry_run = sys.argv[1:6]
report_path = pathlib.Path(report)
content = report_path.read_text(errors="replace") if report_path.exists() else ""

items = []
for line in content.splitlines():
    match = re.match(r"^\s*\d+\.\s+\*\*([^*]+)\*\*", line)
    if match:
        items.append(match.group(1).strip())
    if len(items) >= 3:
        break

summary = "\n".join(f"- {item}" for item in items) or "- got a fresh improvement pass ready to review"
report_name = pathlib.Path(report).name
message = (
    f"quick improvement update: {body}\n\n"
    f"{summary}\n\n"
    f"saved it as `{report_name}` when you want to pick one up."
)
payload = {
    "username": "restart",
    "content": "",
    "embeds": [{
        "description": message[:4000],
        "color": 0x20A7E8,
    }],
    "allowed_mentions": {"parse": []},
}

if dry_run == "1":
    preview = payload.get("content") or payload.get("embeds", [{}])[0].get("description", "")
    print("[dry-run] would post to Discord:\n" + preview)
    raise SystemExit(0)

req = urllib.request.Request(
    webhook,
    data=json.dumps(payload).encode(),
    headers={"content-type": "application/json", "user-agent": "decryptmypack-improvement-goal"},
    method="POST",
)
with urllib.request.urlopen(req, timeout=20) as res:
    if res.status >= 300:
        raise SystemExit(f"discord webhook failed: HTTP {res.status}")
PY_POST
  if [[ "$DRY_RUN" != "1" ]]; then
    mark_posted_update
  fi
}

post_status() {
  local body="$1"
  local color="${2:-2140136}"

  if [[ -z "${WEBHOOK_URL:-}" ]]; then
    return 0
  fi

  python3 - "$WEBHOOK_URL" "$body" "$color" "$DRY_RUN" <<'PY_STATUS'
import json, sys, urllib.request
webhook, body, color, dry_run = sys.argv[1:5]
payload = {
    "username": "restart",
    "embeds": [{
        "description": body[:4000],
        "color": int(color),
    }],
    "allowed_mentions": {"parse": []},
}
if dry_run == "1":
    print("[dry-run] would post status to Discord:\n" + payload["embeds"][0]["description"])
    raise SystemExit(0)
req = urllib.request.Request(
    webhook,
    data=json.dumps(payload).encode(),
    headers={"content-type": "application/json", "user-agent": "decryptmypack-improvement-goal"},
    method="POST",
)
with urllib.request.urlopen(req, timeout=20) as res:
    if res.status >= 300:
        raise SystemExit(f"discord webhook failed: HTTP {res.status}")
PY_STATUS
}

make_auto_prompt() {
  local out="$1"
  cat > "$out" <<PROMPT
You are running an automatic improvement pass for this repository.

Objective:
$OBJECTIVE

Do the whole loop without asking the user for clarification:
1. Inspect the current worktree and repository state.
2. Find exactly one meaningful, low-risk improvement anywhere in the project.
   Consider UX/product, correctness, reliability, performance, security/privacy,
   accessibility, observability/admin, ops/deploy safety, tests, maintainability,
   and developer experience.
3. Implement the improvement in a small focused diff.
4. Run relevant checks/tests/builds. If a check is not possible, explain why.
5. Commit with a concise conventional commit message.
6. Deploy and push, because this repo policy requires it.
7. Final response must include:
   - what changed
   - checks run
   - deploy/push result
   - commit hash

Rules:
- Use current files and command output as authoritative.
- Avoid risky rewrites and broad refactors.
- Do not expose secrets or webhook URLs.
- Do not modify reports/ or ignored state files unless that is the chosen improvement.
- Prefer one practical improvement over a long analysis.
- Run commands non-interactively. If a command/session already exited, start a new
  command instead of trying to write to its closed stdin.
PROMPT
}

run_auto() {
  ensure_under_update_limit

  if [[ "$DRY_RUN" != "1" ]] && [[ -n "$(git status --short)" ]]; then
    if [[ "${IMPROVEMENT_GOAL_ALLOW_DIRTY:-0}" != "1" ]]; then
      echo "worktree has uncommitted changes; refusing to auto-run" >&2
      echo "commit/stash them first, or set IMPROVEMENT_GOAL_ALLOW_DIRTY=1 to override" >&2
      exit 1
    fi
    echo "worktree has uncommitted changes; continuing because IMPROVEMENT_GOAL_ALLOW_DIRTY=1" >&2
  fi

  if ! command -v codex >/dev/null 2>&1; then
    echo "codex is required for auto mode" >&2
    exit 1
  fi

  local stamp prompt_file output_file before after
  stamp="$(date +%Y-%m-%d-%H%M%S)"
  prompt_file="$REPORT_DIR/$stamp-auto-prompt.md"
  output_file="$REPORT_DIR/$stamp-auto-result.md"
  make_auto_prompt "$prompt_file"

  echo "Auto improvement prompt: $prompt_file"
  if [[ "$DRY_RUN" == "1" ]]; then
    echo "[dry-run] would run:"
    echo "  codex exec --cd '$ROOT_DIR' --dangerously-bypass-approvals-and-sandbox --output-last-message '$output_file' \"$(basename "$prompt_file") contents\""
    post_status "automatic improvement dry run: prompt is ready at \`$(basename "$prompt_file")\`"
    return 0
  fi

  before="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
  post_status "automatic improvement run started\n\n- starting from \`$before\`\n- objective: $OBJECTIVE\n- I’ll pick one low-risk improvement, implement it, verify it, deploy, push, and post the result."

  local prompt_text
  prompt_text="$(cat "$prompt_file")"
  if codex exec \
      --cd "$ROOT_DIR" \
      --dangerously-bypass-approvals-and-sandbox \
      --output-last-message "$output_file" \
      "$prompt_text"; then
    after="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
    python3 - "$WEBHOOK_URL" "$output_file" "$after" "$DRY_RUN" <<'PY_DONE'
import json, pathlib, sys, urllib.request
webhook, output_file, commit, dry_run = sys.argv[1:5]
body = pathlib.Path(output_file).read_text(errors="replace") if pathlib.Path(output_file).exists() else ""
message = (
    "automatic improvement run finished\n\n"
    f"deployed + pushed: `{commit}`\n\n"
    + (body.strip() or "Codex finished without a final summary.")
)
payload = {
    "username": "restart",
    "embeds": [{"description": message[:4000], "color": 0x20A7E8}],
    "allowed_mentions": {"parse": []},
}
if webhook and dry_run != "1":
    req = urllib.request.Request(
        webhook,
        data=json.dumps(payload).encode(),
        headers={"content-type": "application/json", "user-agent": "decryptmypack-improvement-goal"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=20) as res:
        if res.status >= 300:
            raise SystemExit(f"discord webhook failed: HTTP {res.status}")
else:
    print(payload["embeds"][0]["description"])
PY_DONE
    if [[ -n "${WEBHOOK_URL:-}" ]]; then
      mark_posted_update
    fi
  else
    post_status "automatic improvement run failed\n\nCheck local output in \`$(basename "$output_file")\` and terminal logs." 15548997
    exit 1
  fi
}

run_scan() {
  local stamp report prompt_file
  stamp="$(date +%Y-%m-%d-%H%M%S)"
  report="$REPORT_DIR/$stamp.md"
  prompt_file="$(mktemp)"
  cat > "$prompt_file" <<PROMPT
Audit this codebase for concrete improvements for this objective:

$OBJECTIVE

Use current repository state as source of truth. Find anything worth improving across:
- user experience and product clarity
- correctness, bugs, edge cases, data integrity
- reliability, error handling, retries, caching, and background jobs
- performance, latency, bundle size, and resource usage
- security, privacy, auth, secrets handling, and abuse resistance
- accessibility and responsive behavior
- observability, admin/debuggability, logs, dashboards, and alerts
- operations/deploy safety and local/prod parity
- tests, CI, maintainability, code organization, and developer experience

Prioritize concrete, high-impact, low-risk work that can be implemented in small commits.

Return a concise markdown report with:
1. Top 10 improvement opportunities ranked by impact and confidence.
2. Evidence: exact files/functions/lines or observed behavior.
3. Why each matters to users, operators, or maintainers.
4. Low-risk implementation plan for each issue.
5. Quick wins suitable for a single commit.
6. Tests or manual checks to verify.

Do not modify files. Do not include secrets.
PROMPT

  if ! command -v omx >/dev/null 2>&1; then
    echo "omx is required for scan mode" >&2
    exit 1
  fi

  echo "Writing improvement report: $report"
  {
    echo "# Improvement report"
    echo
    echo "- Generated: $(date -Is)"
    echo "- Objective: $OBJECTIVE"
    echo "- Commit: $(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
    echo
    omx explore --prompt-file "$prompt_file"
  } | tee "$report"
  rm -f "$prompt_file"

  post_discord "Improvement report generated" "A broad codebase improvement report was generated." "$report"

  if [[ "$OPEN_REPORT" == "1" ]]; then
    ${PAGER:-less} "$report"
  fi
}

run_implement_prompt() {
  if [[ -z "$REPORT_INPUT" ]]; then
    echo "implement mode needs a report path" >&2
    exit 2
  fi
  if [[ ! -f "$REPORT_INPUT" ]]; then
    echo "report not found: $REPORT_INPUT" >&2
    exit 1
  fi

  local stamp out
  stamp="$(date +%Y-%m-%d-%H%M%S)"
  out="$REPORT_DIR/$stamp-implementation-prompt.md"
  cat > "$out" <<PROMPT
Implement the best low-risk improvements from this report:

$(cat "$REPORT_INPUT")

Rules:
- Inspect current worktree before editing.
- Prefer small, verifiable changes that improve users, operators, or maintainers.
- Avoid risky rewrites unless the report proves they are necessary.
- Run relevant checks.
- Commit with a concise conventional commit message.
- Deploy and push if repository policy requires it.
PROMPT

  echo "Implementation prompt written: $out"
  echo "Run this when ready, for example:"
  echo "  codex --prompt-file '$out'"

  post_discord "Improvement implementation prompt prepared" "A prompt is ready for implementing selected improvements." "$out"
}

case "$MODE" in
  auto) run_auto ;;
  scan) run_scan ;;
  implement) run_implement_prompt ;;
  status) run_status ;;
  *) usage; exit 2 ;;
esac
