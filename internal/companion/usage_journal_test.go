package companion

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestUsageJournalSurvivesRestartAndSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	db, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.savePlan(routingPlan{Name: "metered", AccountIDs: []string{"account"}, BudgetPicoUSD: picoUSD}); err != nil {
		t.Fatal(err)
	}
	plan := db.plans()[0]
	snapshot, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := db.recordUsage(plan.ID, 10_000_000, ""); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(snapshot, after) {
		t.Fatalf("per-request usage rewrote store.json: %v", err)
	}
	reopened, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := reopened.getPlan(plan.ID)
	if stored.UsedPicoUSD != 200_000_000 {
		t.Fatalf("journal was not replayed: %d", stored.UsedPicoUSD)
	}
	if err := reopened.upsertUser(user{ID: "alice", Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.recordUsage(plan.ID, 30_000_000, "metering failed"); err != nil {
		t.Fatal(err)
	}
	again, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ = again.getPlan(plan.ID)
	if stored.UsedPicoUSD != 230_000_000 || stored.UsageError != "metering failed" {
		t.Fatalf("snapshot checkpoint replayed incorrectly: %+v", stored)
	}
}

func TestUsageJournalDiscardsTornTailAndRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	db, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.savePlan(routingPlan{Name: "metered", AccountIDs: []string{"account"}, BudgetPicoUSD: picoUSD}); err != nil {
		t.Fatal(err)
	}
	plan := db.plans()[0]
	if err := db.recordUsage(plan.ID, 10, ""); err != nil {
		t.Fatal(err)
	}
	journal := db.usageJournalPath()
	f, err := os.OpenFile(journal, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"plan_id":"torn"`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	reopened, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.recordUsage(plan.ID, 20, ""); err != nil {
		t.Fatal(err)
	}
	if err := reopened.rotateUsageJournal(rotateUsageJournalAt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("old journal still present: %v", err)
	}
	again, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := again.getPlan(plan.ID)
	if stored.UsedPicoUSD != 30 {
		t.Fatalf("rotation changed usage: %d", stored.UsedPicoUSD)
	}
}

func TestUnchangedOAuthProfileDoesNotWriteAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	db, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	profile := user{ID: "alice", Username: "alice", Name: "Alice"}
	if err := db.upsertUser(profile); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := db.usersDB.QueryRow(`SELECT total_changes()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := db.upsertUser(profile); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := db.usersDB.QueryRow(`SELECT total_changes()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("unchanged OAuth login wrote the users database")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("OAuth profile unexpectedly wrote store.json: %v", err)
	}
}
