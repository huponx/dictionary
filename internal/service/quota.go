package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/huponx/dictionary/internal/settings"
	"github.com/huponx/dictionary/internal/store"
	"github.com/huponx/gocommon/apperror"
	dictionaryv1 "github.com/huponx/protogen/go/dictionary/v1"
)

// effectiveQuota is the limit in effect: the own limit of the user, else the one of the system settings.
func effectiveQuota(own *int64, system int64) int64 {
	if own != nil {
		return *own
	}
	return system
}

// byteSize writes a size for a message: a whole number of KiB, MiB or GiB as such, else in bytes.
func byteSize(n int) string {
	for _, u := range []struct {
		size int
		name string
	}{{1 << 30, "GiB"}, {1 << 20, "MiB"}, {1 << 10, "KiB"}} {
		if n >= u.size && n%u.size == 0 {
			return fmt.Sprintf("%d %s", n/u.size, u.name)
		}
	}
	return fmt.Sprintf("%d bytes", n)
}

// storageOf reads what the user stores of one kind and the limit in effect (the pick function chooses the kind).
func (s *Service) storageOf(ctx context.Context, user string, pick func(store.StorageUsage, settings.System) (used, limit int64)) (used, limit int64, err error) {
	u, err := s.store.GetStorageUsage(ctx, user)
	if errors.Is(err, store.ErrUnknownUser) {
		return 0, 0, apperror.Unauthenticated("unknown user").WithReason("unknown_user")
	}
	if err != nil {
		return 0, 0, internal(ctx, "storage usage", err)
	}
	sys, err := s.system(ctx)
	if err != nil {
		return 0, 0, err
	}
	used, limit = pick(u, sys)
	return used, limit, nil
}

func quotaTriple(c, i, a int64) *dictionaryv1.StorageQuota {
	return &dictionaryv1.StorageQuota{Content: c, Image: i, Audio: a}
}

func zeroIfNil(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// userQuota is what GetUserQuota and SetUserQuota answer.
func userQuota(u store.StorageUsage, sys settings.System) (used, override, effective *dictionaryv1.StorageQuota) {
	return quotaTriple(u.ContentBytes, u.ImageBytes, u.AudioBytes),
		quotaTriple(zeroIfNil(u.ContentQuota), zeroIfNil(u.ImageQuota), zeroIfNil(u.AudioQuota)),
		quotaTriple(effectiveQuota(u.ContentQuota, sys.ContentUserQuota), effectiveQuota(u.ImageQuota, sys.ImageUserQuota),
			effectiveQuota(u.AudioQuota, sys.AudioUserQuota))
}

func adminUser(id string) (string, error) {
	if _, err := uuid.Parse(id); err != nil {
		return "", apperror.InvalidArgument("user_id must be a uuid").WithReason("user_id_invalid")
	}
	return id, nil
}

// GetUserQuota returns what a user stores, the own limits of the user and the limits in effect.
func (s *Service) GetUserQuota(ctx context.Context, req *dictionaryv1.GetUserQuotaRequest) (*dictionaryv1.GetUserQuotaResponse, error) {
	user, err := adminUser(req.GetUserId())
	if err != nil {
		return nil, err
	}
	u, err := s.store.GetStorageUsage(ctx, user)
	if errors.Is(err, store.ErrUnknownUser) {
		return nil, apperror.NotFound("user not found").WithReason("user_not_found")
	}
	if err != nil {
		return nil, internal(ctx, "storage usage", err)
	}
	sys, err := s.system(ctx)
	if err != nil {
		return nil, err
	}
	used, override, effective := userQuota(u, sys)
	return &dictionaryv1.GetUserQuotaResponse{Used: used, Override: override, Effective: effective}, nil
}

// SetUserQuota sets the own limits of a user: 0 or absent removes the limit of a kind. A limit above the ceiling
// of the settings is refused.
func (s *Service) SetUserQuota(ctx context.Context, req *dictionaryv1.SetUserQuotaRequest) (*dictionaryv1.SetUserQuotaResponse, error) {
	user, err := adminUser(req.GetUserId())
	if err != nil {
		return nil, err
	}
	sys, err := s.system(ctx)
	if err != nil {
		return nil, err
	}
	var q store.Quotas
	for _, k := range []struct {
		name    string
		v       int64
		ceiling int64
		dst     **int64
	}{
		{"content", req.GetQuotas().GetContent(), sys.ContentUserQuotaCeiling, &q.Content},
		{"image", req.GetQuotas().GetImage(), sys.ImageUserQuotaCeiling, &q.Image},
		{"audio", req.GetQuotas().GetAudio(), sys.AudioUserQuotaCeiling, &q.Audio},
	} {
		switch {
		case k.v == 0:
		case k.v < settings.MinUserQuota || k.v > k.ceiling:
			return nil, apperror.InvalidArgument(fmt.Sprintf("%s quota must be between %d and %d bytes, or 0 for the default", k.name, settings.MinUserQuota, k.ceiling)).WithReason("quota_range", "kind", k.name, "min", fmt.Sprint(settings.MinUserQuota), "max", fmt.Sprint(k.ceiling))
		default:
			v := k.v
			*k.dst = &v
		}
	}
	if err := s.store.SetStorageQuota(ctx, user, q); err != nil {
		if errors.Is(err, store.ErrUnknownUser) {
			return nil, apperror.NotFound("user not found").WithReason("user_not_found")
		}
		return nil, internal(ctx, "set storage quota", err)
	}
	u, err := s.store.GetStorageUsage(ctx, user)
	if err != nil {
		return nil, internal(ctx, "storage usage", err)
	}
	used, override, effective := userQuota(u, sys)
	return &dictionaryv1.SetUserQuotaResponse{Used: used, Override: override, Effective: effective}, nil
}

// GetMyStorage returns what the user stores of each kind and the limit in effect for the user.
func (s *Service) GetMyStorage(ctx context.Context, _ *dictionaryv1.GetMyStorageRequest) (*dictionaryv1.GetMyStorageResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	u, err := s.store.GetStorageUsage(ctx, user)
	if errors.Is(err, store.ErrUnknownUser) {
		return nil, apperror.Unauthenticated("unknown user").WithReason("unknown_user")
	}
	if err != nil {
		return nil, internal(ctx, "storage usage", err)
	}
	sys, err := s.system(ctx)
	if err != nil {
		return nil, err
	}
	used, _, effective := userQuota(u, sys)
	return &dictionaryv1.GetMyStorageResponse{
		Content: &dictionaryv1.StorageUsage{Used: used.Content, Limit: effective.Content},
		Image:   &dictionaryv1.StorageUsage{Used: used.Image, Limit: effective.Image},
		Audio:   &dictionaryv1.StorageUsage{Used: used.Audio, Limit: effective.Audio},
	}, nil
}
