// Copyright 2023 Harness, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package database

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/harness/gitness/app/store"
	"github.com/harness/gitness/store/database"
	"github.com/harness/gitness/store/database/dbtx"
	"github.com/harness/gitness/types"
	"github.com/harness/gitness/types/enum"

	"github.com/Masterminds/squirrel"
	"github.com/jmoiron/sqlx"
	"github.com/pkg/errors"
)

var _ store.CheckStore = (*CheckStore)(nil)

// NewCheckStore returns a new CheckStore.
func NewCheckStore(
	db *sqlx.DB,
	pCache store.PrincipalInfoCache,
) *CheckStore {
	return &CheckStore{
		db:     db,
		pCache: pCache,
	}
}

// CheckStore implements store.CheckStore backed by a relational database.
type CheckStore struct {
	db     *sqlx.DB
	pCache store.PrincipalInfoCache
}

const (
	checkColumns = `
		 check_id
		,check_created_by
		,check_created
		,check_updated
		,check_repo_id
		,check_commit_sha
		,check_uid
		,check_status
		,check_summary
		,check_link
		,check_payload
		,check_metadata
		,check_payload_kind
		,check_payload_version`
)

type check struct {
	ID             int64                 `db:"check_id"`
	CreatedBy      int64                 `db:"check_created_by"`
	Created        int64                 `db:"check_created"`
	Updated        int64                 `db:"check_updated"`
	RepoID         int64                 `db:"check_repo_id"`
	CommitSHA      string                `db:"check_commit_sha"`
	UID            string                `db:"check_uid"`
	Status         enum.CheckStatus      `db:"check_status"`
	Summary        string                `db:"check_summary"`
	Link           string                `db:"check_link"`
	Payload        json.RawMessage       `db:"check_payload"`
	Metadata       json.RawMessage       `db:"check_metadata"`
	PayloadKind    enum.CheckPayloadKind `db:"check_payload_kind"`
	PayloadVersion string                `db:"check_payload_version"`
}

// Upsert creates new or updates an existing status check result.
func (s *CheckStore) Upsert(ctx context.Context, check *types.Check) error {
	const sqlQuery = `
	INSERT INTO checks (
		 check_created_by
		,check_created
		,check_updated
		,check_repo_id
		,check_commit_sha
		,check_uid
		,check_status
		,check_summary
		,check_link
		,check_payload
		,check_metadata
		,check_payload_kind
		,check_payload_version
	) VALUES (
		 :check_created_by
		,:check_created
		,:check_updated
		,:check_repo_id
		,:check_commit_sha
		,:check_uid
		,:check_status
		,:check_summary
		,:check_link
		,:check_payload
		,:check_metadata
		,:check_payload_kind
		,:check_payload_version
	)
	ON CONFLICT (check_repo_id, check_commit_sha, check_uid) DO
	UPDATE SET
		 check_updated = :check_updated
		,check_status = :check_status
		,check_summary = :check_summary
		,check_link = :check_link
		,check_payload = :check_payload
		,check_metadata = :check_metadata
		,check_payload_kind = :check_payload_kind
		,check_payload_version = :check_payload_version
	RETURNING check_id, check_created_by, check_created`

	db := dbtx.GetAccessor(ctx, s.db)

	query, arg, err := db.BindNamed(sqlQuery, mapInternalCheck(check))
	if err != nil {
		return database.ProcessSQLErrorf(err, "Failed to bind status check object")
	}

	if err = db.QueryRowContext(ctx, query, arg...).Scan(&check.ID, &check.CreatedBy, &check.Created); err != nil {
		return database.ProcessSQLErrorf(err, "Upsert query failed")
	}

	return nil
}

// Count counts status check results for a specific commit in a repo.
func (s *CheckStore) Count(ctx context.Context,
	repoID int64,
	commitSHA string,
	opts types.CheckListOptions,
) (int, error) {
	stmt := database.Builder.
		Select("count(*)").
		From("checks").
		Where("check_repo_id = ?", repoID).
		Where("check_commit_sha = ?", commitSHA)

	stmt = s.applyOpts(stmt, opts.Query)

	sql, args, err := stmt.ToSql()
	if err != nil {
		return 0, errors.Wrap(err, "Failed to convert query to sql")
	}

	db := dbtx.GetAccessor(ctx, s.db)

	var count int
	err = db.QueryRowContext(ctx, sql, args...).Scan(&count)
	if err != nil {
		return 0, database.ProcessSQLErrorf(err, "Failed to execute count status checks query")
	}

	return count, nil
}

// List returns a list of status check results for a specific commit in a repo.
func (s *CheckStore) List(ctx context.Context,
	repoID int64,
	commitSHA string,
	opts types.CheckListOptions,
) ([]types.Check, error) {
	stmt := database.Builder.
		Select(checkColumns).
		From("checks").
		Where("check_repo_id = ?", repoID).
		Where("check_commit_sha = ?", commitSHA)

	stmt = s.applyOpts(stmt, opts.Query)

	stmt = stmt.
		Limit(database.Limit(opts.Size)).
		Offset(database.Offset(opts.Page, opts.Size)).
		OrderBy("check_updated desc")

	sql, args, err := stmt.ToSql()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to convert query to sql")
	}

	dst := make([]*check, 0)

	db := dbtx.GetAccessor(ctx, s.db)

	if err = db.SelectContext(ctx, &dst, sql, args...); err != nil {
		return nil, database.ProcessSQLErrorf(err, "Failed to execute list status checks query")
	}

	result, err := s.mapSliceCheck(ctx, dst)
	if err != nil {
		return nil, err
	}

	return result, nil
}

// ListRecent returns a list of recently executed status checks in a repository.
func (s *CheckStore) ListRecent(ctx context.Context,
	repoID int64,
	opts types.CheckRecentOptions,
) ([]string, error) {
	stmt := database.Builder.
		Select("distinct check_uid").
		From("checks").
		Where("check_repo_id = ?", repoID).
		Where("check_created > ?", opts.Since)

	stmt = s.applyOpts(stmt, opts.Query)

	stmt = stmt.OrderBy("check_uid")

	sql, args, err := stmt.ToSql()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to convert list recent status checks query to sql")
	}

	dst := make([]string, 0)

	db := dbtx.GetAccessor(ctx, s.db)

	if err = db.SelectContext(ctx, &dst, sql, args...); err != nil {
		return nil, database.ProcessSQLErrorf(err, "Failed to execute list recent status checks query")
	}

	return dst, nil
}

// ListResults returns a list of status check results for a specific commit in a repo.
func (s *CheckStore) ListResults(ctx context.Context,
	repoID int64,
	commitSHA string,
) ([]types.CheckResult, error) {
	const checkColumns = "check_uid, check_status"
	stmt := database.Builder.
		Select(checkColumns).
		From("checks").
		Where("check_repo_id = ?", repoID).
		Where("check_commit_sha = ?", commitSHA)

	sql, args, err := stmt.ToSql()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to convert query to sql")
	}

	result := make([]types.CheckResult, 0)

	db := dbtx.GetAccessor(ctx, s.db)

	if err = db.SelectContext(ctx, &result, sql, args...); err != nil {
		return nil, database.ProcessSQLErrorf(err, "Failed to execute list status checks results query")
	}

	return result, nil
}

func (*CheckStore) applyOpts(stmt squirrel.SelectBuilder, query string) squirrel.SelectBuilder {
	if query != "" {
		stmt = stmt.Where("LOWER(check_uid) LIKE ?", fmt.Sprintf("%%%s%%", strings.ToLower(query)))
	}

	return stmt
}

func mapInternalCheck(c *types.Check) *check {
	m := &check{
		ID:             c.ID,
		CreatedBy:      c.CreatedBy,
		Created:        c.Created,
		Updated:        c.Updated,
		RepoID:         c.RepoID,
		CommitSHA:      c.CommitSHA,
		UID:            c.UID,
		Status:         c.Status,
		Summary:        c.Summary,
		Link:           c.Link,
		Payload:        c.Payload.Data,
		Metadata:       c.Metadata,
		PayloadKind:    c.Payload.Kind,
		PayloadVersion: c.Payload.Version,
	}

	return m
}

func mapCheck(c *check) types.Check {
	return types.Check{
		ID:        c.ID,
		CreatedBy: c.CreatedBy,
		Created:   c.Created,
		Updated:   c.Updated,
		RepoID:    c.RepoID,
		CommitSHA: c.CommitSHA,
		UID:       c.UID,
		Status:    c.Status,
		Summary:   c.Summary,
		Link:      c.Link,
		Metadata:  c.Metadata,
		Payload: types.CheckPayload{
			Version: c.PayloadVersion,
			Kind:    c.PayloadKind,
			Data:    c.Payload,
		},
		ReportedBy: types.PrincipalInfo{},
	}
}

func (s *CheckStore) mapSliceCheck(ctx context.Context, checks []*check) ([]types.Check, error) {
	// collect all principal IDs
	ids := make([]int64, len(checks))
	for i, req := range checks {
		ids[i] = req.CreatedBy
	}

	// pull principal infos from cache
	infoMap, err := s.pCache.Map(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("failed to load status check principal reporters: %w", err)
	}

	// attach the principal infos back to the slice items
	m := make([]types.Check, len(checks))
	for i, c := range checks {
		m[i] = mapCheck(c)
		if reportedBy, ok := infoMap[c.CreatedBy]; ok {
			m[i].ReportedBy = *reportedBy
		}
	}

	return m, nil
}
