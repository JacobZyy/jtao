package memory

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type sqlRows = sql.Rows

const defaultArtifactReadLimit = 4000

// StoreArtifact persists a large payload in <memDir>/artifacts/<sha256>.txt
// and indexes its metadata in SQLite.
func (s *SQLiteService) StoreArtifact(ctx context.Context, workspaceRoot, sessionID, workItemID, tool, content string) (Artifact, error) {
	db, memDir, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return Artifact{}, err
	}

	hash := sha256Hex(content)
	relPath := filepath.ToSlash(filepath.Join("artifacts", hash+".txt"))
	absPath := filepath.Join(memDir, "artifacts", hash+".txt")

	if _, err := os.Stat(absPath); os.IsNotExist(err) {
		tmpPath := absPath + ".tmp"
		if err := os.WriteFile(tmpPath, []byte(content), 0o644); err != nil {
			return Artifact{}, fmt.Errorf("write artifact temp file: %w", err)
		}
		if err := os.Rename(tmpPath, absPath); err != nil {
			_ = os.Remove(tmpPath)
			return Artifact{}, fmt.Errorf("commit artifact file: %w", err)
		}
	}

	art := Artifact{
		ID:         newPrefixedID("artifact"),
		SessionID:  strings.TrimSpace(sessionID),
		WorkItemID: strings.TrimSpace(workItemID),
		Tool:       strings.TrimSpace(tool),
		SHA256:     hash,
		ByteSize:   int64(len(content)),
		RelPath:    relPath,
		Preview:    clipRunes(strings.TrimSpace(content), 240),
		CreatedAt:  s.nowISO(),
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO artifacts (id, session_id, work_item_id, tool, sha256, byte_size, rel_path, preview, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, art.ID, art.SessionID, art.WorkItemID, art.Tool, art.SHA256, art.ByteSize, art.RelPath, art.Preview, art.CreatedAt)
	if err != nil {
		return Artifact{}, fmt.Errorf("insert artifacts: %w", err)
	}
	return art, nil
}

// ReadArtifact reads a paginated slice from an offloaded artifact by its ID.
func (s *SQLiteService) ReadArtifact(ctx context.Context, workspaceRoot, artifactRef string, offset, limit int64) (ArtifactReadResult, error) {
	db, memDir, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return ArtifactReadResult{}, err
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = defaultArtifactReadLimit
	}

	var art Artifact
	err = db.QueryRowContext(ctx, `
		SELECT id, session_id, work_item_id, tool, sha256, byte_size, rel_path, preview, created_at
		FROM artifacts WHERE id = ?
	`, strings.TrimSpace(artifactRef)).Scan(
		&art.ID, &art.SessionID, &art.WorkItemID, &art.Tool,
		&art.SHA256, &art.ByteSize, &art.RelPath, &art.Preview, &art.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return ArtifactReadResult{}, fmt.Errorf("artifact not found: %s", artifactRef)
	}
	if err != nil {
		return ArtifactReadResult{}, err
	}

	absPath := filepath.Join(memDir, filepath.FromSlash(art.RelPath))
	raw, err := os.ReadFile(absPath)
	if err != nil {
		return ArtifactReadResult{}, fmt.Errorf("read artifact file: %w", err)
	}

	total := int64(len(raw))
	if offset >= total {
		return ArtifactReadResult{Artifact: art, Offset: offset, Limit: limit, Content: "", HasMore: false}, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return ArtifactReadResult{
		Artifact: art,
		Offset:   offset,
		Limit:    limit,
		Content:  string(raw[offset:end]),
		HasMore:  end < total,
	}, nil
}

// GrepArtifact searches matching lines inside an offloaded artifact using a Go regexp
// (falling back to case-insensitive literal match if pattern is not valid regex).
func (s *SQLiteService) GrepArtifact(ctx context.Context, workspaceRoot, artifactRef, pattern string, maxMatches int) ([]ArtifactGrepMatch, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, fmt.Errorf("grep pattern is required")
	}
	if maxMatches <= 0 {
		maxMatches = 30
	}
	db, memDir, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}
	var relPath string
	err = db.QueryRowContext(ctx, `SELECT rel_path FROM artifacts WHERE id = ?`, strings.TrimSpace(artifactRef)).Scan(&relPath)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("artifact not found: %s", artifactRef)
	}
	if err != nil {
		return nil, err
	}

	file, err := os.Open(filepath.Join(memDir, filepath.FromSlash(relPath)))
	if err != nil {
		return nil, fmt.Errorf("open artifact file: %w", err)
	}
	defer file.Close()

	re, reErr := regexp.Compile(pattern)
	lowerPat := strings.ToLower(pattern)

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var matches []ArtifactGrepMatch
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		matched := false
		if reErr == nil {
			matched = re.MatchString(line)
		} else {
			matched = strings.Contains(strings.ToLower(line), lowerPat)
		}
		if matched {
			matches = append(matches, ArtifactGrepMatch{LineNumber: lineNo, Line: clipRunes(line, 400)})
			if len(matches) >= maxMatches {
				break
			}
		}
	}
	return matches, scanner.Err()
}

// ListArtifacts lists stored artifacts filtered by session or workItem.
func (s *SQLiteService) ListArtifacts(ctx context.Context, workspaceRoot, sessionID, workItemID string, limit int) ([]Artifact, error) {
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	var clauses []string
	var args []any
	if strings.TrimSpace(sessionID) != "" {
		clauses = append(clauses, "session_id = ?")
		args = append(args, strings.TrimSpace(sessionID))
	}
	if strings.TrimSpace(workItemID) != "" {
		clauses = append(clauses, "work_item_id = ?")
		args = append(args, strings.TrimSpace(workItemID))
	}
	query := `SELECT id, session_id, work_item_id, tool, sha256, byte_size, rel_path, preview, created_at FROM artifacts`
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Artifact
	for rows.Next() {
		var art Artifact
		if err := rows.Scan(
			&art.ID, &art.SessionID, &art.WorkItemID, &art.Tool,
			&art.SHA256, &art.ByteSize, &art.RelPath, &art.Preview, &art.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, art)
	}
	return out, rows.Err()
}
