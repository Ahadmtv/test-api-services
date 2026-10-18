package auth

import (
	"context"
	"errors"
	"sync"

	"test-api/internal/identity"
)

// mockService implements serviceInterface in memory for handler unit tests.
type mockService struct {
	mu          sync.Mutex
	users       map[int64]*identity.User
	byEmail     map[string]int64
	passwords   map[string]string
	roles       map[int64][]string
	refresh     map[string]bool // refresh token -> valid
	nextID      int64
	registerErr error
	loginErr    error
	refreshErr  error
	logoutErr   error
	meErr       error
}

func newMockService() *mockService {
	return &mockService{
		users:     make(map[int64]*identity.User),
		byEmail:   make(map[string]int64),
		passwords: make(map[string]string),
		roles:     make(map[int64][]string),
		refresh:   make(map[string]bool),
		nextID:    1,
	}
}

func (m *mockService) Register(ctx context.Context, email, phone, password string) (*identity.TokenPair, error) {
	if m.registerErr != nil {
		return nil, m.registerErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byEmail[email]; ok {
		return nil, identity.ErrUserAlreadyExists
	}
	id := m.nextID
	m.nextID++
	m.users[id] = &identity.User{ID: id, Email: email, Phone: &phone, IsActive: true, Roles: []string{"client"}}
	m.byEmail[email] = id
	m.roles[id] = []string{"client"}
	m.passwords[email] = password
	m.roles[m.nextID] = []string{"client"}
	return &identity.TokenPair{AccessToken: "access-" + email, RefreshToken: "refresh-" + email}, nil
}

func (m *mockService) Login(ctx context.Context, email, password string) (*identity.TokenPair, error) {
	if m.loginErr != nil {
		return nil, m.loginErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.passwords[email] != password {
		return nil, identity.ErrInvalidCredentials
	}
	return &identity.TokenPair{AccessToken: "access-" + email, RefreshToken: "refresh-" + email}, nil
}

func (m *mockService) Refresh(ctx context.Context, refreshToken string) (*identity.TokenPair, error) {
	if m.refreshErr != nil {
		return nil, m.refreshErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.refresh[refreshToken] {
		return nil, identity.ErrInvalidCredentials
	}
	return &identity.TokenPair{AccessToken: "access-new", RefreshToken: "refresh-new"}, nil
}

func (m *mockService) Logout(ctx context.Context, refreshToken string) error {
	if m.logoutErr != nil {
		return m.logoutErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.refresh, refreshToken)
	return nil
}

func (m *mockService) Me(ctx context.Context, userID int64) (*identity.User, error) {
	if m.meErr != nil {
		return nil, m.meErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[userID]
	if !ok {
		return nil, errors.New("not found")
	}
	return u, nil
}

func (m *mockService) addRefreshToken(token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refresh[token] = true
}
