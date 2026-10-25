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
	workspaces := antigravityWorkspaces{byID: make(map[string]string)}
	workspaces.err = forEachAntigravityStoreWorkspace(ctx, root, "",
		func(id, workspace string) error {
			workspaces.byID[id] = workspace
			return nil
		},
	)
	return workspaces
}

// loadAntigravityWorkspace reads one conversation's workspace, for lookups
// that resolve a single source.
func loadAntigravityWorkspace(ctx context.Context, root, id string) antigravityWorkspaces {
	workspaces := antigravityWorkspaces{byID: make(map[string]string, 1)}
	workspaces.err = forEachAntigravityStoreWorkspace(ctx, root, id,
		func(id, workspace string) error {
			workspaces.byID[id] = workspace
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

// forEachAntigravityStoreWorkspace yields each conversation in the root's
// summary store that records a workspace, with its first workspace folder as
// a local path. A workspace that is not a local folder, such as a remote
// development URI, yields "". When id is set, only that conversation is read.
// A missing store, table, or column yields nothing: builds that predate the
// column record no workspace.
func forEachAntigravityStoreWorkspace(
	ctx context.Context, root, id string, yield func(id, workspace string) error,
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
	var hasID, hasWorkspaces bool
	if err := db.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM pragma_table_info('conversation_summaries') WHERE name = 'conversation_id'),
		EXISTS(SELECT 1 FROM pragma_table_info('conversation_summaries') WHERE name = 'workspace_uris')`,
	).Scan(&hasID, &hasWorkspaces); err != nil {
		return err
	}
	if !hasID || !hasWorkspaces {
		return nil
	}
	query := "SELECT conversation_id, workspace_uris FROM conversation_summaries"
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
		var conversationID, uris string
		if err := rows.Scan(&conversationID, &uris); err != nil {
			return err
		}
		first := gjson.Get(uris, "0")
		if conversationID == "" || first.Type != gjson.String {
			continue
		}
		if err := yield(conversationID, antigravityWorkspacePath(first.Str)); err != nil {
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
