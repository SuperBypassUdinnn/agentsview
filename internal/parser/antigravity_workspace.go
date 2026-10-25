package parser

import (
	"context"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/tidwall/gjson"
)

// antigravityWorkspaces holds the workspace each conversation records in one
// root's conversation_summaries.db. A conversation whose recorded workspace is
// not a local folder maps to "". err is set when the store exists but cannot
// be read, so callers can keep a stored working directory instead of clearing
// it.
type antigravityWorkspaces struct {
	byID map[string]string
	err  error
}

func loadAntigravityWorkspaces(ctx context.Context, root string) antigravityWorkspaces {
	return loadAntigravityWorkspacesFor(ctx, root, "")
}

// loadAntigravityWorkspace reads one conversation's workspace, for lookups
// that resolve a single source.
func loadAntigravityWorkspace(ctx context.Context, root, id string) antigravityWorkspaces {
	return loadAntigravityWorkspacesFor(ctx, root, id)
}

func loadAntigravityWorkspacesFor(ctx context.Context, root, id string) antigravityWorkspaces {
	workspaces := antigravityWorkspaces{byID: make(map[string]string)}
	workspaces.err = forEachAntigravityStoreSummary(ctx, root, id,
		func(summary antigravityStoreSummary) error {
			if summary.hasWorkspace {
				workspaces.byID[summary.id] = summary.workspace
			}
			return nil
		},
	)
	return workspaces
}

// cwdResolution describes what the store knows about a conversation's local
// working directory. A conversation the store does not list, or lists with no
// workspace, leaves the parsed value alone.
func (w antigravityWorkspaces) cwdResolution(id string) SourceCwdResolution {
	if w.err != nil {
		return SourceCwdResolution{State: SourceCwdUnavailable}
	}
	workspace, ok := w.byID[id]
	switch {
	case !ok:
		return SourceCwdResolution{}
	case workspace == "":
		return SourceCwdResolution{State: SourceCwdAmbiguous}
	}
	return SourceCwdResolution{State: SourceCwdResolved, Path: workspace}
}

// antigravityStoreParent returns the conversation that started id, as the
// root's summary store records it, or "" when it records none. Sub-agent
// conversations, such as a research agent the main conversation invokes,
// are stored as conversations of their own that name their parent.
func antigravityStoreParent(ctx context.Context, root, id string) string {
	var parent string
	_ = forEachAntigravityStoreSummary(ctx, root, id,
		func(summary antigravityStoreSummary) error {
			parent = summary.parentID
			return nil
		},
	)
	if strings.EqualFold(parent, id) {
		return ""
	}
	return parent
}

// antigravityStoreSummary is what one summary-store row says about a
// conversation outside its transcript.
type antigravityStoreSummary struct {
	id string
	// workspace is the first workspace folder as a local path, or "" when the
	// folder is not local. hasWorkspace reports whether any folder is named.
	workspace    string
	hasWorkspace bool
	// parentID is the canonical ID of the conversation that started this
	// one, or "".
	parentID string
}

// forEachAntigravityStoreSummary yields each conversation row in the root's
// summary store. When id is set, only that conversation is read. A missing
// store or table yields nothing, and a missing workspace or parent column
// yields rows without that value: builds that predate the columns record
// neither.
func forEachAntigravityStoreSummary(
	ctx context.Context, root, id string, yield func(antigravityStoreSummary) error,
) error {
	path := filepath.Join(root, AntigravitySummariesDBName)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	observeSharedContainerScan(ctx)
	db, err := openSQLiteReadOnly(path, sqliteReadOptions{busyTimeoutMS: 3000})
	if err != nil {
		return err
	}
	defer db.Close()
	var hasID, hasWorkspaces, hasParent bool
	if err := db.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM pragma_table_info('conversation_summaries') WHERE name = 'conversation_id'),
		EXISTS(SELECT 1 FROM pragma_table_info('conversation_summaries') WHERE name = 'workspace_uris'),
		EXISTS(SELECT 1 FROM pragma_table_info('conversation_summaries') WHERE name = 'parent_conversation_id')`,
	).Scan(&hasID, &hasWorkspaces, &hasParent); err != nil {
		return err
	}
	if !hasID || !hasWorkspaces && !hasParent {
		return nil
	}
	workspaceColumn, parentColumn := "''", "''"
	if hasWorkspaces {
		workspaceColumn = "workspace_uris"
	}
	if hasParent {
		parentColumn = "parent_conversation_id"
	}
	query := "SELECT conversation_id, COALESCE(" + workspaceColumn + ", ''), COALESCE(" +
		parentColumn + ", '') FROM conversation_summaries"
	var args []any
	if id != "" {
		query += " WHERE conversation_id = ?"
		args = append(args, id)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var conversationID, uris, parentID string
		if err := rows.Scan(&conversationID, &uris, &parentID); err != nil {
			return err
		}
		if conversationID == "" {
			continue
		}
		summary := antigravityStoreSummary{
			id:       conversationID,
			parentID: canonicalAgyCascadeID(strings.TrimSpace(parentID)),
		}
		if first := gjson.Get(uris, "0"); first.Type == gjson.String {
			summary.workspace = antigravityWorkspacePath(first.Str)
			summary.hasWorkspace = true
		}
		if err := yield(summary); err != nil {
			return err
		}
	}
	return rows.Err()
}

// antigravityWorkspacePath converts a recorded workspace URI to a local
// folder path, or returns "" when the URI does not name a local folder.
// Windows drive folders come back with backslashes, as Windows tools report
// them; a URI with a host names a UNC share.
func antigravityWorkspacePath(uri string) string {
	u, err := url.Parse(strings.TrimSpace(uri))
	if err != nil || !strings.EqualFold(u.Scheme, "file") || u.Path == "" {
		return ""
	}
	path := u.Path
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	if host := u.Host; host != "" && !strings.EqualFold(host, "localhost") {
		return `\\` + host + strings.ReplaceAll(path, "/", `\`)
	}
	if len(path) >= 3 && path[0] == '/' && path[2] == ':' && isASCIILetter(path[1]) {
		drive := path[1:]
		if len(drive) == 2 {
			drive += "/"
		}
		return strings.ReplaceAll(drive, "/", `\`)
	}
	if !strings.HasPrefix(path, "/") {
		return ""
	}
	return path
}

func isASCIILetter(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z'
}
