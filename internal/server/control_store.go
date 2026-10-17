package server

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const controlSchemaVersion = 1

// ControlStore owns management state. Bootstrap paths/listeners stay outside SQL;
// upstream credentials are individually authenticated with an external key.
// The single connection also gives Security the same foreign-key/busy settings.
type ControlStore struct {
	db   *sql.DB
	mu   sync.Mutex
	aead cipher.AEAD
}

type AuditEvent struct {
	ID      int64  `json:"id"`
	TS      int64  `json:"ts"`
	Action  string `json:"action"`
	Subject string `json:"subject"`
}

// controlSettings is deliberately a whitelist, never a serialized Config.
// In particular neither ProxyKey, legacy Keys nor bootstrap paths belong here.
type controlSettings struct {
	DefaultProvider  string        `json:"default_provider"`
	AuthHeader       string        `json:"auth_header"`
	AuthHeaderFormat string        `json:"auth_header_format"`
	CooldownSeconds  int           `json:"cooldown_seconds"`
	RequestTimeout   int           `json:"request_timeout_seconds"`
	RecentRequests   int           `json:"recent_requests"`
	Alerts           []AlertRule   `json:"alerts"`
	Limits           GatewayLimits `json:"limits"`
}

func controlDSN(path string, readOnly bool) string {
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{}
	if readOnly {
		q.Set("mode", "ro")
	}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	u.RawQuery = q.Encode()
	return u.String()
}

func privateControlDir(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("management directory must not be writable by group or others")
	}
	// Existing config/project directories belong to the user. Their modes
	// remain unchanged; only directories created above are private (0700).
	return nil
}

func controlRegularFile(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("management storage must be a regular file")
	}
	return info, nil
}

// createControlKey publishes a fully written 32-byte key without replacing any
// existing key, including when another opener wins the creation race.
func createControlKey(path string) error {
	if err := privateControlDir(path); err != nil {
		return err
	}
	key := make([]byte, 32)
	defer clear(key)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".verloc-key-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(key); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Link(f.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr = dir.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func OpenControlStore(path, masterKeyPath string) (*ControlStore, error) {
	if path == "" || masterKeyPath == "" {
		return nil, fmt.Errorf("management database and master-key paths are required")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	masterKeyPath, err = filepath.Abs(masterKeyPath)
	if err != nil {
		return nil, err
	}
	if path == masterKeyPath {
		return nil, fmt.Errorf("master key must be separate from database")
	}
	info, err := controlRegularFile(path)
	newDB := errors.Is(err, os.ErrNotExist) || (err == nil && info.Size() == 0)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if _, err := controlRegularFile(masterKeyPath); err != nil {
		if !newDB || !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("management master key unavailable: %w", err)
		}
		if err := createControlKey(masterKeyPath); err != nil {
			return nil, err
		}
	}
	if _, err := controlRegularFile(masterKeyPath); err != nil {
		return nil, err
	}
	key, err := os.ReadFile(masterKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read management master key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("management master key must contain exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	clear(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	s := &ControlStore{aead: aead}
	// Verify through a read-only connection before WAL, migrations, chmod, or
	// any writable connection. A wrong key must not alter an existing DB.
	if !newDB {
		probe, err := sql.Open("sqlite", controlDSN(path, true))
		if err != nil {
			return nil, err
		}
		probe.SetMaxOpenConns(1)
		err = s.checkKey(probe)
		closeErr := probe.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	if err := privateControlDir(path); err != nil {
		return nil, err
	}
	if err := privateControlDir(masterKeyPath); err != nil {
		return nil, err
	}
	if err := os.Chmod(masterKeyPath, 0o600); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", controlDSN(path, false))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s.db = db
	if _, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL`); err == nil {
		err = s.migrate(newDB)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *ControlStore) seal(plain, aad []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, plain, aad), nil
}

func (s *ControlStore) unseal(data, aad []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(data) < n {
		return nil, fmt.Errorf("invalid encrypted management data")
	}
	plain, err := s.aead.Open(nil, data[:n], data[n:], aad)
	if err != nil {
		return nil, fmt.Errorf("management master key incorrect or encrypted data corrupted")
	}
	return plain, nil
}

func credentialAAD(provider, id string) []byte {
	// JSON framing prevents ambiguous provider/credential concatenations.
	b, _ := json.Marshal([]string{"verloc-credential-v1", provider, id})
	return b
}

func (s *ControlStore) checkKey(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version != controlSchemaVersion {
		return fmt.Errorf("unsupported management schema version %d", version)
	}
	var encrypted []byte
	if err := db.QueryRow(`SELECT value FROM control_meta WHERE name='key_check'`).Scan(&encrypted); err != nil {
		return fmt.Errorf("management key verification unavailable: %w", err)
	}
	plain, err := s.unseal(encrypted, []byte("verloc-control-key-check-v1"))
	if err != nil {
		return err
	}
	if string(plain) != "verloc-control-store" {
		return fmt.Errorf("management key verification failed")
	}
	rows, err := db.Query(`SELECT provider_id,id,encrypted FROM provider_credentials`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var provider, id string
		var data []byte
		if err := rows.Scan(&provider, &id, &data); err != nil {
			return err
		}
		if _, err := s.unseal(data, credentialAAD(provider, id)); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *ControlStore) migrate(newDB bool) error {
	if !newDB {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmts := []string{
		`CREATE TABLE control_meta(name TEXT PRIMARY KEY,value BLOB NOT NULL)`,
		`CREATE TABLE control_settings(id INTEGER PRIMARY KEY CHECK(id=1),settings TEXT NOT NULL)`,
		`CREATE TABLE providers(id TEXT PRIMARY KEY,options TEXT NOT NULL)`,
		`CREATE TABLE provider_credentials(id TEXT PRIMARY KEY,provider_id TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,position INTEGER NOT NULL,encrypted BLOB NOT NULL,UNIQUE(provider_id,position))`,
		`CREATE TABLE models(id TEXT PRIMARY KEY,backend TEXT NOT NULL REFERENCES providers(id),upstream TEXT NOT NULL,disabled INTEGER NOT NULL DEFAULT 0,metadata TEXT NOT NULL)`,
		`CREATE TABLE model_routes(model_id TEXT NOT NULL REFERENCES models(id) ON DELETE CASCADE,position INTEGER NOT NULL,backend TEXT NOT NULL REFERENCES providers(id),upstream TEXT NOT NULL,PRIMARY KEY(model_id,position))`,
		`CREATE TABLE admin_auth(id INTEGER PRIMARY KEY CHECK(id=1),password_salt BLOB NOT NULL,password_hash BLOB NOT NULL,created_at INTEGER NOT NULL)`,
		`CREATE TABLE admin_sessions(token_hash BLOB PRIMARY KEY,csrf_token TEXT NOT NULL,expires_at INTEGER NOT NULL)`,
		`CREATE TABLE client_keys(id TEXT PRIMARY KEY,name TEXT NOT NULL,key_hash BLOB UNIQUE NOT NULL,prefix TEXT NOT NULL,allowed_models TEXT NOT NULL DEFAULT '[]',expires_at INTEGER NOT NULL DEFAULT 0,rpm INTEGER NOT NULL DEFAULT 0,tpm INTEGER NOT NULL DEFAULT 0,max_concurrent INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL,revoked_at INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE audit_events(id INTEGER PRIMARY KEY AUTOINCREMENT,ts INTEGER NOT NULL,action TEXT NOT NULL,subject TEXT NOT NULL)`,
		`CREATE INDEX audit_events_ts ON audit_events(ts)`,
		`PRAGMA user_version=1`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	check, err := s.seal([]byte("verloc-control-store"), []byte("verloc-control-key-check-v1"))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO control_meta(name,value) VALUES('key_check',?)`, check); err != nil {
		return err
	}
	return tx.Commit()
}

func controlModelTarget(value, defaultProvider string) (string, string) {
	if i := strings.IndexByte(value, ':'); i >= 0 {
		return value[:i], value[i+1:]
	}
	return defaultProvider, value
}

func prepareControlConfig(cfg *Config, importCatalog bool) (*Config, error) {
	if cfg == nil {
		return nil, fmt.Errorf("configuration is required")
	}
	out, err := cloneConfig(cfg)
	if err != nil {
		return nil, err
	}
	if err := out.Limits.Validate(); err != nil {
		return nil, err
	}
	out.ProxyKey = ""
	if len(out.Providers) == 0 && (len(out.Keys) > 0 || out.UpstreamBase != "") {
		p := &ProviderConfig{BaseURL: out.UpstreamBase, AuthHeader: out.AuthHeader, AuthHeaderFormat: out.AuthHeaderFormat, CooldownSeconds: out.CooldownSeconds, RequestTimeout: out.RequestTimeout}
		for _, key := range out.Keys {
			p.Keys = append(p.Keys, KeyEntry{Key: key})
		}
		out.Providers[out.DefaultProvider] = p
	}
	out.Keys = nil
	out.UpstreamBase = ""
	for name, p := range out.Providers {
		if name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, ":\r\n") || p == nil {
			return nil, fmt.Errorf("invalid provider")
		}
		u, err := url.Parse(strings.ReplaceAll(p.BaseURL, "%ACCOUNT%", "account"))
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return nil, fmt.Errorf("provider %q requires an HTTP(S) URL without user information", name)
		}
		if !p.Disabled && !p.NoAuth && len(p.Keys) == 0 {
			return nil, fmt.Errorf("provider %q has no credentials", name)
		}
		if strings.ContainsAny(p.AuthHeader+p.AuthHeaderFormat, "\r\n") {
			return nil, fmt.Errorf("provider %q has invalid authentication header", name)
		}
		for _, key := range p.Keys {
			if !p.NoAuth && strings.TrimSpace(key.Key) == "" {
				return nil, fmt.Errorf("provider %q has an empty credential", name)
			}
		}
		if p.CooldownSeconds < 0 || p.CooldownSeconds > 3600 || p.RequestTimeout < 0 || p.MaxTokens < 0 {
			return nil, fmt.Errorf("provider %q has invalid limits", name)
		}
	}
	if out.DisabledModels == nil {
		out.DisabledModels = map[string]bool{}
	}
	if out.ModelRoutes == nil {
		out.ModelRoutes = map[string][]RouteTarget{}
	}
	for id, target := range out.ModelMap {
		backend, upstream := controlModelTarget(target, out.DefaultProvider)
		if id == "" || strings.TrimSpace(id) != id || strings.ContainsAny(id, "\r\n") || upstream == "" || out.Providers[backend] == nil {
			return nil, fmt.Errorf("model %q references an invalid target", id)
		}
		for _, route := range out.ModelRoutes[id] {
			if out.Providers[route.Backend] == nil || strings.TrimSpace(route.Upstream) == "" {
				return nil, fmt.Errorf("model %q references an invalid fallback", id)
			}
		}
	}
	for id := range out.ModelMeta {
		if _, ok := out.ModelMap[id]; !ok {
			return nil, fmt.Errorf("metadata references unknown model %q", id)
		}
	}
	for id := range out.ModelRoutes {
		if _, ok := out.ModelMap[id]; !ok {
			return nil, fmt.Errorf("routes reference unknown model %q", id)
		}
	}
	for id := range out.DisabledModels {
		if _, ok := out.ModelMap[id]; !ok {
			return nil, fmt.Errorf("disabled state references unknown model %q", id)
		}
	}
	if importCatalog {
		var embedded []modelInfo
		if err := json.Unmarshal(modelsJSON, &embedded); err != nil {
			return nil, err
		}
		for _, m := range embedded {
			if _, ok := out.ModelMap[m.ID]; !ok || out.ModelMeta[m.ID] != nil {
				continue
			}
			known := true
			out.ModelMeta[m.ID] = &ModelMeta{Name: m.Name, Description: m.Description, OwnedBy: m.OwnedBy, Upstream: m.Upstream, ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens, Tags: append([]string(nil), m.Tags...), PricingInput: m.PricingPerMillion.Input, PricingOutput: m.PricingPerMillion.Output, PricingCache: m.PricingPerMillion.CacheRead, PricingKnown: &known}
		}
	}
	return out, nil
}

func (s *ControlStore) LoadOrImportConfig(seed *Config) (*Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var initialized int
	if err := tx.QueryRow(`SELECT count(*) FROM control_meta WHERE name='imported'`).Scan(&initialized); err != nil {
		return nil, err
	}
	if initialized == 0 {
		cfg, err := prepareControlConfig(seed, true)
		if err != nil {
			return nil, err
		}
		if err := s.writeConfig(tx, cfg, "config.import"); err != nil {
			return nil, err
		}
	}
	out, err := s.loadConfig(tx, seed)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *ControlStore) SaveConfig(cfg *Config) error {
	prepared, err := prepareControlConfig(cfg, false)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.writeConfig(tx, prepared, "config.save"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Publish generated stable credential IDs only after the entire commit.
	*cfg = *prepared
	return nil
}

type controlCredential struct {
	provider, id string
	key          KeyEntry
}

func (s *ControlStore) existingCredentials(tx *sql.Tx) ([]controlCredential, error) {
	rows, err := tx.Query(`SELECT provider_id,id,encrypted FROM provider_credentials ORDER BY provider_id,position`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []controlCredential
	for rows.Next() {
		var c controlCredential
		var encrypted []byte
		if err := rows.Scan(&c.provider, &c.id, &encrypted); err != nil {
			return nil, err
		}
		plain, err := s.unseal(encrypted, credentialAAD(c.provider, c.id))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(plain, &c.key); err != nil {
			return nil, fmt.Errorf("invalid encrypted credential")
		}
		all = append(all, c)
	}
	return all, rows.Err()
}

func (s *ControlStore) writeConfig(tx *sql.Tx, cfg *Config, action string) error {
	old, err := s.existingCredentials(tx)
	if err != nil {
		return err
	}
	used := map[string]bool{}
	providers := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		providers = append(providers, name)
	}
	sort.Strings(providers)
	for _, name := range providers {
		for i := range cfg.Providers[name].Keys {
			key := &cfg.Providers[name].Keys[i]
			if key.ID == "" {
				for _, previous := range old {
					if previous.provider == name && !used[previous.id] && previous.key.Key == key.Key && previous.key.AccountID == key.AccountID {
						key.ID = previous.id
						break
					}
				}
			}
			if key.ID == "" {
				var id [16]byte
				if _, err := rand.Read(id[:]); err != nil {
					return err
				}
				key.ID = hex.EncodeToString(id[:])
			}
			if used[key.ID] {
				return fmt.Errorf("duplicate credential identity")
			}
			used[key.ID] = true
		}
	}
	// Delete children first; this replacement and its audit event share one
	// transaction. Auth/session/client-key state is intentionally untouched.
	for _, table := range []string{"model_routes", "models", "provider_credentials", "providers"} {
		if _, err := tx.Exec(`DELETE FROM ` + table); err != nil {
			return err
		}
	}
	for _, name := range providers {
		p := cfg.Providers[name]
		options := *p
		options.Keys = nil
		data, err := json.Marshal(options)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO providers(id,options) VALUES(?,?)`, name, string(data)); err != nil {
			return err
		}
		for i, key := range p.Keys {
			plain, err := json.Marshal(key)
			if err != nil {
				return err
			}
			encrypted, err := s.seal(plain, credentialAAD(name, key.ID))
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO provider_credentials(id,provider_id,position,encrypted) VALUES(?,?,?,?)`, key.ID, name, i, encrypted); err != nil {
				return err
			}
		}
	}
	for id, target := range cfg.ModelMap {
		backend, upstream := controlModelTarget(target, cfg.DefaultProvider)
		meta, err := json.Marshal(cfg.ModelMeta[id])
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO models(id,backend,upstream,disabled,metadata) VALUES(?,?,?,?,?)`, id, backend, upstream, cfg.DisabledModels[id], string(meta)); err != nil {
			return err
		}
		for i, route := range cfg.ModelRoutes[id] {
			if _, err := tx.Exec(`INSERT INTO model_routes(model_id,position,backend,upstream) VALUES(?,?,?,?)`, id, i, route.Backend, route.Upstream); err != nil {
				return err
			}
		}
	}
	settings := controlSettings{DefaultProvider: cfg.DefaultProvider, AuthHeader: cfg.AuthHeader, AuthHeaderFormat: cfg.AuthHeaderFormat, CooldownSeconds: cfg.CooldownSeconds, RequestTimeout: cfg.RequestTimeout, RecentRequests: cfg.Dashboard.RecentRequests, Alerts: cfg.Dashboard.Alerts, Limits: cfg.Limits}
	data, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO control_settings(id,settings) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET settings=excluded.settings`, string(data)); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO control_meta(name,value) VALUES('imported',?) ON CONFLICT(name) DO NOTHING`, []byte("1")); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO audit_events(ts,action,subject) VALUES(?,?,?)`, time.Now().UTC().UnixMilli(), action, "gateway")
	return err
}

func (s *ControlStore) loadConfig(tx *sql.Tx, seed *Config) (*Config, error) {
	out := &Config{Providers: map[string]*ProviderConfig{}, ModelMap: map[string]string{}, ModelMeta: map[string]*ModelMeta{}, DisabledModels: map[string]bool{}, ModelRoutes: map[string][]RouteTarget{}, Dashboard: &DashboardConfig{}}
	if seed != nil {
		out.Listen, out.ManagementDB, out.MasterKeyFile, out.SetupTokenFile, out.SecureCookies = seed.Listen, seed.ManagementDB, seed.MasterKeyFile, seed.SetupTokenFile, seed.SecureCookies
		if seed.Dashboard != nil {
			out.Dashboard.DBPath = seed.Dashboard.DBPath
		}
	}
	var settingsJSON string
	if err := tx.QueryRow(`SELECT settings FROM control_settings WHERE id=1`).Scan(&settingsJSON); err != nil {
		return nil, err
	}
	var settings controlSettings
	if err := json.Unmarshal([]byte(settingsJSON), &settings); err != nil {
		return nil, err
	}
	out.DefaultProvider, out.AuthHeader, out.AuthHeaderFormat = settings.DefaultProvider, settings.AuthHeader, settings.AuthHeaderFormat
	out.CooldownSeconds, out.RequestTimeout, out.Limits = settings.CooldownSeconds, settings.RequestTimeout, settings.Limits
	out.Dashboard.RecentRequests, out.Dashboard.Alerts = settings.RecentRequests, settings.Alerts
	rows, err := tx.Query(`SELECT id,options FROM providers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, options string
		if err = rows.Scan(&id, &options); err != nil {
			break
		}
		var p ProviderConfig
		if err = json.Unmarshal([]byte(options), &p); err != nil {
			break
		}
		p.Keys = nil
		out.Providers[id] = &p
	}
	rowsErr := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if rowsErr != nil {
		return nil, rowsErr
	}
	credentials, err := s.existingCredentials(tx)
	if err != nil {
		return nil, err
	}
	for _, c := range credentials {
		p := out.Providers[c.provider]
		if p == nil {
			return nil, fmt.Errorf("credential references unknown provider")
		}
		c.key.ID = c.id
		p.Keys = append(p.Keys, c.key)
	}
	rows, err = tx.Query(`SELECT id,backend,upstream,disabled,metadata FROM models ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, backend, upstream, metadata string
		var disabled bool
		if err = rows.Scan(&id, &backend, &upstream, &disabled, &metadata); err != nil {
			break
		}
		out.ModelMap[id] = backend + ":" + upstream
		if disabled {
			out.DisabledModels[id] = true
		}
		var meta *ModelMeta
		if err = json.Unmarshal([]byte(metadata), &meta); err != nil {
			break
		}
		if meta != nil {
			out.ModelMeta[id] = meta
		}
	}
	rowsErr = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if rowsErr != nil {
		return nil, rowsErr
	}
	rows, err = tx.Query(`SELECT model_id,backend,upstream FROM model_routes ORDER BY model_id,position`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var route RouteTarget
		if err = rows.Scan(&id, &route.Backend, &route.Upstream); err != nil {
			break
		}
		out.ModelRoutes[id] = append(out.ModelRoutes[id], route)
	}
	rowsErr = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if rowsErr != nil {
		return nil, rowsErr
	}
	out.applyDefaults()
	return out, nil
}

func (s *ControlStore) AuditEvents(ctx context.Context, limit int) ([]AuditEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,ts,action,subject FROM audit_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]AuditEvent, 0)
	for rows.Next() {
		var e AuditEvent
		if err := rows.Scan(&e.ID, &e.TS, &e.Action, &e.Subject); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// Backup includes committed management and authentication state, never the
// external master key. VACUUM INTO reads one consistent SQLite snapshot and
// includes WAL commits without copying live database/journal files.
func (s *ControlStore) Backup(path string) error {
	if path == "" {
		return fmt.Errorf("backup path is required")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := privateControlDir(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`VACUUM INTO ?`, path); err != nil {
		os.Remove(path)
		return err
	}
	f, err = os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (s *ControlStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}
