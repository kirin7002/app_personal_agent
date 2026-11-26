package integration

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kirin7002/app_personal_agent/vehicle/runs"
)

func startHermes(t *testing.T, ctx context.Context, mcpURL, mcpToken string) *runs.Client {
	t.Helper()
	python, install := os.Getenv("H07_HERMES_PYTHON"), os.Getenv("H07_HERMES_AGENT_DIR")
	if python == "" || install == "" {
		t.Fatal("set H07_HERMES_PYTHON and H07_HERMES_AGENT_DIR for the pinned Hermes runtime")
	}
	repo, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	revision := exec.CommandContext(ctx, "git", "-C", install, "rev-parse", "HEAD")
	commit, err := revision.Output()
	if err != nil || !strings.HasPrefix(string(commit), "72c88037") {
		t.Fatal("use the Hermes commit pinned in H01")
	}
	base, port := localAddress(t)
	home := t.TempDir()
	token := rand.Text()
	cfg := map[string]any{
		"model": map[string]any{"default": "deepseek-v4.1-flash-1017", "provider": "custom:vehicle-bailian", "context_length": 65536},
		"providers": map[string]any{"vehicle-bailian": map[string]any{
			"api": "https://dashscope.aliyuncs.com/apps/anthropic", "key_env": "DASHSCOPE_API_KEY",
			"transport": "anthropic_messages", "default_model": "deepseek-v4.1-flash-1017",
			"context_length": 65536, "discover_models": false,
		}},
		"auth":              map[string]any{"adopt_external_logins": false},
		"updates":           map[string]any{"check": false},
		"memory":            map[string]any{"memory_enabled": false, "user_profile_enabled": false},
		"agent":             map[string]any{"max_turns": 6},
		"tools":             map[string]any{"tool_search": map[string]any{"enabled": "off"}},
		"toolsets":          []string{"mcp-vehicle"},
		"platform_toolsets": map[string]any{"api_server": []string{"mcp-vehicle"}},
		"mcp_servers": map[string]any{"vehicle": map[string]any{
			"url": mcpURL, "headers": map[string]string{"Authorization": "Bearer " + mcpToken},
			"strict_redirect_headers": true, "timeout": 15, "connect_timeout": 10, "trust": "full",
			"sampling": map[string]any{"enabled": false},
		}},
		"gateway": map[string]any{"api_server": map[string]any{
			"enabled": true, "host": "127.0.0.1", "port": port, "max_concurrent_runs": 6,
		}},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	logs, err := os.Create(filepath.Join(home, "gateway.log"))
	if err != nil {
		t.Fatal(err)
	}
	childCtx, stop := context.WithCancel(ctx)
	child := exec.CommandContext(childCtx, python, "-m", "hermes.gateway_launcher")
	child.Dir = repo
	child.Env = append(os.Environ(),
		"HERMES_HOME="+home, "HERMES_AGENT_DIR="+install, "PYTHONPATH="+install+string(os.PathListSeparator)+repo,
		"API_SERVER_KEY="+token, "API_SERVER_ENABLED=true", "PYTHONUNBUFFERED=1", "PYTHONDONTWRITEBYTECODE=1",
	)
	child.Stdout, child.Stderr = logs, logs
	if err := child.Start(); err != nil {
		stop()
		logs.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait(); close(done) }()
	t.Cleanup(func() {
		stop()
		<-done
		logs.Close()
	})

	httpClient := &http.Client{Timeout: time.Second}
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			t.Fatalf("isolated Hermes exited before readiness: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-timer.C:
			t.Fatal("isolated Hermes health check timed out")
		case <-ticker.C:
			resp, err := httpClient.Get(base + "/health")
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					client, err := runs.New(base, token, nil)
					if err != nil {
						t.Fatal(err)
					}
					client.RetryAdmission = true
					return client
				}
			}
		}
	}
}

func localAddress(t *testing.T) (string, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return fmt.Sprintf("http://127.0.0.1:%d", port), port
}
