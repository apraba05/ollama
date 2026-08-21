package main

import (
	"encoding/json"
	"testing"
)

func TestCacheKeyStable(t *testing.T) {
	a := cacheKey(GenerateRequest{Model: "m", Prompt: "hello"})
	b := cacheKey(GenerateRequest{Model: "m", Prompt: "hello"})
	c := cacheKey(GenerateRequest{Model: "m", Prompt: "hello!"})
	if a != b {
		t.Fatalf("same prompt should share key")
	}
	if a == c {
		t.Fatalf("different prompt should differ")
	}
	if len(a) != 4+64 { // "gen:" + sha256 hex
		t.Fatalf("unexpected key length: %d (%s)", len(a), a)
	}
}

func TestGenerateResponseShape(t *testing.T) {
	raw, err := json.Marshal(GenerateResponse{
		Model:     "llama3.2:1b",
		CreatedAt: "2026-01-01T00:00:00Z",
		Response:  "hi",
		Done:      true,
		Source:    "ollama",
	})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"model", "created_at", "response", "done", "source"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing field %q", k)
		}
	}
}
