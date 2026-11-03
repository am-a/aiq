package main

import (
	"reflect"
	"testing"
)

func TestLoginModeFlags(t *testing.T) {
	rest, mode, err := loginModeFlags([]string{"codex", "work", "--device-auth", "--no-login"})
	if err != nil || mode == nil || !*mode || !reflect.DeepEqual(rest, []string{"codex", "work", "--no-login"}) {
		t.Fatalf("device-auth: rest=%v mode=%v err=%v", rest, mode, err)
	}
	rest, mode, err = loginModeFlags([]string{"codex/work", "--browser"})
	if err != nil || mode == nil || *mode || !reflect.DeepEqual(rest, []string{"codex/work"}) {
		t.Fatalf("browser: rest=%v mode=%v err=%v", rest, mode, err)
	}
	if _, mode, err = loginModeFlags([]string{"codex/work"}); err != nil || mode != nil {
		t.Fatalf("no flag: mode=%v err=%v", mode, err)
	}
	if _, _, err = loginModeFlags([]string{"codex/work", "--browser", "--device-auth"}); err == nil {
		t.Fatal("conflicting flags accepted")
	}
}

func TestCheckLoginFlags(t *testing.T) {
	on, off := true, false
	for _, c := range []struct {
		name string
		sub  string
		rest []string
		mode *bool
		ok   bool
	}{
		{"codex add with a mode", "add", []string{"codex", "work"}, &on, true},
		{"codex login with a mode", "login", []string{"codex/work"}, &off, true},
		{"no mode for any provider", "login", []string{"claude/work"}, nil, true},
		{"no-login with no mode", "add", []string{"claude", "work", "--no-login"}, nil, true},
		{"mode on a claude login", "login", []string{"claude/work"}, &on, false},
		{"mode on an agy add", "add", []string{"agy", "work"}, &off, false},
		{"mode and no-login", "add", []string{"codex", "work", "--no-login"}, &on, false},
		{"misspelled flag on add", "add", []string{"codex", "work", "--device-code"}, nil, false},
		{"misspelled flag on login", "login", []string{"codex/work", "--borwser"}, nil, false},
		{"no-login on login", "login", []string{"codex/work", "--no-login"}, nil, false},
		{"too few arguments stay a usage error", "login", nil, &on, true},
	} {
		if err := checkLoginFlags(c.sub, c.rest, c.mode); (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok = %v", c.name, err, c.ok)
		}
	}
}
