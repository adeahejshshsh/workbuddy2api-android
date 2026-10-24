package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func TestCapacityRequiresBearerKeyAndModel(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1"}), APIKey: "secret"})

	for _, test := range []struct {
		name   string
		target string
		auth   string
		want   int
	}{
		{name: "missing key", target: "/v1/capacity?model=gpt-4", want: http.StatusUnauthorized},
		{name: "wrong key", target: "/v1/capacity?model=gpt-4", auth: "Bearer wrong", want: http.StatusUnauthorized},
		{name: "wrong scheme", target: "/v1/capacity?model=gpt-4", auth: "Basic secret", want: http.StatusUnauthorized},
		{name: "missing model", target: "/v1/capacity", auth: "Bearer secret", want: http.StatusBadRequest},
		{name: "blank model", target: "/v1/capacity?model=%20", auth: "Bearer secret", want: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, test.target, nil)
			if test.auth != "" {
				req.Header.Set("Authorization", test.auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != test.want {
				t.Fatalf("status=%d want %d body=%s", rec.Code, test.want, rec.Body)
			}
		})
	}
}

func TestCapacityRejectsUnconfiguredKey(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1"})})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/capacity?model=gpt-4", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rec.Code)
	}
}

func TestCapacityCountsHealthyModelAccountsWithoutInFlightFiltering(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "available"},
		&auth.Auth{UID: "model-off"},
		&auth.Auth{UID: "model-cooling"},
		&auth.Auth{UID: "paused"},
		&auth.Auth{UID: "disabled"},
		&auth.Auth{UID: "cooling"},
		&auth.Auth{UID: "in-flight-full"},
	)
	p.SetModelOff("model-off", "gpt-4", "")
	p.CooldownSoftForModel("model-cooling", time.Minute, time.Now().Add(time.Hour), "gpt-4", "test")
	p.Pause("paused")
	p.Disable("disabled", "test")
	p.Cooldown("cooling", pool.CoolSoft, time.Hour, "test")
	p.SetMaxInFlight(1)
	if !p.Acquire("in-flight-full") {
		t.Fatal("expected in-flight slot to be acquired")
	}
	t.Cleanup(func() { p.Release("in-flight-full") })

	h := NewHandler(Config{Pool: p, APIKey: "secret"})
	req := httptest.NewRequest(http.MethodGet, "/v1/capacity?model=gpt-4", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want %d body=%s", rec.Code, http.StatusOK, rec.Body)
	}
	var got struct {
		Model                  string `json:"model"`
		ModelAvailableAccounts int    `json:"model_available_accounts"`
		AvailableAccounts      int    `json:"available_accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Model != "gpt-4" || got.ModelAvailableAccounts != 2 || got.AvailableAccounts != 4 {
		t.Fatalf("capacity=%+v want model=gpt-4 model_available_accounts=2 available_accounts=4", got)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("capacity must not be stored in shared caches")
	}
	otherModel, all := p.CapacityForModel("other-model", "cn")
	if otherModel != 4 || all != 4 {
		t.Fatalf("other model capacity=%d pool=%d want 4/4", otherModel, all)
	}
	for _, uid := range []string{"available", "model-off", "model-cooling", "in-flight-full"} {
		p.Pause(uid)
	}
	model, all := p.CapacityForModel("gpt-4", "cn")
	if model != 0 || all != 0 {
		t.Fatalf("paused pool capacity=%d/%d want 0/0", model, all)
	}
}

func TestCapacityHonorsRealmAliasesAndHiddenModels(t *testing.T) {
	global := &auth.Auth{UID: "global-account", Domain: "www.workbuddy.ai"}
	p := testPoolWith(&auth.Auth{UID: "cn-account"}, global)
	p.SetModelOff("cn-account", "gpt-4", "")
	h := NewHandler(Config{Pool: p, APIKey: "secret", ModelsAliases: []string{"cn:alias=global:gpt-4"}, ModelsHidden: []string{"cn:hidden"}})
	for _, tc := range []struct {
		model string
		want  int
	}{
		{"cn:gpt-4", 0},
		{"global:gpt-4", 1},
		{"alias", 1},
		{"cn:hidden", 0},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/capacity?model="+tc.model, nil)
		req.Header.Set("Authorization", "Bearer secret")
		h.ServeHTTP(rec, req)
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if rec.Code != 200 || got["model_available_accounts"] != float64(tc.want) || got["available_accounts"] != float64(2) {
			t.Fatalf("model=%s status=%d body=%s", tc.model, rec.Code, rec.Body)
		}
	}
}
