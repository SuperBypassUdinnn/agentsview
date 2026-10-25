package sync_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// Archives written before the summary store's workspace was read hold
// Antigravity sessions with no working directory. A later sync fills them
// without any change to the conversation's own files, including in archives
// that keep usage only.
func TestAntigravityWorkspaceFillsArchivedSessions(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentUsage} {
		t.Run(string(policy), func(t *testing.T) {
			root := t.TempDir()
			dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
			dbPath := filepath.Join(root, "conversations", dbtest.AntigravityTitleID+".db")
			require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
			copyAntigravityCLITestSchemaTemplate(t, dbPath)
			archive, err := db.OpenWithArchiveContent(t.Context(), filepath.Join(t.TempDir(), "sessions.db"), policy)
			require.NoError(t, err)
			t.Cleanup(func() { archive.Close() })
			require.Equal(t, policy == config.ArchiveContentUsage, archive.ArchiveContent().UsageOnly())
			engine := sync.NewEngine(t.Context(), archive, sync.EngineConfig{
				AgentDirs:      map[parser.AgentType][]string{parser.AgentAntigravity: {root}},
				Machine:        "host-a",
				ArchiveContent: policy,
			})
			t.Cleanup(engine.Close)
			id := "antigravity:" + dbtest.AntigravityTitleID

			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			before := antigravityStoredSession(t, archive, id)
			require.Empty(t, before.Cwd)
			require.Empty(t, before.Project)

			store := testjsonl.WriteAntigravityWorkspaces(t, root, map[string]string{
				dbtest.AntigravityTitleID: `["file:///tmp/ws-proj"]`,
			})
			assert.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			filled := antigravityStoredSession(t, archive, id)
			assert.Equal(t, "/tmp/ws-proj", filled.Cwd)
			assert.Equal(t, "ws_proj", filled.Project)
			assert.Equal(t, before.FileHash, filled.FileHash, "the conversation's files did not change")
			assert.Equal(t, before.MessageCount, filled.MessageCount)

			// An unreadable store keeps the stored working directory when the
			// conversation changes.
			require.NoError(t, os.WriteFile(store, []byte("not a database"), 0o644))
			dbtest.WriteAntigravityTitleTranscript(t, root, "hello again")
			engine.SyncAll(t.Context(), nil)
			changed := antigravityStoredSession(t, archive, id)
			assert.NotEqual(t, filled.FileHash, changed.FileHash, "the changed conversation was reparsed")
			assert.Equal(t, "/tmp/ws-proj", changed.Cwd)
		})
	}
}

func antigravityStoredSession(t *testing.T, archive *db.DB, id string) *db.Session {
	t.Helper()
	session, err := archive.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, session)
	return session
}
