package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type call struct {
	Method string
	Base   Base
	Path   string
	Params map[string]string
	Body   any
	Token  string
}

type fakeAPI struct {
	logins    int
	token     string
	calls     []call
	responses map[string]any
	rejectOld string
}

func (f *fakeAPI) Login(ctx context.Context, email, password string) (Token, error) {
	if email != "me@example.com" || password != "pw" {
		return Token{}, APIError{StatusCode: http.StatusUnauthorized, Status: "401"}
	}
	f.logins++
	f.token = "token-" + string(rune('0'+f.logins))
	return Token{AccessToken: f.token, UserID: "u1", ExpiresIn: 3600}, nil
}

func (f *fakeAPI) Do(ctx context.Context, method string, base Base, path string, params map[string]string, body any, token string) (any, error) {
	f.calls = append(f.calls, call{method, base, path, params, body, token})
	if f.rejectOld != "" && token == f.rejectOld {
		return nil, APIError{StatusCode: http.StatusUnauthorized, Status: "401"}
	}
	if resp, ok := f.responses[method+" "+path]; ok {
		return resp, nil
	}
	return anyMap{}, nil
}

func newFake() *fakeAPI {
	return &fakeAPI{responses: map[string]any{
		"GET /users/me": anyMap{"user": map[string]any{
			"userId": "u1", "email": "me@example.com", "firstName": "Me", "timezone": "America/Los_Angeles",
			"currentDevice": map[string]any{"id": "dev1", "side": "left"},
			"devices":       []any{"dev1"},
		}},
		"GET /devices/dev1": anyMap{"result": map[string]any{
			"deviceId": "dev1", "online": true, "needsPriming": false, "hasWater": true,
			"leftUserId": "u1", "rightUserId": "u2", "leftHeatingLevel": json.Number("-12"), "rightHeatingLevel": json.Number("5"),
		}},
		"GET /users/u1/temperature": anyMap{"currentLevel": json.Number("-20"), "currentDeviceLevel": json.Number("-12"), "currentState": map[string]any{"type": "smart"}},
		"GET /users/u2/temperature": anyMap{"currentLevel": json.Number("10"), "currentState": map[string]any{"type": "off"}},
	}}
}

var fixedNow = time.Date(2026, 5, 6, 15, 4, 5, 0, time.UTC)

func run(t *testing.T, args []string, api API, store ConfigStore) (any, int, error) {
	t.Helper()
	payload, code, err := runCLI(args, api, store, func() time.Time { return fixedNow })
	if err == nil {
		if _, err := json.Marshal(payload); err != nil {
			t.Fatalf("payload is not JSON-serializable: %v", err)
		}
	}
	return payload, code, err
}

func testStore(t *testing.T) FileConfigStore {
	t.Helper()
	t.Setenv("EIGHTSLEEP_EMAIL", "")
	t.Setenv("EIGHTSLEEP_PASSWORD", "")
	return NewConfigStore(filepath.Join(t.TempDir(), "config.toml"))
}

func loggedInStore(t *testing.T) FileConfigStore {
	store := testStore(t)
	if err := store.Save(Config{Email: "me@example.com", Password: "pw"}); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestMissingLoginExitsWithAuthCode(t *testing.T) {
	_, code, err := run(t, []string{"status"}, newFake(), testStore(t))
	if !errors.Is(err, errMissingLogin) || code != 2 {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

func TestConfigSetLoginShowRedactsAndRoundTrips(t *testing.T) {
	store := testStore(t)
	if _, _, err := run(t, []string{"config", "set-login", "me@example.com", `p"w\x`}, newFake(), store); err != nil {
		t.Fatal(err)
	}
	config, err := store.Load()
	if err != nil || config.Password != `p"w\x` {
		t.Fatalf("password did not round-trip: %#v %v", config, err)
	}
	payload, _, err := run(t, []string{"config", "show"}, newFake(), store)
	if err != nil {
		t.Fatal(err)
	}
	if payload.(anyMap)["password"] != redactedSecret {
		t.Fatalf("password not redacted: %#v", payload)
	}
}

func TestTokenIsCachedAcrossRuns(t *testing.T) {
	store := loggedInStore(t)
	api := newFake()
	for i := 0; i < 2; i++ {
		if _, _, err := run(t, []string{"me"}, api, store); err != nil {
			t.Fatal(err)
		}
	}
	if api.logins != 1 {
		t.Fatalf("expected one login, got %d", api.logins)
	}
	config, _ := store.Load()
	if config.AccessToken != "token-1" || !config.TokenExpiresAt.Equal(fixedNow.Add(time.Hour)) {
		t.Fatalf("session not cached: %#v", config)
	}
}

func TestRejectedTokenTriggersOneRelogin(t *testing.T) {
	store := loggedInStore(t)
	api := newFake()
	if _, _, err := run(t, []string{"me"}, api, store); err != nil {
		t.Fatal(err)
	}
	api.rejectOld = "token-1"
	if _, _, err := run(t, []string{"me"}, api, store); err != nil {
		t.Fatal(err)
	}
	if api.logins != 2 || api.calls[len(api.calls)-1].Token != "token-2" {
		t.Fatalf("logins=%d calls=%#v", api.logins, api.calls)
	}
}

func TestStatusSummarizesOwnSide(t *testing.T) {
	payload, _, err := run(t, []string{"status"}, newFake(), loggedInStore(t))
	if err != nil {
		t.Fatal(err)
	}
	out := payload.(anyMap)
	if out["side"] != "left" || out["on"] != true || out["target_level"] != json.Number("-20") {
		t.Fatalf("unexpected status: %#v", out)
	}
	if out["device"].(anyMap)["heating_level"] != json.Number("-12") {
		t.Fatalf("unexpected device summary: %#v", out["device"])
	}
}

func TestSideFlagTargetsPartner(t *testing.T) {
	payload, _, err := run(t, []string{"status", "--side", "right"}, newFake(), loggedInStore(t))
	if err != nil {
		t.Fatal(err)
	}
	out := payload.(anyMap)
	if out["user_id"] != "u2" || out["on"] != false || out["device"].(anyMap)["heating_level"] != json.Number("5") {
		t.Fatalf("unexpected partner status: %#v", out)
	}
}

func TestTempValidatesAndSendsLevel(t *testing.T) {
	store := loggedInStore(t)
	if _, code, err := run(t, []string{"temp", "150"}, newFake(), store); err == nil || code != 1 {
		t.Fatalf("expected range error, code=%d err=%v", code, err)
	}
	api := newFake()
	if _, _, err := run(t, []string{"temp", "-30"}, api, store); err != nil {
		t.Fatal(err)
	}
	last := api.calls[len(api.calls)-1]
	want := anyMap{"currentLevel": -30, "currentState": anyMap{"type": "smart"}}
	if last.Method != http.MethodPut || last.Base != BaseApp || last.Path != "/users/u1/temperature" || !reflect.DeepEqual(last.Body, want) {
		t.Fatalf("unexpected call: %#v", last)
	}
}

func TestOffSendsOffState(t *testing.T) {
	api := newFake()
	if _, _, err := run(t, []string{"off"}, api, loggedInStore(t)); err != nil {
		t.Fatal(err)
	}
	last := api.calls[len(api.calls)-1]
	if !reflect.DeepEqual(last.Body, anyMap{"currentState": anyMap{"type": "off"}}) {
		t.Fatalf("unexpected body: %#v", last.Body)
	}
}

func TestSleepSummarizesTrends(t *testing.T) {
	api := newFake()
	api.responses["GET /users/u1/trends"] = anyMap{"days": []any{map[string]any{
		"day": "2026-05-05", "score": json.Number("82"), "sleepDuration": json.Number("25200"), "deepDuration": json.Number("5400"),
		"sessions":          []any{"big"},
		"sleepQualityScore": map[string]any{"total": json.Number("80"), "hrv": map[string]any{"current": json.Number("61.27")}},
	}}}
	payload, _, err := run(t, []string{"sleep", "--days", "3"}, api, loggedInStore(t))
	if err != nil {
		t.Fatal(err)
	}
	out := payload.(anyMap)
	if out["from"] != "2026-05-04" || out["to"] != "2026-05-06" {
		t.Fatalf("unexpected range: %#v", out)
	}
	night := out["data"].([]any)[0].(anyMap)
	want := anyMap{"day": "2026-05-05", "score": json.Number("82"), "total_sleep_min": 420, "deep_min": 90, "quality_score": json.Number("80"), "hrv": 61.3}
	if !reflect.DeepEqual(night, want) {
		t.Fatalf("got %#v", night)
	}
	if api.calls[len(api.calls)-1].Params["tz"] != "America/Los_Angeles" {
		t.Fatalf("expected account timezone, got %#v", api.calls[len(api.calls)-1].Params)
	}
}

func TestSelectProjectsFields(t *testing.T) {
	payload, _, err := run(t, []string{"status", "--select", "on,device.online"}, newFake(), loggedInStore(t))
	if err != nil {
		t.Fatal(err)
	}
	want := anyMap{"on": true, "device": anyMap{"online": true}}
	if !reflect.DeepEqual(payload, want) {
		t.Fatalf("got %#v", payload)
	}
}

func TestHTTPClientLoginAndRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/tokens":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["grant_type"] != "password" || body["username"] != "me@example.com" || body["client_id"] != appClientID {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"abc","expires_in":72000,"userId":"u1"}`))
		case "/app/users/u1/temperature":
			if r.Header.Get("Authorization") != "Bearer abc" || r.Method != http.MethodPut {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"currentLevel":5}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	client := NewHTTPClient(Endpoints{Auth: srv.URL + "/auth", Client: srv.URL + "/client", App: srv.URL + "/app"})

	tok, err := client.Login(context.Background(), "me@example.com", "pw")
	if err != nil || tok != (Token{AccessToken: "abc", UserID: "u1", ExpiresIn: 72000}) {
		t.Fatalf("tok=%#v err=%v", tok, err)
	}
	if _, err := client.Login(context.Background(), "bad@example.com", "pw"); err == nil {
		t.Fatal("expected login failure")
	}
	payload, err := client.Do(context.Background(), http.MethodPut, BaseApp, "/users/u1/temperature", nil, anyMap{"currentLevel": 5}, "abc")
	if err != nil || payload.(map[string]any)["currentLevel"] != json.Number("5") {
		t.Fatalf("payload=%#v err=%v", payload, err)
	}
}
