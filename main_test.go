package main

import "testing"

func TestCommandVersion(t *testing.T) {
	t.Parallel()

	const want = "git-0123456789ab"
	if got := newCommand(want).Version; got != want {
		t.Errorf("command version = %q, want %q", got, want)
	}
}
