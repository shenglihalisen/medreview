package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 临时凭据：正常退出时删除；持久凭据不动。
func TestCleanupEphemeralAuth(t *testing.T) {
	dir := t.TempDir()
	eph := filepath.Join(dir, "users.json")
	pst := filepath.Join(dir, "persist.json")
	for _, p := range []string{eph, pst} {
		if err := os.WriteFile(p, []byte(`{"users":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cleanupEphemeralAuth(&Handler{authCfg: authConfig{Enabled: true, Ephemeral: true, UsersPath: eph}})
	if _, err := os.Stat(eph); !os.IsNotExist(err) {
		t.Fatal("临时凭据应被删除")
	}

	cleanupEphemeralAuth(&Handler{authCfg: authConfig{Enabled: true, Ephemeral: false, UsersPath: pst}})
	if _, err := os.Stat(pst); err != nil {
		t.Fatal("持久凭据不应被删除")
	}
}
