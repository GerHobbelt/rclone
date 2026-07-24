package oauthutil

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"golang.org/x/oauth2"
)

const (
	rotatingTokenStateVersion = 1
	maxRefreshLead            = 10 * time.Minute
	minRefreshLead            = 30 * time.Second
)

// ExchangeFailure describes how safely a failed one-time token exchange can be
// retried.
type ExchangeFailure uint8

const (
	// ExchangeFailureUncertain means the broker may have consumed the refresh
	// token. It is the conservative default for an unclassified error.
	ExchangeFailureUncertain ExchangeFailure = iota

	// ExchangeFailureNotSent means the request was proven not to have reached
	// the broker, so the existing refresh token remains usable.
	ExchangeFailureNotSent

	// ExchangeFailureReauthenticationRequired means the broker definitively
	// rejected or revoked the refresh token.
	ExchangeFailureReauthenticationRequired
)

// ExchangeError reports the failure classification of a token exchange.
type ExchangeError interface {
	error
	ExchangeFailure() ExchangeFailure
}

type classifiedExchangeError struct {
	failure ExchangeFailure
	err     error
}

func (e *classifiedExchangeError) Error() string {
	return e.err.Error()
}

func (e *classifiedExchangeError) Unwrap() error {
	return e.err
}

func (e *classifiedExchangeError) ExchangeFailure() ExchangeFailure {
	return e.failure
}

// NewExchangeError classifies err for a one-time token exchange. Callers must
// use ExchangeFailureNotSent only when the transport proves that the broker did
// not receive the request.
func NewExchangeError(failure ExchangeFailure, err error) error {
	if err == nil {
		return nil
	}
	switch failure {
	case ExchangeFailureNotSent, ExchangeFailureReauthenticationRequired:
	default:
		failure = ExchangeFailureUncertain
	}
	return &classifiedExchangeError{failure: failure, err: err}
}

// ClassifyExchangeTransportError marks an exchange failure retryable only when
// the transport proves that no HTTP request reached the broker.
func ClassifyExchangeTransportError(err error) error {
	if err == nil {
		return nil
	}
	var classified ExchangeError
	if errors.As(err, &classified) {
		return err
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return NewExchangeError(ExchangeFailureNotSent, err)
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return NewExchangeError(ExchangeFailureNotSent, err)
	}
	var verifyErr *tls.CertificateVerificationError
	if errors.As(err, &verifyErr) {
		return NewExchangeError(ExchangeFailureNotSent, err)
	}
	var headerErr tls.RecordHeaderError
	if errors.As(err, &headerErr) && headerErr.Conn != nil {
		return NewExchangeError(ExchangeFailureNotSent, err)
	}
	var unknownAuthorityErr x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthorityErr) {
		return NewExchangeError(ExchangeFailureNotSent, err)
	}
	var invalidCertificateErr x509.CertificateInvalidError
	if errors.As(err, &invalidCertificateErr) {
		return NewExchangeError(ExchangeFailureNotSent, err)
	}
	var hostnameErr x509.HostnameError
	if errors.As(err, &hostnameErr) {
		return NewExchangeError(ExchangeFailureNotSent, err)
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Op == "parse" {
		return NewExchangeError(ExchangeFailureNotSent, err)
	}
	return err
}

// ErrTokenStateUncertain is returned when it is unsafe to reuse a one-time
// refresh token.
var ErrTokenStateUncertain = errors.New("rotating token state is uncertain")

// ErrTokenReauthenticationRequired is returned when a refresh credential has
// been definitively rejected or revoked.
var ErrTokenReauthenticationRequired = errors.New("token reauthentication is required")

// ErrNoPersistentToken is returned when a persistent remote has no token.
var ErrNoPersistentToken = errors.New("persistent token is empty")

// PutRotatingToken replaces the persistent rotating credential for m with a
// complete credential produced by an explicit authentication flow.
func PutRotatingToken(ctx context.Context, name string, m configmap.Mapper, token *oauth2.Token) error {
	if !completeToken(token) {
		return errors.New("rotating token is not a complete token")
	}
	section, err := persistentTokenSection(name, m)
	if err != nil {
		return err
	}

	unlock, err := config.LockConfigSection(ctx, section)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()

	_, err = putCompleteRotatingToken(ctx, section, token)
	return err
}

// ReauthenticateRotatingToken runs authenticate and saves its complete
// replacement credential while holding the same section lock as refreshes.
// A failed or cancelled authentication leaves the stored state unchanged.
func ReauthenticateRotatingToken(ctx context.Context, name string, m configmap.Mapper, authenticate func(context.Context) (*oauth2.Token, error)) (*oauth2.Token, uint64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if authenticate == nil {
		return nil, 0, errors.New("rotating token authentication callback is nil")
	}
	section, err := persistentTokenSection(name, m)
	if err != nil {
		return nil, 0, err
	}

	unlock, err := config.LockConfigSection(ctx, section)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = unlock() }()

	token, err := authenticate(ctx)
	if err != nil {
		return nil, 0, err
	}
	if !completeToken(token) {
		return nil, 0, errors.New("rotating token authentication returned an incomplete token")
	}
	generation, err := putCompleteRotatingToken(ctx, section, token)
	if err != nil {
		return nil, 0, err
	}
	return cloneOAuthToken(token), generation, nil
}

// putCompleteRotatingToken saves token as the next ready generation while the
// caller holds the persistent section lock.
func putCompleteRotatingToken(ctx context.Context, section string, token *oauth2.Token) (generation uint64, err error) {
	nextToken := cloneOAuthToken(token)
	err = config.FileTransaction(ctx, func() error {
		generation = 1
		_, currentState, legacy, loadErr := loadStoredToken(section)
		if loadErr == nil && legacy {
			currentState.Generation = 1
		}
		if loadErr == nil {
			generation = currentState.Generation + 1
		}
		return saveStoredToken(section, nextToken, readyStateWithGeneration(nextToken, generation))
	})
	return generation, err
}

type reconnectSnapshot struct {
	present bool
	raw     string
}

// ReconnectRotatingToken exchanges a user-supplied fresh refresh token and
// atomically replaces the persistent state only after a complete success.
//
// A rejected or unsent reconnect attempt restores the previous state. An
// indeterminate attempt records the fresh credential as uncertain because the
// broker may already have consumed it.
func ReconnectRotatingToken(ctx context.Context, name string, m configmap.Mapper, token *oauth2.Token, exchange TokenExchange) (*oauth2.Token, uint64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if token == nil || token.RefreshToken == "" {
		return nil, 0, errors.New("rotating token has no refresh token")
	}
	if exchange == nil {
		return nil, 0, errors.New("token exchange is nil")
	}
	section, err := persistentTokenSection(name, m)
	if err != nil {
		return nil, 0, err
	}

	unlock, err := config.LockConfigSection(ctx, section)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = unlock() }()

	claimedToken := &oauth2.Token{RefreshToken: token.RefreshToken}
	var (
		snapshot        reconnectSnapshot
		claimGeneration uint64
		owner           string
	)
	err = config.FileTransaction(ctx, func() error {
		snapshot.raw, snapshot.present = config.FileGetValue(section, config.ConfigToken)
		_, state, legacy, loadErr := loadStoredToken(section)
		if loadErr == nil {
			if legacy {
				state = readyStateWithGeneration(nil, 1)
			}
			claimGeneration = state.Generation
		} else if !errors.Is(loadErr, ErrNoPersistentToken) {
			// An explicit reconnect can recover a malformed old credential. Keep
			// its raw value so a rejected fresh credential can restore it.
			claimGeneration = 0
		}
		var ownerErr error
		owner, ownerErr = tokenOwner()
		if ownerErr != nil {
			return ownerErr
		}
		return saveStoredToken(section, claimedToken, rotatingTokenState{
			Version:          rotatingTokenStateVersion,
			Generation:       claimGeneration,
			Status:           tokenStateInFlight,
			Owner:            owner,
			RefreshTokenHash: tokenHash(claimedToken.RefreshToken),
		})
	})
	if err != nil {
		return nil, 0, err
	}

	if err = ctx.Err(); err != nil {
		if restoreErr := restoreReconnectSnapshot(context.Background(), section, snapshot, owner, claimGeneration); restoreErr != nil {
			return nil, 0, restoreErr
		}
		return nil, 0, err
	}

	nextToken, exchangeErr := exchange(ctx, claimedToken.RefreshToken)
	failure := exchangeFailure(exchangeErr)
	if exchangeErr == nil && !completeToken(nextToken) {
		exchangeErr = errors.New("token exchange returned incomplete token")
		failure = ExchangeFailureUncertain
	}

	var reconnectErr error
	err = config.FileTransaction(stateCommitContext(ctx), func() error {
		persistedToken, persistedState, legacy, loadErr := loadStoredToken(section)
		if loadErr != nil {
			return loadErr
		}
		if legacy || persistedState.Status != tokenStateInFlight || persistedState.Owner != owner || persistedState.Generation != claimGeneration || persistedToken.RefreshToken != claimedToken.RefreshToken {
			return errors.New("rotating token state changed while reconnect was in flight")
		}
		if exchangeErr != nil {
			reconnectErr = exchangeErr
			switch failure {
			case ExchangeFailureNotSent, ExchangeFailureReauthenticationRequired:
				return restoreReconnectSnapshotValue(section, snapshot)
			default:
				persistedState = uncertainState(persistedState)
				if err := saveStoredToken(section, persistedToken, persistedState); err != nil {
					return err
				}
			}
			return nil
		}

		claimGeneration++
		if err := saveStoredToken(section, nextToken, readyStateWithGeneration(nextToken, claimGeneration)); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	if reconnectErr != nil {
		return nil, 0, reconnectErr
	}
	return cloneOAuthToken(nextToken), claimGeneration, nil
}

// restoreReconnectSnapshot restores the state that preceded a reconnect claim
// after confirming that claim is still current.
func restoreReconnectSnapshot(ctx context.Context, section string, snapshot reconnectSnapshot, owner string, generation uint64) error {
	return config.FileTransaction(ctx, func() error {
		_, state, legacy, err := loadStoredToken(section)
		if err != nil {
			return err
		}
		if legacy || state.Status != tokenStateInFlight || state.Owner != owner || state.Generation != generation {
			return errors.New("rotating token state changed while reconnect was in flight")
		}
		return restoreReconnectSnapshotValue(section, snapshot)
	})
}

// restoreReconnectSnapshotValue restores a raw token value while a config
// transaction is already active.
func restoreReconnectSnapshotValue(section string, snapshot reconnectSnapshot) error {
	if snapshot.present {
		config.FileSetValue(section, config.ConfigToken, snapshot.raw)
		return nil
	}
	config.FileDeleteKey(section, config.ConfigToken)
	return nil
}

// TokenExchange exchanges refreshToken for a complete, rotated OAuth token.
// The returned token must contain an access token, refresh token, and expiry.
type TokenExchange func(ctx context.Context, refreshToken string) (*oauth2.Token, error)

// RotatingTokenSource coordinates a one-time rotating OAuth token across
// rclone processes sharing one local persistent config file.
type RotatingTokenSource struct {
	mu       sync.Mutex
	ctx      context.Context
	section  string
	exchange TokenExchange
	token    *oauth2.Token
	state    rotatingTokenState
}

type rotatingTokenState struct {
	Version          int           `json:"version"`
	Generation       uint64        `json:"generation"`
	Status           string        `json:"status"`
	Owner            string        `json:"owner,omitempty"`
	RefreshTokenHash string        `json:"refresh_token_hash,omitempty"`
	RefreshLead      time.Duration `json:"refresh_lead,omitempty"`
}

type storedRotatingToken struct {
	AccessToken  string              `json:"access_token"`
	TokenType    string              `json:"token_type,omitempty"`
	RefreshToken string              `json:"refresh_token,omitempty"`
	Expiry       time.Time           `json:"expiry,omitempty"`
	ExpiresIn    int64               `json:"expires_in,omitempty"`
	State        *rotatingTokenState `json:"rclone_token_state,omitempty"`
}

// NewRotatingTokenSource creates a source backed by the persistent config
// section associated with m. Rotating credentials from connection strings,
// flags, or environment variables are rejected.
func NewRotatingTokenSource(ctx context.Context, name string, m configmap.Mapper, exchange TokenExchange) (*RotatingTokenSource, error) {
	if exchange == nil {
		return nil, errors.New("token exchange is nil")
	}
	section, err := persistentTokenSection(name, m)
	if err != nil {
		return nil, err
	}

	s := &RotatingTokenSource{
		ctx:      ctx,
		section:  section,
		exchange: exchange,
	}
	if s.ctx == nil {
		s.ctx = context.Background()
	}
	if err := s.loadOrMigrate(s.ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// persistentTokenSection validates the provenance of a rotating token and
// returns the stable config section that owns it.
func persistentTokenSection(name string, m configmap.Mapper) (string, error) {
	persistent, ok := m.(configmap.PersistentMapper)
	if !ok {
		return "", errors.New("rotating tokens require a persistent config mapper")
	}
	section := persistent.PersistentConfigName()
	if section == "" || strings.HasPrefix(section, ":") {
		return "", errors.New("rotating tokens require a named persistent remote")
	}
	if persistent.HasOverride(config.ConfigToken) {
		return "", fmt.Errorf("rotating token for %q must be stored in the local config, not an override", name)
	}
	return section, nil
}

// Token returns a usable OAuth token using the context supplied at creation.
// It implements oauth2.TokenSource.
func (s *RotatingTokenSource) Token() (*oauth2.Token, error) {
	token, _, err := s.TokenContext(s.ctx)
	return token, err
}

// TokenContext returns a usable token and its persisted generation.
func (s *RotatingTokenSource) TokenContext(ctx context.Context) (*oauth2.Token, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state.Status == tokenStateReady && !s.needsRefresh() {
		return cloneOAuthToken(s.token), s.state.Generation, nil
	}
	return s.refresh(ctx, false, s.state.Generation)
}

// RefreshIfCurrent refreshes the token only when generation is still current.
// If another process has already written a later generation, it adopts that
// generation without calling the token broker.
func (s *RotatingTokenSource) RefreshIfCurrent(ctx context.Context, generation uint64) (*oauth2.Token, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refresh(ctx, true, generation)
}

// MarkReauthenticationRequiredIfCurrent records a definitive provider
// rejection only when generation is still current. If another process has
// already persisted a newer generation, it returns that token without changing
// its state.
func (s *RotatingTokenSource) MarkReauthenticationRequiredIfCurrent(ctx context.Context, generation uint64) (*oauth2.Token, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	unlock, err := config.LockConfigSection(ctx, s.section)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = unlock() }()

	var (
		currentToken *oauth2.Token
		currentState rotatingTokenState
		stateErr     error
	)
	err = config.FileTransaction(ctx, func() error {
		var legacy bool
		var loadErr error
		currentToken, currentState, legacy, loadErr = loadStoredToken(s.section)
		if loadErr != nil {
			return loadErr
		}
		if legacy {
			currentState = readyState(currentToken)
			if err := saveStoredToken(s.section, currentToken, currentState); err != nil {
				return err
			}
		}
		switch currentState.Status {
		case tokenStateUncertain:
			stateErr = terminalTokenError(s.section, ErrTokenStateUncertain)
			return nil
		case tokenStateReauthenticationRequired:
			stateErr = terminalTokenError(s.section, ErrTokenReauthenticationRequired)
			return nil
		case tokenStateInFlight:
			currentState = uncertainState(currentState)
			if err := saveStoredToken(s.section, currentToken, currentState); err != nil {
				return err
			}
			stateErr = terminalTokenError(s.section, ErrTokenStateUncertain)
			return nil
		case tokenStateReady:
		default:
			return fmt.Errorf("unknown rotating token state %q", currentState.Status)
		}
		if currentState.Generation != generation {
			return nil
		}
		currentState = reauthenticationRequiredState(currentState)
		if err := saveStoredToken(s.section, currentToken, currentState); err != nil {
			return err
		}
		stateErr = terminalTokenError(s.section, ErrTokenReauthenticationRequired)
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	s.token = currentToken
	s.state = currentState
	if stateErr != nil {
		return nil, 0, stateErr
	}
	return cloneOAuthToken(s.token), s.state.Generation, nil
}

// RefreshAfter returns the remaining time until this source should check for
// a refresh. A non-positive result means that it should check immediately.
func (s *RotatingTokenSource) RefreshAfter() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token == nil || s.token.Expiry.IsZero() {
		return 0
	}
	return time.Until(s.token.Expiry.Add(-s.state.RefreshLead))
}

func (s *RotatingTokenSource) loadOrMigrate(ctx context.Context) error {
	unlock, err := config.LockConfigSection(ctx, s.section)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()

	var (
		token  *oauth2.Token
		state  rotatingTokenState
		legacy bool
	)
	if err = config.FileReadTransaction(ctx, func() error {
		var loadErr error
		token, state, legacy, loadErr = loadStoredToken(s.section)
		return loadErr
	}); err != nil {
		return err
	}
	if !legacy && state.Status != tokenStateInFlight {
		s.token = token
		s.state = state
		return nil
	}

	return config.FileTransaction(ctx, func() error {
		currentToken, currentState, currentLegacy, err := loadStoredToken(s.section)
		if err != nil {
			return err
		}
		if currentLegacy {
			currentState = readyState(currentToken)
			if err := saveStoredToken(s.section, currentToken, currentState); err != nil {
				return err
			}
		} else if currentState.Status == tokenStateInFlight {
			currentState = uncertainState(currentState)
			if err := saveStoredToken(s.section, currentToken, currentState); err != nil {
				return err
			}
		}
		s.token = currentToken
		s.state = currentState
		return nil
	})
}

func (s *RotatingTokenSource) refresh(ctx context.Context, force bool, expectedGeneration uint64) (*oauth2.Token, uint64, error) {
	unlock, err := config.LockConfigSection(ctx, s.section)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = unlock() }()

	var (
		currentToken *oauth2.Token
		currentState rotatingTokenState
		exchange     bool
		owner        string
		stateErr     error
	)
	err = config.FileTransaction(ctx, func() error {
		var err error
		var legacy bool
		currentToken, currentState, legacy, err = loadStoredToken(s.section)
		if err != nil {
			return err
		}
		if legacy {
			currentState = readyState(currentToken)
			if err := saveStoredToken(s.section, currentToken, currentState); err != nil {
				return err
			}
		}
		switch currentState.Status {
		case tokenStateUncertain:
			return terminalTokenError(s.section, ErrTokenStateUncertain)
		case tokenStateReauthenticationRequired:
			return terminalTokenError(s.section, ErrTokenReauthenticationRequired)
		case tokenStateInFlight:
			currentState = uncertainState(currentState)
			if err := saveStoredToken(s.section, currentToken, currentState); err != nil {
				return err
			}
			stateErr = terminalTokenError(s.section, ErrTokenStateUncertain)
			return nil
		case tokenStateReady:
		default:
			return fmt.Errorf("unknown rotating token state %q", currentState.Status)
		}

		if force && currentState.Generation != expectedGeneration {
			s.token = currentToken
			s.state = currentState
			return nil
		}
		if !force && tokenUsable(currentToken, currentState) {
			s.token = currentToken
			s.state = currentState
			return nil
		}
		if currentToken.RefreshToken == "" {
			currentState.Status = tokenStateReauthenticationRequired
			if err := saveStoredToken(s.section, currentToken, currentState); err != nil {
				return err
			}
			stateErr = terminalTokenError(s.section, ErrTokenReauthenticationRequired)
			return nil
		}

		owner, err = tokenOwner()
		if err != nil {
			return err
		}
		currentState.Status = tokenStateInFlight
		currentState.Owner = owner
		currentState.RefreshTokenHash = tokenHash(currentToken.RefreshToken)
		if err := saveStoredToken(s.section, currentToken, currentState); err != nil {
			return err
		}
		exchange = true
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	if stateErr != nil {
		s.token = currentToken
		s.state = currentState
		return nil, 0, stateErr
	}
	if !exchange {
		return cloneOAuthToken(s.token), s.state.Generation, nil
	}

	nextToken, exchangeErr := s.exchange(ctx, currentToken.RefreshToken)
	failure := exchangeFailure(exchangeErr)
	if exchangeErr == nil && !completeToken(nextToken) {
		exchangeErr = errors.New("token exchange returned incomplete token")
		failure = ExchangeFailureUncertain
	}

	var exchangeStateErr error
	err = config.FileTransaction(stateCommitContext(ctx), func() error {
		persistedToken, persistedState, legacy, err := loadStoredToken(s.section)
		if err != nil {
			return err
		}
		if legacy || persistedState.Status != tokenStateInFlight || persistedState.Owner != owner || persistedState.Generation != currentState.Generation {
			return fmt.Errorf("rotating token state changed while exchange was in flight")
		}
		if exchangeErr != nil {
			switch failure {
			case ExchangeFailureNotSent:
				persistedState = readyStateWithGeneration(persistedToken, persistedState.Generation)
			case ExchangeFailureReauthenticationRequired:
				persistedState = reauthenticationRequiredState(persistedState)
			default:
				persistedState = uncertainState(persistedState)
			}
			if err := saveStoredToken(s.section, persistedToken, persistedState); err != nil {
				return err
			}
			s.token = persistedToken
			s.state = persistedState
			exchangeStateErr = exchangeErr
			return nil
		}

		persistedState = readyStateWithGeneration(nextToken, persistedState.Generation+1)
		if err := saveStoredToken(s.section, nextToken, persistedState); err != nil {
			return err
		}
		s.token = nextToken
		s.state = persistedState
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	if exchangeStateErr != nil {
		return nil, 0, exchangeStateErr
	}
	return cloneOAuthToken(s.token), s.state.Generation, nil
}

const (
	tokenStateReady                    = "ready"
	tokenStateInFlight                 = "in_flight"
	tokenStateUncertain                = "uncertain"
	tokenStateReauthenticationRequired = "reauth_required"
)

func loadStoredToken(section string) (*oauth2.Token, rotatingTokenState, bool, error) {
	tokenString, ok := config.FileGetValue(section, config.ConfigToken)
	if !ok || tokenString == "" {
		return nil, rotatingTokenState{}, false, fmt.Errorf("%w - run \"rclone config reconnect %s:\"", ErrNoPersistentToken, section)
	}
	var stored storedRotatingToken
	if err := json.Unmarshal([]byte(tokenString), &stored); err != nil {
		return nil, rotatingTokenState{}, false, err
	}
	token := &oauth2.Token{
		AccessToken:  stored.AccessToken,
		TokenType:    stored.TokenType,
		RefreshToken: stored.RefreshToken,
		Expiry:       stored.Expiry,
		ExpiresIn:    stored.ExpiresIn,
	}
	if token.AccessToken == "" {
		var old oldToken
		if err := json.Unmarshal([]byte(tokenString), &old); err != nil {
			return nil, rotatingTokenState{}, false, err
		}
		if old.AccessToken != "" {
			token.AccessToken = old.AccessToken
			token.RefreshToken = old.RefreshToken
			token.Expiry = old.Expiry
		}
	}
	if token.AccessToken == "" && token.RefreshToken == "" {
		return nil, rotatingTokenState{}, false, errors.New("token has no access or refresh token")
	}
	if stored.State == nil {
		return token, rotatingTokenState{}, true, nil
	}
	state := *stored.State
	if state.Version != rotatingTokenStateVersion {
		return nil, rotatingTokenState{}, false, fmt.Errorf("unsupported rotating token state version %d", state.Version)
	}
	return token, state, false, nil
}

func saveStoredToken(section string, token *oauth2.Token, state rotatingTokenState) error {
	stored := storedRotatingToken{
		AccessToken:  token.AccessToken,
		TokenType:    token.TokenType,
		RefreshToken: token.RefreshToken,
		Expiry:       token.Expiry,
		ExpiresIn:    token.ExpiresIn,
		State:        &state,
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	config.FileSetValue(section, config.ConfigToken, string(encoded))
	return nil
}

func readyState(token *oauth2.Token) rotatingTokenState {
	return readyStateWithGeneration(token, 1)
}

func readyStateWithGeneration(token *oauth2.Token, generation uint64) rotatingTokenState {
	return rotatingTokenState{
		Version:     rotatingTokenStateVersion,
		Generation:  generation,
		Status:      tokenStateReady,
		RefreshLead: refreshLead(token),
	}
}

func uncertainState(state rotatingTokenState) rotatingTokenState {
	state.Status = tokenStateUncertain
	state.Owner = ""
	return state
}

func reauthenticationRequiredState(state rotatingTokenState) rotatingTokenState {
	state.Status = tokenStateReauthenticationRequired
	state.Owner = ""
	return state
}

func tokenUsable(token *oauth2.Token, state rotatingTokenState) bool {
	if token == nil || token.AccessToken == "" || token.Expiry.IsZero() {
		return false
	}
	return time.Now().Before(token.Expiry.Add(-state.RefreshLead))
}

func (s *RotatingTokenSource) needsRefresh() bool {
	return !tokenUsable(s.token, s.state)
}

func refreshLead(token *oauth2.Token) time.Duration {
	if token == nil || token.Expiry.IsZero() {
		return 0
	}
	lifetime := time.Until(token.Expiry)
	if lifetime <= 0 {
		return 0
	}
	lead := maxRefreshLead
	if lifetime <= lead {
		lead = lifetime / 2
	}
	if lifetime > minRefreshLead && lead < minRefreshLead {
		lead = minRefreshLead
	}
	if lead >= lifetime {
		lead = lifetime / 2
	}
	return lead
}

func completeToken(token *oauth2.Token) bool {
	return token != nil && token.AccessToken != "" && token.RefreshToken != "" && !token.Expiry.IsZero()
}

func cloneOAuthToken(token *oauth2.Token) *oauth2.Token {
	if token == nil {
		return nil
	}
	copy := *token
	return &copy
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func tokenOwner() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("failed to create token refresh owner: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

func terminalTokenError(name string, err error) error {
	return fmt.Errorf("%w - run \"rclone config reconnect %s:\"", err, name)
}

// stateCommitContext keeps a claimed one-time credential from being stranded
// in flight when the caller cancels after an exchange has started.
func stateCommitContext(ctx context.Context) context.Context {
	if ctx == nil || ctx.Err() != nil {
		return context.Background()
	}
	return ctx
}

func exchangeFailure(err error) ExchangeFailure {
	if err == nil {
		return ExchangeFailureUncertain
	}
	var classified ExchangeError
	if errors.As(err, &classified) {
		return classified.ExchangeFailure()
	}
	return ExchangeFailureUncertain
}

// Check the interface is satisfied.
var _ oauth2.TokenSource = (*RotatingTokenSource)(nil)
