package main

import "testing"

func TestCommandVersion(t *testing.T) {
	t.Parallel()

	const want = "git-0123456789ab"
	if got := newCommand(want).Version; got != want {
		t.Errorf("command version = %q, want %q", got, want)
	}
}

func TestStartupLogMessageIncludesVersion(t *testing.T) {
	t.Parallel()

	const want = "Starting nix-store-gcs-proxy version git-0123456789ab " +
		"on address 127.0.0.1:3000 for bucket cache"
	got := startupLogMessage(
		"git-0123456789ab",
		"127.0.0.1:3000",
		"cache",
	)
	if got != want {
		t.Errorf("startup log message = %q, want %q", got, want)
	}
}
