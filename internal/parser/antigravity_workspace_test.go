package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestAntigravityProviderUsesRecordedWorkspace(t *testing.T) {
	root := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	dbPath := filepath.Join(root, "conversations", id+".db")
	writeAntigravityIDEProviderFixture(t, root, id)
	testjsonl.WriteAntigravityWorkspaces(t, root, map[string]string{
		id:                                     `["file:///tmp/ws-proj","file:///tmp/second-folder"]`,
		"bbbbbbbb-cccc-dddd-eeee-ffffffffffff": `["file:///tmp/other-proj"]`,
	})

	provider, ok := NewProvider(AgentAntigravity, ProviderConfig{
		Roots: []string{root}, Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	var streamed []SourceRef
	require.NoError(t, provider.(StreamingDiscoverer).DiscoverEach(t.Context(), func(source SourceRef) error {
		streamed = append(streamed, source)
		return nil
	}))
	require.Len(t, streamed, 1)
	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: id})
	require.NoError(t, err)
	require.True(t, ok)
	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: dbPath + "-wal", EventKind: "write",
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)

	for name, source := range map[string]SourceRef{
		"discover":      discovered[0],
		"discover each": streamed[0],
		"find source":   found,
		"changed path":  changed[0],
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, SourceCwdResolution{
				State: SourceCwdResolved, Path: "/tmp/ws-proj",
			}, source.CwdResolution)
			assert.Equal(t, "/tmp/ws-proj", source.ProjectHint)
		})
	}

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: discovered[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	session := outcome.Results[0].Result.Session
	assert.Equal(t, "/tmp/ws-proj", session.Cwd)
	assert.Equal(t, "ws_proj", session.Project)
}

func TestAntigravityProviderWorkspaceResolutionStates(t *testing.T) {
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	tests := map[string]struct {
		store func(t *testing.T, root string)
		want  SourceCwdState
	}{
		"no summary store": {
			store: func(*testing.T, string) {},
			want:  SourceCwdUnspecified,
		},
		"conversation not listed": {
			store: func(t *testing.T, root string) {
				testjsonl.WriteAntigravityWorkspaces(t, root, map[string]string{
					"bbbbbbbb-cccc-dddd-eeee-ffffffffffff": `["file:///tmp/other-proj"]`,
				})
			},
			want: SourceCwdUnspecified,
		},
		"no workspace folder": {
			store: func(t *testing.T, root string) {
				testjsonl.WriteAntigravityWorkspaces(t, root, map[string]string{id: `[]`})
			},
			want: SourceCwdUnspecified,
		},
		"store predates workspace column": {
			store: func(t *testing.T, root string) {
				conn, err := sql.Open("sqlite3", filepath.Join(root, AntigravitySummariesDBName))
				require.NoError(t, err)
				defer conn.Close()
				_, err = conn.ExecContext(t.Context(), testjsonl.AntigravityTitleSchema)
				require.NoError(t, err)
				_, err = conn.ExecContext(t.Context(), `INSERT INTO conversation_summaries VALUES (?, 'Title')`, id)
				require.NoError(t, err)
			},
			want: SourceCwdUnspecified,
		},
		"remote development folder": {
			store: func(t *testing.T, root string) {
				testjsonl.WriteAntigravityWorkspaces(t, root, map[string]string{
					id: `["vscode-remote://ssh-remote%2Bbuildbox/home/dev/proj"]`,
				})
			},
			want: SourceCwdAmbiguous,
		},
		"unreadable store": {
			store: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, AntigravitySummariesDBName),
					[]byte("not a sqlite database, long enough to fill a header page"))
			},
			want: SourceCwdUnavailable,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeAntigravityIDEProviderFixture(t, root, id)
			tc.store(t, root)
			provider, ok := NewProvider(AgentAntigravity, ProviderConfig{
				Roots: []string{root}, Machine: "devbox",
			})
			require.True(t, ok)

			discovered, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, discovered, 1)
			assert.Equal(t, SourceCwdResolution{State: tc.want}, discovered[0].CwdResolution)
			assert.Empty(t, discovered[0].ProjectHint)
			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: discovered[0]})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			assert.Empty(t, outcome.Results[0].Result.Session.Cwd)
			assert.Empty(t, outcome.Results[0].Result.Session.Project)
		})
	}
}

func TestAntigravityProviderMarksRemoteWorkspace(t *testing.T) {
	tests := map[string]func(string) ProviderConfig{
		"path rewriter": func(root string) ProviderConfig {
			return ProviderConfig{
				Roots:        []string{root},
				Machine:      "localbox",
				PathRewriter: func(path string) string { return "remote:" + path },
			}
		},
		"foreign source machine": func(root string) ProviderConfig {
			return ProviderConfig{
				Roots:          []string{root},
				Machine:        "localbox",
				SourceMachines: map[string]string{root: "archivebox"},
			}
		},
	}
	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
			writeAntigravityIDEProviderFixture(t, root, id)
			// A folder on this machine with the recorded path's parent as a
			// git repository must not decide the remote session's project.
			localRepo := filepath.Join(t.TempDir(), "conflicting-local-repo")
			workspace := filepath.Join(localRepo, "recorded-project")
			mustMkdir(t, filepath.Join(localRepo, ".git"))
			mustMkdir(t, workspace)
			testjsonl.WriteAntigravityWorkspaces(t, root, map[string]string{
				id: `["file://` + filepath.ToSlash(workspace) + `"]`,
			})

			provider, ok := NewProvider(AgentAntigravity, config(root))
			require.True(t, ok)
			source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: id})
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, SourceCwdResolution{State: SourceCwdRemote}, source.CwdResolution)
			assert.Equal(t, workspace, source.ProjectHint)

			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			assert.Empty(t, outcome.Results[0].Result.Session.Cwd)
			assert.Equal(t, "recorded_project", outcome.Results[0].Result.Session.Project)
		})
	}
}

func TestAntigravityWorkspacePath(t *testing.T) {
	tests := []struct {
		uri  string
		want string
	}{
		{uri: "file:///home/dev/proj", want: "/home/dev/proj"},
		{uri: "file:///home/dev/proj/", want: "/home/dev/proj"},
		{uri: "file:///home/dev/my%20proj", want: "/home/dev/my proj"},
		{uri: "file://localhost/home/dev/proj", want: "/home/dev/proj"},
		{uri: "file:///", want: "/"},
		{uri: "file:///c%3A/Users/dev/proj", want: `c:\Users\dev\proj`},
		{uri: "file:///C:/Users/dev/proj/", want: `C:\Users\dev\proj`},
		{uri: "file:///d%3A", want: `d:\`},
		{uri: "file://fileserver/share/proj", want: `\\fileserver\share\proj`},
		{uri: "vscode-remote://ssh-remote%2Bbuildbox/home/dev/proj", want: ""},
		{uri: "https://example.com/proj", want: ""},
		{uri: "file:relative/proj", want: ""},
		{uri: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.uri, func(t *testing.T) {
			assert.Equal(t, tc.want, antigravityWorkspacePath(tc.uri))
		})
	}
}

func TestAntigravityCLIProviderFallsBackToRecordedWorkspace(t *testing.T) {
	id := "44444444-5555-6666-7777-888888888888"
	tests := map[string]struct {
		history bool
		cache   string
		want    string
	}{
		// The CLI keeps only each folder's latest conversation in its cache,
		// and current history rows carry no conversation ID, so an older
		// conversation is mapped by the summary store alone.
		"only the summary store maps it": {want: "/tmp/store-proj"},
		"history maps it":                {history: true, want: "/tmp/db-proj"},
		"cache maps it": {
			cache: `{"/tmp/cache-proj":"` + id + `"}`,
			want:  "/tmp/cache-proj",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeAntigravityCLIProviderFixture(t, root, id)
			if !tc.history {
				require.NoError(t, os.Remove(filepath.Join(root, "history.jsonl")))
			}
			if tc.cache != "" {
				cachePath := filepath.Join(root, "cache", "last_conversations.json")
				mustMkdir(t, filepath.Dir(cachePath))
				mustWrite(t, cachePath, []byte(tc.cache))
			}
			testjsonl.WriteAntigravityWorkspaces(t, root, map[string]string{
				id: `["file:///tmp/store-proj"]`,
			})
			provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
				Roots: []string{root}, Machine: "devbox",
			})
			require.True(t, ok)

			discovered, err := provider.Discover(t.Context())
			require.NoError(t, err)
			var streamed []SourceRef
			require.NoError(t, provider.(StreamingDiscoverer).DiscoverEach(t.Context(), func(source SourceRef) error {
				streamed = append(streamed, source)
				return nil
			}))
			conversation := filepath.Join(root, "conversations", id+".db")
			for name, sources := range map[string][]SourceRef{
				"discover": discovered, "discover each": streamed,
			} {
				source := antigravitySourceAt(t, sources, conversation)
				assert.Equal(t, SourceCwdResolution{
					State: SourceCwdResolved, Path: tc.want,
				}, source.CwdResolution, name)
			}

			// The parse resolves the workspace itself when the source carries
			// none, as for a source handed over without discovery.
			source := antigravitySourceAt(t, discovered, conversation)
			source.CwdResolution = SourceCwdResolution{}
			source.ProjectHint = ""
			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			assert.Equal(t, tc.want, outcome.Results[0].Result.Session.Cwd)
		})
	}
}

func antigravitySourceAt(t *testing.T, sources []SourceRef, path string) SourceRef {
	t.Helper()
	for _, source := range sources {
		if source.DisplayPath == path {
			return source
		}
	}
	require.Failf(t, "source not found", "no source at %s", path)
	return SourceRef{}
}
