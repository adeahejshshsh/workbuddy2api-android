package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestConfigureThreeDCredentials(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
		auto bool
	}{
		{name: "default automatic", json: `{}`, auto: true},
		{name: "explicitly static", json: `{"features":{"three_d_auto_token":false}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfig([]byte(tc.json))
			if err != nil {
				t.Fatal(err)
			}
			up := upstream.New()
			static := configureThreeDCredentials(up, cfg)
			_, isAuto := up.ThreeDCredential.(*upstream.AutoThreeDCredentials)
			if isAuto != tc.auto {
				t.Fatalf("credential type=%T, auto=%v", up.ThreeDCredential, isAuto)
			}
			if tc.auto {
				if _, ok := up.ThreeDCredential.(*upstream.StaticThreeDCredentials); ok {
					t.Fatal("automatic configuration must not use static-only credentials")
				}
			} else if up.ThreeDCredential != static {
				t.Fatal("static configuration must preserve the hot-replaced credential table")
			}
		})
	}
}

func TestSaveConfigThreeDCredentialsAndRestart(t *testing.T) {
	for _, auto := range []bool{true, false} {
		t.Run(map[bool]string{true: "automatic", false: "static"}[auto], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			raw, err := json.Marshal(map[string]any{"features": map[string]any{
				"three_d_auto_token":  auto,
				"three_d_temp_tokens": map[string]string{"u1": "ck_t_old"},
			}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			up := upstream.New()
			configureThreeDCredentials(up, cfg)
			p := pool.New("")
			defer p.Close()
			sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
			live := livecfg.New(livecfg.Snapshot{})
			fields, err := saveConfig([]byte(`{"features":{"three_d_temp_tokens":{"u1":"ck_t_new"}}}`), path, live, p, up, sch)
			if err != nil || len(fields) != 0 {
				t.Fatalf("hot update fields=%v err=%v", fields, err)
			}
			var saved struct {
				Features struct {
					Auto   bool              `json:"three_d_auto_token"`
					Tokens map[string]string `json:"three_d_temp_tokens"`
				} `json:"features"`
			}
			content, err := os.ReadFile(path)
			if err != nil || json.Unmarshal(content, &saved) != nil {
				t.Fatalf("saved config err=%v", err)
			}
			if saved.Features.Auto != auto || saved.Features.Tokens["u1"] != "ck_t_new" {
				t.Fatalf("hot update lost configuration: %+v", saved.Features)
			}
			token, err := up.ThreeDCredential.TempToken(context.Background(), &auth.Auth{UID: "u1"})
			if err != nil || token != "ck_t_new" {
				t.Fatalf("hot credential token=%q err=%v", token, err)
			}
			fields, err = saveConfig([]byte(`{"features":{"three_d_auto_token":`+map[bool]string{true: "false", false: "true"}[auto]+`}}`), path, live, p, up, sch)
			if err != nil || !reflect.DeepEqual(fields, []string{"features.three_d_auto_token"}) {
				t.Fatalf("automatic switch fields=%v err=%v", fields, err)
			}
			if saved.Features.Tokens["u1"] != "ck_t_new" {
				t.Fatal("partial update discarded the credential table")
			}
		})
	}
}
