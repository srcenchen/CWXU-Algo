package platform

import (
	"context"
	"testing"
)

func TestLogin(t *testing.T) {
	gu := NewQOJ{}
	t.Log(gu.FetchSubmitLog(context.Background(), 1, "sanenchen", true))
}
