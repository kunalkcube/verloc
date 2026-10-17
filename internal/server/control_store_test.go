package server

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// privateTestDir tightens Go's test temp dir: this environment creates it
// mode 0775, but management storage refuses group/other-writable parents.
func privateTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func controlTestStore(t *testing.T) (*ControlStore, string, string) {
	t.Helper()
	dir := privateTestDir(t)
	path, keyPath := filepath.Join(dir, "management.db"), filepath.Join(dir, "master.key")
	s, err := OpenControlStore(path, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path, keyPath
}

func controlTestConfig() *Config {
	return &Config{
		Listen: ":8123", ProxyKey: "legacy-proxy-secret-do-not-store", DefaultProvider: "primary",
		Providers: map[string]*ProviderConfig{
			"primary":  {BaseURL: "https://primary.example/v1", Keys: []KeyEntry{{ID: "configured-credential-id", Key: "upstream-private-credential-one", AccountID: "private-account-one"}}},
			"fallback": {BaseURL: "https://fallback.example/v1", NoAuth: true},
		},
		ModelMap:    map[string]string{"stable/model-id": "primary:vendor/upstream"},
		ModelMeta:   map[string]*ModelMeta{"stable/model-id": {Name: "Preserved name", ContextWindow: 12345, PricingInput: 1.25}},
		ModelRoutes: map[string][]RouteTarget{"stable/model-id": {{Backend: "fallback", Upstream: "vendor/alternative"}}},
		Dashboard:   &DashboardConfig{DBPath: "bootstrap-analytics.db", RecentRequests: 6000},
	}
}

func TestControlStoreImportOnceEncryptedAndAuthoritative(t *testing.T) {
	s, path, keyPath := controlTestStore(t)
	seed := controlTestConfig()
	cfg, err := s.LoadOrImportConfig(seed)
	if err != nil {
		t.Fatal(err)
	}
	key := cfg.Providers["primary"].Keys[0]
	if key.ID != "configured-credential-id" || key.Key != seed.Providers["primary"].Keys[0].Key || key.AccountID != "private-account-one" {
		t.Fatalf("credential import lost identity or payload: %#v", key.ID)
	}
	if cfg.ModelMeta["stable/model-id"].Name != "Preserved name" || len(cfg.ModelRoutes["stable/model-id"]) != 1 {
		t.Fatal("model metadata/routes not imported")
	}
	if cfg.ProxyKey != "" || len(cfg.Keys) != 0 {
		t.Fatal("legacy auth escaped normalization")
	}
	var encrypted []byte
	if err := s.db.QueryRow(`SELECT encrypted FROM provider_credentials`).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{key.Key, key.AccountID, seed.ProxyKey} {
		if bytes.Contains(encrypted, []byte(secret)) {
			t.Fatal("credential ciphertext contains plaintext")
		}
	}
	for _, query := range []string{`SELECT settings FROM control_settings`, `SELECT options FROM providers`, `SELECT metadata FROM models`, `SELECT action||subject FROM audit_events`} {
		rows, err := s.db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{key.Key, key.AccountID, seed.ProxyKey} {
				if strings.Contains(value, secret) {
					t.Fatal("plaintext credential leaked into SQL text")
				}
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	for _, p := range []string{path, keyPath, path + "-wal", path + "-shm"} {
		info, err := os.Stat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("non-private storage permissions on %s", filepath.Base(p))
		}
	}
	var fk, timeout int
	var journal string
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if fk != 1 || timeout != 5000 || journal != "wal" {
		t.Fatalf("unsafe SQLite options: %d %d %s", fk, timeout, journal)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenControlStore(path, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// A later seed may even have invalid management state. Only its explicit
	// bootstrap settings can affect the already-imported database.
	changed := &Config{Listen: ":9123", Providers: map[string]*ProviderConfig{"invalid": nil}, ProxyKey: "replacement-proxy-secret", Dashboard: &DashboardConfig{DBPath: "new-analytics.db"}}
	reloaded, err := s.LoadOrImportConfig(changed)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Listen != ":9123" || reloaded.Dashboard.DBPath != "new-analytics.db" {
		t.Fatal("bootstrap settings were not honored")
	}
	if reloaded.Providers["primary"].Keys[0].ID != key.ID || reloaded.ModelMap["stable/model-id"] != "primary:vendor/upstream" {
		t.Fatal("seed overwrote authoritative management state")
	}
	events, err := s.AuditEvents(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Action != "config.import" || events[0].TS <= 0 {
		t.Fatalf("unexpected import audit: %#v", events)
	}
}

func TestControlStoreLoadsOldRetentionJSONWithoutReserializingIt(t *testing.T) {
	s, path, keyPath := controlTestStore(t)
	seed := controlTestConfig()
	if err := json.Unmarshal([]byte(`{"dashboard":{"retention_days":-1,"recent_requests":6000}}`), seed); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.LoadOrImportConfig(seed)
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err = s.db.QueryRow(`SELECT settings FROM control_settings WHERE id=1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var settings map[string]json.RawMessage
	if err = json.Unmarshal([]byte(raw), &settings); err != nil {
		t.Fatal(err)
	}
	settings["retention_days"] = json.RawMessage(`-1`)
	oldSettings, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE control_settings SET settings=? WHERE id=1`, string(oldSettings)); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenControlStore(path, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg, err = s.LoadOrImportConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Dashboard.RecentRequests != 6000 {
		t.Fatal("old JSON lost supported dashboard settings")
	}
	if err = s.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	exported, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT settings FROM control_settings WHERE id=1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "retention_days") || bytes.Contains(exported, []byte("retention_days")) {
		t.Fatal("removed retention setting survived serialization")
	}
}

func TestControlStoreSaveAtomicAndStableIdentity(t *testing.T) {
	s, _, _ := controlTestStore(t)
	cfg, err := s.LoadOrImportConfig(controlTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	originalID := cfg.Providers["primary"].Keys[0].ID
	// Old string-style clients omit IDs when replacing unchanged credentials.
	cfg.Providers["primary"].Keys[0].ID = ""
	cfg.Providers["primary"].Disabled = true
	cfg.DisabledModels["stable/model-id"] = true
	cfg.Dashboard.RecentRequests = 9100
	if err := s.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Providers["primary"].Keys[0].ID != originalID {
		t.Fatal("unchanged credential identity was replaced")
	}
	before, err := s.LoadOrImportConfig(&Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !before.Providers["primary"].Disabled || !before.DisabledModels["stable/model-id"] || before.Dashboard.RecentRequests != 9100 {
		t.Fatal("management fields not committed together")
	}
	beforeJSON, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	beforeAudit, err := s.AuditEvents(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	// Force a failure after providers/credentials have already been replaced
	// inside the transaction, not merely a pre-validation failure.
	if _, err := s.db.Exec(`CREATE TRIGGER reject_model BEFORE INSERT ON models BEGIN SELECT RAISE(ABORT,'forced test failure'); END`); err != nil {
		t.Fatal(err)
	}
	cfg.Providers["primary"].BaseURL = "https://mutated.example/v1"
	cfg.Providers["primary"].Keys = []KeyEntry{{Key: "new-uncommitted-upstream-secret"}}
	cfg.ModelMeta["stable/model-id"].Name = "Uncommitted name"
	cfg.Dashboard.RecentRequests = 4000
	if err := s.SaveConfig(cfg); err == nil {
		t.Fatal("expected transactional SQL failure")
	}
	if cfg.Providers["primary"].Keys[0].ID != "" {
		t.Fatal("failed transaction published a new identity")
	}
	after, err := s.LoadOrImportConfig(&Config{})
	if err != nil {
		t.Fatal(err)
	}
	afterJSON, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeJSON, afterJSON) {
		t.Fatal("failed save partially changed management state")
	}
	afterAudit, err := s.AuditEvents(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterAudit) != len(beforeAudit) {
		t.Fatal("failed save committed its audit event")
	}
}

func TestControlStoreWrongOrMissingKeyDoesNotMutate(t *testing.T) {
	s, path, keyPath := controlTestStore(t)
	if _, err := s.LoadOrImportConfig(controlTestConfig()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wrong := filepath.Join(filepath.Dir(path), "wrong.key")
	if err := os.WriteFile(wrong, bytes.Repeat([]byte{42}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenControlStore(path, wrong); err == nil {
		opened.Close()
		t.Fatal("wrong master key accepted")
	}
	missing := filepath.Join(filepath.Dir(path), "missing.key")
	if opened, err := OpenControlStore(path, missing); err == nil {
		opened.Close()
		t.Fatal("missing master key accepted")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("missing key regenerated for encrypted database")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed key verification mutated the database")
	}
	reopened, err := OpenControlStore(path, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.LoadOrImportConfig(&Config{}); err != nil {
		t.Fatal(err)
	}
}

func TestControlStoreBackupConsistentAndDecryptable(t *testing.T) {
	s, _, keyPath := controlTestStore(t)
	cfg, err := s.LoadOrImportConfig(controlTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO admin_auth(id,password_salt,password_hash,created_at) VALUES(1,?,?,?)`, []byte("salt"), []byte("hash"), int64(1234)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO admin_sessions(token_hash,csrf_token,expires_at) VALUES(?,?,?)`, []byte("session-hash"), "csrf", int64(999999)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO client_keys(id,name,key_hash,prefix,created_at) VALUES('client','Client',?,'vk_prefix',1234)`, []byte("client-hash")); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(privateTestDir(t), "snapshot.db")
	if err := s.Backup(backupPath); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(backupPath); err == nil {
		t.Fatal("backup overwrote an existing path")
	}
	// A later source commit cannot bleed into the completed snapshot.
	cfg.ModelMeta["stable/model-id"].Name = "Changed after snapshot"
	if err := s.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	backup, err := OpenControlStore(backupPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	restored, err := backup.LoadOrImportConfig(&Config{})
	if err != nil {
		t.Fatal(err)
	}
	if restored.ModelMeta["stable/model-id"].Name != "Preserved name" || restored.Providers["primary"].Keys[0].Key != "upstream-private-credential-one" {
		t.Fatal("backup not a decryptable consistent snapshot")
	}
	for _, table := range []string{"admin_auth", "admin_sessions", "client_keys"} {
		var count int
		if err := backup.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("backup omitted %s", table)
		}
	}
}

func TestControlStoreLegacyImportAndEmbeddedMetadata(t *testing.T) {
	s, _, _ := controlTestStore(t)
	var embedded []modelInfo
	if err := json.Unmarshal(modelsJSON, &embedded); err != nil {
		t.Fatal(err)
	}
	if len(embedded) == 0 {
		t.Fatal("missing embedded model catalog")
	}
	m := embedded[0]
	cfg, err := s.LoadOrImportConfig(&Config{DefaultProvider: "legacy", UpstreamBase: "https://legacy.example/v1", Keys: []string{"legacy-upstream-secret"}, ProxyKey: "legacy-proxy-secret", ModelMap: map[string]string{m.ID: "legacy:preserved-upstream"}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers["legacy"].Keys[0].Key != "legacy-upstream-secret" || cfg.Providers["legacy"].Keys[0].ID == "" {
		t.Fatal("legacy provider credentials not migrated")
	}
	if cfg.ModelMap[m.ID] != "legacy:preserved-upstream" {
		t.Fatal("legacy configured routing was changed")
	}
	meta := cfg.ModelMeta[m.ID]
	if meta == nil || meta.Name != m.Name || meta.ContextWindow != m.ContextWindow || meta.PricingInput != m.PricingPerMillion.Input {
		t.Fatal("embedded configured-ID metadata not imported")
	}
	if meta.PricingKnown == nil || !*meta.PricingKnown {
		t.Fatal("catalog-derived pricing was not marked known")
	}
	if cfg.UpstreamBase != "" || len(cfg.Keys) != 0 || cfg.ProxyKey != "" {
		t.Fatal("legacy credential-bearing fields survived normalization")
	}
}

func TestControlStoreImportIsOneTransaction(t *testing.T) {
	s, _, _ := controlTestStore(t)
	var wg sync.WaitGroup
	results := make(chan *Config, 2)
	errors := make(chan error, 2)
	for _, name := range []string{"First", "Second"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			seed := controlTestConfig()
			seed.ModelMeta["stable/model-id"].Name = name
			cfg, err := s.LoadOrImportConfig(seed)
			results <- cfg
			errors <- err
		}(name)
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first string
	for cfg := range results {
		name := cfg.ModelMeta["stable/model-id"].Name
		if first == "" {
			first = name
		} else if first != name {
			t.Fatal("concurrent import returned different authoritative state")
		}
	}
	events, err := s.AuditEvents(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatal("concurrent import committed more than once")
	}
}

func TestControlStoreCredentialBindingRejectsCiphertextSwap(t *testing.T) {
	s, path, keyPath := controlTestStore(t)
	seed := controlTestConfig()
	seed.Providers["primary"].Keys = append(seed.Providers["primary"].Keys, KeyEntry{Key: "second-private-upstream-key"})
	cfg, err := s.LoadOrImportConfig(seed)
	if err != nil {
		t.Fatal(err)
	}
	keys := cfg.Providers["primary"].Keys
	var first, second []byte
	if err := s.db.QueryRow(`SELECT encrypted FROM provider_credentials WHERE id=?`, keys[0].ID).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT encrypted FROM provider_credentials WHERE id=?`, keys[1].ID).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE provider_credentials SET encrypted=CASE WHEN id=? THEN ? ELSE ? END WHERE provider_id='primary'`, keys[0].ID, second, first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadOrImportConfig(&Config{}); err == nil {
		t.Fatal("credential ciphertext accepted under a different identity")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := OpenControlStore(path, keyPath); err == nil {
		reopened.Close()
		t.Fatal("corrupted credential database opened")
	}
}

func TestControlStoreFailedImportCanRetryAndEmptySetupIsDurable(t *testing.T) {
	s, _, _ := controlTestStore(t)
	if _, err := s.db.Exec(`CREATE TRIGGER reject_import BEFORE INSERT ON models BEGIN SELECT RAISE(ABORT,'forced import failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadOrImportConfig(controlTestConfig()); err == nil {
		t.Fatal("expected import failure")
	}
	for _, query := range []string{`SELECT count(*) FROM providers`, `SELECT count(*) FROM provider_credentials`, `SELECT count(*) FROM control_settings`, `SELECT count(*) FROM audit_events`, `SELECT count(*) FROM control_meta WHERE name='imported'`} {
		var count int
		if err := s.db.QueryRow(query).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatal("failed import committed partial management state")
		}
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_import`); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.LoadOrImportConfig(&Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 0 || len(cfg.ModelMap) != 0 {
		t.Fatal("empty setup imported unexpected state")
	}
	cfg, err = s.LoadOrImportConfig(controlTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 0 || len(cfg.ModelMap) != 0 {
		t.Fatal("later seed replaced initialized empty setup")
	}
}

func TestControlStorePreservesExistingDirectoryPermissions(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "existing")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := OpenControlStore(filepath.Join(existing, "control.db"), filepath.Join(existing, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(existing)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatal("existing user directory permissions changed")
	}
	created := filepath.Join(root, "new", "private")
	s, err = OpenControlStore(filepath.Join(created, "control.db"), filepath.Join(created, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(created)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatal("new directory is not private")
	}
	unsafe := filepath.Join(root, "unsafe")
	if err := os.Mkdir(unsafe, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafe, 0o777); err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenControlStore(filepath.Join(unsafe, "control.db"), filepath.Join(unsafe, "master.key")); err == nil {
		opened.Close()
		t.Fatal("group/other-writable management directory accepted")
	}
	info, err = os.Stat(unsafe)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o777 {
		t.Fatal("unsafe existing directory permissions changed")
	}
}

func TestControlStorePreservesExplicitUnknownPricing(t *testing.T) {
	s, _, _ := controlTestStore(t)
	var embedded []modelInfo
	if err := json.Unmarshal(modelsJSON, &embedded); err != nil {
		t.Fatal(err)
	}
	if len(embedded) == 0 {
		t.Fatal("missing embedded model catalog")
	}
	id := embedded[0].ID
	unknown := false
	seed := controlTestConfig()
	seed.ModelMap = map[string]string{id: "primary:override-upstream"}
	seed.ModelMeta = map[string]*ModelMeta{id: {Name: "Deliberately unknown price", PricingKnown: &unknown}}
	seed.ModelRoutes = nil
	cfg, err := s.LoadOrImportConfig(seed)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModelMeta[id].PricingKnown == nil || *cfg.ModelMeta[id].PricingKnown {
		t.Fatal("catalog replaced explicit unknown pricing")
	}
	if err := s.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	reloaded, err := s.LoadOrImportConfig(&Config{})
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ModelMeta[id].PricingKnown == nil || *reloaded.ModelMeta[id].PricingKnown {
		t.Fatal("save/reload lost explicit unknown pricing")
	}
}
