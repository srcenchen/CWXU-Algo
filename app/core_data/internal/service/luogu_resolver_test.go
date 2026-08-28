package service

import "testing"

func TestParseLuoguSearchBody(t *testing.T) {
	user, err := parseLuoguSearchBody([]byte(`{"users":[{"uid":2245873,"name":"srcenchen"}]}`), "srcenchen")
	if err != nil || user.UID != "2245873" || user.Username != "srcenchen" {
		t.Fatalf("unexpected user=%+v err=%v", user, err)
	}
}

func TestParseLuoguSearchBodyPrefersExactName(t *testing.T) {
	body := []byte(`{"users":[{"uid":1,"name":"srcenchen_old"},{"uid":2245873,"name":"srcenchen"}]}`)
	user, err := parseLuoguSearchBody(body, "srcenchen")
	if err != nil || user.UID != "2245873" {
		t.Fatalf("unexpected user=%+v err=%v", user, err)
	}
}

func TestParseLuoguSearchBodyRejectsEmptyResults(t *testing.T) {
	if _, err := parseLuoguSearchBody([]byte(`{"users":[]}`), "missing"); err == nil {
		t.Fatal("expected not found")
	}
}
