package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/JacobZyy/jtao/internal/memory"
)

func newMemoCmd(app *App) *cobra.Command {
	memoCmd := &cobra.Command{
		Use:   "memo",
		Short: "Manage durable project knowledge (memo, pin, feedback, candidates)",
	}

	// jtao memo add <text>
	var space, scope, agent, workItem, evidence, confidence, validAt, claimStatus string
	var refs, entities, supersedes []string
	addCmd := &cobra.Command{
		Use:   "add <text>",
		Short: "Record a durable memo entry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := app.Memory.WriteMemo(cmd.Context(), memory.WriteMemoInput{
				WorkspaceRoot:        app.Workspace,
				Text:                 args[0],
				Space:                space,
				Scope:                memory.Scope(scope),
				Agent:                agent,
				WorkItemID:           workItem,
				Refs:                 refs,
				Entities:             entities,
				EvidenceRef:          evidence,
				Confidence:           confidence,
				ClaimStatus:          memory.ClaimStatus(claimStatus),
				Supersedes:           supersedes,
				ValidAt:              validAt,
				ReturnSupersedeHints: true,
			})
			if err != nil {
				return err
			}
			return app.printResult(cmd, res, func() string {
				return fmt.Sprintf("memo recorded: %s (space=%s status=%s)", res.Event.ID, res.Event.Space, res.Event.ClaimStatus)
			})
		},
	}
	addCmd.Flags().StringVar(&space, "space", "default", "Memo space")
	addCmd.Flags().StringVar(&scope, "scope", "project_shared", "project_shared|agent_private|agent_ephemeral")
	addCmd.Flags().StringVar(&agent, "agent", "", "Agent identity")
	addCmd.Flags().StringVar(&workItem, "work-item", "", "Associated WorkItem ID")
	addCmd.Flags().StringSliceVar(&refs, "ref", nil, "Related file/ref (repeatable)")
	addCmd.Flags().StringSliceVar(&entities, "entity", nil, "Key entity (repeatable)")
	addCmd.Flags().StringVar(&evidence, "evidence", "", "Single concrete evidence ref")
	addCmd.Flags().StringVar(&confidence, "confidence", "", "high|medium")
	addCmd.Flags().StringVar(&claimStatus, "status", "verified", "verified|candidate|observed")
	addCmd.Flags().StringSliceVar(&supersedes, "supersedes", nil, "Superseded memo ID(s)")
	addCmd.Flags().StringVar(&validAt, "valid-at", "", "ISO-8601 effective timestamp")

	// jtao memo search <query>
	var sSpace, sAgent, sWorkItem string
	var sLimit int
	searchCmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search active project memos via FTS5 + entity spread + feedback ranking",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			hits, err := app.Memory.SearchMemos(cmd.Context(), memory.SearchMemosInput{
				WorkspaceRoot: app.Workspace,
				Query:         args[0],
				Space:         sSpace,
				Agent:         sAgent,
				WorkItemID:    sWorkItem,
				Limit:         sLimit,
			})
			if err != nil {
				return err
			}
			return app.printResult(cmd, hits, func() string {
				if len(hits) == 0 {
					return "(no memo hits)"
				}
				var b strings.Builder
				for _, h := range hits {
					fmt.Fprintf(&b, "- [%.2f %s] %s: %s\n", h.Score, h.MatchReason, h.Event.ID, h.Event.Text)
				}
				return strings.TrimRight(b.String(), "\n")
			})
		},
	}
	searchCmd.Flags().StringVar(&sSpace, "space", "", "Filter by space")
	searchCmd.Flags().StringVar(&sAgent, "agent", "", "Filter by agent")
	searchCmd.Flags().StringVar(&sWorkItem, "work-item", "", "Filter by WorkItem ID")
	searchCmd.Flags().IntVar(&sLimit, "limit", 10, "Max results")

	// jtao memo list
	var lSpace, lStatus, lWorkItem string
	var lLimit int
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List active memos",
		RunE: func(cmd *cobra.Command, _ []string) error {
			items, err := app.Memory.ListMemos(cmd.Context(), memory.ListMemosInput{
				WorkspaceRoot: app.Workspace,
				Space:         lSpace,
				WorkItemID:    lWorkItem,
				ClaimStatus:   memory.ClaimStatus(lStatus),
				Limit:         lLimit,
			})
			if err != nil {
				return err
			}
			return app.printResult(cmd, items, func() string {
				if len(items) == 0 {
					return "(no memos)"
				}
				var b strings.Builder
				for _, m := range items {
					fmt.Fprintf(&b, "- [%s] (%s) %s\n", m.ID, m.ClaimStatus, m.Text)
				}
				return strings.TrimRight(b.String(), "\n")
			})
		},
	}
	listCmd.Flags().StringVar(&lSpace, "space", "", "Filter by space")
	listCmd.Flags().StringVar(&lStatus, "status", "", "verified|candidate|observed")
	listCmd.Flags().StringVar(&lWorkItem, "work-item", "", "Filter by WorkItem ID")
	listCmd.Flags().IntVar(&lLimit, "limit", 20, "Max results")

	// jtao memo useful <id...>
	var uSession, uAgent string
	usefulCmd := &cobra.Command{
		Use:   "useful <memoId...>",
		Short: "Mark recalled memo(s) as useful",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := app.Memory.MarkUseful(cmd.Context(), memory.FeedbackInput{
				WorkspaceRoot: app.Workspace,
				MemoIDs:       args,
				SessionID:     uSession,
				Agent:         uAgent,
			})
			if err != nil {
				return err
			}
			return app.printResult(cmd, res, func() string {
				return fmt.Sprintf("accepted=%d ignored=%d", len(res.AcceptedIDs), len(res.IgnoredIDs))
			})
		},
	}
	usefulCmd.Flags().StringVar(&uSession, "session", "", "Session ID")
	usefulCmd.Flags().StringVar(&uAgent, "agent", "", "Agent identity")

	// jtao memo pin
	var pinSpace string
	pinCmd := &cobra.Command{Use: "pin", Short: "Manage pinned project rules and milestones"}
	pinCmd.PersistentFlags().StringVar(&pinSpace, "space", "default", "Memo space")

	pinCmd.AddCommand(
		&cobra.Command{
			Use:   "get",
			Short: "Show pinned content",
			RunE: func(cmd *cobra.Command, _ []string) error {
				entry, err := app.Memory.PinGet(cmd.Context(), app.Workspace, pinSpace)
				if err != nil {
					return err
				}
				return app.printResult(cmd, entry, func() string {
					if entry == nil || entry.Content == "" {
						return "(no pinned content)"
					}
					return entry.Content
				})
			},
		},
		&cobra.Command{
			Use:   "append <line>",
			Short: "Append a pinned rule or milestone line",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				entry, err := app.Memory.PinAppend(cmd.Context(), app.Workspace, pinSpace, args[0])
				if err != nil {
					return err
				}
				return app.printResult(cmd, entry, func() string {
					return fmt.Sprintf("pinned updated (space=%s hash=%s)", entry.Space, entry.ContentHash[:12])
				})
			},
		},
	)

	// jtao memo candidates promote <id>
	candCmd := &cobra.Command{Use: "candidates", Short: "Review and promote candidate memos"}
	candCmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List pending candidate memos",
			RunE: func(cmd *cobra.Command, _ []string) error {
				items, err := app.Memory.ListCandidates(cmd.Context(), app.Workspace, "", 20)
				if err != nil {
					return err
				}
				return app.printResult(cmd, items, func() string {
					if len(items) == 0 {
						return "(no candidates)"
					}
					var b strings.Builder
					for _, m := range items {
						fmt.Fprintf(&b, "- [%s] %s\n", m.ID, m.Text)
					}
					return strings.TrimRight(b.String(), "\n")
				})
			},
		},
		&cobra.Command{
			Use:   "promote <candidateId>",
			Short: "Explicitly promote a candidate memo to verified",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				promoted, err := app.Memory.PromoteCandidate(cmd.Context(), app.Workspace, args[0], "local_manual", "promoted via CLI")
				if err != nil {
					return err
				}
				return app.printResult(cmd, promoted, func() string {
					return fmt.Sprintf("promoted %s -> %s", args[0], promoted.ID)
				})
			},
		},
	)

	memoCmd.AddCommand(addCmd, searchCmd, listCmd, usefulCmd, pinCmd, candCmd)
	return memoCmd
}

func newRecallCmd(app *App) *cobra.Command {
	var workItem string
	var limit int
	cmd := &cobra.Command{
		Use:   "recall [query]",
		Short: "Unified recall across memos and checkpoints (or full context by --work-item)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if workItem != "" && len(args) == 0 {
				res, err := app.Memory.RecallForWorkItem(cmd.Context(), app.Workspace, workItem, 4000)
				if err != nil {
					return err
				}
				return app.printResult(cmd, res, func() string { return res.Text })
			}
			if len(args) == 0 {
				return fmt.Errorf("provide a search query or --work-item")
			}
			hits, err := app.Memory.RecallQuery(cmd.Context(), memory.RecallQueryInput{
				WorkspaceRoot: app.Workspace,
				Query:         strings.Join(args, " "),
				WorkItemID:    workItem,
				Limit:         limit,
			})
			if err != nil {
				return err
			}
			return app.printResult(cmd, hits, func() string {
				if len(hits) == 0 {
					return "(no recall hits)"
				}
				var b strings.Builder
				for _, h := range hits {
					fmt.Fprintf(&b, "- [%s|%s] (%.2f) %s\n", h.Source, h.ID, h.Score, h.Text)
				}
				return strings.TrimRight(b.String(), "\n")
			})
		},
	}
	cmd.Flags().StringVar(&workItem, "work-item", "", "Filter or load full WorkItem memory bundle")
	cmd.Flags().IntVar(&limit, "limit", 8, "Max hits")
	return cmd
}

func newSessionCmd(app *App) *cobra.Command {
	sessCmd := &cobra.Command{Use: "session", Short: "Manage sessions, checkpoints, and compact recovery packs"}

	var agent, goal, workItem string
	newCmd := &cobra.Command{
		Use:   "new",
		Short: "Create a new session",
		RunE: func(cmd *cobra.Command, _ []string) error {
			sess, err := app.Memory.CreateSession(cmd.Context(), app.Workspace, agent, "", goal, workItem)
			if err != nil {
				return err
			}
			return app.printResult(cmd, sess, func() string { return sess.ID })
		},
	}
	newCmd.Flags().StringVar(&agent, "agent", "codex", "Agent identity")
	newCmd.Flags().StringVar(&goal, "goal", "", "Session goal")
	newCmd.Flags().StringVar(&workItem, "work-item", "", "Associated WorkItem ID")

	var cpSess, cpWork, cpStatus string
	cpCmd := &cobra.Command{
		Use:   "checkpoint <summary>",
		Short: "Append a checkpoint to a session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sess, _, err := app.Memory.EnsureSession(cmd.Context(), app.Workspace, cpSess, "codex", "", args[0], cpWork)
			if err != nil {
				return err
			}
			cp, err := app.Memory.AddCheckpoint(cmd.Context(), app.Workspace, memory.Checkpoint{
				SessionID:  sess.ID,
				WorkItemID: cpWork,
				Status:     cpStatus,
				Summary:    args[0],
			})
			if err != nil {
				return err
			}
			return app.printResult(cmd, cp, func() string { return fmt.Sprintf("checkpoint %s on %s", cp.ID, sess.ID) })
		},
	}
	cpCmd.Flags().StringVar(&cpSess, "session", "", "Session ID")
	cpCmd.Flags().StringVar(&cpWork, "work-item", "", "WorkItem ID")
	cpCmd.Flags().StringVar(&cpStatus, "status", "running", "running|blocked|done")

	packCmd := &cobra.Command{
		Use:   "pack <sessionId>",
		Short: "Render a compact recovery pack for a session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pack, err := app.Memory.PackSession(cmd.Context(), app.Workspace, args[0], 4000)
			if err != nil {
				return err
			}
			return app.printResult(cmd, pack, func() string { return pack.Text })
		},
	}

	sessCmd.AddCommand(newCmd, cpCmd, packCmd)
	return sessCmd
}

func newArtifactCmd(app *App) *cobra.Command {
	artCmd := &cobra.Command{Use: "artifact", Short: "Inspect offloaded large tool/agent outputs"}

	var offset, limit int64
	readCmd := &cobra.Command{
		Use:   "read <artifactId>",
		Short: "Read a paginated slice of an artifact",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := app.Memory.ReadArtifact(cmd.Context(), app.Workspace, args[0], offset, limit)
			if err != nil {
				return err
			}
			return app.printResult(cmd, res, func() string { return res.Content })
		},
	}
	readCmd.Flags().Int64Var(&offset, "offset", 0, "Byte offset")
	readCmd.Flags().Int64Var(&limit, "limit", 4000, "Byte limit")

	var maxMatches int
	grepCmd := &cobra.Command{
		Use:   "grep <artifactId> <pattern>",
		Short: "Search lines inside an offloaded artifact",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			matches, err := app.Memory.GrepArtifact(cmd.Context(), app.Workspace, args[0], args[1], maxMatches)
			if err != nil {
				return err
			}
			return app.printResult(cmd, matches, func() string {
				if len(matches) == 0 {
					return "(no matches)"
				}
				var b strings.Builder
				for _, m := range matches {
					fmt.Fprintf(&b, "%d: %s\n", m.LineNumber, m.Line)
				}
				return strings.TrimRight(b.String(), "\n")
			})
		},
	}
	grepCmd.Flags().IntVar(&maxMatches, "max", 30, "Maximum matching lines")

	var listSess, listWork string
	var listLimit int
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List stored artifacts",
		RunE: func(cmd *cobra.Command, _ []string) error {
			items, err := app.Memory.ListArtifacts(cmd.Context(), app.Workspace, listSess, listWork, listLimit)
			if err != nil {
				return err
			}
			return app.printResult(cmd, items, func() string {
				if len(items) == 0 {
					return "(no artifacts)"
				}
				var b strings.Builder
				for _, art := range items {
					fmt.Fprintf(&b, "- %s (%d bytes, tool=%s)\n", art.ID, art.ByteSize, art.Tool)
				}
				return strings.TrimRight(b.String(), "\n")
			})
		},
	}
	listCmd.Flags().StringVar(&listSess, "session", "", "Filter by session ID")
	listCmd.Flags().StringVar(&listWork, "work-item", "", "Filter by WorkItem ID")
	listCmd.Flags().IntVar(&listLimit, "limit", 20, "Max results")

	artCmd.AddCommand(readCmd, grepCmd, listCmd)
	return artCmd
}

func newMemoryAdminCmd(app *App) *cobra.Command {
	memCmd := &cobra.Command{Use: "memory", Short: "Memory maintenance: doctor, hygiene, rebuild-index, export"}

	var staleDays int
	var apply bool
	hygieneCmd := &cobra.Command{
		Use:   "hygiene",
		Short: "Inspect or clean stale sessions and orphan artifact files",
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep, err := app.Memory.Hygiene(cmd.Context(), app.Workspace, staleDays, apply)
			if err != nil {
				return err
			}
			return app.printResult(cmd, rep, func() string {
				return fmt.Sprintf(
					"active=%d superseded=%d archived=%d candidates=%d expiredPrivate=%d staleSessions=%d(archived=%d) orphanArtifacts=%d(cleaned=%d)",
					rep.ActiveMemos, rep.SupersededMemos, rep.ArchivedMemos, rep.PendingCandidates,
					rep.ExpiredPrivateMemos, rep.StaleActiveSessions, rep.ArchivedStaleSessions,
					rep.OrphanArtifactFiles, rep.CleanedOrphanFiles,
				)
			})
		},
	}
	hygieneCmd.Flags().IntVar(&staleDays, "stale-days", 14, "Age threshold in days for stale active sessions")
	hygieneCmd.Flags().BoolVar(&apply, "apply", false, "Archive stale sessions and remove orphan artifact files")

	rebuildCmd := &cobra.Command{
		Use:   "rebuild-index",
		Short: "Rebuild FTS5 search indexes from canonical SQLite tables",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := app.Memory.RebuildIndex(cmd.Context(), app.Workspace); err != nil {
				return err
			}
			return app.printResult(cmd, map[string]bool{"ok": true}, func() string { return "FTS5 indexes rebuilt" })
		},
	}

	exportCmd := &cobra.Command{
		Use:   "export",
		Short: "Export all memo events as JSONL",
		RunE: func(cmd *cobra.Command, _ []string) error {
			jsonl, err := app.Memory.ExportJSONL(cmd.Context(), app.Workspace)
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), jsonl)
			return err
		},
	}

	memCmd.AddCommand(hygieneCmd, rebuildCmd, exportCmd)
	return memCmd
}
