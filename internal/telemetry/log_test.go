// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package telemetry_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/core/auth"
	"github.com/divbast/pisati/internal/telemetry"
)

func TestLoggerJSONAndRedaction(t *testing.T) {
	t.Parallel()

	const secretText, passwordText = "quokka-secret-value", "quokka-password-value"
	secret, err := config.NewSecret([]byte(secretText))
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	password, err := auth.ParsePassword(passwordText)
	if err != nil {
		t.Fatalf("ParsePassword: %v", err)
	}
	logSecrets := func(log *slog.Logger) {
		log.Info("signed in", "secret", secret, "password", password,
			slog.Group("nested", "secret", secret, "password", password))
	}

	t.Run("json", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		logSecrets(telemetry.NewLogger(&buf, config.Log{Level: slog.LevelInfo, Format: config.LogJSON}))

		type pair struct {
			Secret   string `json:"secret"`
			Password string `json:"password"`
		}
		var rec struct {
			pair

			Level  string `json:"level"`
			Msg    string `json:"msg"`
			Nested pair   `json:"nested"`
		}
		if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
			t.Fatalf("the record %q is not one JSON object: %v", buf.String(), err)
		}
		if rec.Level != "INFO" || rec.Msg != "signed in" {
			t.Errorf("level %q, msg %q; want INFO, signed in", rec.Level, rec.Msg)
		}
		for name, got := range map[string]string{
			"secret": rec.Secret, "password": rec.Password,
			"nested.secret": rec.Nested.Secret, "nested.password": rec.Nested.Password,
		} {
			if got != "[redacted]" {
				t.Errorf("%s logged as %q, want [redacted]", name, got)
			}
		}
		assertNoSecret(t, buf.String(), secretText, passwordText)
	})

	t.Run("text", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		logSecrets(telemetry.NewLogger(&buf, config.Log{Level: slog.LevelInfo, Format: config.LogText}))

		line := buf.String()
		if strings.HasPrefix(strings.TrimSpace(line), "{") {
			t.Errorf("the text format wrote JSON: %q", line)
		}
		for _, want := range []string{
			"level=INFO", `msg="signed in"`, "secret=[redacted]", "password=[redacted]",
			"nested.secret=[redacted]", "nested.password=[redacted]",
		} {
			if !strings.Contains(line, want) {
				t.Errorf("the record %q has no %s", line, want)
			}
		}
		assertNoSecret(t, line, secretText, passwordText)
	})
}

func TestLoggerLevel(t *testing.T) {
	t.Parallel()

	for _, format := range []config.LogFormat{config.LogJSON, config.LogText} {
		t.Run(format.String(), func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			log := telemetry.NewLogger(&buf, config.Log{Level: slog.LevelWarn, Format: format})
			log.Info("below the level")
			log.Warn("at the level")
			out := buf.String()
			if strings.Contains(out, "below the level") {
				t.Errorf("an info record was written at level warn: %q", out)
			}
			if !strings.Contains(out, "at the level") {
				t.Errorf("the warn record is missing: %q", out)
			}
		})
	}
}

// assertNoSecret fails the test if out holds any of secrets.
func assertNoSecret(t *testing.T, out string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("the log shows a secret: %q", out)
		}
	}
}
