// Ollama-compatible /api/generate proxy:
//   1) local Ollama  →  2) AWS Bedrock  →  3) Redis cache (offline)
//
// Redis: gen:{sha256(model+"\n"+prompt)} → JSON GenerateResponse
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/redis/go-redis/v9"
)

const (
	defaultListen       = ":11435"
	defaultOllamaURL    = "http://127.0.0.1:11434"
	defaultModel        = "llama3.2:1b"
	defaultBedrockModel = "anthropic.claude-3-haiku-20240307-v1:0"
)

// GenerateRequest mirrors Ollama POST /api/generate (non-streaming).
type GenerateRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream *bool  `json:"stream"`
	System string `json:"system,omitempty"`
}

// GenerateResponse mirrors Ollama's reply + Source for the demo tiers.
type GenerateResponse struct {
	Model     string `json:"model"`
	CreatedAt string `json:"created_at"`
	Response  string `json:"response"`
	Done      bool   `json:"done"`
	Source    string `json:"source"` // ollama | bedrock | offline
}

type server struct {
	rdb          *redis.Client
	br           *bedrockruntime.Client
	httpClient   *http.Client
	ollamaURL    string
	bedrockModel string
	defaultModel string
}

func main() {
	listen := env("LISTEN", defaultListen)
	redisAddr := env("REDIS_ADDR", "127.0.0.1:26379")
	ollamaURL := strings.TrimRight(env("OLLAMA_URL", defaultOllamaURL), "/")
	bedrockModel := env("BEDROCK_MODEL", defaultBedrockModel)
	region := env("AWS_REGION", env("AWS_DEFAULT_REGION", "us-east-1"))

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("redis %s: %v", redisAddr, err)
	}
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(region))
	if err != nil {
		log.Fatalf("aws config: %v", err)
	}

	s := &server{
		rdb:          rdb,
		br:           bedrockruntime.NewFromConfig(cfg),
		httpClient:   &http.Client{Timeout: 45 * time.Second},
		ollamaURL:    ollamaURL,
		bedrockModel: bedrockModel,
		defaultModel: env("OLLAMA_MODEL", defaultModel),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"ok": "true"})
	})
	mux.HandleFunc("/api/generate", s.handleGenerate)
	log.Printf("proxy %s  ollama=%s  redis=%s  bedrock=%s", listen, ollamaURL, redisAddr, bedrockModel)
	log.Fatal(http.ListenAndServe(listen, mux))
}

func (s *server) handleGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req GenerateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		http.Error(w, "prompt required", http.StatusBadRequest)
		return
	}
	if req.Model == "" {
		req.Model = s.defaultModel
	}
	if req.Stream != nil && *req.Stream {
		http.Error(w, "stream=true not supported; set stream=false", http.StatusBadRequest)
		return
	}

	if resp, err := s.fromOllama(r.Context(), req); err == nil {
		s.cachePut(r.Context(), req, resp)
		writeJSON(w, resp)
		return
	} else {
		log.Printf("ollama miss: %v", err)
	}

	if resp, err := s.fromBedrock(r.Context(), req); err == nil {
		s.cachePut(r.Context(), req, resp)
		writeJSON(w, resp)
		return
	} else {
		log.Printf("bedrock miss: %v", err)
	}

	if resp, ok := s.cacheGet(r.Context(), req); ok {
		resp.Source = "offline"
		resp.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		writeJSON(w, resp)
		return
	}
	http.Error(w, "ollama and bedrock unavailable; no cached response", http.StatusBadGateway)
}

func (s *server) fromOllama(ctx context.Context, req GenerateRequest) (GenerateResponse, error) {
	stream := false
	body, _ := json.Marshal(GenerateRequest{Model: req.Model, Prompt: req.Prompt, Stream: &stream, System: req.System})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.ollamaURL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return GenerateResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	out, err := s.httpClient.Do(httpReq)
	if err != nil {
		return GenerateResponse{}, err
	}
	defer out.Body.Close()
	raw, _ := io.ReadAll(out.Body)
	if out.StatusCode != http.StatusOK {
		return GenerateResponse{}, fmt.Errorf("status %d: %s", out.StatusCode, truncate(string(raw), 160))
	}
	var resp GenerateResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return GenerateResponse{}, err
	}
	resp.Source, resp.Done = "ollama", true
	if resp.Model == "" {
		resp.Model = req.Model
	}
	return resp, nil
}

func (s *server) fromBedrock(ctx context.Context, req GenerateRequest) (GenerateResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	system := req.System
	if system == "" {
		system = "Reply concisely."
	}
	body, err := json.Marshal(map[string]any{
		"anthropic_version": "bedrock-2023-05-31",
		"max_tokens":        256,
		"temperature":       0.2,
		"system":            system,
		"messages":          []map[string]string{{"role": "user", "content": req.Prompt}},
	})
	if err != nil {
		return GenerateResponse{}, err
	}
	out, err := s.br.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(s.bedrockModel),
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
		Body:        body,
	})
	if err != nil {
		return GenerateResponse{}, err
	}
	var br struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out.Body, &br); err != nil {
		return GenerateResponse{}, err
	}
	if len(br.Content) == 0 || strings.TrimSpace(br.Content[0].Text) == "" {
		return GenerateResponse{}, fmt.Errorf("empty bedrock response")
	}
	return GenerateResponse{
		Model:     s.bedrockModel,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Response:  strings.TrimSpace(br.Content[0].Text),
		Done:      true,
		Source:    "bedrock",
	}, nil
}

func cacheKey(req GenerateRequest) string {
	sum := sha256.Sum256([]byte(req.Model + "\n" + req.Prompt))
	return "gen:" + hex.EncodeToString(sum[:])
}

func (s *server) cachePut(ctx context.Context, req GenerateRequest, resp GenerateResponse) {
	raw, err := json.Marshal(resp)
	if err != nil {
		return
	}
	if err := s.rdb.Set(ctx, cacheKey(req), raw, 0).Err(); err != nil {
		log.Printf("redis set: %v", err)
	}
}

func (s *server) cacheGet(ctx context.Context, req GenerateRequest) (GenerateResponse, bool) {
	raw, err := s.rdb.Get(ctx, cacheKey(req)).Bytes()
	if err != nil {
		return GenerateResponse{}, false
	}
	var resp GenerateResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return GenerateResponse{}, false
	}
	return resp, true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
