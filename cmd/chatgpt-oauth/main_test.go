package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trapped/chatgpt-oauth/internal/proxy"
)

func TestParseBodyLimit(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int64
	}{{"", proxy.DefaultMaxBodyBytes}, {"104857600", 100 << 20}, {"1024", 1024}} {
		got, err := parseBodyLimit(test.value)
		if err != nil || got != test.want {
			t.Errorf("parseBodyLimit(%q) = %d, %v; want %d", test.value, got, err, test.want)
		}
	}
	for _, value := range []string{"0", "-1", "64MiB", "1.5", "1e6", " 1024", "1024 ", "9223372036854775808"} {
		if _, err := parseBodyLimit(value); err == nil {
			t.Errorf("invalid byte limit %q accepted", value)
		}
	}
}

func TestInvalidBodyConfigurationFailsBeforeStateAccess(t *testing.T) {
	for _, environment := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "not-created", "auth.json")
		args := []string{"chatgpt-oauth", "serve", "--auth-file", path}
		t.Setenv("CHATGPT_OAUTH_MAX_BODY_BYTES", "")
		if environment {
			t.Setenv("CHATGPT_OAUTH_MAX_BODY_BYTES", "0")
		} else {
			args = append(args, "--max-body-bytes", "-1")
		}
		original := os.Args
		os.Args = args
		err := run()
		os.Args = original
		if err == nil || !strings.Contains(err.Error(), "positive decimal byte count") {
			t.Fatalf("invalid size configuration did not fail: %v", err)
		}
		if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
			t.Error("invalid configuration accessed credential state")
		}
	}
}

func TestBodyLimitFlagOverridesEnvironment(t *testing.T) {
	t.Setenv("CHATGPT_OAUTH_MAX_BODY_BYTES", "invalid")
	path := filepath.Join(t.TempDir(), "auth.json")
	original := os.Args
	os.Args = []string{"chatgpt-oauth", "serve", "--auth-file", path, "--max-body-bytes", "104857600"}
	defer func() { os.Args = original }()
	// A valid flag reaches the existing model validation, rather than failing
	// on the overridden environment value or starting a listener.
	if err := run(); err == nil || err.Error() != "at least one model is required" {
		t.Fatalf("CLI did not override environment limit: %v", err)
	}
}
