package bili

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewClient(Config{BaseURL: server.URL, HTTPClient: server.Client(), Gate: NewRequestGate(GateConfig{Disabled: true}), RetryMax: 0, Now: func() time.Time { return time.Unix(1702204169, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func writeJSON(w http.ResponseWriter, payload string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(payload))
}

func TestWBIKnownVectorAndEncoding(t *testing.T) {
	keys := WBIKeys{ImgKey: "7cd084941338484aae1ad9425b84077c", SubKey: "4932caff0ff746eab6f01bf08b70ac45"}
	params := url.Values{"foo": {"114"}, "bar": {"514"}, "baz": {"1919810"}}
	signed := SignWBI(params, keys, time.Unix(1702204169, 0))
	if got, want := signed.Get("w_rid"), "6149fdadf571698ca7e6a567265cd0ee"; got != want {
		t.Fatalf("w_rid=%s, want %s", got, want)
	}
	if got, want := EncodeWBI(url.Values{"z": {"a b!()*"}, "a": {"2"}}), "a=2&z=a%20b%21%28%29%2A"; got != want {
		t.Fatalf("encode=%q, want %q", got, want)
	}
	filtered := SignWBI(url.Values{"x": {"a!b(c)*'"}}, keys, time.Unix(1702204169, 0))
	if got, want := filtered.Get("x"), "abc"; got != want {
		t.Fatalf("filtered=%q, want %q", got, want)
	}
}

func TestAPIErrorMappingAnd412Cooldown(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/x/web-interface/view" {
			writeJSON(w, `{"code":-101,"message":"not login"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	err := client.getJSON(context.Background(), "/x/web-interface/view", nil, false, nil)
	if !errors.Is(err, ErrAuthentication) {
		t.Fatalf("error %v does not map to authentication", err)
	}

	var calls atomic.Int32
	client = testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusPreconditionFailed)
	}))
	err = client.getJSON(context.Background(), "/x/web-interface/view", nil, false, nil)
	var cooldown *CooldownError
	if !errors.As(err, &cooldown) || !errors.Is(err, ErrRateLimited) || cooldown.RetryAfter != 30*time.Minute {
		t.Fatalf("unexpected cooldown: %#v (%v)", cooldown, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("412 retried %d times", calls.Load())
	}
}

func TestRetryAfter(t *testing.T) {
	if got := retryAfter(http.Header{"Retry-After": []string{"7"}}, time.Unix(0, 0)); got != 7*time.Second {
		t.Fatalf("retry-after=%s", got)
	}
}

func TestWBIKeysRefreshAfterSixHours(t *testing.T) {
	var navCalls atomic.Int32
	now := time.Unix(1702204169, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/x/web-interface/nav" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		navCalls.Add(1)
		writeJSON(w, `{"code":0,"data":{"wbi_img":{"img_url":"https://i.test/7cd084941338484aae1ad9425b84077c.png","sub_url":"https://i.test/4932caff0ff746eab6f01bf08b70ac45.png"}}}`)
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, HTTPClient: server.Client(), Gate: NewRequestGate(GateConfig{Disabled: true}), RetryMax: 0, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.wbiKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.wbiKeys(context.Background()); err != nil || navCalls.Load() != 1 {
		t.Fatalf("fresh key calls=%d err=%v", navCalls.Load(), err)
	}
	now = now.Add(wbiKeyTTL + time.Second)
	if _, err := client.wbiKeys(context.Background()); err != nil || navCalls.Load() != 2 {
		t.Fatalf("expired key calls=%d err=%v", navCalls.Load(), err)
	}
}

func TestGateCanCancel(t *testing.T) {
	gate := NewRequestGate(GateConfig{MinDelay: time.Second, MaxDelay: time.Second, EveryRequests: 999, FirstImmediate: false})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := gate.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error=%v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := gate.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error=%v", err)
	}
}

func TestQRUsesPassportHost(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("QR request incorrectly sent to API host: %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer api.Close()
	var passportCalls atomic.Int32
	passport := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		passportCalls.Add(1)
		switch r.URL.Path {
		case "/x/passport-login/web/qrcode/generate":
			writeJSON(w, `{"code":0,"data":{"url":"https://qr.test/login","qrcode_key":"key"}}`)
		case "/x/passport-login/web/qrcode/poll":
			if got := r.URL.Query().Get("qrcode_key"); got != "key" {
				t.Errorf("key=%q", got)
			}
			writeJSON(w, `{"code":0,"data":{"code":86101,"message":"not scanned"}}`)
		default:
			t.Errorf("unexpected passport path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer passport.Close()
	client, err := NewClient(Config{BaseURL: api.URL, PassportURL: passport.URL, Gate: NewRequestGate(GateConfig{Disabled: true}), RetryMax: 1})
	if err != nil {
		t.Fatal(err)
	}
	login, err := client.GenerateQRLogin(context.Background())
	if err != nil || login.Key != "key" {
		t.Fatalf("login=%#v err=%v", login, err)
	}
	status, err := client.PollQRLogin(context.Background(), login.Key)
	if err != nil || status.Code != 86101 {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	if passportCalls.Load() != 2 {
		t.Fatalf("passport calls=%d", passportCalls.Load())
	}
}
