package openaiclient

import "testing"

func TestNormalizeBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://api.openai.com/v1":             "https://api.openai.com/v1/",
		"https://api.openai.com/v1/":            "https://api.openai.com/v1/",
		"http://host:8001/api":                  "http://host:8001/api/v1/",
		"http://host/v1/chat/completions":       "http://host/v1/",
		"https://gateway.example.com/custom/v1": "https://gateway.example.com/custom/v1/",
		"https://gateway.example.com/custom":    "https://gateway.example.com/custom/v1/",
	}
	for in, want := range cases {
		if got := NormalizeBaseURL(in); got != want {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}
