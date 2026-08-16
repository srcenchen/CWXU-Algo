package opsrelease

import (
	"strings"
	"testing"
)

const validImage = imagePrefix + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func validRelease() string {
	return "FRONTEND_IMAGE=" + validImage + "\n" +
		"GATEWAY_IMAGE=" + validImage + "\n" +
		"USER_IMAGE=" + validImage + "\n" +
		"CORE_DATA_IMAGE=" + validImage + "\n" +
		"AGENT_IMAGE=" + validImage + "\n"
}

func TestParseValidRelease(t *testing.T) {
	release, err := Parse(strings.NewReader(validRelease()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(release.Images) != 5 {
		t.Fatalf("expected 5 images, got %d", len(release.Images))
	}
}

func TestParseMissingRequired(t *testing.T) {
	if _, err := Parse(strings.NewReader("FRONTEND_IMAGE=" + validImage + "\n")); err == nil {
		t.Fatal("expected error for missing assignments")
	}
}

func TestParseBadDigest(t *testing.T) {
	bad := "FRONTEND_IMAGE=registry.cn-hangzhou.aliyuncs.com/sanenchen/goalgo:latest\n" +
		"GATEWAY_IMAGE=" + validImage + "\nUSER_IMAGE=" + validImage + "\n" +
		"CORE_DATA_IMAGE=" + validImage + "\nAGENT_IMAGE=" + validImage + "\n"
	if _, err := Parse(strings.NewReader(bad)); err == nil {
		t.Fatal("expected error for mutable tag")
	}
}

func TestParseDuplicate(t *testing.T) {
	release := "FRONTEND_IMAGE=" + validImage + "\nFRONTEND_IMAGE=" + validImage + "\n" +
		"GATEWAY_IMAGE=" + validImage + "\nUSER_IMAGE=" + validImage + "\n" +
		"CORE_DATA_IMAGE=" + validImage + "\nAGENT_IMAGE=" + validImage + "\n"
	if _, err := Parse(strings.NewReader(release)); err == nil {
		t.Fatal("expected error for duplicate assignment")
	}
}

func TestParseExtraAssignment(t *testing.T) {
	release := validRelease() + "EXTRA=value\n"
	if _, err := Parse(strings.NewReader(release)); err == nil {
		t.Fatal("expected error for extra assignment")
	}
}
