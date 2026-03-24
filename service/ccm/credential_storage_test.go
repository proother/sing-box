package ccm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type fakeJSONStorage struct {
	container map[string]json.RawMessage
	writeErr  error
	deleted   bool
}

func (s *fakeJSONStorage) readContainer() (map[string]json.RawMessage, bool, error) {
	if s.container == nil {
		return make(map[string]json.RawMessage), false, nil
	}
	cloned := make(map[string]json.RawMessage, len(s.container))
	for key, value := range s.container {
		cloned[key] = value
	}
	return cloned, true, nil
}

func (s *fakeJSONStorage) writeContainer(container map[string]json.RawMessage) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	s.container = make(map[string]json.RawMessage, len(container))
	for key, value := range container {
		s.container[key] = value
	}
	return nil
}

func (s *fakeJSONStorage) delete() error {
	s.deleted = true
	s.container = nil
	return nil
}

func TestPersistStorageValueDeletesFallbackOnPrimarySuccess(t *testing.T) {
	t.Parallel()

	primary := &fakeJSONStorage{}
	fallback := &fakeJSONStorage{container: map[string]json.RawMessage{"stale": json.RawMessage(`true`)}}
	if err := persistStorageValue(primary, fallback, "claudeAiOauth", &oauthCredentials{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	if !fallback.deleted {
		t.Fatal("expected fallback storage to be deleted after primary write")
	}
}

func TestPersistStorageValueDeletesPrimaryAfterFallbackSuccess(t *testing.T) {
	t.Parallel()

	primary := &fakeJSONStorage{
		container: map[string]json.RawMessage{"claudeAiOauth": json.RawMessage(`{"accessToken":"old"}`)},
		writeErr:  os.ErrPermission,
	}
	fallback := &fakeJSONStorage{}
	if err := persistStorageValue(primary, fallback, "claudeAiOauth", &oauthCredentials{AccessToken: "new"}); err != nil {
		t.Fatal(err)
	}
	if !primary.deleted {
		t.Fatal("expected primary storage to be deleted after fallback write")
	}
}

func TestWriteCredentialsToFilePreservesTopLevelKeys(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, ".credentials.json")
	initial := []byte(`{"keep":{"nested":true},"claudeAiOauth":{"accessToken":"old"}}`)
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeCredentialsToFile(&oauthCredentials{AccessToken: "new"}, path); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var container map[string]json.RawMessage
	if err := json.Unmarshal(data, &container); err != nil {
		t.Fatal(err)
	}
	if _, exists := container["keep"]; !exists {
		t.Fatal("expected unknown top-level key to be preserved")
	}
}

func TestWriteClaudeCodeOAuthAccountPreservesTopLevelKeys(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, ".claude.json")
	initial := []byte(`{"keep":{"nested":true},"oauthAccount":{"accountUuid":"old"}}`)
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeClaudeCodeOAuthAccount(path, &claudeOAuthAccount{AccountUUID: "new"}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var container map[string]json.RawMessage
	if err := json.Unmarshal(data, &container); err != nil {
		t.Fatal(err)
	}
	if _, exists := container["keep"]; !exists {
		t.Fatal("expected unknown config key to be preserved")
	}
}
