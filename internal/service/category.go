package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/huponx/dictionary/internal/lookup"
	"github.com/huponx/dictionary/internal/store"
	"github.com/huponx/gocommon/apperror"
	dictionaryv1 "github.com/huponx/protogen/go/dictionary/v1"
	"google.golang.org/grpc/codes"
)

// categoryName trims and checks the name of a category: 1 to maxLen characters, no control characters.
func categoryName(name string, maxLen int) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxLen {
		return "", apperror.InvalidArgument(fmt.Sprintf("category name must be 1 to %d characters", maxLen)).WithReason("category_name_length", "max", fmt.Sprint(maxLen))
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", apperror.InvalidArgument("category name has control characters").WithReason("category_name_control_chars")
		}
	}
	return name, nil
}

// categoryErr maps the errors of the category methods of the store.
func categoryErr(ctx context.Context, what string, err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return apperror.NotFound("category not found").WithReason("category_not_found")
	case errors.Is(err, store.ErrCategoryExists):
		return apperror.Wrap(codes.AlreadyExists, CodeCategoryExists+": a category with this name exists", nil).WithReason("category_exists")
	case errors.Is(err, store.ErrLimit):
		return apperror.Wrap(codes.ResourceExhausted, CodeCategoryLimit+": maximum number of categories reached, delete some first", nil).WithReason("category_limit_reached")
	case errors.Is(err, store.ErrUnknownUser):
		return apperror.Unauthenticated("unknown user").WithReason("unknown_user")
	}
	return internal(ctx, what, err)
}

func toProtoCategory(c store.Category) *dictionaryv1.Category {
	return &dictionaryv1.Category{Id: c.ID, Name: c.Name, Total: int32(c.Total), NewTotal: int32(c.New)}
}

// ListCategories lists the categories of the user with their word counts.
func (s *Service) ListCategories(ctx context.Context, req *dictionaryv1.ListCategoriesRequest) (*dictionaryv1.ListCategoriesResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	group, err := s.groupOf(req.GetGroup())
	if err != nil {
		return nil, err
	}
	res, ok := s.cats.get(user, group.ID, time.Now())
	if !ok {
		res, err = s.store.ListCategories(ctx, user, group.ID)
		if err != nil {
			return nil, internal(ctx, "list categories", err)
		}
		s.cats.put(user, group.ID, time.Now(), res)
	}
	resp := &dictionaryv1.ListCategoriesResponse{
		Categories: make([]*dictionaryv1.Category, 0, len(res.List)), UncategorizedTotal: int32(res.UncatTotal),
		UncategorizedNewTotal: int32(res.UncatNew), CountsCapped: res.Capped,
	}
	for _, c := range res.List {
		resp.Categories = append(resp.Categories, toProtoCategory(c))
	}
	return resp, nil
}

// CreateCategory adds a category.
func (s *Service) CreateCategory(ctx context.Context, req *dictionaryv1.CreateCategoryRequest) (*dictionaryv1.CreateCategoryResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	defer s.cats.forget(user)
	sys, err := s.system(ctx)
	if err != nil {
		return nil, err
	}
	name, err := categoryName(req.GetName(), sys.MaxCategoryNameLength)
	if err != nil {
		return nil, err
	}
	c, err := s.store.CreateCategory(ctx, user, name, sys.MaxCategories)
	if err != nil {
		return nil, categoryErr(ctx, "create category", err)
	}
	return &dictionaryv1.CreateCategoryResponse{Category: toProtoCategory(c)}, nil
}

// RenameCategory renames a category.
func (s *Service) RenameCategory(ctx context.Context, req *dictionaryv1.RenameCategoryRequest) (*dictionaryv1.RenameCategoryResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	defer s.cats.forget(user)
	sys, err := s.system(ctx)
	if err != nil {
		return nil, err
	}
	name, err := categoryName(req.GetName(), sys.MaxCategoryNameLength)
	if err != nil {
		return nil, err
	}
	c, err := s.store.RenameCategory(ctx, user, req.GetId(), name)
	if err != nil {
		return nil, categoryErr(ctx, "rename category", err)
	}
	return &dictionaryv1.RenameCategoryResponse{Category: toProtoCategory(c)}, nil
}

// DeleteCategory deletes a category; its words stay in the list.
func (s *Service) DeleteCategory(ctx context.Context, req *dictionaryv1.DeleteCategoryRequest) (*dictionaryv1.DeleteCategoryResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	defer s.cats.forget(user)
	if err := s.store.DeleteCategory(ctx, user, req.GetId()); err != nil {
		return nil, categoryErr(ctx, "delete category", err)
	}
	return &dictionaryv1.DeleteCategoryResponse{}, nil
}

// UpdateWordCategories adds words to categories and takes them out of others.
func (s *Service) UpdateWordCategories(ctx context.Context, req *dictionaryv1.UpdateWordCategoriesRequest) (*dictionaryv1.UpdateWordCategoriesResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	defer s.cats.forget(user)
	group, err := s.groupOf(req.GetGroup())
	if err != nil {
		return nil, err
	}
	sys, err := s.system(ctx)
	if err != nil {
		return nil, err
	}
	if len(req.GetWords()) == 0 || len(req.GetWords()) > maxBatchWords {
		return nil, apperror.InvalidArgument(fmt.Sprintf("send between 1 and %d words", maxBatchWords)).WithReason("word_batch_range", "max", fmt.Sprint(maxBatchWords))
	}
	if len(req.GetAddCategoryIds())+len(req.GetRemoveCategoryIds()) > sys.MaxCategories*2 {
		return nil, apperror.InvalidArgument("too many categories").WithReason("too_many_categories")
	}
	words := make([]string, 0, len(req.GetWords()))
	for _, w := range req.GetWords() {
		n, err := lookup.NormalizeWord(w)
		if err != nil {
			return nil, lookupErr(ctx, err)
		}
		words = append(words, n)
	}
	n, err := s.store.SetWordCategories(ctx, user, group.ID, words, req.GetAddCategoryIds(), req.GetRemoveCategoryIds())
	if err != nil {
		return nil, categoryErr(ctx, "update word categories", err)
	}
	return &dictionaryv1.UpdateWordCategoriesResponse{Updated: int32(n)}, nil
}
