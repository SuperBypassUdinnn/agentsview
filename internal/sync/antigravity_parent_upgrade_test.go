package sync_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// Parser version 146 did not read the summary store's parent, so archived
// Antigravity sub-agent conversations have no parent. Their working directory
// can already be right, which gives sync no other reason to reparse an
// unchanged source. The upgrade must link them in place.
func TestAntigravityParentUpgradeReparsesUnchangedSource(t *testing.T) {
	const (
		parentID        = "99999999-8888-7777-6666-555555555555"
		previousVersion = 146
	)
	tests := map[string]struct {
		agent  parser.AgentType
		id     string
		source func(t *testing.T, root string)
	}{
		"app and IDE": {
			agent: parser.AgentAntigravity,
			id:    dbtest.AntigravityTitleID,
			source: func(t *testing.T, root string) {
				t.Helper()
				dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
				dbPath := filepath.Join(root, "conversations", dbtest.AntigravityTitleID+".db")
				require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
				copyAntigravityCLITestSchemaTemplate(t, dbPath)
			},
		},
		"CLI": {
			agent: parser.AgentAntigravityCLI,
			id:    "12121212-3434-5656-7878-909090909090",
			source: func(t *testing.T, root string) {
				t.Helper()
				convDir := filepath.Join(root, "conversations")
				require.NoError(t, os.MkdirAll(convDir, 0o755))
				createAntigravityCLIDisplayStepDB(t,
					filepath.Join(convDir, "12121212-3434-5656-7878-909090909090.db"),
					"Read calc.py and report its functions")
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			tc.source(t, root)
			testjsonl.WriteAntigravityWorkspaces(t, root, map[string]string{tc.id: `["file:///tmp/ws-proj"]`})
			testjsonl.WriteAntigravityParents(t, root, map[string]string{tc.id: parentID})
			sessionID := string(tc.agent) + ":" + tc.id
			wantParent := string(tc.agent) + ":" + parentID

			archivePath := filepath.Join(t.TempDir(), "archive.db")
			database, err := db.Open(t.Context(), archivePath)
			require.NoError(t, err)
			config := sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{tc.agent: {root}},
				Machine:   "local",
			}
			engine := sync.NewEngine(t.Context(), database, config)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			before, err := database.GetSessionFull(t.Context(), sessionID)
			require.NoError(t, err)
			require.NotNil(t, before)
			require.Equal(t, &wantParent, before.ParentSessionID)
			assert.Equal(t, "subagent", before.RelationshipType)
			require.Equal(t, "/tmp/ws-proj", before.Cwd)
			hashBefore, ok := database.GetSessionFileHash(t.Context(), sessionID)
			require.True(t, ok)
			engine.Close()
			require.NoError(t, database.Close())

			// Model an archive version 146 wrote: the working directory is
			// filled, the parent is missing, and the source files are untouched.
			archive, err := sql.Open("sqlite3", archivePath)
			require.NoError(t, err)
			_, err = archive.ExecContext(t.Context(), `UPDATE sessions SET parent_session_id = NULL,
				parser_parent_session_id = NULL, relationship_type = '', parser_relationship_type = NULL,
				data_version = ? WHERE id = ?`, previousVersion, sessionID)
			require.NoError(t, err)
			_, err = archive.ExecContext(t.Context(), "PRAGMA user_version = 146")
			require.NoError(t, err)
			require.NoError(t, archive.Close())

			assert.Equal(t,
				[]parser.AgentType{parser.AgentAntigravity, parser.AgentAntigravityCLI},
				db.ScopedDataVersionProviders(previousVersion, previousVersion+1),
				"the parent upgrade must reparse only Antigravity sources in place")
			reopened, err := db.Open(t.Context(), archivePath)
			require.NoError(t, err)
			defer func() { require.NoError(t, reopened.Close()) }()
			stale, err := reopened.GetSessionFull(t.Context(), sessionID)
			require.NoError(t, err)
			require.Nil(t, stale.ParentSessionID)
			upgraded := sync.NewEngine(t.Context(), reopened, config)
			defer upgraded.Close()
			require.Equal(t, 1, upgraded.SyncAll(t.Context(), nil).Synced,
				"the version bump must bypass freshness checks on unchanged sources")

			after, err := reopened.GetSessionFull(t.Context(), sessionID)
			require.NoError(t, err)
			assert.Equal(t, &wantParent, after.ParentSessionID)
			assert.Equal(t, "subagent", after.RelationshipType)
			assert.Equal(t, before.ID, after.ID)
			assert.Equal(t, before.FilePath, after.FilePath)
			hashAfter, ok := reopened.GetSessionFileHash(t.Context(), sessionID)
			require.True(t, ok)
			assert.Equal(t, hashBefore, hashAfter)
			assert.Equal(t, db.CurrentDataVersion(), reopened.GetSessionDataVersion(t.Context(), sessionID))
			counter, err := sql.Open("sqlite3", "file:"+archivePath+"?mode=ro")
			require.NoError(t, err)
			defer counter.Close()
			var sessions int
			require.NoError(t, counter.QueryRowContext(t.Context(), "SELECT count(*) FROM sessions").Scan(&sessions))
			assert.Equal(t, 1, sessions, "the upgrade must not create a duplicate session")
		})
	}
}
