package companion

const testVaultKey = "test-only-key-vault-secret"

func assignPlanForTest(db *store, userID, planID string, disabled bool) error {
	if err := db.assignPlanKeepingStatus(userID, planID); err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	u := db.db.Users[userID]
	u.Disabled = disabled
	return db.saveUserLocked(u)
}
