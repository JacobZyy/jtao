package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/JacobZyy/jtao/internal/memory"
	"github.com/JacobZyy/jtao/internal/scheduler"
	"github.com/JacobZyy/jtao/internal/workflow"
)

// App holds shared root flags and decoupled core service instances.
type App struct {
	Workspace  string
	JSONOutput bool
	Memory     *memory.SQLiteService
	Workflow   *workflow.SQLiteService
}

// NewRootCmd constructs the single-binary `jtao` Cobra CLI.
func NewRootCmd() *cobra.Command {
	app := &App{
		Memory:   memory.NewService(),
		Workflow: workflow.NewService(),
	}

	root := &cobra.Command{
		Use:           "jtao",
		Short:         "Local engineering Harness for Coding Agents (Memory · Workflow · Solo/Team · MCP)",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if app.Workspace == "" {
				cwd, err := os.Getwd()
				if err != nil {
					return err
				}
				app.Workspace = cwd
			}
			abs, err := filepath.Abs(app.Workspace)
			if err != nil {
				return err
			}
			app.Workspace = abs
			return nil
		},
		PersistentPostRun: func(_ *cobra.Command, _ []string) {
			_ = app.Memory.Close()
			_ = app.Workflow.Close()
		},
	}

	root.PersistentFlags().StringVarP(&app.Workspace, "workspace", "w", "", "Workspace root directory (defaults to current working directory)")
	root.PersistentFlags().BoolVar(&app.JSONOutput, "json", false, "Emit machine-readable JSON output")

	root.AddCommand(
		newMemoCmd(app),
		newRecallCmd(app),
		newSessionCmd(app),
		newArtifactCmd(app),
		newMemoryAdminCmd(app),
		newWorkCmd(app),
		newSoloCmd(app),
		newTeamCmd(app),
		newMCPCmd(app),
		newHookCmd(app),
		newInitCmd(app),
		newDoctorCmd(app),
	)

	return root
}

func (a *App) printResult(cmd *cobra.Command, value any, textFn func() string) error {
	if a.JSONOutput {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(value)
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), textFn())
	return err
}

// memoryPortAdapter bridges memory.SQLiteService onto scheduler.MemoryPort.
type memoryPortAdapter struct {
	svc *memory.SQLiteService
}

func (m memoryPortAdapter) EnsureSession(ctx context.Context, ws, sessionID, agent, goal, workItemID string) (string, error) {
	sess, _, err := m.svc.EnsureSession(ctx, ws, sessionID, agent, filepath.Base(ws), goal, workItemID)
	return sess.ID, err
}

func (m memoryPortAdapter) RecallForTurn(ctx context.Context, ws, query, sessionID, agent, workItemID string) (string, error) {
	res, err := m.svc.RecallForTurn(ctx, memory.TurnRecallInput{
		WorkspaceRoot: ws,
		Message:       query,
		SessionID:     sessionID,
		Agent:         agent,
		WorkItemID:    workItemID,
	})
	return res.Text, err
}

func (m memoryPortAdapter) Checkpoint(ctx context.Context, ws, sessionID, workItemID, status, summary string, nextActions, artifacts []string) error {
	_, err := m.svc.AddCheckpoint(ctx, ws, memory.Checkpoint{
		SessionID:   sessionID,
		WorkItemID:  workItemID,
		Status:      status,
		Summary:     summary,
		NextActions: nextActions,
		Artifacts:   artifacts,
	})
	return err
}

func (m memoryPortAdapter) UpdateContinuity(ctx context.Context, ws, sessionID, workItemID, intent, summary string, touchedFiles, nextActions []string) error {
	_, err := m.svc.UpdateContinuity(ctx, ws, memory.Continuity{
		SessionID:    sessionID,
		WorkItemID:   workItemID,
		Intent:       intent,
		Summary:      summary,
		TouchedFiles: touchedFiles,
		NextActions:  nextActions,
	})
	return err
}

func (m memoryPortAdapter) StoreArtifact(ctx context.Context, ws, sessionID, workItemID, tool, content string) (string, error) {
	art, err := m.svc.StoreArtifact(ctx, ws, sessionID, workItemID, tool, content)
	return art.ID, err
}

func (a *App) newSoloService() *scheduler.SoloService {
	return &scheduler.SoloService{
		Workflow: a.Workflow,
		Memory:   memoryPortAdapter{svc: a.Memory},
	}
}

func (a *App) newTeamService() *scheduler.TeamService {
	return &scheduler.TeamService{
		Workflow: a.Workflow,
		Memory:   memoryPortAdapter{svc: a.Memory},
	}
}
