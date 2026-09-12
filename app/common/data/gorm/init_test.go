package gorm

import (
	"testing"
	"time"

	"gorm.io/gorm/logger"
)

func TestParseLogLevel(t *testing.T) {
	cases := map[string]logger.LogLevel{
		"":       logger.Warn,
		"WARN":   logger.Warn,
		" warn ": logger.Warn,
		"silent": logger.Silent,
		"ERROR":  logger.Error,
		"info":   logger.Info,
		"bogus":  logger.Warn,
	}
	for raw, want := range cases {
		if got := parseLogLevel(raw); got != want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestParseSlowThreshold(t *testing.T) {
	cases := map[string]time.Duration{
		"":    defaultSlowThreshold,
		"0":   0,
		"250": 250 * time.Millisecond,
		"-1":  defaultSlowThreshold,
		"abc": defaultSlowThreshold,
	}
	for raw, want := range cases {
		if got := parseSlowThreshold(raw); got != want {
			t.Errorf("parseSlowThreshold(%q) = %v, want %v", raw, got, want)
		}
	}
}
