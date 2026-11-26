package service

import (
	"context"
	"database/sql"
	"errors"
)

// Task recovery checks durable enterprise ownership and restores the original
// group snapshot. Editing a key's current group cannot strand an accepted task.
func (s *SEPService) RestoreTaskGroup(ctx context.Context, key *APIKey, taskID string) (*APIKey, int64, error) {
	var groupID, accountID int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(group_id,0),COALESCE(account_id,0) FROM sep_executions WHERE enterprise_pk=$1 AND task_id=$2 AND kind='video'`, key.SEP.EnterprisePK, taskID).Scan(&groupID, &accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, ErrVideoTaskNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	copy := *key
	if groupID > 0 {
		group, err := s.groups.GetByID(ctx, groupID)
		if err != nil {
			return nil, 0, err
		}
		copy.GroupID = &groupID
		copy.Group = group
	}
	return &copy, accountID, nil
}
