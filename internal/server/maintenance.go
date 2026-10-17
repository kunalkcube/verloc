package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type backupManifest struct {
	Version          int    `json:"version"`
	CreatedAt        int64  `json:"created_at"`
	ManagementSHA256 string `json:"management_sha256"`
	AnalyticsSHA256  string `json:"analytics_sha256"`
}

// Close follows the writer dependency order: final metrics, queued request
// metadata, then management state. Each subsystem's close is idempotent.
func (s *Server) Close() error {
	s.shuttingDown.Store(true)
	if s.hub != nil {
		s.hub.Close()
	}
	if s.ticker != nil {
		s.ticker.Stop()
	}
	for _, b := range s.runtime().backends {
		b.client.CloseIdleConnections()
	}
	var errs []error
	if s.store != nil {
		errs = append(errs, s.store.Close())
	}
	if s.control != nil {
		errs = append(errs, s.control.Close())
	}
	return errors.Join(errs...)
}

func (s *Server) backupGateway(dir string) (err error) {
	if s.control == nil || s.store == nil {
		return fmt.Errorf("management and analytics stores are required")
	}
	adminMu.Lock()
	defer adminMu.Unlock()
	if err = os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("create new backup directory: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(dir)
		}
	}()
	management := filepath.Join(dir, "management.db")
	analytics := filepath.Join(dir, "analytics.db")
	if err = s.control.Backup(management); err != nil {
		return err
	}
	if err = s.store.Backup(analytics); err != nil {
		return err
	}
	controlHash, err := checksumFile(management)
	if err != nil {
		return err
	}
	analyticsHash, err := checksumFile(analytics)
	if err != nil {
		return err
	}
	manifest := backupManifest{Version: 1, CreatedAt: time.Now().UnixMilli(), ManagementSHA256: controlHash, AnalyticsSHA256: analyticsHash}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "manifest.json"), append(data, '\n'), 0o600); err != nil {
		return err
	}
	complete = true
	return nil
}

func restoreGateway(cfg *Config, dir string) error {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return err
	}
	var manifest backupManifest
	if err := json.Unmarshal(data, &manifest); err != nil || manifest.Version != 1 {
		return fmt.Errorf("unsupported or invalid backup manifest")
	}
	for name, expected := range map[string]string{"management.db": manifest.ManagementSHA256, "analytics.db": manifest.AnalyticsSHA256} {
		got, err := checksumFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if expected == "" || got != expected {
			return fmt.Errorf("backup checksum mismatch: %s", name)
		}
	}
	if _, err := os.Stat(cfg.MasterKeyFile); err != nil {
		return fmt.Errorf("restore requires the original separately secured master key: %w", err)
	}
	for _, dest := range []string{cfg.ManagementDB, cfg.Dashboard.DBPath} {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if _, err := os.Lstat(dest + suffix); !os.IsNotExist(err) {
				return fmt.Errorf("restore requires new, unused database paths: %s", dest)
			}
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
	}
	// Validate encryption before creating either destination database.
	stage, err := os.MkdirTemp(filepath.Dir(cfg.ManagementDB), ".verloc-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	stagedControl := filepath.Join(stage, "management.db")
	if err := copyPrivateFile(filepath.Join(dir, "management.db"), stagedControl); err != nil {
		return err
	}
	control, err := OpenControlStore(stagedControl, cfg.MasterKeyFile)
	if err != nil {
		return fmt.Errorf("validate backup encryption: %w", err)
	}
	_, loadErr := control.LoadOrImportConfig(cfg)
	closeErr := control.Close()
	if err := errors.Join(loadErr, closeErr); err != nil {
		return fmt.Errorf("validate backup: %w", err)
	}
	if err := copyPrivateFile(filepath.Join(dir, "management.db"), cfg.ManagementDB); err != nil {
		return err
	}
	if err := copyPrivateFile(filepath.Join(dir, "analytics.db"), cfg.Dashboard.DBPath); err != nil {
		_ = os.Remove(cfg.ManagementDB)
		return err
	}
	return nil
}

func copyPrivateFile(source, dest string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		_ = os.Remove(dest)
		return err
	}
	return nil
}

func checksumFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Server) handleAdminBackup(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	base := filepath.Join(filepath.Dir(s.runtime().cfg.ManagementDB), "backups")
	if err := os.MkdirAll(base, 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot create backup directory")
		return
	}
	dir := filepath.Join(base, time.Now().UTC().Format("20060102T150405.000000000Z"))
	if err := s.backupGateway(dir); err != nil {
		writeErr(w, http.StatusInternalServerError, "backup failed; check server logs and disk space")
		return
	}
	_, _ = s.control.db.ExecContext(r.Context(), "INSERT INTO audit_events(ts,action,subject) VALUES(?,?,?)", time.Now().UnixMilli(), "backup.created", filepath.Base(dir))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "backup": dir})
}

func exportBootstrapFile(cfg *Config, path string) error {
	absolute := func(p string) (string, error) { return filepath.Abs(p) }
	management, err := absolute(cfg.ManagementDB)
	if err != nil {
		return err
	}
	master, err := absolute(cfg.MasterKeyFile)
	if err != nil {
		return err
	}
	setup, err := absolute(cfg.SetupTokenFile)
	if err != nil {
		return err
	}
	analytics, err := absolute(cfg.Dashboard.DBPath)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(map[string]any{
		"listen": cfg.Listen, "management_db": management,
		"master_key_file": master, "setup_token_file": setup,
		"secure_cookies": cfg.SecureCookies,
		"dashboard":      map[string]string{"db_path": analytics},
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(append(data, '\n'))
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}
