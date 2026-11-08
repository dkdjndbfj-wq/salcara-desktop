package codexaccounts

import (
	"context"
	"errors"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func (s *Service) StartLogin(ctx context.Context, request LoginRequest) (LoginSession, error) {
	if !validAccountName(request.Name) {
		return LoginSession{}, ErrInvalidImport
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Mode == "" {
		request.Mode = LoginModeBrowser
	}
	if request.Mode != LoginModeBrowser && request.Mode != LoginModeDevice {
		return LoginSession{}, ErrLoginFailed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return LoginSession{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return LoginSession{}, err
	}
	connector := s.login
	if request.Mode == LoginModeDevice {
		connector = s.deviceLogin
	}
	if connector == nil {
		return LoginSession{}, ErrLoginFailed
	}
	// Retain a bounded number of completed sessions. Pending browser callback
	// sessions remain bounded too because the connector serializes its one
	// loopback listener.
	if len(s.logins) >= MaxAccounts {
		for id, l := range s.logins {
			if l.session.Status != "pending" && l.done != nil {
				select {
				case <-l.done:
					delete(s.logins, id)
				default:
				}
			}
		}
		if len(s.logins) >= MaxAccounts {
			return LoginSession{}, ErrConflict
		}
	}
	id, err := newID()
	if err != nil {
		return LoginSession{}, ErrLoginFailed
	}
	var epoch, nameRevision uint64
	if request.ReplaceID != "" {
		current, ok := s.vault.Accounts[request.ReplaceID]
		if !ok {
			return LoginSession{}, ErrNotFound
		}
		// Starting a new login invalidates older refresh/login work durably while
		// retaining the previous access token until the new login is saved.
		next := cloneVault(s.vault)
		current.Epoch++
		next.Epochs[current.ID] = current.Epoch
		next.Accounts[current.ID] = current
		if err := writeVault(s.path, s.protector, next); err != nil {
			return LoginSession{}, err
		}
		s.vault = next
		s.fenceLocked(current.ID)
		epoch = current.Epoch
		nameRevision = current.NameRevision
	} else if len(s.vault.Accounts) >= MaxAccounts {
		return LoginSession{}, ErrConflict
	}
	loginCtx, cancel := context.WithCancel(ctx)
	l := &loginState{session: LoginSession{ID: id, Mode: request.Mode, Status: "pending"}, cancel: cancel, request: request, epoch: epoch, nameRevision: nameRevision, connector: connector, done: make(chan struct{})}
	s.logins[id] = l
	go s.runLogin(loginCtx, l)
	return l.session, nil
}

func (s *Service) runLogin(ctx context.Context, l *loginState) {
	defer close(l.done)
	defer l.cancel()
	var record *coreauth.Auth
	var err error
	progress := func(progress LoginProgress) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.closed && l.session.Status == "pending" && ctx.Err() == nil {
			if progress.ExpiresAt != nil {
				copy := *progress.ExpiresAt
				progress.ExpiresAt = &copy
			}
			l.session.LoginProgress = progress
		}
	}
	if connector, ok := l.connector.(SessionProgressLoginConnector); ok {
		record, err = connector.LoginWithSessionProgress(ctx, l.session.ID, progress)
	} else if connector, ok := l.connector.(ProgressLoginConnector); ok {
		record, err = connector.LoginWithProgress(ctx, progress)
	} else {
		record, err = l.connector.Login(ctx)
	}
	var c credential
	if err == nil {
		c, err = credentialFromSDK(record)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() { l.session.LoginProgress = LoginProgress{} }()
	if s.closed || l.session.Status != "pending" || errors.Is(ctx.Err(), context.Canceled) {
		l.session.Status = "cancelled"
		return
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if l.request.ReplaceID != "" {
		current, ok := s.vault.Accounts[l.request.ReplaceID]
		if !ok || current.Epoch != l.epoch {
			l.session.Status = "cancelled"
			return
		}
	}
	if err != nil {
		l.session.Status = "failed"
		l.session.Error = ErrLoginFailed.Error()
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			l.session.ErrorCode = "expired"
		case errors.Is(err, errLoginCallbackUnavailable):
			l.session.ErrorCode = "callback_unavailable"
		}
		return
	}
	if s.credentialConflictLocked([]credential{c}, l.request.ReplaceID) {
		l.session.Status = "failed"
		l.session.Error = ErrConflict.Error()
		return
	}
	stampSubscriptionHint(&c, s.now())
	if l.request.ReplaceID == "" && len(s.vault.Accounts) >= MaxAccounts {
		l.session.Status = "failed"
		l.session.Error = ErrConflict.Error()
		return
	}
	id := l.request.ReplaceID
	if id == "" {
		id, err = newID()
		if err != nil {
			l.session.Status = "failed"
			l.session.Error = ErrLoginFailed.Error()
			return
		}
	}
	c.ID = id
	c.Epoch = s.vault.Epochs[id] + 1
	c.IdentityEpoch = c.Epoch
	c.Status = StatusReady
	if l.request.Name != "" {
		c.Name, c.NameMode = strings.TrimSpace(l.request.Name), nameModeCustom
	}
	if l.request.ReplaceID != "" {
		current := migrateAccountName(s.vault.Accounts[id])
		preserveCustomAccountName(&c, current)
		if current.NameRevision != l.nameRevision && current.NameMode == nameModeDefault {
			// A user restored the default while authorization was in flight.
			c.NameMode, c.Name = nameModeDefault, defaultAccountName(c)
		}
	}
	next := cloneVault(s.vault)
	next.Accounts[id] = c
	next.Epochs[id] = c.Epoch
	if err = writeVault(s.path, s.protector, next); err != nil {
		l.session.Status = "failed"
		l.session.Error = ErrSaveFailed.Error()
		return
	}
	s.vault = next
	if f := s.flights[id]; f != nil {
		f.cancel()
		delete(s.flights, id)
	}
	delete(s.pending, id)
	s.fenceUsageLocked(id)
	s.fenceSubscriptionLocked(id)
	a := s.accountLocked(c)
	l.session.Status = "complete"
	l.session.Account = &a
}

// SubmitLoginCallback completes only the named pending login. The connector
// validates the complete loopback URL, state, and single-consumption boundary;
// Service retains the replacement/identity fence before any credential save.
func (s *Service) SubmitLoginCallback(id, callbackURL string) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	l, ok := s.logins[id]
	if !ok {
		s.mu.Unlock()
		return ErrNotFound
	}
	if l.session.Status != "pending" {
		s.mu.Unlock()
		return ErrConflict
	}
	if l.session.Mode != LoginModeBrowser {
		s.mu.Unlock()
		return ErrLoginFailed
	}
	connector, ok := l.connector.(SessionProgressLoginConnector)
	s.mu.Unlock()
	if !ok {
		return ErrLoginFailed
	}
	if err := connector.SubmitCallback(id, callbackURL); err != nil {
		return ErrLoginFailed
	}
	return nil
}

func (s *Service) LoginStatus(id string) (LoginSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.logins[id]
	if !ok {
		return LoginSession{}, ErrNotFound
	}
	result := l.session
	if result.Account != nil {
		copy := *result.Account
		result.Account = &copy
	}
	if result.ExpiresAt != nil {
		copy := *result.ExpiresAt
		result.ExpiresAt = &copy
	}
	return result, nil
}
func (s *Service) CancelLogin(id string) error {
	s.mu.Lock()
	l, ok := s.logins[id]
	if !ok {
		s.mu.Unlock()
		return ErrNotFound
	}
	if l.session.Status == "pending" {
		l.session.Status = "cancelled"
		l.session.LoginProgress = LoginProgress{}
		l.cancel()
	}
	done := l.done
	s.mu.Unlock()
	// Let the listener or device request drain before the UI starts another
	// login. A connector ignoring cancellation stays fenced and bounded by the
	// session limit rather than hanging the console indefinitely.
	if done != nil {
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
		}
	}
	return nil
}
