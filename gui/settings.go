package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Settings is the GUI's own persisted state: manual nodes and tokens. The
// GUI is an ephemeral orchestrator — nothing here is authoritative, and any
// number of GUIs may run at once.
type Settings struct {
	mu   sync.Mutex `json:"-"`
	path string     `json:"-"`

	// DefaultToken is tried for any node without a specific token — handy
	// when all nodes share one token.
	DefaultToken string `json:"default_token"`
	// ManualNodes are base URLs added by hand (VPN, other subnets, mDNS-less
	// networks).
	ManualNodes []string `json:"manual_nodes"`
	// NodeTokens maps node base URL -> bearer token.
	NodeTokens map[string]string `json:"node_tokens"`
}

func settingsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "SuperSpeedySearch", "gui.json"), nil
}

func LoadSettings() (*Settings, error) {
	path, err := settingsPath()
	if err != nil {
		return nil, err
	}
	s := &Settings{path: path, NodeTokens: map[string]string{}}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, s); err != nil {
		return nil, err
	}
	if s.NodeTokens == nil {
		s.NodeTokens = map[string]string{}
	}
	return s, nil
}

func (s *Settings) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// tokens inside: keep it user-only
	return os.WriteFile(s.path, raw, 0o600)
}

func (s *Settings) TokenFor(url string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.NodeTokens[url]; ok && t != "" {
		return t
	}
	return s.DefaultToken
}

func (s *Settings) SetToken(url, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if token == "" {
		delete(s.NodeTokens, url)
	} else {
		s.NodeTokens[url] = token
	}
	return s.save()
}

func (s *Settings) SetDefaultToken(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.DefaultToken = token
	return s.save()
}

func (s *Settings) AddManualNode(url string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.ManualNodes {
		if u == url {
			return nil
		}
	}
	s.ManualNodes = append(s.ManualNodes, url)
	return s.save()
}

func (s *Settings) RemoveManualNode(url string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.ManualNodes[:0]
	for _, u := range s.ManualNodes {
		if u != url {
			out = append(out, u)
		}
	}
	s.ManualNodes = out
	delete(s.NodeTokens, url)
	return s.save()
}

func (s *Settings) ManualNodeList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ManualNodes...)
}

func (s *Settings) HasSpecificToken(url string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.NodeTokens[url] != ""
}
