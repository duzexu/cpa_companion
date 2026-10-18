package companion

import (
	"path/filepath"
	"testing"
)

func TestUsersPersistInSQLiteAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	wanted := user{ID: "123", Username: "alice", Name: "Alice", AvatarURL: "https://example.com/avatar.png",
		PlanID: "plan-1", ResetUsed: map[string]map[string]int{"plan-1": {"account-1": 2}}, Disabled: true, SessionVersion: 3}
	if err := s.upsertUser(wanted); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, ok := reopened.getUser(wanted.ID)
	if !ok || loaded.Username != wanted.Username || loaded.Name != wanted.Name || loaded.AvatarURL != wanted.AvatarURL || loaded.PlanID != wanted.PlanID ||
		loaded.ResetUsed["plan-1"]["account-1"] != 2 || !loaded.Disabled || loaded.SessionVersion != 3 {
		t.Fatalf("SQLite user did not survive restart: %+v", loaded)
	}
}
