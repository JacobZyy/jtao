package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/JacobZyy/jtao/internal/adapter"
	"github.com/JacobZyy/jtao/internal/hook"
	jtaomcp "github.com/JacobZyy/jtao/internal/mcp"
)

func newMCPCmd(app *App) *cobra.Command {
	var only string
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Start the jtao stdio MCP server (memory_* and workflow_* tools)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return jtaomcp.RunStdio(cmd.Context(), jtaomcp.Config{
				DefaultWorkspace: app.Workspace,
				Only:             only,
				Memory:           app.Memory,
				Workflow:         app.Workflow,
			})
		},
	}
	cmd.Flags().StringVar(&only, "only", "", "Limit exposed toolset: memory | workflow (empty = all)")
	return cmd
}

func newHookCmd(app *App) *cobra.Command {
	hookCmd := &cobra.Command{
		Use:   "hook",
		Short: "Fast stdin/stdout host hook entry points (<10ms cold start)",
	}

	readStdin := func(cmd *cobra.Command) ([]byte, error) {
		return io.ReadAll(cmd.InOrStdin())
	}

	hookCmd.AddCommand(
		&cobra.Command{
			Use:   "user-prompt-submit",
			Short: "Handle UserPromptSubmit hook (hit-gated recall + active work item)",
			RunE: func(cmd *cobra.Command, _ []string) error {
				raw, err := readStdin(cmd)
				if err != nil {
					return err
				}
				out, err := hook.HandleUserPromptSubmit(cmd.Context(), app.Memory, app.Workflow, app.Workspace, raw)
				if err != nil {
					return err
				}
				_, err = cmd.OutOrStdout().Write(out)
				return err
			},
		},
		&cobra.Command{
			Use:   "session-start",
			Short: "Handle SessionStart hook (inject pinned rules + continuity)",
			RunE: func(cmd *cobra.Command, _ []string) error {
				raw, err := readStdin(cmd)
				if err != nil {
					return err
				}
				out, err := hook.HandleSessionStart(cmd.Context(), app.Memory, app.Workflow, app.Workspace, raw)
				if err != nil {
					return err
				}
				_, err = cmd.OutOrStdout().Write(out)
				return err
			},
		},
		&cobra.Command{
			Use:   "stop",
			Short: "Handle Stop hook (save-guard checkpoint)",
			RunE: func(cmd *cobra.Command, _ []string) error {
				raw, err := readStdin(cmd)
				if err != nil {
					return err
				}
				out, err := hook.HandleStop(cmd.Context(), app.Memory, app.Workspace, raw)
				if err != nil {
					return err
				}
				_, err = cmd.OutOrStdout().Write(out)
				return err
			},
		},
		&cobra.Command{
			Use:   "post-tool-use",
			Short: "Handle PostToolUse hook (offload large tool outputs)",
			RunE: func(cmd *cobra.Command, _ []string) error {
				raw, err := readStdin(cmd)
				if err != nil {
					return err
				}
				out, err := hook.HandlePostToolUse(cmd.Context(), app.Memory, app.Workspace, raw)
				if err != nil {
					return err
				}
				_, err = cmd.OutOrStdout().Write(out)
				return err
			},
		},
		&cobra.Command{
			Use:   "agy-session-start",
			Short: "Handle Antigravity SessionStart hook (camelCase stdin -> injectSteps stdout)",
			RunE: func(cmd *cobra.Command, _ []string) error {
				raw, err := readStdin(cmd)
				if err != nil {
					return err
				}
				out, err := hook.HandleAGYSessionStart(cmd.Context(), app.Memory, app.Workflow, raw)
				if err != nil {
					return err
				}
				_, err = cmd.OutOrStdout().Write(out)
				return err
			},
		},
		&cobra.Command{
			Use:   "agy-pre-invocation",
			Short: "Handle Antigravity PreInvocation hook with per-prompt deduplication",
			RunE: func(cmd *cobra.Command, _ []string) error {
				raw, err := readStdin(cmd)
				if err != nil {
					return err
				}
				out, err := hook.HandleAGYPreInvocation(cmd.Context(), app.Memory, app.Workflow, raw)
				if err != nil {
					return err
				}
				_, err = cmd.OutOrStdout().Write(out)
				return err
			},
		},
	)

	return hookCmd
}

func newInitCmd(app *App) *cobra.Command {
	var client string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Idempotently register jtao MCP, Hooks, and embedded Skills for Codex/Claude",
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep, err := adapter.Init(cmd.Context(), adapter.InitInput{
				WorkspaceRoot: app.Workspace,
				Client:        client,
				DryRun:        dryRun,
			})
			if err != nil {
				return err
			}
			return app.printResult(cmd, rep, func() string {
				var b strings.Builder
				fmt.Fprintf(&b, "jtao init (binary=%s dryRun=%v)\n", rep.BinaryPath, rep.DryRun)
				for _, act := range rep.Actions {
					fmt.Fprintf(&b, "  [%s] %s — %s\n", act.Action, act.Target, act.Detail)
				}
				return strings.TrimRight(b.String(), "\n")
			})
		},
	}
	cmd.Flags().StringVar(&client, "client", "all", "codex | claude | all")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview changes without writing files")
	return cmd
}

func newDoctorCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check jtao binary, SQLite WAL/FTS5 health, and Codex/Claude CLI availability",
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep, err := adapter.Doctor(cmd.Context(), app.Memory, app.Workflow, app.Workspace)
			if err != nil {
				return err
			}
			return app.printResult(cmd, rep, func() string {
				var b strings.Builder
				status := "OK"
				if !rep.OK {
					status = "FAIL"
				}
				fmt.Fprintf(&b, "jtao doctor: %s\n", status)
				fmt.Fprintf(&b, "  binary:   %s\n", rep.BinaryPath)
				fmt.Fprintf(&b, "  memory:   ok=%v wal=%s fts5=%v memos=%d sessions=%d (%s)\n",
					rep.Memory.OK, rep.Memory.JournalMode, rep.Memory.FTS5Available, rep.Memory.MemoCount, rep.Memory.SessionCount, rep.Memory.DBPath)
				fmt.Fprintf(&b, "  workflow: ok=%v (%s)\n", rep.WorkflowOK, rep.WorkflowDB)
				fmt.Fprintf(&b, "  codex:    ok=%v (%s)\n", rep.Codex.OK, rep.Codex.BinaryPath)
				fmt.Fprintf(&b, "  claude:   ok=%v (%s)\n", rep.Claude.OK, rep.Claude.BinaryPath)
				for _, note := range rep.Notes {
					fmt.Fprintf(&b, "  note:     %s\n", note)
				}
				return strings.TrimRight(b.String(), "\n")
			})
		},
	}
}
