package service

import (
	"context"
	"time"
)

// Existing marker writers remain compatible with the revision-bound store.
type OpenAIRescueLaneAtomicRepository interface {
	GraduateOpenAIRescue(context.Context, int64, any, []int64, map[string]any) (bool, error)
	UpdateOpenAIRescueMarker(context.Context, int64, any, any) (bool, error)
}

func (l *OpenAIRescueLane) updateMarkerFields(ctx context.Context, accountID int64, mutate func(*OpenAIRescueLaneMarker)) error {
	return l.updateMarkerFieldsForEpisode(ctx, accountID, time.Time{}, mutate)
}

func (l *OpenAIRescueLane) updateMarkerFieldsForEpisode(ctx context.Context, accountID int64, enteredAt time.Time, mutate func(*OpenAIRescueLaneMarker)) error {
	account, err := l.accounts.GetByID(ctx, accountID)
	if err != nil {
		return err
	}
	marker := GetOpenAIRescueLaneMarker(account)
	if marker == nil || (!enteredAt.IsZero() && !marker.EnteredAt.Equal(enteredAt)) {
		return ErrRescueLaneIneligible
	}
	mutate(marker)
	return l.commitMarker(ctx, account, marker, false)
}
