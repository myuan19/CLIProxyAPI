package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type deleteTestStore struct {
	deleted []string
}

func (s *deleteTestStore) List(ctx context.Context) ([]*coreauth.Auth, error) {
	_ = ctx
	return nil, nil
}

func (s *deleteTestStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	_ = ctx
	_ = auth
	return "", nil
}

func (s *deleteTestStore) Delete(ctx context.Context, id string) error {
	_ = ctx
	s.deleted = append(s.deleted, id)
	return nil
}

func TestDeleteAuthFileSupportsBatchQueryNames(t *testing.T) {
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	writeTestFile(t, authDir, "a.json", `{"a":1}`)
	writeTestFile(t, authDir, "b.json", `{"b":2}`)

	store := &deleteTestStore{}
	handler := &Handler{
		cfg:         &config.Config{AuthDir: authDir},
		authManager: coreauth.NewManager(nil, nil, nil),
		tokenStore:  store,
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/auth-files?names=a.json,b.json", nil)

	handler.DeleteAuthFile(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var resp struct {
		Status       string   `json:"status"`
		Requested    int      `json:"requested"`
		Deleted      int      `json:"deleted"`
		DeletedNames []string `json:"deleted_names"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("status field = %q, want ok", resp.Status)
	}
	if resp.Requested != 2 || resp.Deleted != 2 {
		t.Fatalf("requested/deleted = %d/%d, want 2/2", resp.Requested, resp.Deleted)
	}
	if len(resp.DeletedNames) != 2 {
		t.Fatalf("deleted_names len = %d, want 2", len(resp.DeletedNames))
	}
	if len(store.deleted) != 2 {
		t.Fatalf("token store deleted len = %d, want 2", len(store.deleted))
	}
}

func TestDeleteAuthFileSupportsBatchBodyNames(t *testing.T) {
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	writeTestFile(t, authDir, "a.json", `{"a":1}`)
	writeTestFile(t, authDir, "b.json", `{"b":2}`)

	handler := &Handler{
		cfg:         &config.Config{AuthDir: authDir},
		authManager: coreauth.NewManager(nil, nil, nil),
		tokenStore:  &deleteTestStore{},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	body := strings.NewReader(`{"names":["a.json","b.json"]}`)
	req := httptest.NewRequest(http.MethodDelete, "/auth-files", body)
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	handler.DeleteAuthFile(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
}

func TestDeleteAuthFileSingleMissingStillReturnsNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := &Handler{
		cfg:         &config.Config{AuthDir: t.TempDir()},
		authManager: coreauth.NewManager(nil, nil, nil),
		tokenStore:  &deleteTestStore{},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/auth-files?name=missing.json", nil)

	handler.DeleteAuthFile(ctx)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
}

func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := dir + "/" + name
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
