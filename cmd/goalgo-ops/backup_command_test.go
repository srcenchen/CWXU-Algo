package main

import (
	"os"
	"strings"
	"testing"
)

func TestBackupCommandDoesNotOfferDownloadOrUpyunCredentials(t *testing.T) {
	source, err := os.ReadFile("backup.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, forbidden := range []string{"backup download", "UPYUN_BUCKET", "UPYUN_OPERATOR", "UPYUN_PASSWORD", `"prefix"`} {
		if strings.Contains(text, forbidden) {
			t.Errorf("backup command still contains %q", forbidden)
		}
	}
}
