package main

import "testing"

func TestStatusEvents(t *testing.T) {
	for _, c := range []struct {
		args []string
		want int
	}{
		{nil, 15},
		{[]string{"--json"}, 15},
		{[]string{"--events", "50"}, 50},
		{[]string{"--json", "--events=200"}, 200},
		{[]string{"--events", "0"}, 0},
	} {
		got, err := statusEvents(c.args)
		if err != nil || got != c.want {
			t.Errorf("%v: got %d, %v, want %d", c.args, got, err, c.want)
		}
	}
	for _, args := range [][]string{{"--events"}, {"--events", "many"}, {"--events=-1"}, {"--events", "--json"}} {
		if _, err := statusEvents(args); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}
