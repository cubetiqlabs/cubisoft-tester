package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// Profile is a saved connection. The password rides along only when the vault
// is encrypted — see saveLocked.
type Profile struct {
	Name      string `json:"name"`
	Config    Config `json:"config"`
	Notes     string `json:"notes"`
	UpdatedAt string `json:"updatedAt"`
}

// ProfilesInfo describes the vault without needing the secret, so the UI knows
// whether to ask for one before it asks for anything else.
type ProfilesInfo struct {
	Path      string `json:"path"`
	Exists    bool   `json:"exists"`
	Encrypted bool   `json:"encrypted"`
	Unlocked  bool   `json:"unlocked"`
	Count     int    `json:"count"`
}

// vault is the on-disk file. An encrypted vault keeps the profile list inside
// Payload and nothing readable outside it.
type vault struct {
	Version    int       `json:"version"`
	Encrypted  bool      `json:"encrypted"`
	Iterations int       `json:"iterations,omitempty"`
	Salt       string    `json:"salt,omitempty"`
	Nonce      string    `json:"nonce,omitempty"`
	Payload    string    `json:"payload,omitempty"`
	Profiles   []Profile `json:"profiles,omitempty"`
}

const (
	vaultVersion = 1
	// OWASP's 2023 floor for PBKDF2-HMAC-SHA256. Costs ~0.2s per unlock, which
	// is invisible here and expensive for anyone brute-forcing a stolen file.
	kdfIterations = 600_000
)

var errLocked = errors.New("profiles are encrypted; unlock them with the secret key first")

// profileStore holds the session's unlocked state. The secret stays in memory
// only, and only for as long as the app runs.
type profileStore struct {
	mu       sync.Mutex
	secret   string
	unlocked bool
}

func profilesPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "MySQLTester", "profiles.json"), nil
}

func readVault() (*vault, error) {
	path, err := profilesPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &vault{Version: vaultVersion, Profiles: []Profile{}}, nil
	}
	if err != nil {
		return nil, err
	}
	return parseVault(data)
}

func parseVault(data []byte) (*vault, error) {
	var v vault
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("not a profile file: %w", err)
	}
	if v.Version != vaultVersion {
		return nil, fmt.Errorf("unsupported profile file version %d", v.Version)
	}
	if v.Profiles == nil {
		v.Profiles = []Profile{}
	}
	return &v, nil
}

// writeVault replaces the file atomically so a crash mid-write cannot leave a
// half-written vault where the profiles used to be.
func writeVault(v *vault) error {
	path, err := profilesPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func deriveKey(secret string, salt []byte, iterations int) ([]byte, error) {
	return pbkdf2.Key(sha256.New, secret, salt, iterations, 32)
}

func sealProfiles(secret string, profiles []Profile) (*vault, error) {
	plain, err := json.Marshal(profiles)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	key, err := deriveKey(secret, salt, kdfIterations)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return &vault{
		Version:    vaultVersion,
		Encrypted:  true,
		Iterations: kdfIterations,
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Payload:    base64.StdEncoding.EncodeToString(gcm.Seal(nil, nonce, plain, nil)),
	}, nil
}

func openProfiles(v *vault, secret string) ([]Profile, error) {
	if !v.Encrypted {
		return v.Profiles, nil
	}
	if secret == "" {
		return nil, errLocked
	}
	salt, err := base64.StdEncoding.DecodeString(v.Salt)
	if err != nil {
		return nil, fmt.Errorf("corrupt salt: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(v.Nonce)
	if err != nil {
		return nil, fmt.Errorf("corrupt nonce: %w", err)
	}
	payload, err := base64.StdEncoding.DecodeString(v.Payload)
	if err != nil {
		return nil, fmt.Errorf("corrupt payload: %w", err)
	}
	iterations := v.Iterations
	if iterations <= 0 {
		iterations = kdfIterations
	}
	key, err := deriveKey(secret, salt, iterations)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, payload, nil)
	if err != nil {
		// GCM authentication covers both a wrong key and a tampered file, and
		// the two are indistinguishable by design.
		return nil, errors.New("wrong secret key, or the profile file has been modified")
	}
	var profiles []Profile
	if err := json.Unmarshal(plain, &profiles); err != nil {
		return nil, err
	}
	if profiles == nil {
		profiles = []Profile{}
	}
	return profiles, nil
}

// load returns the current profiles, using the session secret when needed.
func (s *profileStore) load() (*vault, []Profile, error) {
	v, err := readVault()
	if err != nil {
		return nil, nil, err
	}
	if v.Encrypted && !s.unlocked {
		return nil, nil, errLocked
	}
	profiles, err := openProfiles(v, s.secret)
	if err != nil {
		return nil, nil, err
	}
	return v, profiles, nil
}

// store writes profiles back in whatever mode the session is in.
func (s *profileStore) store(profiles []Profile, encrypted bool) error {
	sort.Slice(profiles, func(i, j int) bool {
		return strings.ToLower(profiles[i].Name) < strings.ToLower(profiles[j].Name)
	})
	if !encrypted {
		return writeVault(&vault{Version: vaultVersion, Profiles: profiles})
	}
	v, err := sealProfiles(s.secret, profiles)
	if err != nil {
		return err
	}
	return writeVault(v)
}

/* ---------- bound methods ---------- */

// ProfilesInfo tells the UI whether it needs to ask for a secret key.
func (t *Tester) ProfilesInfo() (ProfilesInfo, error) {
	t.profiles.mu.Lock()
	defer t.profiles.mu.Unlock()

	path, err := profilesPath()
	if err != nil {
		return ProfilesInfo{}, err
	}
	info := ProfilesInfo{Path: path, Unlocked: t.profiles.unlocked}
	v, err := readVault()
	if err != nil {
		return info, err
	}
	if _, statErr := os.Stat(path); statErr == nil {
		info.Exists = true
	}
	info.Encrypted = v.Encrypted
	if !v.Encrypted {
		info.Unlocked = true
		info.Count = len(v.Profiles)
		return info, nil
	}
	if t.profiles.unlocked {
		if profiles, err := openProfiles(v, t.profiles.secret); err == nil {
			info.Count = len(profiles)
		}
	}
	return info, nil
}

// UnlockProfiles caches the secret for this session after proving it decrypts.
func (t *Tester) UnlockProfiles(secret string) error {
	t.profiles.mu.Lock()
	defer t.profiles.mu.Unlock()

	v, err := readVault()
	if err != nil {
		return err
	}
	if !v.Encrypted {
		t.profiles.unlocked = true
		t.profiles.secret = ""
		return nil
	}
	if _, err := openProfiles(v, secret); err != nil {
		return err
	}
	t.profiles.secret = secret
	t.profiles.unlocked = true
	return nil
}

// ListProfiles returns the saved connections, newest edit first by name order.
func (t *Tester) ListProfiles() ([]Profile, error) {
	t.profiles.mu.Lock()
	defer t.profiles.mu.Unlock()
	_, profiles, err := t.profiles.load()
	return profiles, err
}

// SaveProfileRequest is one save. SavePassword is a per-save choice rather than
// part of the profile, so it lives here and not on Profile.
type SaveProfileRequest struct {
	Name         string `json:"name"`
	Config       Config `json:"config"`
	SavePassword bool   `json:"savePassword"`
}

// SaveProfile adds or replaces a profile by name.
func (t *Tester) SaveProfile(req SaveProfileRequest) error {
	t.profiles.mu.Lock()
	defer t.profiles.mu.Unlock()

	p := Profile{Name: strings.TrimSpace(req.Name), Config: req.Config}
	if p.Name == "" {
		return errors.New("give the profile a name")
	}
	if strings.ContainsFunc(p.Name, func(r rune) bool { return r < 0x20 }) {
		return errors.New("profile names cannot contain control characters")
	}
	v, profiles, err := t.profiles.load()
	if err != nil {
		return err
	}
	// A plaintext vault is a file anyone with disk access can read, so a
	// password can only be kept once the vault is encrypted.
	switch {
	case !req.SavePassword:
		p.Config.Password = ""
	case !v.Encrypted:
		return errors.New("set a secret key before saving passwords: an unencrypted profile file would store it in the clear")
	}
	p.UpdatedAt = time.Now().Format(time.RFC3339)

	replaced := false
	for i := range profiles {
		if strings.EqualFold(profiles[i].Name, p.Name) {
			profiles[i] = p
			replaced = true
			break
		}
	}
	if !replaced {
		profiles = append(profiles, p)
	}
	return t.profiles.store(profiles, v.Encrypted)
}

// DeleteProfile removes one profile by name.
func (t *Tester) DeleteProfile(name string) error {
	t.profiles.mu.Lock()
	defer t.profiles.mu.Unlock()

	v, profiles, err := t.profiles.load()
	if err != nil {
		return err
	}
	kept := profiles[:0]
	for _, p := range profiles {
		if !strings.EqualFold(p.Name, name) {
			kept = append(kept, p)
		}
	}
	return t.profiles.store(kept, v.Encrypted)
}

// SetProfilesSecret re-encrypts the vault under a new secret. An empty secret
// removes encryption, which also drops every stored password.
func (t *Tester) SetProfilesSecret(newSecret string) error {
	t.profiles.mu.Lock()
	defer t.profiles.mu.Unlock()

	_, profiles, err := t.profiles.load()
	if err != nil {
		return err
	}
	if newSecret == "" {
		for i := range profiles {
			profiles[i].Config.Password = ""
		}
		t.profiles.secret = ""
		t.profiles.unlocked = true
		return t.profiles.store(profiles, false)
	}
	t.profiles.secret = newSecret
	t.profiles.unlocked = true
	return t.profiles.store(profiles, true)
}

// exportBytes renders the vault to export. A non-empty secret encrypts it and
// keeps the stored passwords; an empty secret writes plain JSON with the
// passwords stripped, because a readable file is no place for them.
func (t *Tester) exportBytes(secret string) ([]byte, error) {
	t.profiles.mu.Lock()
	defer t.profiles.mu.Unlock()

	_, profiles, err := t.profiles.load()
	if err != nil {
		return nil, err
	}
	if len(profiles) == 0 {
		return nil, errors.New("there is nothing saved to export yet")
	}

	out := &vault{Version: vaultVersion, Profiles: profiles}
	if secret != "" {
		if out, err = sealProfiles(secret, profiles); err != nil {
			return nil, err
		}
	} else {
		stripped := make([]Profile, len(profiles))
		for i, p := range profiles {
			p.Config.Password = ""
			stripped[i] = p
		}
		out.Profiles = stripped
	}
	return json.MarshalIndent(out, "", "  ")
}

// ExportProfiles writes the profiles to a file the user picks. The export key
// is independent of the local one, so a vault can be handed to another machine
// without sharing the key that guards this one.
func (t *Tester) ExportProfiles(secret string) (string, error) {
	app := application.Get()
	if app == nil {
		return "", errors.New("no application window")
	}
	data, err := t.exportBytes(secret)
	if err != nil {
		return "", err
	}
	path, err := app.Dialog.SaveFile().
		SetMessage("Export profiles").
		SetFilename("mysqltester-profiles.json").
		AddFilter("JSON", "*.json").
		PromptForSingleSelection()
	if err != nil || path == "" {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// ImportEncrypted reports whether a file needs a key before it can be imported,
// so a chosen or dropped file only prompts when it has to.
func (t *Tester) ImportEncrypted(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	v, err := parseVault(data)
	if err != nil {
		return false, err
	}
	return v.Encrypted, nil
}

// PickProfileFile opens the file dialog and returns the chosen path, or "" if
// the user cancelled. Choosing the file first lets the caller ask whether it
// needs a key before deciding to prompt for one.
func (t *Tester) PickProfileFile() (string, error) {
	app := application.Get()
	if app == nil {
		return "", errors.New("no application window")
	}
	return app.Dialog.OpenFile().
		SetTitle("Import profiles").
		SetMessage("Import profiles").
		AddFilter("JSON", "*.json").
		CanChooseFiles(true).
		PromptForSingleSelection()
}

// ImportProfiles merges a vault file into the current one and returns the names
// it brought in. fileSecret is the key the file was encrypted with, blank when
// it was plaintext.
func (t *Tester) ImportProfiles(path, fileSecret string, replace bool) ([]string, error) {
	if path == "" {
		return nil, errors.New("no file chosen")
	}

	t.profiles.mu.Lock()
	defer t.profiles.mu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	incoming, err := parseVault(data)
	if err != nil {
		return nil, err
	}
	imported, err := openProfiles(incoming, fileSecret)
	if err != nil {
		return nil, err
	}

	current, existing, err := t.profiles.load()
	if err != nil {
		return nil, err
	}
	merged := existing
	if replace {
		merged = []Profile{}
	}
	names := []string{}
	for _, p := range imported {
		if !current.Encrypted {
			p.Config.Password = ""
		}
		names = append(names, p.Name)
		found := false
		for i := range merged {
			if strings.EqualFold(merged[i].Name, p.Name) {
				merged[i] = p
				found = true
				break
			}
		}
		if !found {
			merged = append(merged, p)
		}
	}
	if err := t.profiles.store(merged, current.Encrypted); err != nil {
		return nil, err
	}
	return names, nil
}
