# P5: Service Management Helper

## Problem Statement

From `llm_calls` tracing on zkian's picoclaw (turn-5, iterations 33-44):

```
iter 33: exec("sudo systemctl start sec-intel-exporter && sleep 2 && sudo systemctl status") → FAILS (2.4s)
iter 34: exec("sudo journalctl -u sec-intel-exporter --no-pager -n 30")                    → OK (94ms)
iter 36: exec("echo 'Waiting 30s' && sleep 30 && sudo journalctl ...")                     → FAILS (30s!)
iter 38: exec("sleep 120 && sudo journalctl ...")                                           → FAILS (120s!)
iter 39: exec("curl -s http://localhost:9100/healthz && cat .cursor_state.json")            → OK (100ms)
iter 41: exec("sudo journalctl --since X --until Y | head -50")                             → OK (53ms)
iter 44: exec("sudo journalctl --since X | grep -v noise")                                  → OK (64ms)
```

**12 iterations** (iter 33-44) spent on service debugging — start, check status,
read journal, filter output, fix permissions, restart, check again. This is a
universal pattern that repeats for every systemd service deployment.

The LLM is manually executing individual shell commands to debug a service
that could be handled by a single helper script/skill.

---

## Design: Service Management Skill

A skill (`service-manager`) that encapsulates the common service debugging workflow
into a single tool call, eliminating the multi-iteration debug loop.

### Skill Interface

```
Skill: service-manager
Location: skills/service-manager/SKILL.md
Tool: manage_service
Parameters:
  - action: "start" | "stop" | "restart" | "status" | "diagnose" | "logs" | "health"
  - service_name: string (e.g., "sec-intel-exporter")
  - wait_seconds: int (default 5)
  - log_lines: int (default 50)
  - health_url: string (optional, for HTTP health checks)
```

### Script Logic (`scripts/manage.sh`)

```bash
#!/bin/bash
# Service management helper — reduces 12-iteration debug loops to 1-2 calls

ACTION="$1"
SERVICE="$2"
WAIT="${3:-5}"
LOG_LINES="${4:-50}"
HEALTH_URL="${5:-}"

case "$ACTION" in
  start)
    sudo systemctl start "$SERVICE" 2>&1
    sleep "$WAIT"
    STATE=$(systemctl is-active "$SERVICE" 2>&1)
    echo "STATE: $STATE"
    if [ "$STATE" != "active" ]; then
      echo "=== JOURNAL (last $LOG_LINES lines) ==="
      sudo journalctl -u "$SERVICE" --no-pager -n "$LOG_LINES" 2>&1
    fi
    ;;

  diagnose)
    echo "=== STATUS ==="
    systemctl is-active "$SERVICE" 2>&1
    systemctl is-enabled "$SERVICE" 2>&1
    echo ""
    echo "=== RECENT LOGS (errors only) ==="
    sudo journalctl -u "$SERVICE" --no-pager -n "$LOG_LINES" -p 3 2>&1
    echo ""
    echo "=== PERMISSIONS ==="
    # Find service file
    SERVICE_FILE=$(systemctl show -p FragmentPath "$SERVICE" 2>/dev/null | cut -d= -f2)
    if [ -f "$SERVICE_FILE" ]; then
      ls -la "$SERVICE_FILE" 2>&1
      # Extract ExecStart path and check its permissions
      EXEC=$(grep -oP 'ExecStart=\K[^ ]+' "$SERVICE_FILE" 2>/dev/null | head -1)
      if [ -n "$EXEC" ] && [ -e "$EXEC" ]; then
        ls -la "$EXEC" 2>&1
      fi
    fi
    echo ""
    echo "=== PROCESS CHECK ==="
    ps aux | grep "$SERVICE" | grep -v grep 2>&1 || echo "No process found"
    ;;

  health)
    if [ -n "$HEALTH_URL" ]; then
      HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" --connect-timeout 5 "$HEALTH_URL" 2>&1)
      echo "HTTP $HTTP_CODE from $HEALTH_URL"
    fi
    # Also check systemd state
    systemctl is-active "$SERVICE" 2>&1
    ;;

  logs)
    sudo journalctl -u "$SERVICE" --no-pager -n "$LOG_LINES" 2>&1
    ;;

  stop)
    sudo systemctl stop "$SERVICE" 2>&1
    systemctl is-active "$SERVICE" 2>&1
    ;;

  restart)
    sudo systemctl restart "$SERVICE" 2>&1
    sleep "$WAIT"
    systemctl is-active "$SERVICE" 2>&1
    ;;

esac
```

### SKILL.md

```markdown
# Service Manager

Manage systemd services with unified commands. Combines start/stop/status/journal
into single operations to avoid multi-iteration debugging loops.

## Usage

```bash
scripts/manage.sh <action> <service_name> [wait_seconds] [log_lines] [health_url]
```

## Actions

- `start` — start service, wait, report status + journal on failure
- `diagnose` — full diagnostic: status, recent errors, permissions, process check
- `health` — HTTP health check + systemd state
- `logs` — recent journal output
- `stop` — stop service
- `restart` — restart service, wait, report status

## Examples

```bash
# Start and auto-diagnose on failure (replaces 5+ exec calls)
bash skills/service-manager/scripts/manage.sh start sec-intel-exporter 5 50

# Full diagnostic (replaces 3+ exec calls)
bash skills/service-manager/scripts/manage.sh diagnose sec-intel-exporter 100

# Health check with HTTP endpoint
bash skills/service-manager/scripts/manage.sh health sec-intel-exporter 0 0 http://localhost:9100/healthz
```
```

---

## Comparison: Before vs After

### Before (turn-5, 12 iterations, ~80 seconds):

```
iter 33: exec("systemctl start && sleep 2 && systemctl status")        [2.4s, FAIL]
iter 34: exec("journalctl -n 30")                                       [0.1s, OK]
iter 35: exec("chown + chmod")                                          [24.1s, OK]
iter 36: exec("echo wait && sleep 30 && journalctl")                   [30.1s, FAIL]
iter 37: exec("journalctl --since X")                                  [0.1s, OK]
iter 38: exec("sleep 120 && journalctl")                                [120.0s, FAIL]
iter 39: exec("curl healthz && cat cursor")                             [0.1s, OK]
iter 41: exec("journalctl --since X --until Y")                        [0.1s, OK]
iter 44: exec("journalctl | grep -v noise")                            [0.1s, OK]
```

### After (1-2 calls, ~5-30 seconds):

```
call 1: exec("bash skills/service-manager/scripts/manage.sh start sec-intel-exporter 5 50")
  → Output: "STATE: failed" + last 50 journal lines
  → LLM reads error, understands root cause immediately

call 2 (if needed): exec("bash skills/service-manager/scripts/manage.sh diagnose sec-intel-exporter 100")
  → Output: status + recent errors + permissions + process check
  → LLM can fix the root cause now (no more journal fishing)
```

**Savings**: 10 iterations eliminated, ~60 seconds latency saved.

---

## Integration Path

### Option A: Skill (recommended)

Register as a skill under `skills/service-manager/`. The LLM reads SKILL.md and
calls `manage.sh` via the `exec` tool. No code changes needed — purely prompt + script.

**Pros**: Zero code changes, works immediately.
**Cons**: LLM must read SKILL.md first (1 iteration overhead).

### Option B: Native Tool

Implement as a Go tool in `pkg/tools/` that calls the script internally. Register
with `ToolRegistry` so it appears in tool definitions.

**Pros**: Always available, no SKILL.md read needed.
**Cons**: Requires code changes + rebuild.

### Option C: Prompt Contributor

Add a system prompt rule encouraging the LLM to use the service management pattern:

> "When debugging a systemd service, combine status + journal + permissions check
> into one exec call. Example: exec('systemctl status X && journalctl -u X -n 50
> && ls -la $(systemctl show -p FragmentPath X | cut -d= -f2)')"

**Pros**: Simplest, zero code.
**Cons**: Less reliable than a script (LLM may hallucinate systemctl flags).

---

## Recommendation

**Option A (Skill)** for immediate deployment. It solves the 12-iteration debug loop
with zero code changes and provides structured output that the LLM can parse reliably.
The SKILL.md acts as documentation, and the script is a single source of truth for
service management patterns.

If the pattern proves effective across multiple sessions, graduate to Option B
(native tool) for always-available access.
