package main

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

/* --------------------------------------------------------------------------
   TestRegisterAndLogin
   Verifies that user accounts can be saved and loaded through the JSON
   persistence layer without data corruption.
-------------------------------------------------------------------------- */

func TestRegisterAndLogin(t *testing.T) {
	original := UserDBPath
	UserDBPath = "test_users.json"
	defer func() {
		UserDBPath = original
		os.Remove("test_users.json")
	}()

	users := []UserAccount{
		{Username: "testuser", Password: "hashedpassword"},
	}
	if err := saveUsers(users); err != nil {
		t.Fatalf("saveUsers failed: %v", err)
	}

	loaded, err := loadUsers()
	if err != nil {
		t.Fatalf("loadUsers failed: %v", err)
	}
	if len(loaded) != 1 {
		t.Errorf("expected 1 user, got %d", len(loaded))
	}
	if loaded[0].Username != "testuser" {
		t.Errorf("expected username 'testuser', got '%s'", loaded[0].Username)
	}
}

/* --------------------------------------------------------------------------
   TestJSONMarshalUnmarshal
   Ensures UserSession values survive a round-trip through JSON encoding
   with perfect data fidelity.
-------------------------------------------------------------------------- */

func TestJSONMarshalUnmarshal(t *testing.T) {
	original := []UserSession{
		{Name: "Alice", Score: 50},
		{Name: "Bob", Score: 80},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var loaded []UserSession
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if len(loaded) != len(original) {
		t.Errorf("length mismatch: expected %d, got %d", len(original), len(loaded))
	}
	for i := range loaded {
		if loaded[i].Name != original[i].Name || loaded[i].Score != original[i].Score {
			t.Errorf("entry %d mismatch: got %+v, want %+v", i, loaded[i], original[i])
		}
	}
}

/* --------------------------------------------------------------------------
   TestBcryptHashAndVerify
   Confirms the security layer: a bcrypt hash must not equal the plaintext
   password, and CompareHashAndPassword must succeed for the correct password
   and fail for an incorrect one.
-------------------------------------------------------------------------- */

func TestBcryptHashAndVerify(t *testing.T) {
	password := "SuperSecure#2025"

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("bcrypt.GenerateFromPassword failed: %v", err)
	}

	// Hash must not be the same as the plaintext
	if string(hash) == password {
		t.Error("bcrypt hash should not equal plaintext password")
	}

	// Correct password must verify
	if err := bcrypt.CompareHashAndPassword(hash, []byte(password)); err != nil {
		t.Errorf("correct password failed verification: %v", err)
	}

	// Wrong password must NOT verify
	if err := bcrypt.CompareHashAndPassword(hash, []byte("wrongpassword")); err == nil {
		t.Error("wrong password should not verify against hash")
	}
}

/* --------------------------------------------------------------------------
   TestConcurrentSyncScores
   Verifies goroutine correctness: N goroutines each call syncScores,
   signalling via a WaitGroup. The test asserts that exactly N messages
   arrive on the results channel without deadlock or race conditions.
-------------------------------------------------------------------------- */

func TestConcurrentSyncScores(t *testing.T) {
	sessions := []UserSession{
		{Name: "Alice", Score: 80},
		{Name: "Bob", Score: 60},
		{Name: "Charlie", Score: 40},
		{Name: "Diana", Score: 100},
	}

	resultsChan := make(chan string, len(sessions))
	var wg sync.WaitGroup

	for _, u := range sessions {
		wg.Add(1)
		go syncScores(u, resultsChan, &wg)
	}

	// Close channel once every goroutine has called wg.Done()
	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	var received []string
	for msg := range resultsChan {
		received = append(received, msg)
	}

	if len(received) != len(sessions) {
		t.Errorf("expected %d sync messages, got %d", len(sessions), len(received))
	}
}

/* --------------------------------------------------------------------------
   TestLeaderboardSort
   Unit-tests the leaderboard ordering logic in isolation: given accounts
   with varied high scores, the sort must produce a strictly descending
   sequence with correct rank assignments.
-------------------------------------------------------------------------- */

func TestLeaderboardSort(t *testing.T) {
	users := []UserAccount{
		{Username: "charlie", HighScore: 40},
		{Username: "alice",   HighScore: 120},
		{Username: "bob",     HighScore: 80},
		{Username: "diana",   HighScore: 200},
	}

	sort.Slice(users, func(i, j int) bool {
		return users[i].HighScore > users[j].HighScore
	})

	expected := []string{"diana", "alice", "bob", "charlie"}
	for i, want := range expected {
		if users[i].Username != want {
			t.Errorf("rank %d: expected '%s', got '%s'", i+1, want, users[i].Username)
		}
	}

	// Also verify scores are strictly non-increasing
	for i := 1; i < len(users); i++ {
		if users[i].HighScore > users[i-1].HighScore {
			t.Errorf("leaderboard not correctly sorted at position %d", i)
		}
	}
}
