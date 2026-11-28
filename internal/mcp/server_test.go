package jtaomcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/JacobZyy/jtao/internal/memory"
	"github.com/JacobZyy/jtao/internal/runner"
	"github.com/JacobZyy/jtao/internal/workflow"
)

func TestMCPServerToolDiscoveryAndRoundTrip(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()

	mem := memory.NewService()
	defer mem.Close()
	wf := workflow.NewService()
	defer wf.Close()

	srv := NewServer(Config{
		DefaultWorkspace: ws,
		Memory:           mem,
		Workflow:         wf,
	})

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect failed: %v", err)
	}
	defer serverSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect failed: %v", err)
	}
	defer clientSession.Close()

	// 1. Verify all 18 memory_* + workflow_* + scheduler_* tools are advertised.
	toolsRes, err := clientSession.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	if len(toolsRes.Tools) != 18 {
		t.Fatalf("expected 18 MCP tools, got %d", len(toolsRes.Tools))
	}

	// 2. Call memory_write then memory_recall and verify eventId is returned (fixes AIOS MCP gap).
	writeRes, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "memory_write",
		Arguments: map[string]any{
			"text":        "MCP 回环测试：记住订单状态机严格单向流转",
			"work_item_id": "wi-mcp-test",
			"entities":    []string{"order-fsm"},
		},
	})
	if err != nil || writeRes.IsError {
		t.Fatalf("memory_write failed: err=%v res=%+v", err, writeRes)
	}

	recallRes, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "memory_recall",
		Arguments: map[string]any{
			"query": "订单状态机",
		},
	})
	if err != nil || recallRes.IsError {
		t.Fatalf("memory_recall failed: err=%v res=%+v", err, recallRes)
	}
	rawJSON, _ := json.Marshal(recallRes.StructuredContent)
	var parsed recallOut
	if err := json.Unmarshal(rawJSON, &parsed); err != nil || len(parsed.Hits) != 1 || parsed.Hits[0].ID == "" {
		t.Fatalf("expected recall hit with real ID, got raw=%s parsed=%+v", string(rawJSON), parsed)
	}

	// 3. Call workflow_work_create -> workflow_node_claim -> workflow_receipt_run -> workflow_node_submit.
	createRes, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "workflow_work_create",
		Arguments: map[string]any{
			"id":         "mcp-e2e",
			"title":      "MCP E2E Delivery",
			"set_active": true,
			"nodes": []map[string]any{
				{
					"id":    "n1",
					"title": "Verify echo",
					"verify_spec": map[string]any{
						"command":          "sh",
						"args":             []string{"-c", "echo pass"},
						"expectedExitCode": 0,
					},
				},
			},
		},
	})
	if err != nil || createRes.IsError {
		t.Fatalf("workflow_work_create failed: err=%v res=%+v", err, createRes)
	}

	claimRes, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "workflow_node_claim",
		Arguments: map[string]any{"work_item_id": "mcp-e2e"},
	})
	if err != nil || claimRes.IsError {
		t.Fatalf("workflow_node_claim failed: err=%v res=%+v", err, claimRes)
	}
	claimBytes, _ := json.Marshal(claimRes.StructuredContent)
	var claimed workflow.ClaimedCommand
	_ = json.Unmarshal(claimBytes, &claimed)
	if claimed.ExecutionToken == "" {
		t.Fatalf("expected execution token, got %s", string(claimBytes))
	}

	recRes, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "workflow_receipt_run",
		Arguments: map[string]any{
			"work_item_id": "mcp-e2e",
			"node_id":      "n1",
			"command":      "sh",
			"args":         []string{"-c", "echo pass"},
		},
	})
	if err != nil || recRes.IsError {
		t.Fatalf("workflow_receipt_run failed: err=%v res=%+v", err, recRes)
	}
	recBytes, _ := json.Marshal(recRes.StructuredContent)
	var recOut struct {
		ReceiptID string `json:"receiptId"`
	}
	_ = json.Unmarshal(recBytes, &recOut)
	if recOut.ReceiptID == "" {
		t.Fatalf("expected real receiptId, got %s", string(recBytes))
	}

	subRes, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "workflow_node_submit",
		Arguments: map[string]any{
			"work_item_id":    "mcp-e2e",
			"node_id":         "n1",
			"execution_token": claimed.ExecutionToken,
			"status":          "pass",
			"summary":         "verified via real MCP receipt",
			"receipt_id":      recOut.ReceiptID,
		},
	})
	if err != nil || subRes.IsError {
		t.Fatalf("workflow_node_submit failed: err=%v res=%+v", err, subRes)
	}
	subBytes, _ := json.Marshal(subRes.StructuredContent)
	var adv workflow.AdvanceNodeResult
	_ = json.Unmarshal(subBytes, &adv)
	if adv.Decision != workflow.DecisionCompleted {
		t.Fatalf("expected completed, got %+v", adv)
	}

	// 4. Call scheduler_status when no runs exist -> returns found=false cleanly without error.
	stRes, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "scheduler_status",
		Arguments: map[string]any{},
	})
	if err != nil || stRes.IsError {
		t.Fatalf("scheduler_status failed: err=%v res=%+v", err, stRes)
	}
	stBytes, _ := json.Marshal(stRes.StructuredContent)
	var stOut schedulerStatusOut
	if err := json.Unmarshal(stBytes, &stOut); err != nil || stOut.Found {
		t.Fatalf("expected found=false when no runs exist, got %s", string(stBytes))
	}
}

func TestResolveWSRejectsPluginCacheFallback(t *testing.T) {
	if _, err := resolveWS("", "/Users/demo/.codex/plugins/cache/jtao/jtao/0.2.0"); err == nil {
		t.Fatalf("expected error when fallback cwd is inside ~/.codex/plugins/ cache")
	}
	if _, err := resolveWS("", "/Users/demo/.gemini/config/plugins/jtao"); err == nil {
		t.Fatalf("expected error when fallback cwd is inside ~/.gemini/config/plugins/")
	}
	ws, err := resolveWS("/tmp/my-project", "/Users/demo/.codex/plugins/cache/jtao/jtao/0.2.0")
	if err != nil || ws != "/tmp/my-project" {
		t.Fatalf("expected explicit workspace_root to override plugin cache fallback, got ws=%q err=%v", ws, err)
	}
}

func TestMCPWorkflowReceiptRunFallbackPATHAndNotFoundError(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	fakeHome := t.TempDir()
	cargoDir := filepath.Join(fakeHome, ".cargo", "bin")
	if err := os.MkdirAll(cargoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cargoDir, "cargo"), []byte("#!/bin/sh\necho mcp-cargo-ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)
	t.Setenv("PATH", "/usr/bin:/bin")

	mem := memory.NewService()
	defer mem.Close()
	wf := workflow.NewService()
	defer wf.Close()

	srv := NewServer(Config{DefaultWorkspace: ws, Memory: mem, Workflow: wf})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	// 1. Minimal PATH finds cargo under simulated $HOME/.cargo/bin and returns exitCode=0.
	okRes, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "workflow_receipt_run",
		Arguments: map[string]any{
			"command": "cargo",
			"args":    []string{"--version"},
		},
	})
	if err != nil || okRes.IsError {
		t.Fatalf("workflow_receipt_run cargo failed: err=%v res=%+v", err, okRes)
	}
	okBytes, _ := json.Marshal(okRes.StructuredContent)
	var okOut runner.ReceiptExecOutput
	if err := json.Unmarshal(okBytes, &okOut); err != nil {
		t.Fatalf("unmarshal okOut: %v", err)
	}
	if okOut.Record.ExitCode != 0 || okOut.Record.ExecError != "" || !strings.Contains(okOut.Record.OutputExcerpt, "mcp-cargo-ok") {
		t.Fatalf("expected exit 0 from $HOME/.cargo/bin/cargo, got %+v", okOut)
	}

	// 2. Missing command persists ExecError with not found detail.
	missRes, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "workflow_receipt_run",
		Arguments: map[string]any{
			"command": "jtao-nonexistent-mcp-cmd",
		},
	})
	if err != nil || missRes.IsError {
		t.Fatalf("workflow_receipt_run missing failed: err=%v res=%+v", err, missRes)
	}
	missBytes, _ := json.Marshal(missRes.StructuredContent)
	var missOut runner.ReceiptExecOutput
	if err := json.Unmarshal(missBytes, &missOut); err != nil {
		t.Fatalf("unmarshal missOut: %v", err)
	}
	if missOut.Record.ExitCode != -1 || !strings.Contains(missOut.Record.ExecError, "not found") {
		t.Fatalf("expected ExecError containing not found, got %+v", missOut)
	}
	stored, err := wf.GetReceipt(ctx, ws, missOut.ReceiptID)
	if err != nil || stored == nil || !strings.Contains(stored.ExecError, "not found") {
		t.Fatalf("expected persisted receipt with ExecError, got err=%v stored=%+v", err, stored)
	}
}
