package httpapi_test

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/scottjab/folio/internal/httpapi"
	"github.com/scottjab/folio/internal/mcpsrv"
	"github.com/scottjab/folio/internal/notes"
)

type mcpPeerTransport struct {
	base   http.RoundTripper
	peer   string
	replay atomic.Pointer[string]
}

func (tr *mcpPeerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Test-Peer", tr.peer)
	if id := tr.replay.Load(); id != nil {
		r.Header.Set("Mcp-Session-Id", *id)
	}
	return tr.base.RoundTrip(r)
}

func TestMCPRequestsUseCurrentIdentity(t *testing.T) {
	h := newHarness(t, func(d *httpapi.Deps) {
		d.Notes = notes.New(notes.Deps{
			DB: d.DB, Index: d.Index, Vaults: d.Vaults,
			Identity: d.Identity, Shares: d.Shares, Bus: d.Bus,
		})
		d.MCP = mcpsrv.New(mcpsrv.Deps{
			Notes: d.Notes, Index: d.Index, Identity: d.Identity, Shares: d.Shares,
		}).HTTPHandler(httpapi.UserFrom)
	})
	alice := h.as("100.64.0.1", aliceWho)
	bob := h.as("100.64.0.2", bobWho)
	connect := func(c *client) (*mcp.ClientSession, *mcpPeerTransport) {
		t.Helper()
		tr := &mcpPeerTransport{base: h.srv.Client().Transport, peer: c.ip}
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(
			t.Context(), &mcp.StreamableClientTransport{
				Endpoint: h.srv.URL + "/mcp", HTTPClient: &http.Client{Transport: tr},
			}, &mcp.ClientSessionOptions{ProtocolVersion: "2025-03-26"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			tr.replay.Store(nil)
			cs.Close()
		})
		return cs, tr
	}
	aliceMCP, _ := connect(alice)
	bobMCP, bobTransport := connect(bob)
	call := func(cs *mcp.ClientSession, name string, args map[string]any, denied bool) *mcp.CallToolResult {
		t.Helper()
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError != denied {
			t.Errorf("%s: isError = %v, want %v: %+v", name, res.IsError, denied, res)
		}
		return res
	}
	read := func(cs *mcp.ClientSession, want string) {
		t.Helper()
		res := call(cs, "read_note", map[string]any{"path": "private.md"}, false)
		data, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var note struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(data, &note); err != nil {
			t.Fatal(err)
		}
		if note.Content != want {
			t.Errorf("read_note returned %q, want current user's %q", note.Content, want)
		}
	}
	call(aliceMCP, "create_note", map[string]any{"path": "private.md", "content": "alice private"}, false)
	call(bobMCP, "create_note", map[string]any{"path": "private.md", "content": "bob private"}, false)
	var aliceNote, bobNote struct {
		Content string `json:"content"`
	}
	wantStatus(t, alice.getJSON("/api/vaults/me/notes/private.md", &aliceNote), http.StatusOK)
	wantStatus(t, bob.getJSON("/api/vaults/me/notes/private.md", &bobNote), http.StatusOK)
	var grant struct {
		ID string `json:"id"`
	}
	resp := alice.do("POST", "/api/shares", map[string]any{
		"path": "private.md", "grantee": "bob@github", "perm": "read",
	})
	wantStatus(t, resp, http.StatusCreated)
	decodeInto(t, resp, &grant)
	call(bobMCP, "read_note", map[string]any{"vault": "alice-github", "path": "private.md"}, false)
	resp = alice.do("DELETE", "/api/shares/"+grant.ID, nil)
	wantStatus(t, resp, http.StatusNoContent)
	resp.Body.Close()

	// A leaked session header must not override the freshly authenticated peer.
	replay := aliceMCP.ID()
	if replay == "" {
		replay = "replayed-session-id"
	}
	bobTransport.replay.Store(&replay)
	read(bobMCP, bobNote.Content)
	resource, err := bobMCP.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "folio://me/private.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resource.Contents) != 1 || resource.Contents[0].Text != bobNote.Content {
		t.Errorf("resources/read returned another user's note: %+v", resource)
	}
	denial := call(bobMCP, "read_note", map[string]any{"vault": "alice-github", "path": "private.md"}, true)
	data, err := json.Marshal(denial)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "permission denied") {
		t.Errorf("read after revoke did not report permission denied: %s", data)
	}
	_, err = bobMCP.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "folio://alice-github/private.md"})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("resource read after revoke = %v, want permission denied", err)
	}
	call(bobMCP, "create_note", map[string]any{"path": "replay.md", "content": "bob created"}, false)
	resp = bob.do("GET", "/api/vaults/me/notes/replay.md", nil)
	wantStatus(t, resp, http.StatusOK)
	if body := bodyOf(t, resp); !strings.Contains(body, "bob created") {
		t.Errorf("Bob's write did not reach his vault: %s", body)
	}
	read(aliceMCP, aliceNote.Content)
}
