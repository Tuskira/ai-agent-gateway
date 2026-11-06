package dptest

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// userIdentityStore is the in-memory store.UserIdentityStore, with the
// semantics pkg/store/storetest asserts.
type userIdentityStore Store

func (u *userIdentityStore) Create(_ context.Context, ni *store.UserIdentity) error {
	s := (*Store)(u)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.idents {
		if e.TenantID != ni.TenantID || e.Provider != ni.Provider || e.Issuer != ni.Issuer {
			continue
		}
		if e.Subject == ni.Subject || e.UserID == ni.UserID {
			return store.ErrConflict
		}
	}
	now := time.Now()
	ni.ID = uuid.NewString()
	ni.CreatedAt, ni.LastLoginAt = now, now
	cp := *ni
	s.idents[ni.ID] = &cp
	return nil
}

func (u *userIdentityStore) find(match func(*store.UserIdentity) bool) (*store.UserIdentity, error) {
	s := (*Store)(u)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.idents {
		if match(e) {
			cp := *e
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (u *userIdentityStore) GetBySubject(_ context.Context, tenantID, provider, issuer, subject string) (*store.UserIdentity, error) {
	return u.find(func(e *store.UserIdentity) bool {
		return e.TenantID == tenantID && e.Provider == provider && e.Issuer == issuer && e.Subject == subject
	})
}

func (u *userIdentityStore) GetByUser(_ context.Context, tenantID, provider, issuer, userID string) (*store.UserIdentity, error) {
	return u.find(func(e *store.UserIdentity) bool {
		return e.TenantID == tenantID && e.Provider == provider && e.Issuer == issuer && e.UserID == userID
	})
}

func (u *userIdentityStore) ListByUser(_ context.Context, tenantID, userID string) ([]*store.UserIdentity, error) {
	s := (*Store)(u)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []*store.UserIdentity{}
	for _, e := range s.idents {
		if e.TenantID == tenantID && e.UserID == userID {
			cp := *e
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Issuer < out[j].Issuer
	})
	return out, nil
}

func (u *userIdentityStore) RecordLogin(_ context.Context, tenantID, id, email string, at time.Time) error {
	s := (*Store)(u)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.idents[id]
	if !ok || e.TenantID != tenantID {
		return store.ErrNotFound
	}
	e.LastLoginAt, e.Email = at, email
	return nil
}

func (u *userIdentityStore) Delete(_ context.Context, tenantID, id string) error {
	s := (*Store)(u)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.idents[id]
	if !ok || e.TenantID != tenantID {
		return store.ErrNotFound
	}
	delete(s.idents, id)
	return nil
}
