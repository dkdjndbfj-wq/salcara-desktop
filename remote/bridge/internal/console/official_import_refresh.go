package console

import (
	"context"
	"sync"
	"time"

	"salcara/bridge/internal/codexaccounts"
)

// Save before exchange so rotations and failed writes stay under the existing
// singleflight/RetrySave owner. Preview never calls this method.
func refreshImportedCredentials(ctx context.Context, accounts *codexaccounts.Service, p *codexaccounts.PreparedImport, rows []codexaccounts.Account) ([]codexaccounts.Account, bool) {
	needsRefresh := false
	for i := range rows {
		needsRefresh = needsRefresh || p.RefreshOnly(i)
	}
	if !needsRefresh {
		return rows, false
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var workers sync.WaitGroup
	jobs := make(chan int)
	for worker := 0; worker < 4; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				if ctx.Err() == nil {
					_, _ = accounts.Refresh(ctx, rows[index].ID)
				}
			}
		}()
	}
	for i := range rows {
		if !p.RefreshOnly(i) {
			continue
		}
		select {
		case jobs <- i:
		case <-ctx.Done():
		}
	}
	close(jobs)
	workers.Wait()
	failed := false
	for i := range rows {
		if !p.RefreshOnly(i) {
			continue
		}
		current, err := accounts.Get(rows[i].ID)
		if err == nil && current.IdentityEpoch == rows[i].IdentityEpoch {
			rows[i] = current
		}
		if err != nil || current.IdentityEpoch != rows[i].IdentityEpoch || current.Status != codexaccounts.StatusReady {
			failed = true
		}
	}
	return rows, failed
}
