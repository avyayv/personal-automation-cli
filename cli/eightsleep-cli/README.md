# Eight Sleep CLI

Go CLI binary named `eightsleep` for checking and controlling an Eight Sleep Pod and reading its nightly sleep data. All output is JSON.

Eight Sleep has no public API. This CLI logs in with your account email and password through the same unofficial endpoints the mobile app uses (`auth-api`, `client-api`, and `app-api.8slp.net`), so it can break if Eight Sleep changes them.

## Setup

```bash
eightsleep config set-login you@example.com   # prompts for password
eightsleep doctor
```

Or set `EIGHTSLEEP_EMAIL` and `EIGHTSLEEP_PASSWORD`. The access token is cached in the config file (mode 0600) and refreshed when it expires or is rejected. Set `EIGHTSLEEP_CONFIG` to override the config path.

## Commands

```bash
eightsleep me                       # account, device id, and side
eightsleep status                   # on/off, target and current level, device health
eightsleep on | off
eightsleep temp -30                 # level -100 (coolest) to 100 (warmest)
eightsleep away on | off
eightsleep prime
eightsleep sleep --days 7           # nightly score, durations, HR, HRV, respiratory rate
eightsleep sleep --raw              # full trends payload
eightsleep get /users/me --param key=value
```

Commands act on your own side by default. Pass `--side left|right` to act on whoever is assigned to that side. `--select a,b.c` and `--pretty` work on every command.

Exit codes: `0` success, `1` usage or API error, `2` missing or rejected login.
