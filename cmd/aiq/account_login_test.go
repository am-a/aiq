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
