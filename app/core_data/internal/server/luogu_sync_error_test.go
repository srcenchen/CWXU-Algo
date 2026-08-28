package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	kratoserrors "github.com/go-kratos/kratos/v2/errors"
)

func TestLuoguSyncErrorEncoderUsesStableJSONCodeAndCooldownFields(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/core/spider/luogu-sync/start", nil)
	rec := httptest.NewRecorder()
	err := kratoserrors.New(http.StatusTooManyRequests, "SYNC_COOLDOWN", "cooldown").WithMetadata(map[string]string{
		"nextAvailableAt": "1787890000", "retryAfterSeconds": "300",
	})
	luoguSyncErrorEncoder(rec, req, err)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "SYNC_COOLDOWN" || body["nextAvailableAt"] != float64(1787890000) || body["retryAfterSeconds"] != float64(300) {
		t.Fatalf("body=%v", body)
	}
}

func TestLuoguSyncOperationsBypassOnlySiteJWT(t *testing.T) {
	matcher := NewWhiteListMatcher()
	for _, operation := range []string{
		"/api.core.v1.spider.Spider/StartLuoguSync",
		"/api.core.v1.spider.Spider/LuoguSyncStatus",
		"/api.core.v1.spider.Spider/UploadLuoguSyncPage",
	} {
		if matcher(nil, operation) {
			t.Fatalf("operation %s still requires site JWT", operation)
		}
	}
	if !matcher(nil, "/api.core.v1.spider.Spider/UpdateAll") {
		t.Fatal("unrelated spider operation was made public")
	}
}
