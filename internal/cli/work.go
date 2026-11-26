package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/JacobZyy/jtao/internal/runner"
	"github.com/JacobZyy/jtao/internal/scheduler"
	"github.com/JacobZyy/jtao/internal/workflow"
)

func newWorkCmd(app *App) *cobra.Command {
	workCmd := &cobra.Command{
		Use:   "work",
		Short: "Manage unified WorkItem task DAGs, tokens, and real verification receipts",
	}

	// jtao work create
	var id, title, objective, blueprint, graphFile string
	var setActive, exportMD bool
	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a WorkItem from a blueprint or custom DAG JSON",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var nodes []workflow.TaskNode
			var edges []workflow.TaskEdge
			if strings.TrimSpace(graphFile) != "" {
				raw, err := os.ReadFile(graphFile)
				if err != nil {
					return err
				}
				var parsed struct {
					Nodes []workflow.TaskNode `json:"nodes"`
					Edges []workflow.TaskEdge `json:"edges"`
				}
				if err := json.Unmarshal(raw, &parsed); err != nil {
					return fmt.Errorf("parse graph file: %w", err)
				}
				nodes = parsed.Nodes
				edges = parsed.Edges
			}
			detail, err := app.Workflow.CreateWorkItem(cmd.Context(), workflow.CreateWorkItemInput{
				WorkspaceRoot:  app.Workspace,
				ID:             id,
				Title:          title,
				Objective:      objective,
				Blueprint:      blueprint,
				Nodes:          nodes,
				Edges:          edges,
				SetActive:      setActive,
				ExportMarkdown: exportMD,
			})
			if err != nil {
				return err
			}
			return app.printResult(cmd, detail, func() string {
				return fmt.Sprintf("work item created: %s (%d nodes, blueprint=%s)", detail.ID, len(detail.Nodes), detail.Blueprint)
			})
		},
	}
	createCmd.Flags().StringVar(&id, "id", "", "Canonical WorkItem ID")
	createCmd.Flags().StringVar(&title, "title", "", "WorkItem title")
	createCmd.Flags().StringVar(&objective, "objective", "", "WorkItem objective")
	createCmd.Flags().StringVar(&blueprint, "blueprint", "single", "single|feature|bugfix|refactor|security")
	createCmd.Flags().StringVar(&graphFile, "graph", "", "Path to JSON file with {nodes, edges}")
	createCmd.Flags().BoolVar(&setActive, "active", true, "Set as workspace active WorkItem")
	createCmd.Flags().BoolVar(&exportMD, "export-md", true, "Project docs/plans/<date>-<id>.md")

	// jtao work status
	var statusID string
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Inspect a WorkItem's DAG nodes and DoneGate",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var detail *workflow.WorkItemDetail
			var err error
			if statusID != "" {
				detail, err = app.Workflow.GetWorkItem(cmd.Context(), app.Workspace, statusID)
			} else {
				detail, err = app.Workflow.GetActiveWorkItem(cmd.Context(), app.Workspace)
			}
			if err != nil {
				return err
			}
			if detail == nil {
				return fmt.Errorf("no matching work item found")
			}
			gate, _ := app.Workflow.EvaluateDoneGate(cmd.Context(), app.Workspace, detail.ID)
			out := map[string]any{"workItem": detail, "doneGate": gate}
			return app.printResult(cmd, out, func() string {
				var b strings.Builder
				fmt.Fprintf(&b, "WorkItem %s [%s] (%s)\n", detail.ID, detail.Status, detail.Title)
				for _, n := range detail.Nodes {
					fmt.Fprintf(&b, "  - %-16s [%-8s] role=%s %s\n", n.ID, n.Status, n.Role, n.Title)
				}
				fmt.Fprintf(&b, "DoneGate: ok=%v (%d/%d done, %d evidence)\n", gate.OK, gate.NodesDone, gate.NodesTotal, gate.EvidenceRefs)
				return strings.TrimRight(b.String(), "\n")
			})
		},
	}
	statusCmd.Flags().StringVar(&statusID, "id", "", "WorkItem ID (defaults to active)")

	// jtao work list
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List WorkItems in workspace",
		RunE: func(cmd *cobra.Command, _ []string) error {
			items, err := app.Workflow.ListWorkItems(cmd.Context(), app.Workspace, nil, 30)
			if err != nil {
				return err
			}
			return app.printResult(cmd, items, func() string {
				if len(items) == 0 {
					return "(no work items)"
				}
				var b strings.Builder
				for _, wi := range items {
					star := " "
					if wi.IsActive {
						star = "*"
					}
					fmt.Fprintf(&b, "%s %-24s [%-9s] %s\n", star, wi.ID, wi.Status, wi.Title)
				}
				return strings.TrimRight(b.String(), "\n")
			})
		},
	}

	// jtao work claim
	var claimID, claimNode string
	claimCmd := &cobra.Command{
		Use:   "claim",
		Short: "Claim the next ready node and issue a one-time execution token",
		RunE: func(cmd *cobra.Command, _ []string) error {
			wiID := claimID
			if wiID == "" {
				active, err := app.Workflow.GetActiveWorkItem(cmd.Context(), app.Workspace)
				if err != nil || active == nil {
					return fmt.Errorf("no active work item found")
				}
				wiID = active.ID
			}
			claimed, err := app.Workflow.ClaimNode(cmd.Context(), app.Workspace, wiID, claimNode)
			if err != nil {
				return err
			}
			if claimed == nil {
				return fmt.Errorf("no ready nodes in work item %s", wiID)
			}
			return app.printResult(cmd, claimed, func() string {
				return fmt.Sprintf("claimed %s/%s token=%s", claimed.WorkItemID, claimed.Node.ID, claimed.ExecutionToken)
			})
		},
	}
	claimCmd.Flags().StringVar(&claimID, "id", "", "WorkItem ID (defaults to active)")
	claimCmd.Flags().StringVar(&claimNode, "node", "", "Specific ready node ID")

	// jtao work submit
	var subWork, subNode, subToken, subStatus, subSummary, subReceipt string
	var subRefs []string
	submitCmd := &cobra.Command{
		Use:   "submit",
		Short: "Submit node outcome and real receipt/evidence to advance the DAG",
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := app.Workflow.SubmitNodeResult(cmd.Context(), workflow.SubmitNodeResultInput{
				WorkspaceRoot:  app.Workspace,
				WorkItemID:     subWork,
				NodeID:         subNode,
				ExecutionToken: subToken,
				Status:         subStatus,
				Summary:        subSummary,
				ReceiptID:      subReceipt,
				EvidenceRefs:   subRefs,
				Handoff: &workflow.NodeHandoff{
					Status:       subStatus,
					Summary:      subSummary,
					EvidenceRefs: subRefs,
				},
			})
			if err != nil {
				return err
			}
			return app.printResult(cmd, res, func() string {
				return fmt.Sprintf("decision=%s node=%s(%s) next=%v reason=%s", res.Decision, res.Node.ID, res.Node.Status, res.NextNodeIDs, res.Reason)
			})
		},
	}
	submitCmd.Flags().StringVar(&subWork, "id", "", "WorkItem ID")
	submitCmd.Flags().StringVar(&subNode, "node", "", "Node ID")
	submitCmd.Flags().StringVar(&subToken, "token", "", "One-time execution token")
	submitCmd.Flags().StringVar(&subStatus, "status", "pass", "pass|blocked|needs-input|fail")
	submitCmd.Flags().StringVar(&subSummary, "summary", "", "Summary")
	submitCmd.Flags().StringVar(&subReceipt, "receipt", "", "Real receipt:<id>")
	submitCmd.Flags().StringSliceVar(&subRefs, "ref", nil, "Additional evidence refs")
	_ = submitCmd.MarkFlagRequired("id")
	_ = submitCmd.MarkFlagRequired("node")
	_ = submitCmd.MarkFlagRequired("token")

	// jtao work receipt run -- <cmd...>
	receiptCmd := &cobra.Command{Use: "receipt", Short: "Execute and verify real command receipts"}
	var recWork, recNode string
	receiptRunCmd := &cobra.Command{
		Use:   "run -- <command> [args...]",
		Short: "Run a local verification command and persist receipt:<id>",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			exec := runner.ReceiptExecutor{
				Sink: func(rctx context.Context, root string, rec runner.ReceiptRecord) (string, error) {
					stored, err := app.Workflow.RecordReceipt(rctx, root, workflow.ExecutionReceipt{
						WorkItemID:    rec.WorkItemID,
						NodeID:        rec.NodeID,
						RunID:         rec.RunID,
						Command:       rec.Command,
						Args:          rec.Args,
						Cwd:           rec.Cwd,
						ExitCode:      rec.ExitCode,
						TimedOut:      rec.TimedOut,
						DurationMs:    rec.DurationMs,
						StdoutSHA256:  rec.StdoutSHA256,
						StderrSHA256:  rec.StderrSHA256,
						OutputExcerpt: rec.OutputExcerpt,
						ArtifactRef:   rec.ArtifactRef,
					})
					if err != nil {
						return "", err
					}
					return stored.ID, nil
				},
			}
			out, err := exec.RunAndRecord(cmd.Context(), runner.ReceiptExecInput{
				WorkspaceRoot: app.Workspace,
				ExecutionCwd:  app.Workspace,
				WorkItemID:    recWork,
				NodeID:        recNode,
				Command:       args[0],
				Args:          args[1:],
			})
			if err != nil {
				return err
			}
			return app.printResult(cmd, out, func() string {
				return fmt.Sprintf("receipt=%s exit=%d duration=%dms", out.ReceiptID, out.Record.ExitCode, out.Record.DurationMs)
			})
		},
	}
	receiptRunCmd.Flags().StringVar(&recWork, "id", "", "WorkItem ID")
	receiptRunCmd.Flags().StringVar(&recNode, "node", "", "Node ID")
	receiptCmd.AddCommand(receiptRunCmd)

	// jtao work export-md
	var mdID string
	exportMDCmd := &cobra.Command{
		Use:   "export-md",
		Short: "Project a WorkItem into docs/plans/<date>-<id>.md",
		RunE: func(cmd *cobra.Command, _ []string) error {
			wiID := mdID
			if wiID == "" {
				active, err := app.Workflow.GetActiveWorkItem(cmd.Context(), app.Workspace)
				if err != nil || active == nil {
					return fmt.Errorf("no active work item found")
				}
				wiID = active.ID
			}
			relPath, _, err := app.Workflow.ExportMarkdown(cmd.Context(), app.Workspace, wiID)
			if err != nil {
				return err
			}
			return app.printResult(cmd, map[string]string{"path": relPath}, func() string { return "exported: " + relPath })
		},
	}
	exportMDCmd.Flags().StringVar(&mdID, "id", "", "WorkItem ID (defaults to active)")

	workCmd.AddCommand(createCmd, statusCmd, listCmd, claimCmd, submitCmd, receiptCmd, exportMDCmd)
	return workCmd
}

func newSoloCmd(app *App) *cobra.Command {
	soloCmd := &cobra.Command{
		Use:   "solo",
		Short: "Run, resume, inspect, or stop graph-driven Solo agent loops",
	}

	var wiID, runIDFlag, provider, baseRef string
	var useWorktree, dryRun, unattended, safeBypass bool
	var maxIter, timeoutSec, quotaMax, quotaWinMin, cadenceBaseSec, cadenceMaxSec, quietThreshold int
	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Run a graph-driven Solo loop against a WorkItem",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if wiID == "" {
				active, err := app.Workflow.GetActiveWorkItem(cmd.Context(), app.Workspace)
				if err != nil || active == nil {
					return fmt.Errorf("provide --work-item or create an active work item first")
				}
				wiID = active.ID
			}
			svc := app.newSoloService()
			if dryRun {
				rep, err := svc.DryRun(cmd.Context(), scheduler.SoloRunInput{
					WorkspaceRoot: app.Workspace,
					WorkItemID:    wiID,
					Provider:      provider,
				})
				if err != nil {
					return err
				}
				return app.printResult(cmd, rep, func() string {
					return fmt.Sprintf("dry-run verdict=%s workItem=%s provider=%s ready=%v", rep.Verdict, rep.WorkItemID, rep.Provider, rep.ReadyNodes)
				})
			}
			pacing := scheduler.PacingConfig{
				Enabled:        unattended || quotaMax > 0 || cadenceBaseSec > 0 || quietThreshold > 0,
				QuotaMaxTurns:  quotaMax,
				QuotaWindow:    time.Duration(quotaWinMin) * time.Minute,
				CadenceBase:    time.Duration(cadenceBaseSec) * time.Second,
				CadenceMax:     time.Duration(cadenceMaxSec) * time.Second,
				QuietThreshold: quietThreshold,
				SafeBypass:     safeBypass,
			}
			res, err := svc.Run(cmd.Context(), scheduler.SoloRunInput{
				WorkspaceRoot: app.Workspace,
				WorkItemID:    wiID,
				RunID:         runIDFlag,
				Provider:      provider,
				UseWorktree:   useWorktree,
				BaseRef:       baseRef,
				MaxIterations: maxIter,
				TurnTimeout:   time.Duration(timeoutSec) * time.Second,
				Pacing:        pacing,
			})
			if err != nil {
				return err
			}
			return app.printResult(cmd, res, func() string {
				return fmt.Sprintf("solo run %s finished: status=%s turns=%d note=%s", res.Run.ID, res.Run.Status, res.Turns, res.FinalNote)
			})
		},
	}
	runCmd.Flags().StringVar(&wiID, "work-item", "", "WorkItem ID (defaults to active)")
	runCmd.Flags().StringVar(&runIDFlag, "run-id", "", "Optional pre-allocated Run ID")
	runCmd.Flags().StringVar(&provider, "provider", "codex", "codex|claude")
	runCmd.Flags().BoolVar(&useWorktree, "worktree", false, "Run inside an isolated git worktree")
	runCmd.Flags().StringVar(&baseRef, "base-ref", "HEAD", "Base git ref for --worktree")
	runCmd.Flags().IntVar(&maxIter, "max-iterations", 20, "Maximum loop iterations")
	runCmd.Flags().IntVar(&timeoutSec, "timeout-sec", 1800, "Per-turn timeout in seconds")
	runCmd.Flags().BoolVar(&unattended, "unattended", false, "Enable unattended pacing gate (quota/cadence/quiet/safe-bypass)")
	runCmd.Flags().IntVar(&quotaMax, "quota", 0, "Max material turns per rolling quota window (0 = unlimited)")
	runCmd.Flags().IntVar(&quotaWinMin, "quota-window-min", 60, "Rolling quota window in minutes")
	runCmd.Flags().IntVar(&cadenceBaseSec, "cadence-base-sec", 0, "Base backoff seconds for monitor_wait cadence")
	runCmd.Flags().IntVar(&cadenceMaxSec, "cadence-max-sec", 300, "Max backoff seconds for monitor_wait cadence")
	runCmd.Flags().IntVar(&quietThreshold, "quiet-threshold", 0, "Consecutive noop turns before quiet stop")
	runCmd.Flags().BoolVar(&safeBypass, "safe-bypass", true, "Grant one read-only probe turn before human-gate stop when --unattended")
	runCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Check readiness without calling any model")

	var resumeRunID string
	var resumeMax int
	var resumeUnattended bool
	resumeCmd := &cobra.Command{
		Use:   "resume",
		Short: "Resume a stopped or crashed Solo run",
		RunE: func(cmd *cobra.Command, _ []string) error {
			pacing := scheduler.PacingConfig{Enabled: resumeUnattended, SafeBypass: resumeUnattended}
			res, err := app.newSoloService().Resume(cmd.Context(), app.Workspace, resumeRunID, resumeMax, 0, pacing, nil)
			if err != nil {
				return err
			}
			return app.printResult(cmd, res, func() string {
				return fmt.Sprintf("solo resume %s finished: status=%s turns=%d note=%s", res.Run.ID, res.Run.Status, res.Turns, res.FinalNote)
			})
		},
	}
	resumeCmd.Flags().StringVar(&resumeRunID, "run", "", "Run ID to resume")
	resumeCmd.Flags().IntVar(&resumeMax, "max-iterations", 0, "Optional updated max iterations")
	resumeCmd.Flags().BoolVar(&resumeUnattended, "unattended", false, "Resume with unattended pacing gate enabled")
	_ = resumeCmd.MarkFlagRequired("run")

	var stopRunID, stopReason string
	stopCmd := &cobra.Command{
		Use:   "stop",
		Short: "Request graceful stop for an active Solo run",
		RunE: func(cmd *cobra.Command, _ []string) error {
			rec, err := app.newSoloService().Stop(cmd.Context(), app.Workspace, stopRunID, stopReason)
			if err != nil {
				return err
			}
			return app.printResult(cmd, rec, func() string {
				return fmt.Sprintf("stop requested on %s (reason=%s)", rec.ID, rec.StopReason)
			})
		},
	}
	stopCmd.Flags().StringVar(&stopRunID, "run", "", "Run ID")
	stopCmd.Flags().StringVar(&stopReason, "reason", "operator-request", "Stop reason")
	_ = stopCmd.MarkFlagRequired("run")

	var stRunID, stWorkID string
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show status of a Solo run",
		RunE: func(cmd *cobra.Command, _ []string) error {
			rec, err := app.newSoloService().Status(cmd.Context(), app.Workspace, stRunID, stWorkID)
			if err != nil {
				return err
			}
			if rec == nil {
				return fmt.Errorf("no matching run found")
			}
			return app.printResult(cmd, rec, func() string {
				return fmt.Sprintf("run=%s workItem=%s status=%s iter=%d/%d summary=%s", rec.ID, rec.WorkItemID, rec.Status, rec.CurrentIteration, rec.MaxIterations, rec.SummaryText)
			})
		},
	}
	statusCmd.Flags().StringVar(&stRunID, "run", "", "Run ID")
	statusCmd.Flags().StringVar(&stWorkID, "work-item", "", "WorkItem ID")

	soloCmd.AddCommand(runCmd, resumeCmd, stopCmd, statusCmd)
	return soloCmd
}

func newTeamCmd(app *App) *cobra.Command {
	teamCmd := &cobra.Command{
		Use:   "team",
		Short: "Plan, run, or retry-blocked multi-role concurrent WorkItem DAGs",
	}

	var planWI string
	var planConc int
	planCmd := &cobra.Command{
		Use:   "plan",
		Short: "Preview execution waves and validate file ownership rules without calling any model",
		RunE: func(cmd *cobra.Command, _ []string) error {
			wiID := planWI
			if wiID == "" {
				active, err := app.Workflow.GetActiveWorkItem(cmd.Context(), app.Workspace)
				if err != nil || active == nil {
					return fmt.Errorf("provide --work-item or create an active work item first")
				}
				wiID = active.ID
			}
			preview, err := app.newTeamService().Plan(cmd.Context(), scheduler.TeamRunInput{
				WorkspaceRoot: app.Workspace,
				WorkItemID:    wiID,
				Concurrency:   planConc,
			})
			if err != nil {
				return err
			}
			return app.printResult(cmd, preview, func() string {
				var b strings.Builder
				fmt.Fprintf(&b, "Team plan for %s (blueprint=%s concurrency=%d)\n", preview.WorkItemID, preview.Blueprint, preview.Concurrency)
				for i, wave := range preview.Waves {
					fmt.Fprintf(&b, "  wave %d: %s\n", i+1, strings.Join(wave, ", "))
				}
				return strings.TrimRight(b.String(), "\n")
			})
		},
	}
	planCmd.Flags().StringVar(&planWI, "work-item", "", "WorkItem ID (defaults to active)")
	planCmd.Flags().IntVar(&planConc, "concurrency", 3, "Max concurrent agent nodes")

	var runWI, teamRunIDFlag, runProvider, runBaseRef string
	var runConc, runTimeoutSec int
	var runWorktree, runSnapshot bool
	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Execute a WorkItem DAG concurrently with ownership checks and Merge Gate",
		RunE: func(cmd *cobra.Command, _ []string) error {
			wiID := runWI
			if wiID == "" {
				active, err := app.Workflow.GetActiveWorkItem(cmd.Context(), app.Workspace)
				if err != nil || active == nil {
					return fmt.Errorf("provide --work-item or create an active work item first")
				}
				wiID = active.ID
			}
			res, err := app.newTeamService().Run(cmd.Context(), scheduler.TeamRunInput{
				WorkspaceRoot:       app.Workspace,
				WorkItemID:          wiID,
				RunID:               teamRunIDFlag,
				Provider:            runProvider,
				Concurrency:         runConc,
				UseWorktree:         runWorktree,
				BaseRef:             runBaseRef,
				PreMutationSnapshot: runSnapshot,
				TurnTimeout:         time.Duration(runTimeoutSec) * time.Second,
			})
			if err != nil {
				return err
			}
			return app.printResult(cmd, res, func() string {
				return fmt.Sprintf("team run %s finished: ok=%v status=%s note=%s", res.Run.ID, res.OK, res.Run.Status, res.FinalNote)
			})
		},
	}
	runCmd.Flags().StringVar(&runWI, "work-item", "", "WorkItem ID (defaults to active)")
	runCmd.Flags().StringVar(&teamRunIDFlag, "run-id", "", "Optional pre-allocated Run ID")
	runCmd.Flags().StringVar(&runProvider, "provider", "codex", "codex|claude")
	runCmd.Flags().IntVar(&runConc, "concurrency", 3, "Max concurrent agent nodes")
	runCmd.Flags().BoolVar(&runWorktree, "worktree", false, "Isolate editable nodes in dedicated git worktrees")
	runCmd.Flags().StringVar(&runBaseRef, "base-ref", "HEAD", "Base git ref for --worktree")
	runCmd.Flags().BoolVar(&runSnapshot, "snapshot", false, "Capture pre-mutation git snapshot before editable nodes")
	runCmd.Flags().IntVar(&runTimeoutSec, "timeout-sec", 900, "Per-node timeout in seconds")

	var retryRunID, retryNextRunID, retryProvider string
	var retryConc int
	var retryForce, retryDryRun, retryWorktree bool
	retryCmd := &cobra.Command{
		Use:   "retry-blocked",
		Short: "Re-run only blocked/failed nodes from a prior Team run, seeding completed upstream nodes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := app.newTeamService().RetryBlocked(cmd.Context(), scheduler.TeamRetryInput{
				WorkspaceRoot: app.Workspace,
				RunID:         retryRunID,
				NextRunID:     retryNextRunID,
				Provider:      retryProvider,
				Concurrency:   retryConc,
				UseWorktree:   retryWorktree,
				Force:         retryForce,
				DryRun:        retryDryRun,
			})
			if err != nil {
				return err
			}
			return app.printResult(cmd, res, func() string {
				return fmt.Sprintf("team retry-blocked %s: ok=%v seeded=%d note=%s", res.Run.ID, res.OK, res.SeededCount, res.FinalNote)
			})
		},
	}
	retryCmd.Flags().StringVar(&retryRunID, "run", "", "Prior Team Run ID")
	retryCmd.Flags().StringVar(&retryNextRunID, "next-run-id", "", "Optional pre-allocated ID for the retry run")
	retryCmd.Flags().StringVar(&retryProvider, "provider", "", "Optional provider override (codex|claude)")
	retryCmd.Flags().IntVar(&retryConc, "concurrency", 3, "Max concurrent agent nodes")
	retryCmd.Flags().BoolVar(&retryWorktree, "worktree", false, "Isolate editable nodes in worktrees")
	retryCmd.Flags().BoolVar(&retryForce, "force", false, "Override repeated-block instability guard")
	retryCmd.Flags().BoolVar(&retryDryRun, "dry-run", false, "Preview seeded and retried nodes without invoking models")
	_ = retryCmd.MarkFlagRequired("run")

	teamCmd.AddCommand(planCmd, runCmd, retryCmd)
	return teamCmd
}
