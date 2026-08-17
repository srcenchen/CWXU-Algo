package blogimg

import "testing"

func TestBrandingObjectKeyForHash(t *testing.T) {
	hash := "A1B2A1B2A1B2A1B2A1B2A1B2A1B2A1B2A1B2A1B2A1B2A1B2A1B2A1B2A1B2A1B2"
	got := BrandingObjectKeyForHash(12, hash, ".PNG")
	if got != "/branding/12/a1b2a1b2a1b2a1b2a1b2a1b2a1b2a1b2a1b2a1b2a1b2a1b2a1b2a1b2a1b2a1b2.png" {
		t.Fatalf("BrandingObjectKeyForHash() = %q", got)
	}
}
