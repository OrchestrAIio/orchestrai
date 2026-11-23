// Package creds stores editor sign-in credentials: the API key goes to the OS
// keychain when one is available, account metadata (and the key itself when
// no keychain works) goes to credentials.json with 0600 permissions.
package creds

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/zalando/go-keyring"
)

// KeyringService is the keychain service name.
const KeyringService = "orchestrai-cli"

// Env variable names.
const (
	EnvAPIKey    = "ORCHESTRAI_API_KEY"
	EnvAPIURL    = "ORCHESTRAI_API_URL"
	EnvNoKeyring = "ORCHESTRAI_NO_KEYRING"
	EnvConfigDir = "ORCHESTRAI_CONFIG_DIR"
)

// User mirrors the device-flow user object.
type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Account is one signed-in backend.
type Account struct {
	APIURL    string    `json:"api_url"`
	User      User      `json:"user"`
	Storage   string    `json:"storage"`           // "keyring" | "file"
	APIKey    string    `json:"api_key,omitempty"` // only when Storage == "file"
	CreatedAt time.Time `json:"created_at"`
}

// File is the credentials.json document.
type File struct {
	DefaultAPIURL string              `json:"default_api_url,omitempty"`
	Accounts      map[string]*Account `json:"accounts"`
}

// Credentials is what commands use.
type Credentials struct {
	APIURL string
	APIKey string
	User   User
	Source string // "env" | "keyring" | "file"
}

// Keyring abstracts go-keyring for tests.
type Keyring interface {
	Set(service, user, secret string) error
	Get(service, user string) (string, error)
	Delete(service, user string) error
}

type osKeyring struct{}

func (osKeyring) Set(s, u, p string) error        { return keyring.Set(s, u, p) }
func (osKeyring) Get(s, u string) (string, error) { return keyring.Get(s, u) }
func (osKeyring) Delete(s, u string) error        { return keyring.Delete(s, u) }

// Store reads and writes credentials.
type Store struct {
	Dir     string
	Keyring Keyring // nil disables the keychain
}

// DefaultDir returns $ORCHESTRAI_CONFIG_DIR, else $XDG_CONFIG_HOME/orchestrai,
// else ~/.config/orchestrai (%AppData%\orchestrai on Windows).
func DefaultDir() (string, error) {
	if d := os.Getenv(EnvConfigDir); d != "" {
		return d, nil
	}
	if runtime.GOOS == "windows" {
		if d := os.Getenv("AppData"); d != "" {
			return filepath.Join(d, "orchestrai"), nil
		}
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "orchestrai"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", "orchestrai"), nil
}

// NewDefault returns the production store.
func NewDefault() (*Store, error) {
	dir, err := DefaultDir()
	if err != nil {
		return nil, err
	}
	s := &Store{Dir: dir, Keyring: osKeyring{}}
	if v := os.Getenv(EnvNoKeyring); v != "" && v != "0" && v != "false" {
		s.Keyring = nil
	}
	return s, nil
}

// Path is the credentials file path.
func (s *Store) Path() string { return filepath.Join(s.Dir, "credentials.json") }

// Load reads credentials.json (empty document if absent).
func (s *Store) Load() (*File, error) {
	data, err := os.ReadFile(s.Path())
	if errors.Is(err, fs.ErrNotExist) {
		return &File{Accounts: map[string]*Account{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.Path(), err)
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.Path(), err)
	}
	if f.Accounts == nil {
		f.Accounts = map[string]*Account{}
	}
	return &f, nil
}

func (s *Store) save(f *File) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", s.Dir, err)
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, ".credentials-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.Path()); err != nil {
		return err
	}
	return os.Chmod(s.Path(), 0o600)
}

// Save stores a new sign-in for apiURL and makes it the default. It returns
// where the key went ("keyring" or "file").
func (s *Store) Save(apiURL, apiKey string, user User) (string, error) {
	f, err := s.Load()
	if err != nil {
		return "", err
	}
	acct := &Account{APIURL: apiURL, User: user, CreatedAt: time.Now().UTC()}
	if s.Keyring != nil && s.Keyring.Set(KeyringService, apiURL, apiKey) == nil {
		acct.Storage = "keyring"
	} else {
		acct.Storage = "file"
		acct.APIKey = apiKey
	}
	f.Accounts[apiURL] = acct
	f.DefaultAPIURL = apiURL
	if err := s.save(f); err != nil {
		if acct.Storage == "keyring" {
			_ = s.Keyring.Delete(KeyringService, apiURL)
		}
		return "", err
	}
	return acct.Storage, nil
}

// ErrNotLoggedIn means no credentials exist for the API URL.
var ErrNotLoggedIn = errors.New("not signed in; run `orchestrai login`")

// Get resolves credentials for apiURL. ORCHESTRAI_API_KEY always wins.
func (s *Store) Get(apiURL string) (*Credentials, error) {
	if k := os.Getenv(EnvAPIKey); k != "" {
		return &Credentials{APIURL: apiURL, APIKey: k, Source: "env"}, nil
	}
	return s.GetStored(apiURL)
}

// GetStored resolves stored credentials for apiURL, ignoring the env override.
func (s *Store) GetStored(apiURL string) (*Credentials, error) {
	f, err := s.Load()
	if err != nil {
		return nil, err
	}
	acct, ok := f.Accounts[apiURL]
	if !ok {
		return nil, ErrNotLoggedIn
	}
	c := &Credentials{APIURL: apiURL, User: acct.User, Source: acct.Storage}
	switch acct.Storage {
	case "keyring":
		if s.Keyring == nil {
			return nil, fmt.Errorf("credentials for %s are in the OS keychain but the keychain is disabled (%s); run `orchestrai login` again", apiURL, EnvNoKeyring)
		}
		k, err := s.Keyring.Get(KeyringService, apiURL)
		if err != nil {
			return nil, fmt.Errorf("read API key from OS keychain: %w (run `orchestrai login` again)", err)
		}
		c.APIKey = k
	default:
		c.APIKey = acct.APIKey
		c.Source = "file"
	}
	if c.APIKey == "" {
		return nil, ErrNotLoggedIn
	}
	return c, nil
}

// DefaultAPIURL returns the URL of the most recent login, if any.
func (s *Store) DefaultAPIURL() string {
	f, err := s.Load()
	if err != nil {
		return ""
	}
	return f.DefaultAPIURL
}

// Delete removes stored credentials for apiURL. It reports whether anything
// was removed.
func (s *Store) Delete(apiURL string) (bool, error) {
	f, err := s.Load()
	if err != nil {
		return false, err
	}
	acct, ok := f.Accounts[apiURL]
	if !ok {
		return false, nil
	}
	if acct.Storage == "keyring" && s.Keyring != nil {
		if err := s.Keyring.Delete(KeyringService, apiURL); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			return false, fmt.Errorf("remove API key from OS keychain: %w", err)
		}
	}
	delete(f.Accounts, apiURL)
	if f.DefaultAPIURL == apiURL {
		f.DefaultAPIURL = ""
		for u := range f.Accounts {
			f.DefaultAPIURL = u
			break
		}
	}
	return true, s.save(f)
}
