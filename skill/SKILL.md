---
name: personal-automation
description: Use when the user asks to use personal automation CLIs, especially Oura Ring health data.
---

# Personal Automation

Use this skill for basic operation of the personal automation CLIs:

- `oura` — query Oura Ring data.

## Install / update

Install the CLI from the latest release:

```bash
curl -fsSL https://github.com/avyayv/personal-automation-cli/releases/latest/download/install.sh | bash -s -- oura
```

Update:

```bash
oura update
```

## Oura

Authentication:

```bash
export OURA_TOKEN="..."
```

Or save the token:

```bash
oura config init
oura config set-token "..."
oura config show
```

Useful commands:

```bash
oura personal-info
oura doctor
oura ring-configuration
oura daily-activity
oura daily-readiness
oura daily-sleep
oura daily-stress
oura daily-spo2
oura sleep
oura heartrate
```

Common date options:

```bash
oura daily-sleep --days 14
oura daily-readiness --start-date 2026-05-01 --end-date 2026-05-06
oura heartrate --start-datetime 2026-05-06T00:00:00Z --end-datetime 2026-05-06T12:00:00Z
```

Oura collection commands return concise JSON summaries by default. Prefer `--select` for very narrow answers, and use `--raw` only when the full API payload is needed, because raw `sleep`, `daily-activity`, and `heartrate` include large per-sample arrays:

```bash
oura sleep --days 7 --select data.day,data.total_sleep_min,data.efficiency
oura sleep --days 7 --raw
oura daily-activity --days 7 --raw
oura heartrate --raw
```

Fallback for unsupported Oura endpoints:

```bash
oura get daily_activity --param start_date=2026-05-01 --param end_date=2026-05-06
oura get /v2/usercollection/workout --param start_date=2026-05-01 --param end_date=2026-05-06
```

All CLI output is JSON.
