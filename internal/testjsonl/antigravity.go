package testjsonl

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

const AntigravityTitleSchema = `CREATE TABLE IF NOT EXISTS conversation_summaries (conversation_id TEXT PRIMARY KEY, title TEXT)`

// AntigravityWorkspaceSchema is the part of Antigravity's summary store that
// records each conversation's workspace folders as a JSON array of URIs.
const AntigravityWorkspaceSchema = `CREATE TABLE IF NOT EXISTS conversation_summaries (conversation_id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', workspace_uris TEXT NOT NULL)`

// WriteAntigravityWorkspaces records workspace_uris values, keyed by
// conversation ID, in root's conversation_summaries.db.
func WriteAntigravityWorkspaces(t *testing.T, root string, uris map[string]string) string {
	t.Helper()
	path := filepath.Join(root, "conversation_summaries.db")
	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), AntigravityWorkspaceSchema)
	require.NoError(t, err)
	for id, value := range uris {
		_, err = conn.ExecContext(t.Context(), `INSERT INTO conversation_summaries (conversation_id, workspace_uris) VALUES (?, ?) ON CONFLICT(conversation_id) DO UPDATE SET workspace_uris = excluded.workspace_uris`, id, value)
		require.NoError(t, err)
	}
	return path
}

func WriteAntigravityBrainTranscript(t *testing.T, root, id, body string) string {
	t.Helper()
	path := filepath.Join(root, "brain", id, ".system_generated", "logs", "transcript.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}
