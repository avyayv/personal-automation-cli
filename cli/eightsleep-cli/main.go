package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Eight Sleep has no public API. These are the endpoints and OAuth client the
// official mobile app uses (also used by pyEight and the Home Assistant integration).
const (
	authBaseURL         = "https://auth-api.8slp.net/v1"
	clientBaseURL       = "https://client-api.8slp.net/v1"
	appBaseURL          = "https://app-api.8slp.net/v1"
	appClientID         = "0894c7f33bb94800a03f1f4df13a4f38"
	appClientSecret     = "f0954a3ed5763ba3d06834c73731a32f15f168f47d4f164751275def86db0c76"
	userAgent           = "okhttp/4.9.3"
	defaultUpdateSource = "https://github.com/avyayv/personal-automation-cli.git"
	redactedSecret      = "********"
)

type anyMap map[string]any

type codedError struct {
	code int
	err  error
}

func (e codedError) Error() string { return e.err.Error() }
func (e codedError) Unwrap() error { return e.err }

func main() {
	args := os.Args[1:]
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		usage()
		return
	}

	api := NewHTTPClient(Endpoints{Auth: authBaseURL, Client: clientBaseURL, App: appBaseURL})
	payload, opts, code, err := runCLIWithOptions(args, api, NewConfigStore(defaultConfigPath()), time.Now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if ce, ok := err.(codedError); ok {
			os.Exit(ce.code)
		}
		os.Exit(code)
	}
	printJSON(payload, opts.Pretty)
}

func usage() {
	fmt.Fprintf(os.Stderr, `Eight Sleep CLI (Go, no Python required)

Usage: eightsleep <command> [args]

Commands:
  doctor                                Check config, login, and API reachability
  me                                    Show account and device assignment
  status                                Show bed side state, temperature level, and device health
  on                                    Turn on your side (smart temperature schedule)
  off                                   Turn off your side
  temp <level>                          Set your side's level, -100 (coolest) to 100 (warmest)
  sleep [date flags]                    Fetch nightly sleep summaries
  away on|off                           Start or end away mode
  prime                                 Start priming the pod's water
  get <path> [--param key=value]        Call a client-api path directly (e.g. /users/me)
  config init                           Create the config file
  config show                           Show config values (secrets redacted)
  config set-login <email> [password]   Save login (password read from stdin if omitted)
  config clear                          Remove saved login and cached session
  update [install_path]                 Download, rebuild, and install latest CLI

Date flags:
  --start-date YYYY-MM-DD               First night to include
  --end-date YYYY-MM-DD                 Last night to include (default: today)
  --days N                              Default window size (default: 7)
  --raw                                 Return the full API response instead of a concise summary

Global flags:
  --side left|right                     Act on a specific bed side instead of your own
  --select a,b.c                        Return only selected JSON fields (repeat or comma-separate)
  --output json|pretty                  Output compact JSON (default) or indented JSON
  --pretty                              Alias for --output pretty

Authentication:
  Set EIGHTSLEEP_EMAIL and EIGHTSLEEP_PASSWORD, or run: eightsleep config set-login <email>
  The access token is cached in the config file and refreshed automatically.

Set EIGHTSLEEP_CONFIG to override the config path.
`)
}

type cliOptions struct {
	Pretty bool
	Select []string
	Side   string
}

func runCLI(args []string, api API, store ConfigStore, now func() time.Time) (any, int, error) {
	payload, _, code, err := runCLIWithOptions(args, api, store, now)
	return payload, code, err
}

func runCLIWithOptions(args []string, api API, store ConfigStore, now func() time.Time) (any, cliOptions, int, error) {
	opts, cleanArgs, err := parseGlobalArgs(args)
	if err != nil {
		return nil, opts, 1, err
	}
	payload, code, err := runCoreCLI(cleanArgs, opts, api, store, now)
	if err != nil || len(opts.Select) == 0 {
		return payload, opts, code, err
	}
	return selectPayloadFields(payload, opts.Select), opts, code, nil
}

func runCoreCLI(args []string, opts cliOptions, api API, store ConfigStore, now func() time.Time) (any, int, error) {
	if len(args) < 1 {
		return nil, 2, errors.New("missing command")
	}
	cmd, rest := args[0], args[1:]
	s := &session{api: api, store: store, now: now, side: opts.Side}
	switch cmd {
	case "doctor":
		if len(rest) != 0 {
			return nil, 1, errors.New("usage: eightsleep doctor")
		}
		return handleDoctor(s)
	case "me", "profile":
		if len(rest) != 0 {
			return nil, 1, fmt.Errorf("usage: eightsleep %s", cmd)
		}
		return wrap(handleMe(s))
	case "status":
		if len(rest) != 0 {
			return nil, 1, errors.New("usage: eightsleep status")
		}
		return wrap(handleStatus(s))
	case "on", "off":
		if len(rest) != 0 {
			return nil, 1, fmt.Errorf("usage: eightsleep %s", cmd)
		}
		stateType := "smart"
		if cmd == "off" {
			stateType = "off"
		}
		return wrap(setTemperature(s, anyMap{"currentState": anyMap{"type": stateType}}))
	case "temp", "level":
		if len(rest) != 1 {
			return nil, 1, errors.New("usage: eightsleep temp <level -100..100>")
		}
		level, err := strconv.Atoi(strings.TrimPrefix(rest[0], "+"))
		if err != nil || level < -100 || level > 100 {
			return nil, 1, errors.New("level must be an integer from -100 to 100")
		}
		return wrap(setTemperature(s, anyMap{"currentLevel": level, "currentState": anyMap{"type": "smart"}}))
	case "sleep", "trends":
		return wrap(handleSleep(s, rest))
	case "away":
		if len(rest) != 1 || (rest[0] != "on" && rest[0] != "off") {
			return nil, 1, errors.New("usage: eightsleep away on|off")
		}
		return wrap(handleAway(s, rest[0] == "on"))
	case "prime":
		if len(rest) != 0 {
			return nil, 1, errors.New("usage: eightsleep prime")
		}
		return wrap(handlePrime(s))
	case "get":
		return wrap(handleGet(s, rest))
	case "config":
		return handleConfig(rest, store)
	case "update":
		payload, err := updateCLI(rest)
		if err != nil {
			return nil, 1, err
		}
		return payload, 0, nil
	default:
		return nil, 1, fmt.Errorf("unknown command %q", cmd)
	}
}

// wrap maps auth failures to exit code 2, matching the oura CLI.
func wrap(payload any, err error) (any, int, error) {
	if err == nil {
		return payload, 0, nil
	}
	var apiErr APIError
	if errors.Is(err, errMissingLogin) || (errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized) {
		return nil, 2, codedError{code: 2, err: err}
	}
	return nil, 1, err
}

func parseGlobalArgs(args []string) (cliOptions, []string, error) {
	var opts cliOptions
	clean := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--pretty":
			opts.Pretty = true
		case "--output", "--select", "--side":
			if !hasValue {
				i++
				if i >= len(args) {
					return opts, nil, fmt.Errorf("%s requires a value", name)
				}
				value = args[i]
			}
			switch name {
			case "--output":
				switch value {
				case "json", "compact":
					opts.Pretty = false
				case "pretty":
					opts.Pretty = true
				default:
					return opts, nil, fmt.Errorf("--output must be json or pretty, got %q", value)
				}
			case "--select":
				for _, field := range strings.Split(value, ",") {
					if field = strings.TrimSpace(field); field != "" {
						opts.Select = append(opts.Select, field)
					}
				}
			case "--side":
				if value != "left" && value != "right" {
					return opts, nil, fmt.Errorf("--side must be left or right, got %q", value)
				}
				opts.Side = value
			}
		default:
			clean = append(clean, arg)
		}
	}
	return opts, clean, nil
}

var errMissingLogin = errors.New("missing Eight Sleep login; set EIGHTSLEEP_EMAIL/EIGHTSLEEP_PASSWORD or run `eightsleep config set-login <email>`")

// session lazily logs in, caches the token in the config file, and resolves
// which user/side a command acts on.
type session struct {
	api   API
	store ConfigStore
	now   func() time.Time
	side  string

	config   Config
	loaded   bool
	me       anyMap
	deviceID string
}

func (s *session) load() error {
	if s.loaded {
		return nil
	}
	config, err := s.store.Load()
	if err != nil {
		return err
	}
	s.config = config
	s.loaded = true
	return nil
}

func (s *session) token(ctx context.Context, forceLogin bool) (string, error) {
	if err := s.load(); err != nil {
		return "", err
	}
	email, password := effectiveLogin(s.config)
	cached := s.config.AccessToken != "" && s.config.TokenEmail == email && s.config.TokenExpiresAt.After(s.now().Add(time.Minute))
	if cached && !forceLogin {
		return s.config.AccessToken, nil
	}
	if email == "" || password == "" {
		return "", errMissingLogin
	}
	tok, err := s.api.Login(ctx, email, password)
	if err != nil {
		return "", err
	}
	s.config.AccessToken = tok.AccessToken
	s.config.UserID = tok.UserID
	s.config.TokenEmail = email
	s.config.TokenExpiresAt = s.now().Add(time.Duration(tok.ExpiresIn) * time.Second).UTC().Truncate(time.Second)
	if err := s.store.Save(s.config); err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

// do performs an authorized request, logging in again once if the cached token is rejected.
func (s *session) do(method string, base Base, path string, params map[string]string, body any) (any, error) {
	ctx := context.Background()
	token, err := s.token(ctx, false)
	if err != nil {
		return nil, err
	}
	payload, err := s.api.Do(ctx, method, base, path, params, body, token)
	var apiErr APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized {
		if token, err = s.token(ctx, true); err != nil {
			return nil, err
		}
		payload, err = s.api.Do(ctx, method, base, path, params, body, token)
	}
	return payload, err
}

func (s *session) user() (anyMap, error) {
	if s.me != nil {
		return s.me, nil
	}
	payload, err := s.do(http.MethodGet, BaseClient, "/users/me", nil, nil)
	if err != nil {
		return nil, err
	}
	m, _ := asAnyMap(payload)
	user, ok := asAnyMap(m["user"])
	if !ok {
		return nil, errors.New("unexpected /users/me response")
	}
	s.me = user
	if device, ok := asAnyMap(user["currentDevice"]); ok {
		s.deviceID, _ = stringValue(device["id"])
	}
	if s.deviceID == "" {
		if devices, ok := user["devices"].([]any); ok && len(devices) > 0 {
			s.deviceID, _ = stringValue(devices[0])
		}
	}
	return user, nil
}

func (s *session) device() (anyMap, error) {
	if _, err := s.user(); err != nil {
		return nil, err
	}
	if s.deviceID == "" {
		return nil, errors.New("no Eight Sleep device is assigned to this account")
	}
	payload, err := s.do(http.MethodGet, BaseClient, "/devices/"+url.PathEscape(s.deviceID), nil, nil)
	if err != nil {
		return nil, err
	}
	m, _ := asAnyMap(payload)
	result, ok := asAnyMap(m["result"])
	if !ok {
		return nil, errors.New("unexpected device response")
	}
	return result, nil
}

// target returns the user ID and side to act on: your own side by default,
// or whoever is assigned to --side.
func (s *session) target() (string, string, error) {
	user, err := s.user()
	if err != nil {
		return "", "", err
	}
	userID, _ := stringValue(user["userId"])
	side := ""
	if device, ok := asAnyMap(user["currentDevice"]); ok {
		side, _ = stringValue(device["side"])
	}
	if s.side == "" || s.side == side {
		return userID, side, nil
	}
	device, err := s.device()
	if err != nil {
		return "", "", err
	}
	other, ok := stringValue(device[s.side+"UserId"])
	if !ok {
		return "", "", fmt.Errorf("no user is assigned to the %s side", s.side)
	}
	return other, s.side, nil
}

func handleDoctor(s *session) (any, int, error) {
	if err := s.load(); err != nil {
		return nil, 1, err
	}
	envEmail := strings.TrimSpace(os.Getenv("EIGHTSLEEP_EMAIL")) != ""
	envPassword := os.Getenv("EIGHTSLEEP_PASSWORD") != ""
	email, password := effectiveLogin(s.config)
	out := anyMap{
		"ok":     false,
		"config": anyMap{"path": s.store.Path(), "exists": s.store.Exists()},
		"auth": anyMap{
			"env_email_set":       envEmail,
			"env_password_set":    envPassword,
			"config_email_set":    s.config.Email != "",
			"config_password_set": s.config.Password != "",
			"cached_token":        s.config.AccessToken != "",
			"token_expires_at":    timeOrNil(s.config.TokenExpiresAt),
			"login_configured":    email != "" && password != "",
		},
		"api": anyMap{"checked": false},
	}
	if email == "" || password == "" {
		if s.config.AccessToken == "" {
			out["hint"] = "Set EIGHTSLEEP_EMAIL/EIGHTSLEEP_PASSWORD or run `eightsleep config set-login <email>`"
			return out, 0, nil
		}
	}
	user, err := s.user()
	api := anyMap{"checked": true, "ok": err == nil}
	if err != nil {
		api["error"] = err.Error()
		var apiErr APIError
		if errors.As(err, &apiErr) {
			api["status_code"] = apiErr.StatusCode
		}
		out["api"] = api
		return out, 0, nil
	}
	api["device_assigned"] = s.deviceID != ""
	if device, ok := asAnyMap(user["currentDevice"]); ok {
		copyFieldAs(api, device, "side", "side")
	}
	out["ok"] = true
	out["api"] = api
	return out, 0, nil
}

func handleMe(s *session) (any, error) {
	user, err := s.user()
	if err != nil {
		return nil, err
	}
	out := anyMap{}
	copyFields(out, user, "userId", "email", "firstName", "lastName", "gender", "zip", "timezone", "devices")
	if device, ok := asAnyMap(user["currentDevice"]); ok {
		out["device_id"] = device["id"]
		out["side"] = device["side"]
	}
	return out, nil
}

func handleStatus(s *session) (any, error) {
	userID, side, err := s.target()
	if err != nil {
		return nil, err
	}
	temp, err := s.do(http.MethodGet, BaseApp, "/users/"+url.PathEscape(userID)+"/temperature", nil, nil)
	if err != nil {
		return nil, err
	}
	out := anyMap{"user_id": userID, "side": side}
	if m, ok := asAnyMap(temp); ok {
		summarizeTemperature(out, m)
	}
	device, err := s.device()
	if err != nil {
		out["device_error"] = err.Error()
		return out, nil
	}
	out["device"] = summarizeDevice(device, side)
	return out, nil
}

func summarizeTemperature(out, m anyMap) {
	if state, ok := asAnyMap(m["currentState"]); ok {
		copyFieldAs(out, state, "state", "type")
		if t, ok := stringValue(state["type"]); ok {
			out["on"] = t != "off"
		}
	}
	copyFieldAs(out, m, "target_level", "currentLevel")
	copyFieldAs(out, m, "current_level", "currentDeviceLevel")
	if smart, ok := asAnyMap(m["smart"]); ok {
		schedule := anyMap{}
		copyFieldAs(schedule, smart, "bedtime_level", "bedTimeLevel")
		copyFieldAs(schedule, smart, "initial_sleep_level", "initialSleepLevel")
		copyFieldAs(schedule, smart, "final_sleep_level", "finalSleepLevel")
		if len(schedule) > 0 {
			out["smart_levels"] = schedule
		}
	}
}

func summarizeDevice(d anyMap, side string) anyMap {
	out := anyMap{}
	copyFields(out, d, "deviceId", "online", "needsPriming", "priming", "hasWater", "lastPrime", "lastLowWater", "firmwareVersion", "timezone")
	if side != "" {
		copyFieldAs(out, d, "heating_level", side+"HeatingLevel")
		copyFieldAs(out, d, "target_heating_level", side+"TargetHeatingLevel")
		copyFieldAs(out, d, "now_heating", side+"NowHeating")
		copyFieldAs(out, d, "heating_duration_sec", side+"HeatingDuration")
		copyFieldAs(out, d, "kelvin", side+"Kelvin")
	}
	return out
}

func setTemperature(s *session, body anyMap) (any, error) {
	userID, side, err := s.target()
	if err != nil {
		return nil, err
	}
	payload, err := s.do(http.MethodPut, BaseApp, "/users/"+url.PathEscape(userID)+"/temperature", nil, body)
	if err != nil {
		return nil, err
	}
	out := anyMap{"ok": true, "user_id": userID, "side": side}
	if m, ok := asAnyMap(payload); ok {
		summarizeTemperature(out, m)
	}
	return out, nil
}

func handleAway(s *session, on bool) (any, error) {
	userID, side, err := s.target()
	if err != nil {
		return nil, err
	}
	key := "end"
	if on {
		key = "start"
	}
	// The app sends a timestamp slightly in the past so the change applies immediately.
	ts := s.now().UTC().Add(-24 * time.Hour).Format("2006-01-02T15:04:05.000Z")
	if _, err := s.do(http.MethodPut, BaseApp, "/users/"+url.PathEscape(userID)+"/away-mode", nil, anyMap{"awayPeriod": anyMap{key: ts}}); err != nil {
		return nil, err
	}
	return anyMap{"ok": true, "user_id": userID, "side": side, "away": on}, nil
}

func handlePrime(s *session) (any, error) {
	user, err := s.user()
	if err != nil {
		return nil, err
	}
	if s.deviceID == "" {
		return nil, errors.New("no Eight Sleep device is assigned to this account")
	}
	userID, _ := stringValue(user["userId"])
	body := anyMap{"notifications": anyMap{"users": []string{userID}, "meta": "rePriming"}}
	if _, err := s.do(http.MethodPost, BaseApp, "/devices/"+url.PathEscape(s.deviceID)+"/priming/tasks", nil, body); err != nil {
		return nil, err
	}
	return anyMap{"ok": true, "device_id": s.deviceID, "priming": "started"}, nil
}

func handleSleep(s *session, args []string) (any, error) {
	opts, positionals, err := parseDateArgs(args)
	if err != nil {
		return nil, err
	}
	if len(positionals) != 0 {
		return nil, errors.New("unexpected positional argument")
	}
	from, to, err := dateRange(opts, s.now())
	if err != nil {
		return nil, err
	}
	userID, side, err := s.target()
	if err != nil {
		return nil, err
	}
	tz := time.Local.String()
	if tz == "Local" || tz == "" {
		tz = "UTC"
	}
	if user, err := s.user(); err == nil {
		if userTZ, ok := stringValue(user["timezone"]); ok {
			tz = userTZ
		}
	}
	params := map[string]string{
		"tz":                   tz,
		"from":                 from,
		"to":                   to,
		"include-main":         "false",
		"include-all-sessions": "true",
		"model-version":        "v2",
	}
	payload, err := s.do(http.MethodGet, BaseClient, "/users/"+url.PathEscape(userID)+"/trends", params, nil)
	if err != nil || opts.Raw {
		return payload, err
	}
	m, ok := asAnyMap(payload)
	if !ok {
		return payload, nil
	}
	days, _ := m["days"].([]any)
	return anyMap{
		"user_id":     userID,
		"side":        side,
		"from":        from,
		"to":          to,
		"count":       len(days),
		"data":        mapItems(days, summarizeNight),
		"raw_omitted": "sessions,stages,timeseries; use --raw",
	}, nil
}

func summarizeNight(m anyMap) anyMap {
	out := anyMap{}
	copyFields(out, m, "day", "score", "tnt")
	copyFieldAs(out, m, "bedtime_start", "presenceStart")
	copyFieldAs(out, m, "bedtime_end", "presenceEnd")
	copyDurationMinutes(out, m, "time_in_bed_min", "presenceDuration")
	copyDurationMinutes(out, m, "total_sleep_min", "sleepDuration")
	copyDurationMinutes(out, m, "light_min", "lightDuration")
	copyDurationMinutes(out, m, "deep_min", "deepDuration")
	copyDurationMinutes(out, m, "rem_min", "remDuration")
	copyDurationMinutes(out, m, "latency_min", "latencyAsleepSeconds")
	if quality, ok := asAnyMap(m["sleepQualityScore"]); ok {
		copyFieldAs(out, quality, "quality_score", "total")
		copyCurrent(out, quality, "heart_rate", "heartRate")
		copyCurrent(out, quality, "hrv", "hrv")
		copyCurrent(out, quality, "respiratory_rate", "respiratoryRate")
		copyCurrent(out, quality, "bed_temp_c", "tempBedC")
		copyCurrent(out, quality, "room_temp_c", "tempRoomC")
	}
	if routine, ok := asAnyMap(m["sleepRoutineScore"]); ok {
		copyFieldAs(out, routine, "routine_score", "total")
	}
	return out
}

func copyCurrent(dst, src anyMap, outKey, inKey string) {
	if metric, ok := asAnyMap(src[inKey]); ok {
		if v, ok := numberAsFloat(metric["current"]); ok {
			dst[outKey] = round1(v)
		}
	}
}

func handleGet(s *session, args []string) (any, error) {
	if len(args) < 1 {
		return nil, errors.New("usage: eightsleep get <path> [--param key=value]")
	}
	path := "/" + strings.TrimPrefix(strings.TrimSpace(args[0]), "/")
	params := map[string]string{}
	for i := 1; i < len(args); i++ {
		name, value, hasValue := strings.Cut(args[i], "=")
		if name != "--param" {
			return nil, fmt.Errorf("unknown flag %q", args[i])
		}
		if !hasValue {
			i++
			if i >= len(args) {
				return nil, errors.New("--param requires key=value")
			}
			value = args[i]
		}
		key, val, ok := strings.Cut(value, "=")
		if !ok || key == "" {
			return nil, errors.New("--param requires key=value")
		}
		params[key] = val
	}
	return s.do(http.MethodGet, BaseClient, path, params, nil)
}

func handleConfig(args []string, store ConfigStore) (any, int, error) {
	if len(args) < 1 {
		return nil, 1, errors.New("usage: eightsleep config <init|show|set-login|clear>")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "init":
		if len(rest) != 0 {
			return nil, 1, errors.New("usage: eightsleep config init")
		}
		created, err := store.Init()
		if err != nil {
			return nil, 1, err
		}
		return anyMap{"path": store.Path(), "created": created}, 0, nil
	case "show":
		if len(rest) != 0 {
			return nil, 1, errors.New("usage: eightsleep config show")
		}
		config, err := store.Load()
		if err != nil {
			return nil, 1, err
		}
		payload := config.ToMap()
		payload["path"] = store.Path()
		payload["exists"] = store.Exists()
		payload["env_email_set"] = os.Getenv("EIGHTSLEEP_EMAIL") != ""
		payload["env_password_set"] = os.Getenv("EIGHTSLEEP_PASSWORD") != ""
		return payload, 0, nil
	case "set-login":
		if len(rest) < 1 || len(rest) > 2 {
			return nil, 1, errors.New("usage: eightsleep config set-login <email> [password]")
		}
		password := ""
		if len(rest) == 2 {
			password = rest[1]
		} else {
			line, err := promptPassword("Eight Sleep password: ")
			if err != nil {
				return nil, 1, err
			}
			password = line
		}
		config, err := store.Load()
		if err != nil {
			return nil, 1, err
		}
		config.Email = strings.TrimSpace(rest[0])
		config.Password = password
		if config.Email == "" || config.Password == "" {
			return nil, 1, errors.New("email and password cannot be empty")
		}
		config.clearSession()
		if err := store.Save(config); err != nil {
			return nil, 1, err
		}
		return anyMap{"email": config.Email, "password": redactedSecret, "path": store.Path()}, 0, nil
	case "clear":
		if len(rest) != 0 {
			return nil, 1, errors.New("usage: eightsleep config clear")
		}
		if err := store.Save(Config{}); err != nil {
			return nil, 1, err
		}
		return anyMap{"cleared": true, "path": store.Path()}, 0, nil
	default:
		return nil, 1, fmt.Errorf("unknown config command %q", sub)
	}
}

// promptPassword reads from the controlling terminal with echo off, so the
// password works even when stdin is not a terminal (e.g. piped or `!` commands).
func promptPassword(prompt string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", errors.New("no terminal available; pass the password as the second argument")
	}
	defer tty.Close()
	fmt.Fprint(tty, prompt)
	off := exec.Command("stty", "-echo")
	off.Stdin = tty
	_ = off.Run()
	defer func() {
		on := exec.Command("stty", "echo")
		on.Stdin = tty
		_ = on.Run()
		fmt.Fprintln(tty)
	}()
	line, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

type dateOptions struct {
	StartDate string
	EndDate   string
	Days      int
	Raw       bool
}

func parseDateArgs(args []string) (dateOptions, []string, error) {
	opts := dateOptions{Days: 7}
	var pos []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--start-date", "--end-date", "--days":
			if !hasValue {
				i++
				if i >= len(args) {
					return opts, nil, fmt.Errorf("%s requires a value", name)
				}
				value = args[i]
			}
			switch name {
			case "--start-date":
				opts.StartDate = value
			case "--end-date":
				opts.EndDate = value
			case "--days":
				days, err := strconv.Atoi(value)
				if err != nil || days <= 0 {
					return opts, nil, errors.New("--days must be a positive integer")
				}
				opts.Days = days
			}
		case "--raw":
			opts.Raw = true
		default:
			if strings.HasPrefix(arg, "--") {
				return opts, nil, fmt.Errorf("unknown flag %q", arg)
			}
			pos = append(pos, arg)
		}
	}
	return opts, pos, nil
}

func dateRange(opts dateOptions, now time.Time) (string, string, error) {
	endDate := strings.TrimSpace(opts.EndDate)
	var end time.Time
	var err error
	if endDate == "" {
		end = now.In(time.Local)
		endDate = end.Format("2006-01-02")
	} else if end, err = parseDate(endDate); err != nil {
		return "", "", err
	}
	startDate := strings.TrimSpace(opts.StartDate)
	if startDate == "" {
		startDate = end.AddDate(0, 0, -(opts.Days - 1)).Format("2006-01-02")
	} else if _, err := parseDate(startDate); err != nil {
		return "", "", err
	}
	if startDate > endDate {
		return "", "", errors.New("start date must be on or before end date")
	}
	return startDate, endDate, nil
}

func parseDate(value string) (time.Time, error) {
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Time{}, fmt.Errorf("date must use YYYY-MM-DD: %q", value)
	}
	return parsed, nil
}

func mapItems(data []any, summarize func(anyMap) anyMap) []any {
	items := make([]any, 0, len(data))
	for _, raw := range data {
		m, ok := asAnyMap(raw)
		if !ok {
			items = append(items, raw)
			continue
		}
		items = append(items, summarize(m))
	}
	return items
}

func asAnyMap(value any) (anyMap, bool) {
	switch v := value.(type) {
	case anyMap:
		return v, true
	case map[string]any:
		return anyMap(v), true
	default:
		return nil, false
	}
}

func copyFields(dst, src anyMap, fields ...string) {
	for _, field := range fields {
		copyFieldAs(dst, src, field, field)
	}
}

func copyFieldAs(dst, src anyMap, outKey, inKey string) {
	value, ok := src[inKey]
	if !ok || value == nil {
		return
	}
	dst[outKey] = value
}

func copyDurationMinutes(dst, src anyMap, outKey, inKey string) {
	seconds, ok := numberAsFloat(src[inKey])
	if !ok {
		return
	}
	dst[outKey] = int(math.Round(seconds / 60))
}

func numberAsFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	default:
		return 0, false
	}
}

func stringValue(value any) (string, bool) {
	v, ok := value.(string)
	return v, ok && v != ""
}

func round1(value float64) float64 {
	return math.Round(value*10) / 10
}

func timeOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Format(time.RFC3339)
}

func selectPayloadFields(payload any, fields []string) any {
	var out any = anyMap{}
	matched := false
	for _, field := range fields {
		var parts []string
		for _, part := range strings.Split(field, ".") {
			if part = strings.TrimSpace(part); part != "" {
				parts = append(parts, part)
			}
		}
		if len(parts) == 0 {
			continue
		}
		projected, ok := projectField(payload, parts)
		if !ok {
			continue
		}
		out = mergeProjected(out, projected)
		matched = true
	}
	if !matched {
		return anyMap{"selected": fields, "data": []any{}}
	}
	return out
}

func projectField(value any, parts []string) (any, bool) {
	if len(parts) == 0 {
		return value, true
	}
	if m, ok := asAnyMap(value); ok {
		child, exists := m[parts[0]]
		if !exists {
			return nil, false
		}
		projected, ok := projectField(child, parts[1:])
		if !ok {
			return nil, false
		}
		return anyMap{parts[0]: projected}, true
	}
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	out := make([]any, len(items))
	matched := false
	for i, item := range items {
		if projected, ok := projectField(item, parts); ok {
			out[i] = projected
			matched = true
		} else {
			out[i] = anyMap{}
		}
	}
	return out, matched
}

func mergeProjected(dst, src any) any {
	dstMap, dstIsMap := asAnyMap(dst)
	srcMap, srcIsMap := asAnyMap(src)
	if dstIsMap && srcIsMap {
		for key, srcValue := range srcMap {
			if dstValue, ok := dstMap[key]; ok {
				dstMap[key] = mergeProjected(dstValue, srcValue)
			} else {
				dstMap[key] = srcValue
			}
		}
		return dstMap
	}
	dstItems, dstIsArray := dst.([]any)
	srcItems, srcIsArray := src.([]any)
	if dstIsArray && srcIsArray {
		if len(srcItems) > len(dstItems) {
			extended := make([]any, len(srcItems))
			copy(extended, dstItems)
			dstItems = extended
		}
		for i, srcValue := range srcItems {
			if dstItems[i] == nil {
				dstItems[i] = srcValue
			} else {
				dstItems[i] = mergeProjected(dstItems[i], srcValue)
			}
		}
		return dstItems
	}
	return src
}

func printJSON(v any, pretty bool) {
	enc := json.NewEncoder(os.Stdout)
	if pretty {
		enc.SetIndent("", "  ")
	}
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

type Config struct {
	Email          string
	Password       string
	AccessToken    string
	UserID         string
	TokenEmail     string
	TokenExpiresAt time.Time
}

func (c Config) ToMap() anyMap {
	return anyMap{
		"email":            nilIfEmpty(c.Email),
		"password":         redactedOrNil(c.Password),
		"access_token":     redactedOrNil(c.AccessToken),
		"user_id":          nilIfEmpty(c.UserID),
		"token_expires_at": timeOrNil(c.TokenExpiresAt),
	}
}

func (c *Config) clearSession() {
	c.AccessToken, c.UserID, c.TokenEmail, c.TokenExpiresAt = "", "", "", time.Time{}
}

func effectiveLogin(config Config) (string, string) {
	email := strings.TrimSpace(os.Getenv("EIGHTSLEEP_EMAIL"))
	password := os.Getenv("EIGHTSLEEP_PASSWORD")
	if email == "" {
		email = config.Email
	}
	if password == "" {
		password = config.Password
	}
	return email, password
}

type ConfigStore interface {
	Path() string
	Exists() bool
	Load() (Config, error)
	Init() (bool, error)
	Save(Config) error
}

type FileConfigStore struct{ path string }

func NewConfigStore(path string) FileConfigStore { return FileConfigStore{path: path} }
func (s FileConfigStore) Path() string           { return s.path }
func (s FileConfigStore) Exists() bool           { _, err := os.Stat(s.path); return err == nil }

func (s FileConfigStore) Load() (Config, error) {
	config := Config{}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return config, nil
	}
	if err != nil {
		return config, err
	}
	for lineNo, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return config, fmt.Errorf("invalid config line %d", lineNo+1)
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if unquoted, err := strconv.Unquote(val); err == nil {
			val = unquoted
		}
		switch key {
		case "email":
			config.Email = val
		case "password":
			config.Password = val
		case "access_token":
			config.AccessToken = val
		case "user_id":
			config.UserID = val
		case "token_email":
			config.TokenEmail = val
		case "token_expires_at":
			if t, err := time.Parse(time.RFC3339, val); err == nil {
				config.TokenExpiresAt = t
			}
		default:
			return config, fmt.Errorf("unknown config key %q on line %d", key, lineNo+1)
		}
	}
	return config, nil
}

func (s FileConfigStore) Init() (bool, error) {
	if s.Exists() {
		return false, nil
	}
	return true, s.Save(Config{})
}

func (s FileConfigStore) Save(config Config) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# Defaults for the eightsleep CLI. Session fields are managed automatically.\n")
	write := func(key, value string) {
		if value != "" {
			b.WriteString(fmt.Sprintf("%s = %q\n", key, value))
		}
	}
	write("email", config.Email)
	write("password", config.Password)
	write("access_token", config.AccessToken)
	write("user_id", config.UserID)
	write("token_email", config.TokenEmail)
	if !config.TokenExpiresAt.IsZero() {
		write("token_expires_at", config.TokenExpiresAt.UTC().Format(time.RFC3339))
	}
	return os.WriteFile(s.path, []byte(b.String()), 0o600)
}

func defaultConfigPath() string {
	if path := os.Getenv("EIGHTSLEEP_CONFIG"); path != "" {
		return path
	}
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "eightsleep", "config.toml")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "eightsleep", "config.toml")
	}
	return "config.toml"
}

type Base int

const (
	BaseClient Base = iota
	BaseApp
)

type Token struct {
	AccessToken string
	UserID      string
	ExpiresIn   int
}

type API interface {
	Login(ctx context.Context, email, password string) (Token, error)
	Do(ctx context.Context, method string, base Base, path string, params map[string]string, body any, token string) (any, error)
}

type Endpoints struct {
	Auth   string
	Client string
	App    string
}

type HTTPClient struct {
	endpoints Endpoints
	http      *http.Client
}

func NewHTTPClient(endpoints Endpoints) HTTPClient {
	return HTTPClient{endpoints: endpoints, http: &http.Client{Timeout: 30 * time.Second}}
}

type APIError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e APIError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 300 {
		body = body[:300] + "..."
	}
	if body == "" {
		return fmt.Sprintf("Eight Sleep API request failed: %s", e.Status)
	}
	return fmt.Sprintf("Eight Sleep API request failed: %s: %s", e.Status, body)
}

func (c HTTPClient) Login(ctx context.Context, email, password string) (Token, error) {
	body := anyMap{
		"client_id":     appClientID,
		"client_secret": appClientSecret,
		"grant_type":    "password",
		"username":      email,
		"password":      password,
	}
	payload, err := c.request(ctx, http.MethodPost, strings.TrimRight(c.endpoints.Auth, "/")+"/tokens", nil, body, "")
	if err != nil {
		var apiErr APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusBadRequest || apiErr.StatusCode == http.StatusUnauthorized) {
			return Token{}, APIError{StatusCode: http.StatusUnauthorized, Status: "login rejected", Body: "check your Eight Sleep email and password"}
		}
		return Token{}, err
	}
	m, _ := asAnyMap(payload)
	tok := Token{}
	tok.AccessToken, _ = stringValue(m["access_token"])
	tok.UserID, _ = stringValue(m["userId"])
	if expires, ok := numberAsFloat(m["expires_in"]); ok {
		tok.ExpiresIn = int(expires)
	}
	if tok.AccessToken == "" {
		return Token{}, errors.New("Eight Sleep login response did not include an access token")
	}
	return tok, nil
}

func (c HTTPClient) Do(ctx context.Context, method string, base Base, path string, params map[string]string, body any, token string) (any, error) {
	root := c.endpoints.Client
	if base == BaseApp {
		root = c.endpoints.App
	}
	return c.request(ctx, method, strings.TrimRight(root, "/")+path, params, body, token)
}

func (c HTTPClient) request(ctx context.Context, method, rawURL string, params map[string]string, body any, token string) (any, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	for key, value := range params {
		q.Set(key, value)
	}
	u.RawQuery = q.Encode()

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, APIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: string(data)}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return anyMap{}, nil
	}
	var payload any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func updateCLI(args []string) (any, error) {
	if len(args) > 1 {
		return nil, errors.New("usage: eightsleep update [install_path]")
	}
	target, err := updateTarget(args)
	if err != nil {
		return nil, err
	}
	previousChecksum, _ := fileSHA256(target)
	source := os.Getenv("EIGHTSLEEP_CLI_UPDATE_SOURCE")
	if source == "" {
		source = defaultUpdateSource
	}
	tmp, err := os.MkdirTemp("", "eightsleep-cli-update-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	checkout := filepath.Join(tmp, "src")
	if err := runCmd("", "git", "clone", "--depth", "1", source, checkout); err != nil {
		return nil, err
	}
	if err := runCmd(filepath.Join(checkout, "cli", "eightsleep-cli"), "go", "build", "-o", target, "."); err != nil {
		return nil, err
	}
	if err := os.Chmod(target, 0o755); err != nil {
		return nil, err
	}
	newChecksum, _ := fileSHA256(target)
	return anyMap{"installed": target, "source": source, "previous_sha256": previousChecksum, "sha256": newChecksum}, nil
}

func updateTarget(args []string) (string, error) {
	if len(args) == 1 {
		return filepath.Abs(args[0])
	}
	if exe, err := os.Executable(); err == nil && exe != "" && !strings.Contains(exe, string(os.PathSeparator)+"go-build") {
		return exe, nil
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "bin", "eightsleep"), nil
	}
	return "", errors.New("could not determine install path")
}

func runCmd(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:]), nil
}

func nilIfEmpty(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func redactedOrNil(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return redactedSecret
}
